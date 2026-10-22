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
	two.callText(t, "send", map[string]any{"to": meta["from"], "text": "thanks", "thread": meta["thread"]})
	if r := one.notification(t); r.Params.Content != "thanks" || r.Params.Meta["thread"] != meta["thread"] {
		t.Fatalf("bad reply %+v", r)
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
	b, _ := json.Marshal(map[string]any{"pid": 1, "sessionId": sid, "cwd": u.state, "name": name, "status": status})
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
