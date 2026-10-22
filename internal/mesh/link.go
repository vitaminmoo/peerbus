package mesh

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/proto"
	"github.com/vitaminmoo/peerbus/internal/transport"
)

// Link is one connection to another session: a QUIC connection with a single
// long-lived stream carrying newline-delimited frames both ways.
type Link struct {
	m      *Mesh
	conn   *transport.Conn
	s      *quic.Stream
	r      *bufio.Reader
	handle transport.Handle // the remote session's
	key    ed25519.PublicKey
	since  time.Time // when it was registered; set before it's shared

	wmu sync.Mutex

	mu         sync.Mutex
	peer       *identity.Peer // nil for one of our own devices
	kind       string
	info       proto.SessionInfo
	pending    map[string]chan proto.Frame
	sends      []time.Time
	notifyIdle bool // they want our next idle
	wantIdle   bool // we want theirs

	done chan struct{}
	once sync.Once
}

func newReader(s *quic.Stream) *bufio.Reader { return bufio.NewReaderSize(s, 1<<16) }

// Info is what the remote session last said about itself.
func (l *Link) Info() proto.SessionInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.info
}

// Address is the remote session's address from here.
func (l *Link) Address() string { return l.m.addr(l) }

func (l *Link) close() {
	l.once.Do(func() {
		close(l.done)
		l.conn.CloseWithError(0, "bye")
		l.mu.Lock()
		for req, ch := range l.pending {
			close(ch)
			delete(l.pending, req)
		}
		l.mu.Unlock()
	})
}

func (l *Link) write(f proto.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	l.wmu.Lock()
	defer l.wmu.Unlock()
	if _, err := l.s.Write(append(b, '\n')); err != nil {
		l.close()
		return err
	}
	return nil
}

func (l *Link) readWithin(d time.Duration) (proto.Frame, error) {
	l.s.SetReadDeadline(time.Now().Add(d))
	defer l.s.SetReadDeadline(time.Time{})
	return l.read()
}

func (l *Link) read() (proto.Frame, error) {
	line, err := l.r.ReadBytes('\n')
	if err != nil {
		return proto.Frame{}, err
	}
	if len(line) > 2*proto.MaxText {
		return proto.Frame{}, errors.New("frame too large")
	}
	var f proto.Frame
	if err := json.Unmarshal(line, &f); err != nil {
		return proto.Frame{}, err
	}
	return f, nil
}

func (l *Link) readLoop() {
	defer l.m.unlink(l)
	for {
		f, err := l.read()
		if err != nil {
			return
		}
		if f.Type == proto.TResult {
			l.mu.Lock()
			ch := l.pending[f.Req]
			delete(l.pending, f.Req)
			l.mu.Unlock()
			if ch != nil {
				ch <- f
			}
			continue
		}
		l.m.handle(l, f)
	}
}

// Request sends a frame and waits for its result, decoding Data into out.
func (l *Link) Request(ctx context.Context, f proto.Frame, out any) error {
	f.Req = proto.NewID(6)
	ch := make(chan proto.Frame, 1)
	l.mu.Lock()
	l.pending[f.Req] = ch
	l.mu.Unlock()
	if err := l.write(f); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestWait)
	defer cancel()
	select {
	case r, ok := <-ch:
		if !ok {
			return errors.New("connection closed")
		}
		if !r.OK {
			return errors.New(r.Error)
		}
		if out != nil && r.Data != nil {
			b, _ := json.Marshal(r.Data)
			return json.Unmarshal(b, out)
		}
		return nil
	case <-l.done:
		return errors.New("connection closed")
	case <-ctx.Done():
		l.mu.Lock()
		delete(l.pending, f.Req)
		l.mu.Unlock()
		return fmt.Errorf("%s did not answer", l.m.describe(l))
	}
}

func (l *Link) reply(req string, data any, err error) {
	if req == "" {
		return
	}
	if err != nil {
		l.write(proto.Frame{Type: proto.TResult, Req: req, Error: err.Error()})
		return
	}
	l.write(proto.Frame{Type: proto.TResult, Req: req, OK: true, Data: data})
}

// isLocalControl reports whether this is the CLI on this machine, run by our
// own OS user.
func (l *Link) isLocalControl() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peer == nil && l.kind == proto.KindControl && l.conn.Path == "local" && l.conn.Inbound
}

// isOwnControl reports whether this is our own CLI or TUI on any of our
// machines: what may list, approve and deny held messages. Sessions (the
// agents) never may.
func (l *Link) isOwnControl() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peer == nil && l.kind == proto.KindControl && l.conn.Inbound
}

func (l *Link) rateOK() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	keep := l.sends[:0]
	for _, t := range l.sends {
		if now.Sub(t) <= rateWindow {
			keep = append(keep, t)
		}
	}
	l.sends = keep
	if len(l.sends) >= rateMax {
		return false
	}
	l.sends = append(l.sends, now)
	return true
}
