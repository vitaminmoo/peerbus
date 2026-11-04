package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vitaminmoo/peerbus/internal/held"
	"github.com/vitaminmoo/peerbus/internal/history"
	"github.com/vitaminmoo/peerbus/internal/proto"
)

func press(m *model, s string) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return cmd
}

func TestHistoryGroupsByThread(t *testing.T) {
	m := sample()
	m.page = historyPage
	// sample's alice messages share a thread; the rest are on their own.
	if s := m.shown(); len(s) != 3 {
		t.Fatalf("conversations: %+v", s)
	}
	if v := m.View(); !strings.Contains(v, "(2)") {
		t.Fatalf("no message count on the list:\n%s", v)
	}
	for m.hcursor = 0; m.hcursor < 3; m.hcursor++ {
		if r, _ := m.currentRecord(); r.Entry.Thread == "7c1e2a" {
			break
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s := m.shown()
	if m.hthread != "7c1e2a" || len(s) != 2 || s[0].Entry.ID != "a4" || s[1].Entry.ID != "a3" {
		t.Fatalf("opened: %q %+v", m.hthread, s)
	}
	if v := m.View(); !strings.Contains(v, "THREAD 7C1E2A") || !strings.Contains(v, "delete thread") {
		t.Fatalf("thread view:\n%s", v)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if r, _ := m.currentRecord(); m.hthread != "" || r.Entry.Thread != "7c1e2a" {
		t.Fatalf("back on %q at %+v", m.hthread, r.Entry)
	}
	// Anything but y keeps it.
	press(m, "d")
	if m.mode != confirmingDelete || !strings.Contains(m.View(), "Delete this thread (2 messages)") {
		t.Fatalf("no confirmation:\n%s", m.View())
	}
	// A refresh of the inbox doesn't lose the question.
	m.applyFetch(fetchedMsg{})
	if m.mode != confirmingDelete {
		t.Fatal("the inbox refresh closed the confirmation")
	}
	if cmd := press(m, "n"); cmd != nil || m.mode != browsing || len(m.shown()) != 3 {
		t.Fatalf("after no: mode %v, %d shown", m.mode, len(m.shown()))
	}
	// A deletion finishing leaves the page without it, and a fetch from
	// before it can't bring it back.
	before := m.hist
	stale := historyMsg{records: before, gen: m.hgen}
	m.Update(deletedMsg{thread: "7c1e2a"})
	if len(m.shown()) != 2 || m.status != "thread deleted" {
		t.Fatalf("after deleting: %d shown, %q", len(m.shown()), m.status)
	}
	m.applyHistory(stale)
	if len(m.shown()) != 2 {
		t.Fatal("a stale fetch brought it back")
	}
}

// A deletion reaches machines with a session online now, and ones that come
// back later.
func TestDeleteThreadAcrossMachines(t *testing.T) {
	srv := rendezvous(t)
	alice := named(t, "alice")
	laptop := sameIdentity(t, alice)
	deskState, lapState := t.TempDir(), t.TempDir()
	got := make(chan proto.Message, 8)
	lapGot := make(chan proto.Message, 8)
	desk := run(t, alice, deskState, srv, proto.KindSession, proto.SessionInfo{SID: "d1d1d1d1", Name: "ghidra", Host: "desk", Status: "idle"}, got)
	lap := run(t, laptop, lapState, srv, proto.KindSession, proto.SessionInfo{SID: "l1l1l1l1", Name: "notes", Host: "laptop", Status: "idle"}, lapGot)
	ctlStore := sameIdentity(t, alice)
	ctl := run(t, ctlStore, t.TempDir(), srv, proto.KindControl, proto.SessionInfo{SID: "c1c1c1c1", Name: "tui", Host: "here"}, nil)
	until(t, "everyone to meet", func() bool {
		return len(ctl.Own()) == 2 && slicesHas(desk, "alice/notes#l1l1l1")
	})
	ctx := context.Background()
	send := func(text string) proto.SendResult {
		t.Helper()
		res, err := desk.Send(ctx, "notes", text, "")
		if err != nil {
			t.Fatal(err)
		}
		<-lapGot
		return res
	}
	logged := func(state string) int {
		t.Helper()
		msgs, err := (history.Store{Dir: state}).Messages()
		if err != nil {
			t.Fatal(err)
		}
		return len(msgs)
	}

	// With both machines up, from the TUI.
	send("delete me everywhere")
	m := newModel(ctx, ctl, "alice")
	m.width, m.height = 120, 40
	m.page = historyPage
	until(t, "the history", func() bool {
		m.applyHistory(m.fetchHistory()().(historyMsg))
		return len(m.shown()) == 1
	})
	press(m, "d")
	cmd := press(m, "y")
	if cmd == nil {
		t.Fatal("y did nothing")
	}
	d := cmd().(deletedMsg)
	if d.err != nil {
		t.Fatal(d.err)
	}
	m.Update(d)
	if logged(deskState) != 0 || logged(lapState) != 0 {
		t.Fatalf("left behind: desk %d, laptop %d", logged(deskState), logged(lapState))
	}

	// With the laptop off for it.
	second := send("delete me while the laptop is off")
	lap.Close()
	until(t, "the laptop to go", func() bool { return len(ctl.Own()) == 1 })
	if _, err := held.DeleteThread(ctx, ctl, second.Thread); err != nil {
		t.Fatal(err)
	}
	if logged(deskState) != 0 || logged(lapState) != 1 {
		t.Fatalf("desk %d, laptop %d", logged(deskState), logged(lapState))
	}
	run(t, laptop, lapState, srv, proto.KindSession, proto.SessionInfo{SID: "l1l1l1l1", Name: "notes", Host: "laptop", Status: "idle"}, lapGot)
	until(t, "the laptop to return", func() bool { return len(ctl.Own()) == 2 })
	// The returning copy never shows, and the laptop learns.
	until(t, "the laptop to hear", func() bool {
		if recs := held.History(ctx, ctl); len(recs) != 0 {
			t.Fatalf("shown again: %+v", recs)
		}
		return logged(lapState) == 0
	})
}

func slicesHas(m interface{ Views() []proto.PeerView }, addr string) bool {
	for _, v := range m.Views() {
		if v.Address == addr {
			return true
		}
	}
	return false
}
