package tui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vitaminmoo/peerbus/internal/proto"
)

func sample() *model {
	m := newModel(context.Background(), nil, "yourname")
	now := time.Now()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.applyFetch(fetchedMsg{
		machines: 2,
		sessions: []session{
			{info: proto.SessionInfo{SID: "d1", Name: "ghidra", Host: "desk", Status: "busy", Cwd: "/home/you/reverse/d2"}},
			{info: proto.SessionInfo{SID: "l1", Name: "notes", Host: "laptop", Status: "idle", Cwd: "/home/you/notes"}},
		},
		items: []item{
			{machine: "desk", v: proto.HeldView{ID: "3fa9", Peer: "alice", From: "alice/re#8c41aa", To: "yourname/ghidra#d1d1d1", Target: "d1", Thread: "7c1e2a", HeldAt: now.Add(-2 * time.Minute).Unix(), Text: "can I borrow ghidra for the d2 binary?\nI'll send the sample offset table next."}},
			{machine: "laptop", v: proto.HeldView{ID: "b210", Peer: "bob", From: "bob/build#55e0f1", To: "yourname/", Routed: true, Thread: "91aa0c", HeldAt: now.Add(-40 * time.Second).Unix(), Text: "the lab run is green, want the logs?"}},
			{machine: "desk", v: proto.HeldView{ID: "c0de", Peer: "alice", From: "yourname/ghidra#d1d1d1", To: "alice/re#8c41aa", Target: "d1", Outbound: true, Thread: "7c1e2a", HeldAt: now.Add(-5 * time.Minute).Unix(), Text: "sure, sending the offsets"}},
		},
	})
	return m
}

// TestRender draws each mode; PEERBUS_TUI_DUMP=1 prints them to look at.
func TestRender(t *testing.T) {
	m := sample()
	views := map[string]string{"browse": m.View()}
	m.cursor = 1
	m.startRouting(m.items[1])
	views["route"] = m.View()
	m.mode, m.cursor = confirmingDeny, 0
	views["deny"] = m.View()
	m.Update(tea.WindowSizeMsg{Width: 72, Height: 30})
	m.mode = browsing
	views["narrow"] = m.View()
	empty := newModel(context.Background(), nil, "yourname")
	empty.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	empty.applyFetch(fetchedMsg{})
	views["empty"] = empty.View()
	for name, v := range views {
		if !strings.Contains(v, "PEERBUS") {
			t.Errorf("%s: no header", name)
		}
		if os.Getenv("PEERBUS_TUI_DUMP") != "" {
			t.Logf("=== %s ===\n%s", name, v)
		}
	}
	if !strings.Contains(views["route"], "ROUTE TO") || !strings.Contains(views["deny"], "Decline this message?") {
		t.Fatal("mode views missing their prompts")
	}
}
