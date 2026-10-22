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
			p.Peers = append(p.Peers, &identity.Peer{Name: name, Key: identity.Key(pub(t, other)), Secret: secret, Enabled: true, Perms: identity.DefaultPerms(), PairedAt: time.Now()})
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
