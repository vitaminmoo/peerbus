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
	m.applyHistory(historyMsg{records: []record{
		{machines: []string{"desk"}, e: proto.HistoryEntry{ID: "a1", From: "yourname/ghidra#d1d1d1", To: "yourname/notes#4be1c2", Status: "delivered", Thread: "0b77e1", At: now.Add(-1 * time.Minute).Unix(), Text: "the d2 offsets are in notes/d2.md"}},
		{machines: []string{"laptop"}, e: proto.HistoryEntry{ID: "a2", From: "bob/build#55e0f1", To: "yourname/", Peer: "bob", Status: "held", Thread: "91aa0c", At: now.Add(-40 * time.Second).Unix(), Text: "the lab run is green, want the logs?"}},
		{machines: []string{"desk"}, e: proto.HistoryEntry{ID: "a3", From: "yourname/ghidra#d1d1d1", To: "alice/re#8c41aa", Peer: "alice", Status: "declined", Thread: "7c1e2a", At: now.Add(-3 * time.Hour).Unix(), Text: "want the whole symbol table?", Cut: true}},
		{machines: []string{"desk", "laptop"}, e: proto.HistoryEntry{ID: "a4", From: "alice/re#8c41aa", To: "yourname/ghidra#d1d1d1", Peer: "alice", Status: "delivered", Thread: "7c1e2a", At: now.Add(-4 * time.Hour).Unix(), Text: "can I borrow ghidra for the d2 binary?"}},
	}})
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
	m.mode = browsing
	m.page = historyPage
	views["history"] = m.View()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")}) // between your sessions
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")}) // with friends
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	views["friends"] = m.View()
	m.page = inboxPage
	m.Update(tea.WindowSizeMsg{Width: 72, Height: 30})
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
	if h := views["history"]; !strings.Contains(h, "HISTORY") || !strings.Contains(h, "ALL MESSAGES") || !strings.Contains(h, "ghidra → notes") || !strings.Contains(h, "bob/build → you") {
		t.Fatal("history page missing its rows")
	}
	// With friends only, second row: alice's declining, with the rest of the
	// text still to come.
	if f := views["friends"]; !strings.Contains(f, "WITH FRIENDS") || strings.Contains(f, "ghidra → notes") || !strings.Contains(f, "declined") || !strings.Contains(f, "loading the rest") {
		t.Fatal("friends filter")
	}
}
