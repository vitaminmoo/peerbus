package tui

import (
	"context"
	"crypto/ed25519"
	"net"
	"testing"
	"time"

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

func until(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
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
	if it.machine != "laptop" || it.section() != forSession || m.sessionLabel(it.v.Target) != "notes@laptop" {
		t.Fatalf("item: %+v on %s", it.v, it.machine)
	}

	m.startRouting(it)
	for i, s := range m.sessions {
		if s.info.SID == "d1d1d1d1" {
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
}
