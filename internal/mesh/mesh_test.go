package mesh

import (
	"context"
	"crypto/ed25519"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vitaminmoo/peerbus/internal/history"
	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/inbox"
	"github.com/vitaminmoo/peerbus/internal/proto"
	"github.com/vitaminmoo/peerbus/internal/transport"
)

func init() { discoverEvery = 300 * time.Millisecond }

func server(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { transport.NewServer(conn).Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return conn.LocalAddr().String()
}

func store(t *testing.T, name string) identity.Store {
	t.Helper()
	s := identity.Store{Dir: t.TempDir()}
	if _, err := s.Identity(); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(p *identity.Peers) error { p.Self.Name = name; return nil }); err != nil {
		t.Fatal(err)
	}
	return s
}

func pub(t *testing.T, s identity.Store) ed25519.PublicKey {
	k, err := s.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return k.Public().(ed25519.PublicKey)
}

// pair makes a and b friends with default permissions, as `peerbus invite`
// and `peerbus pair` would.
func pair(t *testing.T, a, b identity.Store) {
	t.Helper()
	secret := identity.NewSecret()
	add := func(s identity.Store, other identity.Store, name string) {
		err := s.Update(func(p *identity.Peers) error {
			p.Add(&identity.Peer{Name: name, Key: identity.Key(pub(t, other)), Secret: secret, Enabled: true, Perms: identity.DefaultPerms(), PairedAt: time.Now()})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	add(a, b, "bob")
	add(b, a, "alice")
}

type events struct {
	mu      sync.Mutex
	msgs    chan proto.Message
	notices chan notice
}

type notice struct {
	kind, text string
	meta       map[string]string
}

func newEvents() *events {
	return &events{msgs: make(chan proto.Message, 16), notices: make(chan notice, 16)}
}

func (e *events) msg(t *testing.T) proto.Message {
	t.Helper()
	select {
	case m := <-e.msgs:
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no message")
	}
	return proto.Message{}
}

func (e *events) notice(t *testing.T, kind string) notice {
	t.Helper()
	for {
		select {
		case n := <-e.notices:
			if n.kind == kind {
				return n
			}
			t.Logf("skipping notice %s: %s", n.kind, n.text)
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s notice", kind)
		}
	}
}

func start(t *testing.T, s identity.Store, state, srv, kind string, info proto.SessionInfo) (*Mesh, *events) {
	t.Helper()
	ev := newEvents()
	m, err := Start(context.Background(), Config{
		Store: s, StateDir: state, Server: srv, Kind: kind, Info: info,
		Log: log.New(&tlog{t, info.Name}, "", 0),
		OnMessage: func(m proto.Message, foreign bool) {
			m.FromUser += map[bool]string{true: "!", false: ""}[foreign] // mark foreign for assertions
			ev.msgs <- m
		},
		OnNotice: func(kind, text string, meta map[string]string) { ev.notices <- notice{kind, text, meta} },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, ev
}

type tlog struct {
	t    *testing.T
	name string
}

func (w *tlog) Write(b []byte) (int, error) {
	w.t.Logf("[%s] %s", w.name, strings.TrimSpace(string(b)))
	return len(b), nil
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func sees(m *Mesh, addr string) bool {
	for _, v := range m.Views() {
		if v.Address == addr {
			return true
		}
	}
	return false
}

func TestOwnSessionsAndFriends(t *testing.T) {
	srv := server(t)
	alice, bob := store(t, "alice"), store(t, "bob")
	pair(t, alice, bob)
	stateA, stateB := t.TempDir(), t.TempDir()

	a1, evA1 := start(t, alice, stateA, srv, proto.KindSession, proto.SessionInfo{SID: "a1a1a1a1", Name: "one", Host: "h", Status: "busy"})
	a2, evA2 := start(t, alice, stateA, srv, proto.KindSession, proto.SessionInfo{SID: "a2a2a2a2", Name: "two", Host: "h", Status: "idle", Cwd: "/tmp/two"})
	b1, evB1 := start(t, bob, stateB, srv, proto.KindSession, proto.SessionInfo{SID: "b1b1b1b1", Name: "bobsess", Host: "h", Status: "idle", Cwd: "/home/bob"})

	waitFor(t, "a1 to see a2 and bob", func() bool { return sees(a1, "alice/two#a2a2a2") && sees(a1, "bob/bobsess#b1b1b1") })
	waitFor(t, "bob to see alice", func() bool { return sees(b1, "alice/one#a1a1a1") && sees(b1, "alice/two#a2a2a2") })

	// Same machine goes over loopback; details stay within our own sessions.
	for _, v := range a1.Views() {
		switch v.Address {
		case "alice/two#a2a2a2":
			if v.Via != "local" || v.Cwd != "/tmp/two" {
				t.Fatalf("own session view: %+v", v)
			}
		case "bob/bobsess#b1b1b1":
			if !v.Foreign || v.Cwd != "" {
				t.Fatalf("foreign view: %+v", v)
			}
		}
	}

	// Own sessions: delivered at once, and reply on the thread.
	res, err := a1.Send(context.Background(), "two", "hello two", "")
	if err != nil || res.Status != "delivered" || res.To != "alice/two#a2a2a2" {
		t.Fatalf("send: %+v %v", res, err)
	}
	m := evA2.msg(t)
	if m.Text != "hello two" || m.From != "alice/one#a1a1a1" || m.FromUser != "alice" || m.Thread != res.Thread {
		t.Fatalf("delivered: %+v", m)
	}
	if _, err := a2.Send(context.Background(), m.From, "hi back", m.Thread); err != nil {
		t.Fatal(err)
	}
	if r := evA1.msg(t); r.Text != "hi back" || r.Thread != res.Thread {
		t.Fatalf("reply: %+v", r)
	}

	// Sending to yourself is refused; unknown names are refused.
	if _, err := a1.Send(context.Background(), "one", "x", ""); err == nil {
		t.Fatal("sent to self")
	}
	if _, err := a1.Send(context.Background(), "carol/x", "x", ""); err == nil || !strings.Contains(err.Error(), "no peer named") {
		t.Fatalf("unknown peer: %v", err)
	}

	// Friends: held by default. Bob's session learns something is waiting,
	// but not what it says.
	res, err = a1.Send(context.Background(), "bob/bobsess", "secret plan", "")
	if err != nil || res.Status != "held" {
		t.Fatalf("send to bob: %+v %v", res, err)
	}
	n := evB1.notice(t, "held")
	if strings.Contains(n.text, "secret plan") || n.meta["from"] != "alice/one#a1a1a1" {
		t.Fatalf("held notice: %+v", n)
	}
	heldID := n.meta["held_id"]
	st, err := (inbox.Store{Dir: stateB}).Load()
	if err != nil || len(st.Held) != 1 || st.Held[0].ID != heldID || st.Held[0].Msg.Text != "secret plan" || st.Held[0].Target != "b1b1b1b1" {
		t.Fatalf("inbox: %+v %v", st, err)
	}

	// Bob's human approves from the CLI, which talks to the session on
	// loopback. The message is delivered there and alice is told.
	cli, _ := start(t, bob, stateB, "", proto.KindControl, proto.SessionInfo{SID: "cli", Name: "cli"})
	<-cli.Ready()
	locals := cli.Locals()
	if len(locals) != 1 || locals[0].Info().SID != "b1b1b1b1" {
		t.Fatalf("locals: %d", len(locals))
	}
	var ares proto.SendResult
	if err := locals[0].Request(context.Background(), proto.Frame{Type: proto.TApprove, ID: heldID}, &ares); err != nil || ares.Status != "delivered" {
		t.Fatalf("approve: %+v %v", ares, err)
	}
	m = evB1.msg(t)
	if m.Text != "secret plan" || m.From != "alice/one#a1a1a1" || m.FromUser != "alice!" || m.Thread != res.Thread {
		t.Fatalf("approved delivery: %+v", m)
	}
	if n := evA1.notice(t, "status"); n.meta["status"] != "approved" || n.meta["msg_id"] != res.ID {
		t.Fatalf("status notice: %+v", n)
	}
	// Bob's reply flows (alice accepts replies in a thread she started),
	// and alice's follow-up flows (bob approved the thread).
	if r, err := b1.Send(context.Background(), m.From, "sounds good", m.Thread); err != nil || r.Status != "delivered" {
		t.Fatalf("reply: %+v %v", r, err)
	}
	if r := evA1.msg(t); r.Text != "sounds good" || r.FromUser != "bob!" {
		t.Fatalf("bob's reply: %+v", r)
	}
	if r, err := a1.Send(context.Background(), "bob/bobsess#b1b1b1", "step two", res.Thread); err != nil || r.Status != "delivered" {
		t.Fatalf("follow-up: %+v %v", r, err)
	}
	evB1.msg(t)
	// A fresh thread is held again, and denying tells the sender.
	res, err = a1.Send(context.Background(), "bob/", "unrelated", "")
	if err != nil || res.Status != "held" || res.To != "bob/" {
		t.Fatalf("routed send: %+v %v", res, err)
	}
	n = evB1.notice(t, "held")
	if h, err := (inbox.Store{Dir: stateB}).Find(n.meta["held_id"]); err != nil || h.Target != "" {
		t.Fatalf("routed hold should leave the session to the human: %+v %v", h, err)
	}
	if err := locals[0].Request(context.Background(), proto.Frame{Type: proto.TDeny, ID: n.meta["held_id"]}, nil); err != nil {
		t.Fatal(err)
	}
	if n := evA1.notice(t, "status"); n.meta["status"] != "declined" {
		t.Fatalf("declined notice: %+v", n)
	}

	// Alice's machine logged all of it, each message once: the one between
	// her own sessions from both ends, bob's answers, and what became of
	// hers.
	got := map[string]proto.HistoryEntry{}
	msgs, err := (history.Store{Dir: stateA}).Messages()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range msgs {
		if _, dup := got[e.Text]; dup {
			t.Fatalf("logged twice: %+v", e)
		}
		got[e.Text] = e
	}
	for text, want := range map[string]proto.HistoryEntry{
		"hello two":   {From: "alice/one#a1a1a1", To: "alice/two#a2a2a2", Status: "delivered"},
		"hi back":     {From: "alice/two#a2a2a2", To: "alice/one#a1a1a1", Status: "delivered"},
		"secret plan": {From: "alice/one#a1a1a1", To: "bob/bobsess#b1b1b1", Peer: "bob", Status: "delivered"}, // held, then approved
		"sounds good": {From: "bob/bobsess#b1b1b1", To: "alice/one#a1a1a1", Peer: "bob", Status: "delivered"},
		"unrelated":   {From: "alice/one#a1a1a1", To: "bob/", Peer: "bob", Status: "declined"},
	} {
		e := got[text]
		if e.From != want.From || e.To != want.To || e.Peer != want.Peer || e.Status != want.Status {
			t.Errorf("%q: %+v", text, e)
		}
	}
	// Bob's side: alice's routed message, held for him and declined.
	if e, err := (history.Store{Dir: stateB}).Find("alice/one#a1a1a1", res.ID); err != nil || e.Status != "declined" || e.To != "bob/" {
		t.Fatalf("bob's record: %+v %v", e, err)
	}

	// A session can't approve or list through another session: only the
	// local CLI may.
	tgt, err := a1.resolve("two")
	if err != nil {
		t.Fatal(err)
	}
	if err := tgt.link.Request(context.Background(), proto.Frame{Type: proto.TApprove, ID: "x"}, nil); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("session approved: %v", err)
	}
	if err := b1.resolveLink(t, "alice/one").Request(context.Background(), proto.Frame{Type: proto.TList}, nil); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("foreign list: %v", err)
	}

	// Idle: one notice when the other session goes idle, then on disconnect.
	status, addr, err := a1.SubscribeIdle(context.Background(), "two")
	if err != nil || status != "idle" || addr != "alice/two#a2a2a2" {
		t.Fatalf("subscribe idle: %s %s %v", status, addr, err)
	}
	status, _, err = a2.SubscribeIdle(context.Background(), "one")
	if err != nil || status != "subscribed" {
		t.Fatalf("subscribe busy: %s %v", status, err)
	}
	a1.Update(proto.SessionInfo{SID: "a1a1a1a1", Name: "one", Host: "h", Status: "idle"})
	if n := evA2.notice(t, "idle"); n.meta["state"] != "idle" || n.meta["peer"] != "alice/one#a1a1a1" {
		t.Fatalf("idle notice: %+v", n)
	}
	// Disconnecting fires the subscription too (bob must be busy first, or
	// subscribing just reports idle).
	b1.Update(proto.SessionInfo{SID: "b1b1b1b1", Name: "bobsess", Host: "h", Status: "busy"})
	waitFor(t, "bob busy", func() bool {
		for _, v := range a2.Views() {
			if v.Address == "bob/bobsess#b1b1b1" && v.Status == "busy" {
				return true
			}
		}
		return false
	})
	if status, _, err := a2.SubscribeIdle(context.Background(), "bob/bobsess"); err != nil || status != "subscribed" {
		t.Fatalf("subscribe bob: %s %v", status, err)
	}
	b1.Close()
	if n := evA2.notice(t, "idle"); n.meta["state"] != "disconnected" {
		t.Fatalf("disconnect notice: %+v", n)
	}
	waitFor(t, "bob to vanish", func() bool { return !sees(a1, "bob/bobsess#b1b1b1") })
}

func TestPermissions(t *testing.T) {
	srv := server(t)
	alice, bob := store(t, "alice"), store(t, "bob")
	pair(t, alice, bob)
	stateA, stateB := t.TempDir(), t.TempDir()
	a1, _ := start(t, alice, stateA, srv, proto.KindSession, proto.SessionInfo{SID: "a1a1a1a1", Name: "one", Host: "h", Status: "busy", Cwd: "/a"})
	b1, evB1 := start(t, bob, stateB, srv, proto.KindSession, proto.SessionInfo{SID: "b1b1b1b1", Name: "bobsess", Host: "h", Status: "idle"})
	waitFor(t, "both ways", func() bool { return sees(a1, "bob/bobsess#b1b1b1") && sees(b1, "alice/one#a1a1a1") })

	// Bob lets alice through and alice shows bob her details; both sides
	// pick up the edited peers.json without restarting.
	set := func(s identity.Store, name string, f func(*identity.Perms)) {
		if err := s.Update(func(p *identity.Peers) error { f(&p.ByName(name).Perms); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	set(bob, "alice", func(p *identity.Perms) { p.Deliver = "auto" })
	set(alice, "bob", func(p *identity.Perms) { p.ShowDetails = true })
	waitFor(t, "details", func() bool {
		for _, v := range b1.Views() {
			if v.Address == "alice/one#a1a1a1" && v.Cwd == "/a" {
				return true
			}
		}
		return false
	})
	waitFor(t, "auto delivery", func() bool {
		res, err := a1.Send(context.Background(), "bob/bobsess", "direct", "")
		return err == nil && res.Status == "delivered"
	})
	for {
		if m := evB1.msg(t); m.Text == "direct" {
			break
		}
	}

	// Hiding a session from bob removes it from his view and his reach.
	set(alice, "bob", func(p *identity.Perms) { p.Sessions = []string{"re-*"} })
	waitFor(t, "hidden", func() bool { return !sees(b1, "alice/one#a1a1a1") })
	if _, err := b1.Send(context.Background(), "alice/one", "x", ""); err == nil {
		t.Fatal("sent to a hidden session")
	}
	// Hidden sessions can still speak.
	if res, err := a1.Send(context.Background(), "bob/bobsess", "still here", ""); err != nil || res.Status != "delivered" {
		t.Fatalf("hidden send: %+v %v", res, err)
	}

	// send=ask holds our own message until the human sends it.
	set(alice, "bob", func(p *identity.Perms) { p.Sessions = []string{"*"}; p.Send = "ask" })
	waitFor(t, "ask", func() bool {
		res, err := a1.Send(context.Background(), "bob/bobsess", "needs ok", "")
		return err == nil && res.Status == "ask"
	})
	st, _ := (inbox.Store{Dir: stateA}).Load()
	var out *inbox.Held
	for _, h := range st.Held {
		if h.Outbound && h.Msg.Text == "needs ok" {
			out = h
		}
	}
	if out == nil {
		t.Fatalf("no outbound hold: %+v", st.Held)
	}
	cli, _ := start(t, alice, stateA, "", proto.KindControl, proto.SessionInfo{SID: "cli", Name: "cli"})
	<-cli.Ready()
	var res proto.SendResult
	if err := cli.Locals()[0].Request(context.Background(), proto.Frame{Type: proto.TApprove, ID: out.ID}, &res); err != nil || res.Status != "delivered" {
		t.Fatalf("approve outbound: %+v %v", res, err)
	}
	for {
		if m := evB1.msg(t); m.Text == "needs ok" {
			break
		}
	}

	// Disabling a peer drops the connection.
	if err := bob.Update(func(p *identity.Peers) error { p.ByName("alice").Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "disabled", func() bool { return !sees(a1, "bob/bobsess#b1b1b1") && !sees(b1, "alice/one#a1a1a1") })
}

func (m *Mesh) resolveLink(t *testing.T, to string) *Link {
	t.Helper()
	tgt, err := m.resolve(to)
	if err != nil || tgt.link == nil {
		t.Fatalf("resolve %s: %v", to, err)
	}
	return tgt.link
}

// otherMachine gives s's identity and peers to a new store, as
// `peerbus invite -self` and `peerbus pair` would.
func otherMachine(t *testing.T, s identity.Store) identity.Store {
	t.Helper()
	key, err := s.Identity()
	if err != nil {
		t.Fatal(err)
	}
	ps, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	o := identity.Store{Dir: t.TempDir()}
	if err := o.ReplaceIdentity(key); err != nil {
		t.Fatal(err)
	}
	if err := o.Update(func(p *identity.Peers) error { *p = *ps; return nil }); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestPeersSyncAcrossOwnMachines(t *testing.T) {
	srv := server(t)
	alice := store(t, "alice")
	laptop := otherMachine(t, alice) // joined before alice paired with anyone
	bob := store(t, "bob")

	a1, _ := start(t, alice, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "a1a1a1a1", Name: "one", Host: "desk", Status: "idle"})
	l1, _ := start(t, laptop, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "l1l1l1l1", Name: "notes", Host: "laptop", Status: "idle"})
	waitFor(t, "alice's machines to meet", func() bool { return sees(a1, "alice/notes#l1l1l1") && sees(l1, "alice/one#a1a1a1") })

	// Pairing with bob on the desk reaches the laptop too.
	pair(t, alice, bob)
	b1, _ := start(t, bob, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "b1b1b1b1", Name: "bobsess", Host: "h", Status: "idle"})
	waitFor(t, "the laptop to learn about bob", func() bool {
		ps, _ := laptop.Load()
		return ps.ByName("bob") != nil
	})
	waitFor(t, "bob and the laptop to meet", func() bool { return sees(l1, "bob/bobsess#b1b1b1") && sees(b1, "alice/notes#l1l1l1") })

	// Limiting bob to the desk by machine name, on the laptop, applies on
	// the desk too.
	if err := laptop.Update(func(p *identity.Peers) error {
		b := p.ByName("bob")
		b.Perms.Sessions = []string{"desk/*"}
		b.Touch()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob to lose the laptop", func() bool { return !sees(b1, "alice/notes#l1l1l1") && sees(b1, "alice/one#a1a1a1") })

	// Removing bob anywhere removes him everywhere.
	if err := laptop.Update(func(p *identity.Peers) error { p.ByName("bob").Remove(); return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the desk to drop bob", func() bool {
		ps, _ := alice.Load()
		return ps.ByName("bob") == nil && !sees(a1, "bob/bobsess#b1b1b1")
	})
}

func TestRemoteApproveAndRoute(t *testing.T) {
	srv := server(t)
	alice, bob := store(t, "alice"), store(t, "bob")
	pair(t, alice, bob)
	laptop := otherMachine(t, alice)
	deskState := t.TempDir()

	bobState := t.TempDir()
	desk1, evDesk := start(t, alice, deskState, srv, proto.KindSession, proto.SessionInfo{SID: "d1d1d1d1", Name: "ghidra", Host: "desk", Status: "idle"})
	start(t, laptop, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "l1l1l1l1", Name: "notes", Host: "laptop", Status: "idle"})
	b1, evB := start(t, bob, bobState, srv, proto.KindSession, proto.SessionInfo{SID: "b1b1b1b1", Name: "bobsess", Host: "h", Status: "idle"})
	waitFor(t, "everyone to meet", func() bool {
		return sees(b1, "alice/notes#l1l1l1") && sees(b1, "alice/ghidra#d1d1d1") && sees(desk1, "alice/notes#l1l1l1")
	})

	// Bob writes to alice's laptop session; it's held on the laptop.
	res, err := b1.Send(context.Background(), "alice/notes", "can I borrow ghidra?", "")
	if err != nil || res.Status != "held" {
		t.Fatalf("send: %+v %v", res, err)
	}

	// A control client on the desk sees the laptop's inbox...
	ctl, _ := start(t, alice, deskState, srv, proto.KindControl, proto.SessionInfo{SID: "ctl", Name: "tui", Host: "desk"})
	var lapLink *Link
	waitFor(t, "the tui to reach the laptop", func() bool {
		for _, l := range ctl.Own() {
			if l.Info().SID == "l1l1l1l1" {
				lapLink = l
			}
		}
		return lapLink != nil
	})
	var view proto.InboxView
	if err := lapLink.Request(context.Background(), proto.Frame{Type: proto.TInbox}, &view); err != nil {
		t.Fatal(err)
	}
	if view.Machine != "laptop" || len(view.Held) != 1 || view.Held[0].Text != "can I borrow ghidra?" || view.Held[0].Target != "l1l1l1l1" {
		t.Fatalf("inbox: %+v", view)
	}

	// ...and routes the message to the desk session instead.
	var ar proto.SendResult
	if err := lapLink.Request(context.Background(), proto.Frame{Type: proto.TApprove, ID: view.Held[0].ID, To: "d1d1d1d1"}, &ar); err != nil || ar.Status != "delivered" {
		t.Fatalf("approve: %+v %v", ar, err)
	}
	m := evDesk.msg(t)
	if m.Text != "can I borrow ghidra?" || m.FromUser != "bob!" || m.To != "alice/ghidra#d1d1d1" {
		t.Fatalf("delivered: %+v", m)
	}
	if n := evB.notice(t, "status"); !strings.Contains(n.text, "approved") {
		t.Fatalf("bob's notice: %s", n.text)
	}

	// The thread is open on the desk now: bob's follow-up goes straight in.
	if r, err := b1.Send(context.Background(), "alice/ghidra", "thanks", m.Thread); err != nil || r.Status != "delivered" {
		t.Fatalf("follow-up: %+v %v", r, err)
	}
	if f := evDesk.msg(t); f.Text != "thanks" {
		t.Fatalf("follow-up: %+v", f)
	}
	if err := lapLink.Request(context.Background(), proto.Frame{Type: proto.TInbox}, &view); err != nil || len(view.Held) != 0 {
		t.Fatalf("laptop inbox after: %+v %v", view, err)
	}

	// Both of alice's machines logged bob's message: the laptop held it and
	// handed it on, the desk received it. The control client reads the
	// desk's history directly and the laptop's over the link, in full on
	// request.
	var lv proto.HistoryView
	if err := lapLink.Request(context.Background(), proto.Frame{Type: proto.THistory}, &lv); err != nil {
		t.Fatal(err)
	}
	if lv.Machine != "laptop" || len(lv.Messages) != 1 || lv.Messages[0].Status != "delivered" || lv.Messages[0].To != "alice/ghidra#d1d1d1" || lv.Messages[0].Peer != "bob" {
		t.Fatalf("laptop history: %+v", lv)
	}
	dv, err := ctl.History()
	if err != nil || len(dv.Messages) != 2 || dv.Messages[0].Text != "thanks" || dv.Messages[1].Text != "can I borrow ghidra?" {
		t.Fatalf("desk history: %+v %v", dv, err)
	}
	var one proto.HistoryEntry
	if err := lapLink.Request(context.Background(), proto.Frame{Type: proto.THistory, ID: res.ID, Meta: map[string]string{"from": lv.Messages[0].From}}, &one); err != nil || one.Text != "can I borrow ghidra?" {
		t.Fatalf("one message: %+v %v", one, err)
	}
	// Bob hears it was approved.
	if e, err := (history.Store{Dir: bobState}).Find("bob/bobsess#b1b1b1", res.ID); err != nil || e.Status != "delivered" {
		t.Fatalf("bob's record: %+v %v", e, err)
	}

	// Sessions can't approve or deliver on their own say-so, or read
	// another machine's history.
	if err := desk1.resolveLink(t, "notes").Request(context.Background(), proto.Frame{Type: proto.THistory}, &lv); err == nil {
		t.Fatal("a session read another machine's history")
	}
	var sv proto.InboxView
	if err := desk1.resolveLink(t, "notes").Request(context.Background(), proto.Frame{Type: proto.TInbox}, &sv); err == nil {
		t.Fatal("a session listed another machine's inbox")
	}
	if err := b1.resolveLink(t, "alice/notes").Request(context.Background(), proto.Frame{Type: proto.TDeliver, Data: map[string]any{}}, nil); err == nil {
		t.Fatal("a friend used deliver")
	}
}

// A conversation resumed in a second Claude Code process shares the first
// one's session id. The newer copy leaves the mesh and says why; the older
// one stays reachable under the one address.
func TestDuplicateSessionLeaves(t *testing.T) {
	srv := server(t)
	alice := store(t, "alice")
	state := t.TempDir()

	old, _ := start(t, alice, state, srv, proto.KindSession, proto.SessionInfo{SID: "d1d1d1d1", Name: "deob", Host: "h", Status: "waiting", Started: 1000, PID: 4242})
	other, _ := start(t, alice, state, srv, proto.KindSession, proto.SessionInfo{SID: "o1o1o1o1", Name: "other", Host: "h", Status: "idle", Started: 1500})
	waitFor(t, "other to see the original", func() bool { return sees(other, "alice/deob#d1d1d1") })

	_, evNew := start(t, alice, state, srv, proto.KindSession, proto.SessionInfo{SID: "d1d1d1d1", Name: "deob", Host: "h", Status: "idle", Started: 2000, PID: 5151})
	n := evNew.notice(t, "duplicate")
	if !strings.Contains(n.text, "pid 4242") || !strings.Contains(n.text, "alice/deob#d1d1d1") || n.meta["pid"] != "4242" {
		t.Fatalf("duplicate notice: %+v", n)
	}

	waitFor(t, "one deob left", func() bool {
		other.mu.Lock()
		links := other.snapshot()
		other.mu.Unlock()
		count := 0
		for _, l := range links {
			if l.Info().SID == "d1d1d1d1" {
				count++
			}
		}
		return count == 1
	})
	if l := other.resolveLink(t, "deob"); l.Info().PID != 4242 {
		t.Fatalf("resolved to %+v, want the original", l.Info())
	}
	if !sees(old, "alice/other#o1o1o1") {
		t.Fatal("original lost its links")
	}
}

// A session that died without closing its connection lingers on the other
// side until the idle timeout. When it comes back (same session id, new
// process, maybe a new name), the stale link must not make its address
// ambiguous or show up twice.
func TestStaleLinkToRestartedSession(t *testing.T) {
	srv := server(t)
	alice, bob := store(t, "alice"), store(t, "bob")
	pair(t, alice, bob)
	a1, _ := start(t, alice, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "a1a1a1a1", Name: "one", Host: "h", Status: "idle"})
	start(t, bob, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "b1b1b1b1", Name: "poly-44", Host: "h", Status: "idle"})
	waitFor(t, "alice to see bob", func() bool { return sees(a1, "bob/poly-44#b1b1b1") })

	a1.mu.Lock()
	peer := a1.peers.ByName("bob")
	a1.mu.Unlock()
	stale := &Link{
		m: a1, conn: &transport.Conn{Path: "direct 192.0.2.1:1"}, key: pub(t, bob), peer: peer, kind: proto.KindSession,
		info:  proto.SessionInfo{SID: "b1b1b1b1", Name: "poly-6d", Host: "h", Status: "idle"},
		since: time.Now().Add(-time.Minute), done: make(chan struct{}), pending: map[string]chan proto.Frame{},
	}
	stale.handle[0] = 0xee
	stale.once.Do(func() {}) // nothing real to close
	a1.mu.Lock()
	a1.links[stale.handle] = stale
	a1.mu.Unlock()
	t.Cleanup(func() { a1.mu.Lock(); delete(a1.links, stale.handle); a1.mu.Unlock() })

	for _, to := range []string{"bob/poly-44#b1b1b1", "bob/#b1b1b1", "bob/poly-44"} {
		if l := a1.resolveLink(t, to); l == stale {
			t.Fatalf("%s resolved to the stale link", to)
		}
	}
	n := 0
	for _, v := range a1.Views() {
		if v.Ref == "b1b1b1" {
			n++
			if v.Name != "poly-44" {
				t.Fatalf("listed the stale link: %+v", v)
			}
		}
	}
	if n != 1 {
		t.Fatalf("bob's session listed %d times", n)
	}
}
