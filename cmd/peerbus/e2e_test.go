package main

import (
	"bufio"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitaminmoo/peerbus/internal/identity"
)

type user struct {
	name  string
	cfg   string
	state string
	env   []string
	store identity.Store
}

func newUser(t *testing.T, dir, name, rv string) *user {
	t.Helper()
	u := &user{name: name, cfg: filepath.Join(dir, name, "config"), state: filepath.Join(dir, name, "state")}
	u.store = identity.Store{Dir: u.cfg}
	if _, err := u.store.Identity(); err != nil {
		t.Fatal(err)
	}
	if err := u.store.Update(func(p *identity.Peers) error { p.Self.Name = name; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(u.cfg, "client.json"), []byte(fmt.Sprintf(`{"rendezvous": %q, "stun": []}`, rv)), 0o600); err != nil {
		t.Fatal(err)
	}
	u.env = append(os.Environ(),
		"PEERBUS_CONFIG_DIR="+u.cfg,
		"PEERBUS_STATE_DIR="+u.state,
		"CLAUDE_CONFIG_DIR="+filepath.Join(dir, name, "claude"),
	)
	return u
}

func pairUsers(t *testing.T, a, b *user) {
	t.Helper()
	secret := identity.NewSecret()
	add := func(s identity.Store, other *user) {
		k, err := other.store.Identity()
		if err != nil {
			t.Fatal(err)
		}
		err = s.Update(func(p *identity.Peers) error {
			p.Peers = append(p.Peers, &identity.Peer{Name: other.name, Key: identity.Key(k.Public().(ed25519.PublicKey)), Secret: secret, Enabled: true, Perms: identity.DefaultPerms(), PairedAt: time.Now()})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	add(a.store, b)
	add(b.store, a)
}

// TestEndToEnd builds the binary, runs a rendezvous server, and drives
// `peerbus channel` processes over MCP stdio the way Claude Code does: two
// sessions of alice on one "machine" and one of bob on another.
func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "peerbus")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	uc, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	rv := uc.LocalAddr().String()
	uc.Close()
	rvCmd := exec.Command(bin, "rendezvous", "-listen", rv)
	rvCmd.Stderr = &prefixWriter{t: t, prefix: "rv"}
	if err := rvCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rvCmd.Process.Kill(); rvCmd.Wait() })

	alice := newUser(t, dir, "alice", rv)
	bob := newUser(t, dir, "bob", rv)
	pairUsers(t, alice, bob)

	one := startChannel(t, bin, alice, "one", "aaaaaaaa-1111", "busy")
	two := startChannel(t, bin, alice, "two", "bbbbbbbb-2222", "idle")
	b1 := startChannel(t, bin, bob, "bobsess", "cccccccc-3333", "idle")

	var listing string
	for i := 0; i < 150; i++ {
		listing = one.callText(t, "list_peers", map[string]any{})
		if strings.Contains(listing, "alice/two#bbbbbb") && strings.Contains(listing, "bob/bobsess#cccccc") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Everyone is on this machine, so even bob is reached over loopback.
	for _, want := range []string{"This session is alice/one#aaaaaa", "alice/two#bbbbbb  idle  host=", "via=local", "bob/bobsess#cccccc  idle"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("listing lacks %q:\n%s", want, listing)
		}
	}
	if strings.Contains(listing, "cwd="+bob.state) {
		t.Fatalf("bob's details leaked:\n%s", listing)
	}

	// one -> two arrives as a channel notification.
	out := one.callText(t, "send", map[string]any{"to": "two", "text": "build is green\nmerged #42"})
	if !strings.HasPrefix(out, "Delivered to alice/two#bbbbbb") {
		t.Fatalf("send: %s", out)
	}
	n := two.notification(t)
	meta := n.Params.Meta
	if n.Params.Content != "build is green\nmerged #42" || meta["from"] != "alice/one#aaaaaa" || meta["foreign"] != "false" || meta["kind"] != "message" {
		t.Fatalf("bad notification %+v", n)
	}
	if _, ok := meta["reply_to"]; ok {
		t.Fatalf("a new message carries reply_to: %+v", meta)
	}
	two.callText(t, "send", map[string]any{"to": meta["from"], "text": "thanks", "thread": meta["thread"], "reply_to": meta["msg_id"]})
	if r := one.notification(t); r.Params.Content != "thanks" || r.Params.Meta["thread"] != meta["thread"] || r.Params.Meta["reply_to"] != meta["msg_id"] {
		t.Fatalf("bad reply %+v", r)
	}
	// Each session can look over its conversations, and only its own.
	if l := one.callText(t, "threads", map[string]any{}); !strings.Contains(l, "thread "+meta["thread"]+"  2 messages") {
		t.Fatalf("threads:\n%s", l)
	}
	shown := one.callText(t, "threads", map[string]any{"thread": meta["thread"]})
	if !strings.Contains(shown, "merged #42") || !strings.Contains(shown, "reply_to="+meta["msg_id"]) {
		t.Fatalf("thread:\n%s", shown)
	}
	if l := b1.callText(t, "threads", map[string]any{}); l != "No conversations yet." {
		t.Fatalf("bob's session sees:\n%s", l)
	}

	// Idle subscription driven by the registry file.
	if out := two.callText(t, "subscribe_idle", map[string]any{"peer": "one"}); !strings.HasPrefix(out, "Subscribed") {
		t.Fatalf("subscribe: %s", out)
	}
	writeRegistry(t, alice, "one", "aaaaaaaa-1111", "idle")
	if r := two.notification(t); r.Params.Meta["kind"] != "idle" || r.Params.Meta["state"] != "idle" {
		t.Fatalf("bad idle notice %+v", r)
	}

	// Renaming in the registry shows up for peers.
	writeRegistryName(t, alice, "one", "aaaaaaaa-1111", "renamed", "idle")
	time.Sleep(800 * time.Millisecond)
	if l := two.callText(t, "list_peers", map[string]any{}); !strings.Contains(l, "alice/renamed#aaaaaa") {
		t.Fatalf("rename not visible:\n%s", l)
	}

	// bob -> alice is held: alice's session hears that something waits, the
	// text stays in her inbox for the CLI.
	for i := 0; i < 150; i++ {
		if l := b1.callText(t, "list_peers", map[string]any{}); strings.Contains(l, "alice/two#bbbbbb") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	out = b1.callText(t, "send", map[string]any{"to": "alice/two", "text": "can I borrow ghidra?"})
	if !strings.HasPrefix(out, "Held for approval") {
		t.Fatalf("send to alice: %s", out)
	}
	h := two.notification(t)
	if h.Params.Meta["kind"] != "held" || strings.Contains(h.Params.Content, "ghidra") || h.Params.Meta["from"] != "bob/bobsess#cccccc" {
		t.Fatalf("held notice: %+v", h)
	}
	inboxCmd := exec.Command(bin, "inbox")
	inboxCmd.Env = alice.env
	if ib, err := inboxCmd.CombinedOutput(); err != nil || !strings.Contains(string(ib), "from bob/bobsess#cccccc") || !strings.Contains(string(ib), "can I borrow ghidra?") {
		t.Fatalf("inbox: %v\n%s", err, ib)
	}

	// The CLI can't approve without a terminal.
	approve := exec.Command(bin, "approve", h.Params.Meta["held_id"])
	approve.Env = alice.env
	if out, err := approve.CombinedOutput(); err == nil || !strings.Contains(string(out), "interactively") {
		t.Fatalf("approve without a tty: %v %s", err, out)
	}

	// alice lets bob through; the running sessions pick it up.
	if err := alice.store.Update(func(p *identity.Peers) error { p.ByName("bob").Perms.Deliver = "auto"; return nil }); err != nil {
		t.Fatal(err)
	}
	var delivered bool
	for i := 0; i < 30 && !delivered; i++ {
		out = b1.callText(t, "send", map[string]any{"to": "alice/two", "text": "ping"})
		delivered = strings.HasPrefix(out, "Delivered to alice/two")
		if !delivered {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !delivered {
		t.Fatalf("after deliver=auto: %s", out)
	}
	// Attempts made before the reload landed left held notices; skip them.
	for {
		r := two.notification(t)
		if r.Params.Meta["kind"] == "held" {
			continue
		}
		if r.Params.Content != "ping" || r.Params.Meta["foreign"] != "true" || r.Params.Meta["from"] != "bob/bobsess#cccccc" {
			t.Fatalf("foreign delivery: %+v", r)
		}
		break
	}

	// The CLI sees the same world.
	peersCmd := exec.Command(bin, "peers")
	peersCmd.Env = alice.env
	pb, err := peersCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(pb), "alice/renamed#aaaaaa") || !strings.Contains(string(pb), "bob/bobsess#cccccc") {
		t.Fatalf("peers: %v\n%s", err, pb)
	}
	sendCmd := exec.Command(bin, "send", "two", "from the shell")
	sendCmd.Env = alice.env
	if sb, err := sendCmd.CombinedOutput(); err != nil || !strings.Contains(string(sb), "Delivered to alice/two") {
		t.Fatalf("cli send: %v\n%s", err, sb)
	}
	if r := two.notification(t); r.Params.Content != "from the shell" || r.Params.Meta["from"] != "alice/cli" {
		t.Fatalf("cli delivery: %+v", r)
	}
}

// TestReload swaps the binary under running `peerbus channel` processes and
// asks them to switch. Like Claude Code, the test keeps talking to the same
// pipes and never repeats the handshake.
func TestReload(t *testing.T) {
	dir := t.TempDir()
	build := filepath.Join(dir, "build", "peerbus")
	if out, err := exec.Command("go", "build", "-o", build, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	bin := filepath.Join(dir, "bin", "peerbus")
	// Installs atomically, as `go install` does, so running ones keep the old
	// file. Each install is a new file, which is how sessions tell.
	install := func(content []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatal(err)
		}
		tmp := bin + ".new"
		if err := os.WriteFile(tmp, content, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, bin); err != nil {
			t.Fatal(err)
		}
	}
	good, err := os.ReadFile(build)
	if err != nil {
		t.Fatal(err)
	}
	install(good)

	uc, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	rv := uc.LocalAddr().String()
	uc.Close()
	rvCmd := exec.Command(build, "rendezvous", "-listen", rv)
	if err := rvCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rvCmd.Process.Kill(); rvCmd.Wait() })

	alice := newUser(t, dir, "alice", rv)
	one := startChannel(t, bin, alice, "one", "aaaaaaaa-1111", "idle")
	two := startChannel(t, bin, alice, "two", "bbbbbbbb-2222", "idle")
	waitListing := func(p *channelProc, want string) string {
		t.Helper()
		var l string
		for i := 0; i < 150; i++ {
			if l = p.callText(t, "list_peers", map[string]any{}); strings.Contains(l, want) {
				return l
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("listing lacks %q:\n%s", want, l)
		return ""
	}
	waitListing(one, "alice/two#bbbbbb")
	waitListing(two, "alice/one#aaaaaa")
	one.callText(t, "advertise", map[string]any{"resources": []map[string]any{{"kind": "note", "name": "still-here"}}})
	waitListing(two, "still-here")

	reload := func() (string, error) {
		cmd := exec.Command(build, "reload") // not bin, which the test breaks on purpose
		cmd.Env = alice.env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	pid := one.cmd.Process.Pid

	// Nothing new on disk: nothing to do, and nobody drops off the mesh.
	if out, err := reload(); err != nil || strings.Count(out, "already running") != 2 {
		t.Fatalf("reload with nothing new: %v\n%s", err, out)
	}

	// A binary that can't take over is refused and everyone carries on.
	install([]byte("#!/bin/sh\nexit 1\n"))
	if out, err := reload(); err == nil || strings.Count(out, "can't take over") != 2 {
		t.Fatalf("reload to a bad binary: %v\n%s", err, out)
	}
	if l := one.callText(t, "list_peers", map[string]any{}); !strings.Contains(l, "alice/two#bbbbbb") {
		t.Fatalf("after refusing:\n%s", l)
	}

	// The real thing, with a request half-written across the switch.
	before := one.callText(t, "send", map[string]any{"to": "two", "text": "before the reload"})
	if !strings.HasPrefix(before, "Delivered") {
		t.Fatal(before)
	}
	if n := two.notification(t); n.Params.Content != "before the reload" {
		t.Fatalf("%+v", n)
	}
	install(good)
	one.nextID++
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": one.nextID, "method": "tools/call", "params": map[string]any{"name": "threads", "arguments": map[string]any{}}})
	half := len(req) / 2
	if _, err := one.in.Write(req[:half]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	out, err := reload()
	if err != nil || strings.Count(out, "reloaded") != 2 {
		t.Fatalf("reload: %v\n%s", err, out)
	}
	if _, err := one.in.Write(append(req[half:], '\n')); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-one.resp:
		if r.Error != nil || !strings.Contains(string(r.Result), "before the reload") {
			t.Fatalf("the split request: %s %+v", r.Result, r.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request split across the switch got no answer")
	}

	// The same process, running the new file.
	if one.cmd.Process.Pid != pid || one.cmd.ProcessState != nil {
		t.Fatal("the process didn't survive")
	}
	running, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		t.Fatal(err)
	}
	onDisk, _ := os.Stat(bin)
	if !os.SameFile(running, onDisk) {
		t.Fatal("still running the old file")
	}
	select {
	case n := <-one.notes:
		if n.Method != "notifications/tools/list_changed" {
			t.Fatalf("first notice after reloading: %s", n.Method)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no tools/list_changed")
	}
	// Still reachable and still advertising, without a new handshake; both ways.
	l := waitListing(two, "still-here")
	if !strings.Contains(l, "alice/one#aaaaaa") {
		t.Fatalf("address changed:\n%s", l)
	}
	<-two.notes // its own list_changed
	if out := one.callText(t, "send", map[string]any{"to": "two", "text": "after the reload"}); !strings.HasPrefix(out, "Delivered") {
		t.Fatal(out)
	}
	if n := two.notification(t); n.Params.Content != "after the reload" {
		t.Fatalf("%+v", n)
	}
	// Nothing of the handover is left lying around.
	if left, _ := filepath.Glob(filepath.Join(alice.state, "resume-*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

type channelProc struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	nextID int
	resp   chan rpc
	notes  chan rpc
}

type rpc struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	Params struct {
		Content string            `json:"content"`
		Meta    map[string]string `json:"meta"`
	} `json:"params"`
}

func writeRegistry(t *testing.T, u *user, name, sid, status string) {
	writeRegistryName(t, u, name, sid, name, status)
}

func writeRegistryName(t *testing.T, u *user, file, sid, name, status string) {
	t.Helper()
	// No pid: there's no Claude Code process whose flags the channel could
	// check, so it joins as when they can't be read.
	b, _ := json.Marshal(map[string]any{"sessionId": sid, "cwd": u.state, "name": name, "status": status})
	path := filepath.Join(u.state, file+".registry.json")
	os.MkdirAll(u.state, 0o700)
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Rename(path+".tmp", path)
}

func startChannel(t *testing.T, bin string, u *user, name, sid, status string) *channelProc {
	t.Helper()
	writeRegistry(t, u, name, sid, status)
	cmd := exec.Command(bin, "channel")
	cmd.Env = append(u.env, "PEERBUS_REGISTRY_FILE="+filepath.Join(u.state, name+".registry.json"))
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = &prefixWriter{t: t, prefix: name}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &channelProc{cmd: cmd, in: in, resp: make(chan rpc, 16), notes: make(chan rpc, 16)}
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m rpc
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if m.ID != nil {
				p.resp <- m
			} else {
				p.notes <- m
			}
		}
	}()
	var init struct {
		Capabilities map[string]any `json:"capabilities"`
		Instructions string         `json:"instructions"`
	}
	json.Unmarshal(p.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}}), &init)
	if exp, _ := init.Capabilities["experimental"].(map[string]any); exp["claude/channel"] == nil {
		t.Fatalf("no claude/channel capability: %+v", init.Capabilities)
	}
	p.write(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return p
}

func (p *channelProc) write(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if _, err := p.in.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (p *channelProc) call(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	p.nextID++
	p.write(t, map[string]any{"jsonrpc": "2.0", "id": p.nextID, "method": method, "params": params})
	select {
	case r := <-p.resp:
		if r.Error != nil {
			t.Fatalf("%s: %s", method, r.Error.Message)
		}
		return r.Result
	case <-time.After(30 * time.Second):
		t.Fatalf("%s: no response", method)
	}
	return nil
}

func (p *channelProc) callText(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	var res struct {
		Content []struct{ Text string } `json:"content"`
		IsError bool                    `json:"isError"`
	}
	json.Unmarshal(p.call(t, "tools/call", map[string]any{"name": tool, "arguments": args}), &res)
	if len(res.Content) == 0 {
		t.Fatalf("%s: empty result", tool)
	}
	if res.IsError && tool != "list_peers" && tool != "send" {
		t.Fatalf("%s error: %s", tool, res.Content[0].Text)
	}
	return res.Content[0].Text
}

func (p *channelProc) notification(t *testing.T) rpc {
	t.Helper()
	select {
	case n := <-p.notes:
		if n.Method != "notifications/claude/channel" {
			t.Fatalf("unexpected notification %s", n.Method)
		}
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("no channel notification")
	}
	return rpc{}
}

type prefixWriter struct {
	t      *testing.T
	prefix string
}

func (w *prefixWriter) Write(b []byte) (int, error) {
	w.t.Log(fmt.Sprintf("[%s] %s", w.prefix, strings.TrimSpace(string(b))))
	return len(b), nil
}
