package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestEndToEnd builds the binary, runs `peerbus init`, then drives two
// `peerbus channel` processes over MCP stdio the way Claude Code does. The
// first channel autostarts the broker.
func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "peerbus")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	envv := append(os.Environ(),
		"PEERBUS_CONFIG_DIR="+filepath.Join(dir, "config"),
		"PEERBUS_STATE_DIR="+filepath.Join(dir, "state"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(dir, "claude"),
	)
	initCmd := exec.Command(bin, "init", "-user", "alice", "-listen", addr)
	initCmd.Env = envv
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(filepath.Join(dir, "state", "broker.pid")); err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			syscall.Kill(pid, syscall.SIGTERM)
			// It saves state on the way out; let it finish before TempDir cleanup.
			for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
				time.Sleep(50 * time.Millisecond)
			}
		}
	})

	one := startChannel(t, bin, envv, dir, "one", "aaaaaaaa-1111", "busy")
	two := startChannel(t, bin, envv, dir, "two", "bbbbbbbb-2222", "idle")

	// Wait until both are connected (the first autostarted the broker).
	var listing string
	for i := 0; i < 50; i++ {
		listing = one.callText(t, "list_peers", map[string]any{})
		if strings.Contains(listing, "alice/two#bbbbbb") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(listing, "This session is alice/one#aaaaaa") || !strings.Contains(listing, "alice/two#bbbbbb  idle") {
		t.Fatalf("listing:\n%s", listing)
	}

	// Send one -> two, which arrives as a channel notification.
	out := one.callText(t, "send", map[string]any{"to": "two", "text": "build is green\nmerged #42"})
	if !strings.HasPrefix(out, "Delivered to alice/two#bbbbbb") {
		t.Fatalf("send: %s", out)
	}
	n := two.notification(t)
	meta := n.Params.Meta
	if n.Params.Content != "build is green\nmerged #42" || meta["from"] != "alice/one#aaaaaa" || meta["foreign"] != "false" || meta["kind"] != "message" {
		t.Fatalf("bad notification %+v", n)
	}

	// Reply on the thread.
	two.callText(t, "send", map[string]any{"to": meta["from"], "text": "thanks", "thread": meta["thread"]})
	if r := one.notification(t); r.Params.Content != "thanks" || r.Params.Meta["thread"] != meta["thread"] {
		t.Fatalf("bad reply %+v", r)
	}

	// Idle subscription driven by the registry file.
	if out := two.callText(t, "subscribe_idle", map[string]any{"peer": "one"}); !strings.HasPrefix(out, "Subscribed") {
		t.Fatalf("subscribe: %s", out)
	}
	writeRegistry(t, dir, "one", "aaaaaaaa-1111", "idle")
	if r := two.notification(t); r.Params.Meta["kind"] != "idle" {
		t.Fatalf("bad idle notice %+v", r)
	}

	// Renaming in the registry shows up for peers.
	writeRegistryName(t, dir, "one", "aaaaaaaa-1111", "renamed", "idle")
	time.Sleep(800 * time.Millisecond)
	if l := two.callText(t, "list_peers", map[string]any{}); !strings.Contains(l, "alice/renamed#aaaaaa") {
		t.Fatalf("rename not visible:\n%s", l)
	}

	// The CLI can't approve without a terminal.
	approve := exec.Command(bin, "approve", "deadbeef")
	approve.Env = envv
	if out, err := approve.CombinedOutput(); err == nil || !strings.Contains(string(out), "interactively") {
		t.Fatalf("approve without a tty: %v %s", err, out)
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

func writeRegistry(t *testing.T, dir, name, sid, status string) {
	writeRegistryName(t, dir, name, sid, name, status)
}

func writeRegistryName(t *testing.T, dir, file, sid, name, status string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"pid": 1, "sessionId": sid, "cwd": dir, "name": name, "status": status})
	path := filepath.Join(dir, file+".registry.json")
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Rename(path+".tmp", path)
}

func startChannel(t *testing.T, bin string, envv []string, dir, name, sid, status string) *channelProc {
	t.Helper()
	writeRegistry(t, dir, name, sid, status)
	cmd := exec.Command(bin, "channel")
	cmd.Env = append(envv, "PEERBUS_REGISTRY_FILE="+filepath.Join(dir, name+".registry.json"))
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
	case <-time.After(20 * time.Second):
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
	if res.IsError && tool != "list_peers" {
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
	case <-time.After(5 * time.Second):
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
