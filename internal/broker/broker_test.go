package broker

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitaminmoo/peerbus/internal/client"
	"github.com/vitaminmoo/peerbus/internal/config"
	"github.com/vitaminmoo/peerbus/internal/proto"
)

type env struct {
	url    string
	tokens map[string]string
}

// alice and bob share "friends"; bob and carol share "other". alice and carol
// can't see each other.
func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{tokens: map[string]string{}}
	cfg := &config.Broker{Users: map[string]*config.User{}, Groups: map[string][]string{
		"friends": {"alice", "bob"},
		"other":   {"bob", "carol"},
	}}
	for _, u := range []string{"alice", "bob", "carol"} {
		tok := config.NewToken(u)
		e.tokens[u] = tok
		cfg.Users[u] = &config.User{Tokens: []string{config.HashToken(tok)}}
	}
	cfgPath := filepath.Join(dir, "broker.json")
	if err := config.WriteJSON(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	b, err := New(cfgPath, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.Log.SetOutput(testWriter{t})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.Serve(ctx, ln)
		close(done)
	}()
	t.Cleanup(func() {
		b.Log.SetOutput(io.Discard)
		cancel()
		<-done
	})
	e.url = "ws://" + ln.Addr().String()
	return e
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

type peer struct {
	t      *testing.T
	c      *client.Client
	frames chan proto.Frame
	info   proto.SessionInfo
	addr   string
}

func (e *env) session(t *testing.T, user, name, sid string) *peer {
	t.Helper()
	p := &peer{t: t, frames: make(chan proto.Frame, 64), info: proto.SessionInfo{
		SID: sid, Name: name, Host: "h-" + user, Account: ".claude", Cwd: "/home/" + user + "/" + name, Status: "idle",
		Resources: []proto.Resource{{Kind: "mcp", Name: "ghidra"}},
	}}
	p.c = &client.Client{
		URL: e.url,
		Hello: func() proto.Frame {
			s := p.info
			return proto.Frame{Type: proto.THello, V: proto.Version, Token: e.tokens[user], Kind: "session", Session: &s}
		},
		OnFrame: func(f proto.Frame) { p.frames <- f },
	}
	if err := p.c.Connect(context.Background()); err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	w, _ := p.c.Welcome()
	p.addr = w.Address
	t.Cleanup(p.c.Close)
	return p
}

func (e *env) control(t *testing.T, user string) *peer {
	t.Helper()
	p := &peer{t: t, frames: make(chan proto.Frame, 64)}
	p.c = &client.Client{
		URL: e.url,
		Hello: func() proto.Frame {
			return proto.Frame{Type: proto.THello, V: proto.Version, Token: e.tokens[user], Kind: "control", Watch: true}
		},
		OnFrame: func(f proto.Frame) { p.frames <- f },
	}
	if err := p.c.Connect(context.Background()); err != nil {
		t.Fatalf("connect control %s: %v", user, err)
	}
	t.Cleanup(p.c.Close)
	return p
}

func (p *peer) send(to, text, thread string) (proto.SendResult, error) {
	var res proto.SendResult
	err := p.c.Request(context.Background(), proto.Frame{Type: proto.TSend, To: to, Text: text, Thread: thread}, &res)
	return res, err
}

func (p *peer) mustSend(to, text, thread, want string) proto.SendResult {
	p.t.Helper()
	res, err := p.send(to, text, thread)
	if err != nil {
		p.t.Fatalf("send to %s: %v", to, err)
	}
	if res.Status != want {
		p.t.Fatalf("send to %s: status %q, want %q", to, res.Status, want)
	}
	return res
}

func (p *peer) next(typ string) proto.Frame {
	p.t.Helper()
	select {
	case f := <-p.frames:
		if f.Type != typ {
			p.t.Fatalf("got %s frame (%+v), want %s", f.Type, f, typ)
		}
		return f
	case <-time.After(2 * time.Second):
		p.t.Fatalf("no %s frame", typ)
	}
	return proto.Frame{}
}

func (p *peer) quiet() {
	p.t.Helper()
	select {
	case f := <-p.frames:
		p.t.Fatalf("unexpected frame %+v", f)
	case <-time.After(150 * time.Millisecond):
	}
}

func (p *peer) list() []proto.PeerView {
	p.t.Helper()
	var v []proto.PeerView
	if err := p.c.Request(context.Background(), proto.Frame{Type: proto.TList}, &v); err != nil {
		p.t.Fatal(err)
	}
	return v
}

func TestSelfSendAndReply(t *testing.T) {
	e := setup(t)
	a1 := e.session(t, "alice", "a1", "11111111-aaaa")
	a2 := e.session(t, "alice", "a2", "22222222-aaaa")

	res := a1.mustSend("a2", "hello from a1", "", "delivered")
	f := a2.next(proto.TDeliver)
	if f.Foreign || f.Msg.Text != "hello from a1" || f.Msg.From != a1.addr || f.Msg.Thread != res.Thread {
		t.Fatalf("bad delivery %+v", f.Msg)
	}
	a2.mustSend(f.Msg.From, "reply", f.Msg.Thread, "delivered")
	if r := a1.next(proto.TDeliver); r.Msg.Thread != res.Thread || r.Msg.Text != "reply" {
		t.Fatalf("bad reply %+v", r.Msg)
	}
	if _, err := a1.send(a1.addr, "me", ""); err == nil {
		t.Fatal("sending to self should fail")
	}
}

func TestVisibilityAndRedaction(t *testing.T) {
	e := setup(t)
	a1 := e.session(t, "alice", "a1", "11111111-aaaa")
	e.session(t, "bob", "b1", "33333333-bbbb")
	c1 := e.session(t, "carol", "c1", "44444444-cccc")

	users := func(vs []proto.PeerView) string {
		var s []string
		for _, v := range vs {
			s = append(s, v.Address)
		}
		return strings.Join(s, " ")
	}
	av := a1.list()
	if got := users(av); !strings.Contains(got, "bob/b1") || strings.Contains(got, "carol") {
		t.Fatalf("alice sees %s", got)
	}
	for _, v := range av {
		if v.User == "bob" && (v.Cwd != "" || v.Account != "" || len(v.Resources) == 0) {
			t.Fatalf("bob's details leaked or resources missing: %+v", v)
		}
		if v.User == "alice" && v.Cwd == "" {
			t.Fatalf("own cwd missing: %+v", v)
		}
	}
	if got := users(c1.list()); strings.Contains(got, "alice") || !strings.Contains(got, "bob/b1") {
		t.Fatalf("carol sees %s", got)
	}
	if _, err := c1.send("alice/a1", "hi", ""); err == nil {
		t.Fatal("carol reached alice without a shared group")
	}
}

func TestForeignHoldApproveAndThreadGrant(t *testing.T) {
	e := setup(t)
	a1 := e.session(t, "alice", "a1", "11111111-aaaa")
	b1 := e.session(t, "bob", "b1", "33333333-bbbb")
	bob := e.control(t, "bob")

	res := a1.mustSend("bob/b1", "secret question", "", "held")
	n := b1.next(proto.TNotice)
	if n.Kind != "held" || strings.Contains(n.Text, "secret question") {
		t.Fatalf("held notice leaked content or wrong kind: %+v", n)
	}
	h := bob.next(proto.THeld)
	if h.Held.Text != "secret question" || h.Held.Routed {
		t.Fatalf("bad held view %+v", h.Held)
	}

	// A session can't approve, even its own user's messages.
	if err := b1.c.Request(context.Background(), proto.Frame{Type: proto.TApprove, ID: h.Held.ID}, nil); err == nil {
		t.Fatal("session connection approved a message")
	}
	if err := bob.c.Request(context.Background(), proto.Frame{Type: proto.TApprove, ID: h.Held.ID}, nil); err != nil {
		t.Fatal(err)
	}
	d := b1.next(proto.TDeliver)
	if !d.Foreign || d.Msg.Text != "secret question" || d.Msg.Thread != res.Thread {
		t.Fatalf("bad approved delivery %+v", d)
	}
	if s := a1.next(proto.TNotice); s.Kind != "status" || s.Meta["status"] != "approved" {
		t.Fatalf("bad status notice %+v", s)
	}

	// The thread is granted both ways now.
	b1.mustSend(d.Msg.From, "answer", d.Msg.Thread, "delivered")
	if r := a1.next(proto.TDeliver); !r.Foreign || r.Msg.Text != "answer" {
		t.Fatalf("bad reply %+v", r)
	}
	a1.mustSend("bob/b1", "follow-up", res.Thread, "delivered")
	b1.next(proto.TDeliver)
	// A new thread needs approval again.
	a1.mustSend("bob/b1", "new topic", "", "held")
}

func TestThreadHijackIsHeld(t *testing.T) {
	e := setup(t)
	a1 := e.session(t, "alice", "a1", "11111111-aaaa")
	b1 := e.session(t, "bob", "b1", "33333333-bbbb")
	c1 := e.session(t, "carol", "c1", "44444444-cccc")
	bob := e.control(t, "bob")

	res := a1.mustSend("bob/b1", "q", "", "held")
	h := bob.next(proto.THeld)
	bob.c.Request(context.Background(), proto.Frame{Type: proto.TApprove, ID: h.Held.ID}, nil)
	b1.next(proto.TNotice)
	b1.next(proto.TDeliver)

	// carol knows the thread id but isn't a participant: she gets a fresh thread, held.
	r := c1.mustSend("bob/b1", "let me in", res.Thread, "held")
	if r.Thread == res.Thread {
		t.Fatal("carol joined alice's thread")
	}
}

func TestRoutedAndDeny(t *testing.T) {
	e := setup(t)
	a1 := e.session(t, "alice", "a1", "11111111-aaaa")
	b1 := e.session(t, "bob", "b1", "33333333-bbbb")
	bob := e.control(t, "bob")

	a1.mustSend("bob/", "who can help with ghidra?", "", "held")
	h := bob.next(proto.THeld)
	if !h.Held.Routed {
		t.Fatalf("expected routed hold %+v", h.Held)
	}
	b1.quiet() // nobody was targeted, so no session hears about it
	ctx := context.Background()
	if err := bob.c.Request(ctx, proto.Frame{Type: proto.TApprove, ID: h.Held.ID}, nil); err == nil {
		t.Fatal("routed approve without a target should fail")
	}
	if err := bob.c.Request(ctx, proto.Frame{Type: proto.TApprove, ID: h.Held.ID, To: "alice/a1"}, nil); err == nil {
		t.Fatal("approve routed to someone else's session")
	}
	if err := bob.c.Request(ctx, proto.Frame{Type: proto.TApprove, ID: h.Held.ID, To: "b1"}, nil); err != nil {
		t.Fatal(err)
	}
	b1.next(proto.TDeliver)
	a1.next(proto.TNotice)

	a1.mustSend("bob/b1", "spam", "", "held")
	b1.next(proto.TNotice)
	h = bob.next(proto.THeld)
	if err := bob.c.Request(ctx, proto.Frame{Type: proto.TDeny, ID: h.Held.ID}, nil); err != nil {
		t.Fatal(err)
	}
	if s := a1.next(proto.TNotice); s.Meta["status"] != "declined" {
		t.Fatalf("bad deny notice %+v", s)
	}
	b1.quiet()
}

func TestOfflineQueue(t *testing.T) {
	e := setup(t)
	a1 := e.session(t, "alice", "a1", "11111111-aaaa")
	a2 := e.session(t, "alice", "a2", "22222222-aaaa")
	a2.c.Close()
	time.Sleep(100 * time.Millisecond)
	a1.mustSend("a2", "while you were out", "", "queued")
	a2 = e.session(t, "alice", "a2", "22222222-aaaa")
	if f := a2.next(proto.TDeliver); f.Msg.Text != "while you were out" {
		t.Fatalf("bad queued delivery %+v", f.Msg)
	}
}

func TestIdleSubscription(t *testing.T) {
	e := setup(t)
	a1 := e.session(t, "alice", "a1", "11111111-aaaa")
	a2 := e.session(t, "alice", "a2", "22222222-aaaa")
	ctx := context.Background()
	a2.c.Send(ctx, proto.Frame{Type: proto.TUpdate, Session: &proto.SessionInfo{Status: "busy"}})
	time.Sleep(50 * time.Millisecond)
	var res map[string]string
	if err := a1.c.Request(ctx, proto.Frame{Type: proto.TSubscribeIdle, Peer: "a2"}, &res); err != nil || res["status"] != "subscribed" {
		t.Fatalf("subscribe: %v %v", res, err)
	}
	a2.c.Send(ctx, proto.Frame{Type: proto.TUpdate, Session: &proto.SessionInfo{Status: "idle"}})
	if n := a1.next(proto.TNotice); n.Kind != "idle" || n.Meta["state"] != "idle" {
		t.Fatalf("bad idle notice %+v", n)
	}
	// one-shot
	a2.c.Send(ctx, proto.Frame{Type: proto.TUpdate, Session: &proto.SessionInfo{Status: "busy"}})
	a2.c.Send(ctx, proto.Frame{Type: proto.TUpdate, Session: &proto.SessionInfo{Status: "idle"}})
	a1.quiet()
}

func TestAmbiguousNames(t *testing.T) {
	e := setup(t)
	a1 := e.session(t, "alice", "a1", "11111111-aaaa")
	e.session(t, "alice", "x", "55555555-aaaa")
	x2 := e.session(t, "alice", "x", "66666666-aaaa")
	_, err := a1.send("x", "hi", "")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
	a1.mustSend(x2.addr, "hi", "", "delivered")
	a1.mustSend("x [666666]", "hi", "", "delivered")
}

func TestBadToken(t *testing.T) {
	e := setup(t)
	e.tokens["mallory"] = "pb_mallory_nope"
	c := &client.Client{URL: e.url, Hello: func() proto.Frame {
		return proto.Frame{Type: proto.THello, V: proto.Version, Token: "pb_mallory_nope", Kind: "control"}
	}}
	if err := c.Connect(context.Background()); err == nil || !strings.Contains(err.Error(), "unknown token") {
		t.Fatalf("want token rejection, got %v", err)
	}
}
