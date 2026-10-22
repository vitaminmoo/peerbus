package transport

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"slices"
	"time"
)

// Server is the rendezvous and relay server. It keeps no state beyond live
// registrations, and runs on a single goroutine.
type Server struct {
	Log *log.Logger
	// MaxRegistrations caps concurrent registrations (default 4096).
	MaxRegistrations int
	// RelayRate is the sustained relay budget per handle in bytes/second
	// (default 1 MiB/s), with a burst of twice that.
	RelayRate float64

	conn    *net.UDPConn
	regs    map[Handle]*registration
	byAddr  map[string]Handle
	byPoint map[Point]map[Handle]struct{}
	lastGC  time.Time
	now     func() time.Time
}

type registration struct {
	addr   *net.UDPAddr
	points []Point
	seen   time.Time
	tokens float64
	refill time.Time
}

func NewServer(conn *net.UDPConn) *Server {
	return &Server{
		Log:     log.New(io.Discard, "", 0),
		conn:    conn,
		regs:    map[Handle]*registration{},
		byAddr:  map[string]Handle{},
		byPoint: map[Point]map[Handle]struct{}{},
		now:     time.Now,
	}
}

func (s *Server) Addr() net.Addr { return s.conn.LocalAddr() }

// Serve handles packets until ctx is done or the socket fails.
func (s *Server) Serve(ctx context.Context) error {
	if s.MaxRegistrations == 0 {
		s.MaxRegistrations = 4096
	}
	if s.RelayRate == 0 {
		s.RelayRate = 1 << 20
	}
	go func() {
		<-ctx.Done()
		s.conn.Close()
	}()
	buf := make([]byte, 65536)
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if n == 0 {
			continue
		}
		s.gc()
		switch buf[0] {
		case pControl:
			if n <= maxCtlPacket {
				var c ctl
				if json.Unmarshal(buf[1:n], &c) == nil {
					s.control(addr, c)
				}
			}
		case pRelay:
			if n > 33 {
				s.relay(addr, buf[:n])
			}
		}
	}
}

func (s *Server) live(h Handle) *registration {
	r := s.regs[h]
	if r == nil || s.now().Sub(r.seen) > regTTL {
		return nil
	}
	return r
}

// sender returns the live registration a packet came from.
func (s *Server) sender(addr *net.UDPAddr) (Handle, *registration) {
	h, ok := s.byAddr[addr.String()]
	if !ok {
		return Handle{}, nil
	}
	return h, s.live(h)
}

func (s *Server) reply(addr *net.UDPAddr, c ctl) {
	s.conn.WriteToUDP(ctlPacket(c), addr)
}

func (s *Server) control(addr *net.UDPAddr, c ctl) {
	switch c.T {
	case "reg":
		key, err := hex.DecodeString(c.Key)
		if err != nil || len(key) != 32 || len(c.Points) > maxPoints {
			return
		}
		h := handleOf(key)
		r := s.regs[h]
		if r == nil {
			if len(s.regs) >= s.MaxRegistrations {
				s.reply(addr, ctl{T: "err", Msg: "server full"})
				return
			}
			r = &registration{tokens: 2 * s.RelayRate, refill: s.now()}
			s.regs[h] = r
		}
		if r.addr != nil && r.addr.String() != addr.String() {
			delete(s.byAddr, r.addr.String()) // NAT rebinding or a moved client
		}
		s.unindex(h, r)
		r.addr, r.points, r.seen = addr, slices.Clone(c.Points), s.now()
		s.byAddr[addr.String()] = h
		for _, p := range r.points {
			m := s.byPoint[p]
			if m == nil {
				m = map[Handle]struct{}{}
				s.byPoint[p] = m
			}
			m[h] = struct{}{}
		}
		s.reply(addr, ctl{T: "regok", Handle: &h, Observed: addr.String()})

	case "query":
		self, r := s.sender(addr)
		if r == nil || c.Point == nil {
			return
		}
		var hs []Handle
		for h := range s.byPoint[*c.Point] {
			if h != self && s.live(h) != nil {
				hs = append(hs, h)
			}
		}
		s.reply(addr, ctl{T: "peers", Req: c.Req, Point: c.Point, Handles: hs})

	case "sig":
		from, r := s.sender(addr)
		if r == nil || c.To == nil {
			return
		}
		dst := s.live(*c.To)
		if dst == nil {
			s.reply(addr, ctl{T: "err", Req: c.Req, Msg: "not registered"})
			return
		}
		s.conn.WriteToUDP(ctlPacket(ctl{T: "sig", From: &from, Payload: c.Payload}), dst.addr)
	}
}

func (s *Server) relay(addr *net.UDPAddr, b []byte) {
	from, r := s.sender(addr)
	if r == nil {
		return
	}
	now := s.now()
	r.tokens = min(2*s.RelayRate, r.tokens+now.Sub(r.refill).Seconds()*s.RelayRate)
	r.refill = now
	if r.tokens < float64(len(b)) {
		return // over budget; QUIC's congestion control backs off
	}
	r.tokens -= float64(len(b))
	var to Handle
	copy(to[:], b[1:33])
	dst := s.live(to)
	if dst == nil {
		return
	}
	copy(b[1:33], from[:])
	s.conn.WriteToUDP(b, dst.addr)
}

func (s *Server) unindex(h Handle, r *registration) {
	for _, p := range r.points {
		if m := s.byPoint[p]; m != nil {
			delete(m, h)
			if len(m) == 0 {
				delete(s.byPoint, p)
			}
		}
	}
}

func (s *Server) gc() {
	now := s.now()
	if now.Sub(s.lastGC) < 5*time.Second {
		return
	}
	s.lastGC = now
	for h, r := range s.regs {
		if now.Sub(r.seen) > regTTL {
			s.unindex(h, r)
			if r.addr != nil && s.byAddr[r.addr.String()] == h {
				delete(s.byAddr, r.addr.String())
			}
			delete(s.regs, h)
		}
	}
}
