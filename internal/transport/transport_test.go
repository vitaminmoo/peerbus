package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pub(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

func only(keys ...ed25519.PrivateKey) func(ed25519.PublicKey) bool {
	return func(p ed25519.PublicKey) bool {
		for _, k := range keys {
			if pub(k).Equal(p) {
				return true
			}
		}
		return false
	}
}

func startServer(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(conn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return conn.LocalAddr().String()
}

// echo serves every stream by copying it back.
func echo(c *Conn) {
	for {
		s, err := c.AcceptStream(context.Background())
		if err != nil {
			return
		}
		go func() { io.Copy(s, s); s.Close() }()
	}
}

func startNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	if cfg.OnConn == nil {
		cfg.OnConn = echo
	}
	n, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	if cfg.Server != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := n.WaitRegistered(ctx); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	return n
}

func point(s string) Point {
	var p Point
	copy(p[:], s)
	return p
}

func roundTrip(t *testing.T, c *Conn, size int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := c.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, size)
	rand.Read(want)
	go func() { s.Write(want); s.Close() }()
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch: %d bytes back, want %d", len(got), len(want))
	}
}

func lookupOne(t *testing.T, n *Node, p Point) Peer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peers, err := n.Lookup(ctx, p)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("lookup found %d peers, want 1", len(peers))
	}
	return peers[0]
}

func dial(t *testing.T, n *Node, p Peer, allow func(ed25519.PublicKey) bool) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := n.Dial(ctx, p, allow)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.CloseWithError(0, "") })
	return c
}

func TestSameMachineWithoutServer(t *testing.T) {
	dir := t.TempDir()
	ka, kb := newKey(t), newKey(t)
	a := startNode(t, Config{Key: ka, LocalDir: dir, Allow: only(kb)})
	b := startNode(t, Config{Key: kb, LocalDir: dir, Allow: only(ka)})
	p := point("pair-ab")
	a.SetPoints([]Point{p})
	b.SetPoints([]Point{p})

	peer := lookupOne(t, a, p)
	if !peer.Local || peer.Handle != b.Handle() {
		t.Fatalf("bad local peer %+v", peer)
	}
	c := dial(t, a, peer, only(kb))
	if c.Path != "local" || !c.PeerKey.Equal(pub(kb)) {
		t.Fatalf("path %q key ok=%v", c.Path, c.PeerKey.Equal(pub(kb)))
	}
	roundTrip(t, c, 4096)

	if _, err := a.Dial(context.Background(), Peer{Handle: b.Handle()}, only(kb)); err != ErrNoServer {
		t.Fatalf("non-local dial without a server: %v", err)
	}
}

func TestServerPathIsDirect(t *testing.T) {
	srv := startServer(t)
	ka, kb := newKey(t), newKey(t)
	a := startNode(t, Config{Key: ka, Server: srv, Allow: only(kb)})
	b := startNode(t, Config{Key: kb, Server: srv, Allow: only(ka)})
	p := point("pair-ab")
	b.SetPoints([]Point{p})
	time.Sleep(50 * time.Millisecond)

	if a.Observed() == "" {
		t.Fatal("no observed address")
	}
	c := dial(t, a, lookupOne(t, a, p), only(kb))
	if c.Path == "relay" {
		t.Fatalf("expected a direct path on one host, got %q", c.Path)
	}
	roundTrip(t, c, 64*1024)
}

func TestRelayPath(t *testing.T) {
	srv := startServer(t)
	ka, kb := newKey(t), newKey(t)
	a := startNode(t, Config{Key: ka, Server: srv, Allow: only(kb), DisablePunch: true})
	b := startNode(t, Config{Key: kb, Server: srv, Allow: only(ka), DisablePunch: true})
	p := point("pair-ab")
	b.SetPoints([]Point{p})
	time.Sleep(50 * time.Millisecond)

	c := dial(t, a, lookupOne(t, a, p), only(kb))
	if c.Path != "relay" {
		t.Fatalf("path %q, want relay", c.Path)
	}
	roundTrip(t, c, 1<<20)
}

func TestPinning(t *testing.T) {
	srv := startServer(t)
	ka, kb, kc := newKey(t), newKey(t), newKey(t)
	a := startNode(t, Config{Key: ka, Server: srv, Allow: only(kb)})
	b := startNode(t, Config{Key: kb, Server: srv, Allow: only(kc)}) // doesn't trust a
	p := point("pair-ab")
	b.SetPoints([]Point{p})
	time.Sleep(50 * time.Millisecond)
	peer := lookupOne(t, a, p)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// b rejects a's key. With TLS 1.3 the client may finish its side of the
	// handshake first, so the rejection can surface on the first stream.
	if c, err := a.Dial(ctx, peer, only(kb)); err == nil {
		_, err = c.OpenStreamSync(ctx)
		if err == nil {
			s, _ := c.OpenStreamSync(ctx)
			s.Write([]byte("x"))
			_, err = s.Read(make([]byte, 1))
		}
		if err == nil {
			t.Fatal("b accepted a key it doesn't trust")
		}
	}
	// a refuses b's key.
	if _, err := a.Dial(ctx, peer, only(kc)); err == nil {
		t.Fatal("a accepted a key it didn't ask for")
	}
}

func TestLookupIsolation(t *testing.T) {
	srv := startServer(t)
	ka, kb, kc := newKey(t), newKey(t), newKey(t)
	a := startNode(t, Config{Key: ka, Server: srv, Allow: only(kb)})
	b := startNode(t, Config{Key: kb, Server: srv, Allow: only(ka)})
	c := startNode(t, Config{Key: kc, Server: srv, Allow: only(kb)})
	b.SetPoints([]Point{point("pair-ab")})
	c.SetPoints([]Point{point("pair-bc")})
	time.Sleep(50 * time.Millisecond)

	ctx := context.Background()
	if peers, err := a.Lookup(ctx, point("pair-bc")); err != nil || len(peers) != 1 {
		// a can see c only because a knows the point; that's the whole model.
		t.Fatalf("lookup with the right point: %v %v", peers, err)
	}
	if peers, err := a.Lookup(ctx, point("guess")); err != nil || len(peers) != 0 {
		t.Fatalf("lookup with a wrong point found %v (%v)", peers, err)
	}
	var unknown Handle
	rand.Read(unknown[:])
	_, err := a.Dial(ctx, Peer{Handle: unknown}, only(kb))
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("dial to an unregistered handle: %v", err)
	}
}
