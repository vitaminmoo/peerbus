package tui

import (
	"context"
	"crypto/ed25519"
	"net"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/mesh"
	"github.com/vitaminmoo/peerbus/internal/proto"
	"github.com/vitaminmoo/peerbus/internal/transport"
)

func rendezvous(t *testing.T) string {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go transport.NewServer(conn).Serve(ctx)
	t.Cleanup(cancel)
	return conn.LocalAddr().String()
}

func named(t *testing.T, name string) identity.Store {
	s := identity.Store{Dir: t.TempDir()}
	if _, err := s.Identity(); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(p *identity.Peers) error { p.Self.Name = name; return nil }); err != nil {
		t.Fatal(err)
	}
	return s
}

func sameIdentity(t *testing.T, s identity.Store) identity.Store {
	key, _ := s.Identity()
	ps, _ := s.Load()
	o := identity.Store{Dir: t.TempDir()}
	if err := o.ReplaceIdentity(key); err != nil {
		t.Fatal(err)
	}
	o.Update(func(p *identity.Peers) error { *p = *ps; return nil })
	return o
}

func befriend(t *testing.T, a, b identity.Store) {
	secret := identity.NewSecret()
	add := func(s, other identity.Store, name string) {
		k, _ := other.Identity()
		s.Update(func(p *identity.Peers) error {
			p.Add(&identity.Peer{Name: name, Key: identity.Key(k.Public().(ed25519.PublicKey)), Secret: secret, Enabled: true, Perms: identity.DefaultPerms(), PairedAt: time.Now()})
			return nil
		})
	}
	add(a, b, "bob")
	add(b, a, "alice")
}

