package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// Pusher sends an opaque push to a phone's token. The server's is
// Firebase Cloud Messaging.
type Pusher interface {
	Push(ctx context.Context, token, data string) error
}

// ErrUnregistered means the push service no longer knows the token: the
// app was uninstalled or its token replaced.
var ErrUnregistered = errors.New("push token unregistered")

const (
	pushTimeout  = 15 * time.Second
	pushRate     = 0.5 // sustained pushes per second per session
	pushBurst    = 20
	pushRemember = time.Minute // answers kept for retries of the same request
)

// pushes tracks requests in flight or recently answered, so a client's
// retry doesn't push twice, and each session's push budget. Pushes run off
// the server's goroutine, hence the lock.
type pushes struct {
	mu      sync.Mutex
	reqs    map[pushReq]*pushState
	budget  map[Handle]*bucket
	running chan struct{} // bounds concurrent pushes
}

type pushReq struct {
	from Handle
	req  uint32
}

type pushState struct {
	at     time.Time
	answer *ctl // nil while in flight
}

type bucket struct {
	tokens float64
	at     time.Time
}

func (s *Server) push(addr *net.UDPAddr, from Handle, c ctl) {
	if s.Push == nil {
		s.reply(addr, ctl{T: "err", Req: c.Req, Msg: "this rendezvous server doesn't relay push notifications"})
		return
	}
	p := &s.pushes
	now := s.now()
	p.mu.Lock()
	if p.reqs == nil {
		p.reqs, p.budget, p.running = map[pushReq]*pushState{}, map[Handle]*bucket{}, make(chan struct{}, 32)
	}
	for k, st := range p.reqs {
		if now.Sub(st.at) > pushRemember {
			delete(p.reqs, k)
		}
	}
	k := pushReq{from, c.Req}
	if st, ok := p.reqs[k]; ok {
		answer := st.answer
		p.mu.Unlock()
		if answer != nil {
			s.reply(addr, *answer)
		}
		return
	}
	b := p.budget[from]
	if b == nil {
		b = &bucket{tokens: pushBurst, at: now}
		p.budget[from] = b
	}
	b.tokens = min(pushBurst, b.tokens+now.Sub(b.at).Seconds()*pushRate)
	b.at = now
	if b.tokens < 1 {
		p.mu.Unlock()
		s.reply(addr, ctl{T: "err", Req: c.Req, Msg: "too many pushes"})
		return
	}
	b.tokens--
	st := &pushState{at: now}
	p.reqs[k] = st
	p.mu.Unlock()

	go func() {
		p.running <- struct{}{}
		defer func() { <-p.running }()
		ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
		defer cancel()
		answer := ctl{T: "pushed", Req: c.Req}
		switch err := s.Push.Push(ctx, c.Token, c.Data); {
		case errors.Is(err, ErrUnregistered):
			answer.Msg = "unregistered"
		case err != nil:
			s.Log.Printf("push: %v", err)
			answer = ctl{T: "err", Req: c.Req, Msg: "push failed"}
		}
		p.mu.Lock()
		st.answer = &answer
		p.mu.Unlock()
		s.reply(addr, answer)
	}()
}

// Push asks the rendezvous server to forward data to a phone's push token.
// It returns ErrUnregistered when the push service has forgotten the token.
func (n *Node) Push(ctx context.Context, token, data string) error {
	// The server answers once the push service has; resending the same
	// request only fetches that answer.
	r, err := n.requestEvery(ctx, ctl{T: "push", Token: token, Data: data}, 3*time.Second, 6)
	if err != nil {
		return err
	}
	if r.Msg == "unregistered" {
		return ErrUnregistered
	}
	return nil
}
