package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type fakePusher struct {
	mu   sync.Mutex
	sent []string
	slow time.Duration
}

func (f *fakePusher) Push(ctx context.Context, token, data string) error {
	f.mu.Lock()
	slow := f.slow
	f.mu.Unlock()
	time.Sleep(slow)
	if token == "gone" {
		return ErrUnregistered
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, token+"="+data)
	return nil
}

func pushServer(t *testing.T, p Pusher) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(conn)
	if p != nil {
		s.Push = p
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return conn.LocalAddr().String()
}

func registered(t *testing.T, srv string) *Node {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	n, err := Start(ctx, Config{Key: newKey(t), Server: srv, Allow: only()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	if err := n.WaitRegistered(wctx); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPush(t *testing.T) {
	// Slower than the client's resend interval, so it asks again while the
	// first push is in flight; the server must push once.
	f := &fakePusher{slow: 3500 * time.Millisecond}
	n := registered(t, pushServer(t, f))
	ctx := context.Background()
	if err := n.Push(ctx, "tok", "sealed"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.slow = 0
	f.mu.Unlock()
	if err := n.Push(ctx, "gone", "sealed"); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("unregistered token: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 || f.sent[0] != "tok=sealed" {
		t.Fatalf("pushed: %v", f.sent)
	}
}

func TestPushUnsupported(t *testing.T) {
	n := registered(t, pushServer(t, nil))
	if err := n.Push(context.Background(), "tok", "sealed"); err == nil {
		t.Fatal("pushed through a server without push")
	}
}
