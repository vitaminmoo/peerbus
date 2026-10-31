package mobile

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vitaminmoo/peerbus/internal/held"
	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/mesh"
	"github.com/vitaminmoo/peerbus/internal/pairing"
	"github.com/vitaminmoo/peerbus/internal/proto"
	"github.com/vitaminmoo/peerbus/internal/push"
	"github.com/vitaminmoo/peerbus/internal/transport"
)

// fcm stands in for Firebase at the rendezvous server.
type fcm struct {
	mu   sync.Mutex
	sent map[string][]string // token -> sealed notes
}

func (f *fcm) Push(ctx context.Context, token, data string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[token] = append(f.sent[token], data)
	return nil
}

func (f *fcm) to(token string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent[token]...)
}

type listener struct {
	mu   sync.Mutex
	last snapshotJSON
	held []string
}

func (l *listener) Changed(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	json.Unmarshal([]byte(s), &l.last)
}

func (l *listener) Held(id, title, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held = append(l.held, title+"|"+text)
}

func (l *listener) get() (snapshotJSON, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last, append([]string(nil), l.held...)
}

func until(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(75 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func named(t *testing.T, name string) identity.Store {
	s := identity.Store{Dir: t.TempDir()}
	if _, err := s.Identity(); err != nil {
		t.Fatal(err)
	}
	s.Update(func(p *identity.Peers) error { p.Self.Name = name; return nil })
	return s
}

func session(t *testing.T, s identity.Store, srv, state string, info proto.SessionInfo, got chan<- proto.Message) *mesh.Mesh {
	m, err := mesh.Start(context.Background(), mesh.Config{
		Store: s, StateDir: state, Server: srv, Kind: proto.KindSession, Info: info,
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

// The phone pairs from a self invite, sees a friend's message held on a
// desk session, announces it, and routes it to another session.
func TestPhone(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rv := transport.NewServer(conn)
	pushes := &fcm{sent: map[string][]string{}}
	rv.Push = pushes
	go rv.Serve(ctx)
	srv := conn.LocalAddr().String()

	alice, bob := named(t, "alice"), named(t, "bob")
	secret := identity.NewSecret()
	for _, p := range []struct {
		s, other identity.Store
		name     string
	}{{alice, bob, "bob"}, {bob, alice, "alice"}} {
		k, _ := p.other.Identity()
		p.s.Update(func(ps *identity.Peers) error {
			ps.Add(&identity.Peer{Name: p.name, Key: identity.Key(k.Public().(ed25519.PublicKey)), Secret: secret, Enabled: true, Perms: identity.DefaultPerms(), PairedAt: time.Now()})
			return nil
		})
	}

	c, err := Open(t.TempDir(), "Pixel 9")
	if err != nil {
		t.Fatal(err)
	}
	if c.Paired() {
		t.Fatal("paired before pairing")
	}
	if err := c.Pair("pb1-aaaa"); err == nil {
		t.Fatal("paired with a damaged code")
	}
	inv, err := pairing.NewInvite(alice, true, time.Minute, srv)
	if err != nil {
		t.Fatal(err)
	}
	hosted := make(chan error, 1)
	go func() {
		_, err := pairing.Host(ctx, alice, inv, "", pairing.Options{})
		hosted <- err
	}()
	// As scanned from the QR code: uppercase.
	if err := c.Pair(strings.ToUpper(inv.Encode())); err != nil {
		t.Fatal(err)
	}
	if err := <-hosted; err != nil {
		t.Fatal(err)
	}
	key, _ := alice.Identity()
	if !c.Paired() || c.Name() != "alice" || c.Fingerprint() != identity.Fingerprint(key.Public().(ed25519.PublicKey)) {
		t.Fatalf("after pairing: %v %q %q", c.Paired(), c.Name(), c.Fingerprint())
	}

	got := make(chan proto.Message, 4)
	desk := t.TempDir() // both desk sessions share its state, as on one machine
	session(t, alice, srv, desk, proto.SessionInfo{SID: "d1d1d1d1", Name: "ghidra", Host: "desk", Status: "idle"}, nil)
	notes := session(t, alice, srv, desk, proto.SessionInfo{SID: "a2a2a2a2", Name: "notes", Host: "desk", Status: "idle"}, got)
	b := session(t, bob, srv, t.TempDir(), proto.SessionInfo{SID: "b1b1b1b1", Name: "bobsess", Host: "h", Status: "idle"}, nil)

	if err := c.SetPushToken("phone-tok"); err != nil {
		t.Fatal(err)
	}
	l := &listener{}
	if err := c.Start(l); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	c.SetForeground(true)
	until(t, "the phone to see both sessions", func() bool {
		s, _ := l.get()
		return s.Live && len(s.Sessions) == 2
	})

	until(t, "the phone to register for pushes on the desk", func() bool {
		ps, _ := alice.Load()
		p := ps.Self.Pushable()
		return p != nil && p.Token == "phone-tok" && p.Name == "Pixel-9"
	})
	c.SetForeground(false) // pushes, not polling, should announce it

	until(t, "bob to reach alice's ghidra", func() bool {
		res, err := b.Send(context.Background(), "alice/ghidra", "can I borrow ghidra?", "")
		return err == nil && res.Status == "held"
	})
	var item itemJSON
	until(t, "the phone to announce the held message", func() bool {
		s, held := l.get()
		if len(s.Items) != 1 || len(held) != 1 {
			return false
		}
		item = s.Items[0]
		return true
	})
	if _, held := l.get(); held[0] != "From bob/bobsess#b1b1b1 → ghidra@desk|can I borrow ghidra?" {
		t.Fatalf("notification: %q", held[0])
	}
	if item.Section != int(held.ForSession) || item.TargetLabel != "ghidra@desk" || item.Machine != "desk" || item.Peer != "bob" {
		t.Fatalf("item: %+v", item)
	}

	note := func(i int) (n push.Note) {
		var sealed []string
		until(t, "a push", func() bool {
			sealed = pushes.to("phone-tok")
			return len(sealed) > i
		})
		js, err := c.Unseal(sealed[i])
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal([]byte(js), &n)
		return n
	}
	if n := note(0); n.Kind != push.Held || n.ID != item.ID || n.Title != "From bob/bobsess#b1b1b1 → ghidra@desk" || n.Text != "can I borrow ghidra?" {
		t.Fatalf("held push: %+v", n)
	}

	text, err := c.Approve(item.ID, "a2a2a2a2")
	if err != nil {
		t.Fatal(err)
	}
	if text != "delivered to alice/notes#a2a2a2" {
		t.Fatalf("approve said %q", text)
	}
	select {
	case msg := <-got:
		if msg.Text != "can I borrow ghidra?" {
			t.Fatalf("delivered: %+v", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("not delivered to notes")
	}
	until(t, "the message to leave the phone's list", func() bool {
		s, _ := l.get()
		return len(s.Items) == 0
	})
	if _, err := c.Deny(item.ID); err == nil {
		t.Fatal("declined a message that's gone")
	}
	if n := note(1); n.Kind != push.Gone || n.ID != item.ID {
		t.Fatalf("gone push: %+v", n)
	}

	// The history has bob's message, routed to notes.
	js, err := c.History()
	if err != nil {
		t.Fatal(err)
	}
	var hist []recordJSON
	json.Unmarshal([]byte(js), &hist)
	if len(hist) != 1 || hist[0].Text != "can I borrow ghidra?" || hist[0].To != "alice/notes#a2a2a2" || hist[0].Key == "" {
		t.Fatalf("history: %s", js)
	}

	// You write from the phone as alice/human to anyone in people; the
	// reply waits for you to read, with a push.
	s, _ := l.get()
	var to string
	for _, p := range s.People {
		if p.Address == "alice/notes#a2a2a2" {
			to = p.Address
		}
		if p.Address == "alice/human" {
			t.Fatal("you're among the people to write to")
		}
	}
	if to == "" {
		t.Fatalf("people: %+v", s.People)
	}
	if text, err := c.Send(to, "thanks for taking that", ""); err != nil || text != "sent to alice/notes#a2a2a2" {
		t.Fatalf("send: %q %v", text, err)
	}
	var mine proto.Message
	select {
	case mine = <-got:
		if mine.From != "alice/human" || mine.Text != "thanks for taking that" {
			t.Fatalf("delivered: %+v", mine)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("not delivered to notes")
	}
	if _, err := notes.Send(context.Background(), mine.From, "any time", mine.Thread); err != nil {
		t.Fatal(err)
	}
	if n := note(2); n.Kind != push.Held || n.Title != "From alice/notes#a2a2a2" || n.Text != "any time" {
		t.Fatalf("reply push: %+v", n)
	}
	until(t, "the reply to wait to be read", func() bool {
		s, _ := l.get()
		return len(s.Items) == 1 && s.Items[0].Note && s.Items[0].Section == int(held.ToRead)
	})
	s, _ = l.get()
	if text, err := c.Deny(s.Items[0].ID); err != nil || text != "dismissed" {
		t.Fatalf("dismiss: %q %v", text, err)
	}

	// Forgetting the phone stops the pushes on every machine.
	if err := c.Forget(); err != nil {
		t.Fatal(err)
	}
	ps, _ := alice.Load()
	if ps.Self.Pushable() != nil || c.Paired() {
		t.Fatalf("after forgetting: %+v, paired %v", ps.Self.Phone, c.Paired())
	}
}