func run(t *testing.T, s identity.Store, state, srv, kind string, info proto.SessionInfo, got chan<- proto.Message) *mesh.Mesh {
	m, err := mesh.Start(context.Background(), mesh.Config{
		Store: s, StateDir: state, Server: srv, Kind: kind, Info: info,
		OnMessage: func(msg proto.Message, foreign bool) {
			if got != nil {
				got <- msg
			}
		},
		OnNotice: func(string, string, map[string]string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// until waits for f. Outside the mesh package's tests discovery runs at its
// real pace (every 5s, 30s before redialing a failed dial), so allow for a
// retry when the machine is busy.
func until(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(75 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The TUI on the desk finds a message held on the laptop and routes it to a
// desk session.
func TestRouteAcrossMachines(t *testing.T) {
	srv := rendezvous(t)
	alice, bob := named(t, "alice"), named(t, "bob")
	befriend(t, alice, bob)
	laptop := sameIdentity(t, alice)
	deskState := t.TempDir()
	got := make(chan proto.Message, 4)

	run(t, alice, deskState, srv, proto.KindSession, proto.SessionInfo{SID: "d1d1d1d1", Name: "ghidra", Host: "desk", Status: "idle"}, got)
	run(t, laptop, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "l1l1l1l1", Name: "notes", Host: "laptop", Status: "idle"}, nil)
	b := run(t, bob, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "b1b1b1b1", Name: "bobsess", Host: "h", Status: "idle"}, nil)
	until(t, "bob to reach alice's laptop", func() bool {
		res, err := b.Send(context.Background(), "alice/notes", "logs are ready", "")
		return err == nil && res.Status == "held"
	})

	ctl := run(t, alice, deskState, srv, proto.KindControl, proto.SessionInfo{SID: "tui", Name: "tui", Host: "desk"}, nil)
	m := newModel(context.Background(), ctl, "alice")
	until(t, "the tui to see the held message", func() bool {
		m.applyFetch(m.fetch()().(fetchedMsg))
		return len(m.items) == 1 && m.machines == 2 && len(m.sessions) == 2
	})
	it := m.items[0]
	if it.Machine != "laptop" || it.Section() != forSession || m.sessionLabel(it.View.Target) != "notes@laptop" {
		t.Fatalf("item: %+v on %s", it.View, it.Machine)
	}

	m.startRouting(it)
	for i, s := range m.sessions {
		if s.Info.SID == "d1d1d1d1" {
			m.pick = i
		}
	}
	done := m.approve(it, m.sessions[m.pick])().(doneMsg)
	if done.err != nil {
		t.Fatal(done.err)
	}
	select {
	case msg := <-got:
		if msg.Text != "logs are ready" || msg.To != "alice/ghidra#d1d1d1" {
			t.Fatalf("delivered: %+v", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("not delivered to the desk")
	}
	m.applyFetch(m.fetch()().(fetchedMsg))
	if len(m.items) != 0 {
		t.Fatalf("still held: %+v", m.items)
	}

	// The history has it once, from both machines that logged it, routed
	// to the desk; it's a friend's message, not one between alice's
	// sessions.
	m.applyHistory(m.fetchHistory()().(historyMsg))
	if len(m.hist) != 1 {
		t.Fatalf("history: %+v", m.hist)
	}
	r := m.hist[0]
	if r.Entry.Text != "logs are ready" || r.Entry.Status != "delivered" || r.Entry.To != "alice/ghidra#d1d1d1" || len(r.Machines) != 2 || !r.Local || len(r.Via) == 0 {
		t.Fatalf("record: %+v on %v", r.Entry, r.Machines)
	}
	m.filter = ownMessages
	if len(m.shown()) != 0 {
		t.Fatal("a friend's message among alice's own")
	}
	m.filter = friendMessages
	if len(m.shown()) != 1 {
		t.Fatal("friend filter lost it")
	}
}

func typeText(m *model, s string) {
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

// You write to a session from the TUI as alice/human; its reply waits for
// you to read, and replying keeps the thread.
func TestWriteAndReply(t *testing.T) {
	srv := rendezvous(t)
	alice := named(t, "alice")
	state := t.TempDir()
	got := make(chan proto.Message, 4)
	desk := run(t, alice, state, srv, proto.KindSession, proto.SessionInfo{SID: "d1d1d1d1", Name: "ghidra", Host: "desk", Status: "idle"}, got)
	ctl := run(t, alice, state, srv, proto.KindControl, proto.SessionInfo{SID: "tui", Name: proto.Human, Host: "desk"}, nil)
	m := newModel(context.Background(), ctl, "alice")
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	until(t, "the tui to reach the session", func() bool {
		m.applyFetch(m.fetch()().(fetchedMsg))
		return len(m.sessions) == 1
	})

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.mode != composing {
		t.Fatal("n didn't open the writer")
	}
	for i, v := range m.compose.targets {
		if v.Address == "alice/ghidra#d1d1d1" {
			m.compose.pick = i
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	typeText(m, "status?")
	if v := m.View(); !strings.Contains(v, "WRITE TO alice/ghidra#d1d1d1") {
		t.Fatalf("writer:\n%s", v)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	done := cmd().(doneMsg)
	if done.err != nil || !strings.Contains(done.text, "sent to alice/ghidra") {
		t.Fatalf("send: %+v", done)
	}
	m.Update(done)
	if m.mode != browsing || m.busy {
		t.Fatal("still writing after sending")
	}
	var msg proto.Message
	select {
	case msg = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("not delivered")
	}
	if msg.From != "alice/human" || msg.Text != "status?" {
		t.Fatalf("delivered: %+v", msg)
	}

	if _, err := desk.Send(context.Background(), msg.From, "all green", msg.Thread); err != nil {
		t.Fatal(err)
	}
	until(t, "the reply to wait for you", func() bool {
		m.applyFetch(m.fetch()().(fetchedMsg))
		return len(m.items) == 1
	})
	if it := m.items[0]; it.Section() != toRead || it.View.Text != "all green" {
		t.Fatalf("reply: %+v", it.View)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m.mode != composing || m.compose.to != "alice/ghidra#d1d1d1" || m.compose.thread != msg.Thread {
		t.Fatalf("reply writer: %+v", m.compose)
	}
	typeText(m, "thanks")
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if done := cmd().(doneMsg); done.err != nil {
		t.Fatal(done.err)
	}
	select {
	case r := <-got:
		if r.Thread != msg.Thread || r.Text != "thanks" {
			t.Fatalf("reply delivered: %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reply not delivered")
	}
}
