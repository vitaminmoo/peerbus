package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

type Config struct {
	Key ed25519.PrivateKey
	// Server is the rendezvous server's host:port. Empty means same-machine
	// peers only.
	Server string
	// LocalDir is where sessions on this machine find each other without a
	// server. Empty disables same-machine discovery.
	LocalDir string
	// Allow decides which peer keys may connect to us.
	Allow func(ed25519.PublicKey) bool
	// OnConn receives each established inbound connection.
	OnConn func(*Conn)
	Log    *log.Logger
	// DisablePunch skips hole punching, so non-local dials use the relay.
	DisablePunch bool
	// STUN lists public STUN servers (host:port) used to learn our public
	// address when the rendezvous server can't see it, e.g. because it sits
	// behind the same NAT as us.
	STUN []string
}

// Conn is an established, mutually authenticated QUIC connection.
type Conn struct {
	*quic.Conn
	PeerKey ed25519.PublicKey
	Path    string // local, direct <addr>, or relay
	Inbound bool
}

// Peer is a session found under a meeting point.
type Peer struct {
	Handle Handle
	Local  bool // on this machine; dialed without the server
	port   int
}

var ErrNoServer = errors.New("no rendezvous server configured")

type Node struct {
	cfg     Config
	log     *log.Logger
	regKey  []byte
	handle  Handle
	cert    tls.Certificate
	machine string
	udp     *net.UDPConn
	tr      *quic.Transport
	relay   *relayConn
	rtr     *quic.Transport
	ctx     context.Context
	cancel  context.CancelFunc
	regOK   chan struct{}
	regOnce sync.Once

	mu         sync.Mutex
	srv        *net.UDPAddr
	points     []Point
	observed   string
	stunMapped string
	stunTx     [12]byte
	stunNext   int
	registered time.Time
	nextReq    uint32
	reqs       map[uint32]chan ctl
	punches    map[Handle]*punch
}

type punch struct {
	nonce  [16]byte
	answer chan sigPayload // set on the dialing side
	found  chan *net.UDPAddr
	once   sync.Once
}

func Start(ctx context.Context, cfg Config) (*Node, error) {
	if len(cfg.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("transport: missing Ed25519 key")
	}
	n := &Node{
		cfg:     cfg,
		log:     cfg.Log,
		regKey:  make([]byte, 32),
		machine: MachineID(),
		regOK:   make(chan struct{}),
		reqs:    map[uint32]chan ctl{},
		punches: map[Handle]*punch{},
	}
	if n.log == nil {
		n.log = log.New(io.Discard, "", 0)
	}
	rand.Read(n.regKey)
	n.handle = handleOf(n.regKey)
	var err error
	if n.cert, err = selfCert(cfg.Key); err != nil {
		return nil, err
	}
	if n.udp, err = net.ListenUDP("udp4", &net.UDPAddr{}); err != nil {
		return nil, err
	}
	n.ctx, n.cancel = context.WithCancel(ctx)
	n.tr = &quic.Transport{Conn: n.udp}
	n.relay = &relayConn{n: n, in: make(chan relayPkt, 512), done: make(chan struct{})}
	n.rtr = &quic.Transport{Conn: n.relay}

	ln, err := n.tr.Listen(serverTLS(n.cert, cfg.Allow), quicConf(false))
	if err != nil {
		n.Close()
		return nil, err
	}
	rln, err := n.rtr.Listen(serverTLS(n.cert, cfg.Allow), quicConf(true))
	if err != nil {
		n.Close()
		return nil, err
	}
	go n.accept(ln, false)
	go n.accept(rln, true)
	go n.readLoop()
	if cfg.Server != "" {
		go n.registerLoop()
	}
	n.writeLocal()
	return n, nil
}

func quicConf(relayed bool) *quic.Config {
	return &quic.Config{
		KeepAlivePeriod: 10 * time.Second, // also holds NAT mappings open
		MaxIdleTimeout:  45 * time.Second,
		// Relayed datagrams carry a 33-byte header, so stay at the initial
		// packet size rather than probing up to the path MTU.
		DisablePathMTUDiscovery: relayed,
	}
}

// MachineID identifies this machine, the same for every process on it.
func MachineID() string {
	b, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		h, _ := os.Hostname()
		b = []byte(h)
	}
	sum := sha256.Sum256(append([]byte("peerbus machine "), b...))
	return hex.EncodeToString(sum[:8])
}

func (n *Node) Handle() Handle { return n.handle }

// Port is the UDP port QUIC listens on.
func (n *Node) Port() int { return n.udp.LocalAddr().(*net.UDPAddr).Port }

// Observed is our address as the rendezvous server sees it.
func (n *Node) Observed() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.observed
}

// STUNMapped is our public address as a STUN server sees it.
func (n *Node) STUNMapped() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stunMapped
}

// Registered reports whether the server acknowledged us recently.
func (n *Node) Registered() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return time.Since(n.registered) < regTTL
}

// WaitRegistered blocks until the first server acknowledgement.
func (n *Node) WaitRegistered(ctx context.Context) error {
	if n.cfg.Server == "" {
		return ErrNoServer
	}
	select {
	case <-n.regOK:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetPoints replaces the meeting points this session is reachable under.
func (n *Node) SetPoints(points []Point) {
	if len(points) > maxPoints {
		points = points[:maxPoints]
	}
	n.mu.Lock()
	n.points = append([]Point(nil), points...)
	n.mu.Unlock()
	n.writeLocal()
	n.sendReg()
}

func (n *Node) Close() error {
	n.cancel()
	n.removeLocal()
	n.relay.Close()
	n.rtr.Close()
	n.tr.Close()
	return n.udp.Close()
}

func (n *Node) server() *net.UDPAddr {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.srv
}

func (n *Node) sendReg() {
	srv := n.server()
	if srv == nil {
		return
	}
	n.mu.Lock()
	c := ctl{T: "reg", Key: hex.EncodeToString(n.regKey), Points: n.points}
	n.mu.Unlock()
	n.tr.WriteTo(ctlPacket(c), srv)
}

func (n *Node) registerLoop() {
	for {
		// Re-resolve whenever we aren't registered, so DNS changes take effect.
		if !n.Registered() {
			if a, err := net.ResolveUDPAddr("udp4", n.cfg.Server); err == nil {
				n.mu.Lock()
				n.srv = a
				n.mu.Unlock()
			} else {
				n.log.Printf("resolve %s: %v", n.cfg.Server, err)
			}
		}
		n.sendReg()
		n.sendSTUN()
		wait := regRefresh
		if !n.Registered() {
			wait = time.Second
		}
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// sendSTUN asks the next configured STUN server for our public mapping.
// Repeating it with each registration refresh also keeps the mapping open.
func (n *Node) sendSTUN() {
	if len(n.cfg.STUN) == 0 {
		return
	}
	n.mu.Lock()
	host := n.cfg.STUN[n.stunNext%len(n.cfg.STUN)]
	n.stunNext++
	n.mu.Unlock()
	a, err := net.ResolveUDPAddr("udp4", host)
	if err != nil {
		return
	}
	pkt, tx := stunRequest()
	n.mu.Lock()
	n.stunTx = tx
	n.mu.Unlock()
	n.tr.WriteTo(pkt, a)
}

func (n *Node) fromServer(addr net.Addr) bool {
	srv := n.server()
	return srv != nil && addr.String() == srv.String()
}

func (n *Node) readLoop() {
	buf := make([]byte, 65536)
	for {
		k, addr, err := n.tr.ReadNonQUICPacket(n.ctx, buf)
		if err != nil {
			return
		}
		if k == 0 {
			continue
		}
		b := buf[:k]
		// STUN responses can start with 0x01 like our control packets; the
		// magic cookie tells them apart, so check first.
		if isSTUN(b) {
			n.mu.Lock()
			tx := n.stunTx
			n.mu.Unlock()
			if mapped, ok := parseSTUNResponse(b, tx); ok {
				n.mu.Lock()
				n.stunMapped = mapped.String()
				n.mu.Unlock()
			}
			continue
		}
		switch b[0] {
		case pControl:
			if !n.fromServer(addr) {
				continue
			}
			var c ctl
			if json.Unmarshal(b[1:], &c) == nil {
				n.onControl(c)
			}
		case pRelay:
			if !n.fromServer(addr) || k <= 33 {
				continue
			}
			var from Handle
			copy(from[:], b[1:33])
			select {
			case n.relay.in <- relayPkt{from: from, data: append([]byte(nil), b[33:]...)}:
			default: // QUIC treats it as loss
			}
		case pPunch, pPunchAck:
			ua, ok := addr.(*net.UDPAddr)
			if !ok || k < 49 {
				continue
			}
			var sender Handle
			copy(sender[:], b[17:49])
			n.mu.Lock()
			p := n.punches[sender]
			n.mu.Unlock()
			if p == nil || string(p.nonce[:]) != string(b[1:17]) {
				continue
			}
			if b[0] == pPunch {
				n.tr.WriteTo(punchPacket(pPunchAck, p.nonce, n.handle), ua)
			}
			p.once.Do(func() { p.found <- ua })
		}
	}
}

func (n *Node) onControl(c ctl) {
	switch c.T {
	case "regok":
		n.mu.Lock()
		n.observed, n.registered = c.Observed, time.Now()
		n.mu.Unlock()
		n.regOnce.Do(func() { close(n.regOK) })
	case "peers", "err":
		n.mu.Lock()
		ch := n.reqs[c.Req]
		n.mu.Unlock()
		if ch != nil {
			select {
			case ch <- c:
			default:
			}
		}
	case "sig":
		if c.From == nil {
			return
		}
		var sp sigPayload
		if json.Unmarshal(c.Payload, &sp) != nil {
			return
		}
		n.onSignal(*c.From, sp)
	}
}

func (n *Node) onSignal(from Handle, sp sigPayload) {
	if sp.Role == "answer" {
		n.mu.Lock()
		p := n.punches[from]
		n.mu.Unlock()
		if p != nil && p.answer != nil && hex.EncodeToString(p.nonce[:]) == sp.Nonce {
			select {
			case p.answer <- sp:
			default:
			}
		}
		return
	}
	// An offer: answer with our candidates and punch toward theirs. The
	// dialer picks the path; both listeners stay open regardless.
	p := &punch{found: make(chan *net.UDPAddr, 1)}
	nb, err := hex.DecodeString(sp.Nonce)
	if err != nil || len(nb) != 16 {
		return
	}
	copy(p.nonce[:], nb)
	n.mu.Lock()
	n.punches[from] = p
	n.mu.Unlock()
	ans, _ := json.Marshal(sigPayload{Role: "answer", Nonce: sp.Nonce, Machine: n.machine, Cands: n.candidates()})
	if srv := n.server(); srv != nil {
		n.tr.WriteTo(ctlPacket(ctl{T: "sig", To: &from, Payload: ans}), srv)
	}
	if !n.cfg.DisablePunch {
		go n.punchLoop(p, sp, 3*time.Second)
	}
}

func (n *Node) candidates() []string {
	port := n.Port()
	var out []string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, net.JoinHostPort(ipn.IP.String(), fmt.Sprint(port)))
		}
	}
	if obs := n.Observed(); obs != "" {
		out = append(out, obs)
	}
	if m := n.STUNMapped(); m != "" && m != n.Observed() {
		out = append(out, m)
	}
	return out
}

func (n *Node) punchLoop(p *punch, sp sigPayload, d time.Duration) {
	var addrs []*net.UDPAddr
	for _, c := range sp.Cands {
		a, err := net.ResolveUDPAddr("udp4", c)
		if err != nil || (a.IP.IsLoopback() && sp.Machine != n.machine) {
			continue
		}
		addrs = append(addrs, a)
	}
	pkt := punchPacket(pPunch, p.nonce, n.handle)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, a := range addrs {
			n.tr.WriteTo(pkt, a)
		}
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// request sends a control message with a fresh request id and waits for
// the matching peers/err reply, retrying over lossy UDP.
func (n *Node) request(ctx context.Context, c ctl) (ctl, error) {
	srv := n.server()
	if srv == nil {
		return ctl{}, ErrNoServer
	}
	ch := make(chan ctl, 1)
	n.mu.Lock()
	n.nextReq++
	c.Req = n.nextReq
	n.reqs[c.Req] = ch
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.reqs, c.Req)
		n.mu.Unlock()
	}()
	pkt := ctlPacket(c)
	for range 3 {
		n.tr.WriteTo(pkt, srv)
		select {
		case r := <-ch:
			if r.T == "err" {
				return r, errors.New(r.Msg)
			}
			return r, nil
		case <-ctx.Done():
			return ctl{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return ctl{}, errors.New("rendezvous server did not answer")
}

// Lookup lists the other sessions registered under p, on this machine and
// at the server.
func (n *Node) Lookup(ctx context.Context, p Point) ([]Peer, error) {
	peers := n.lookupLocal(p)
	if n.cfg.Server == "" {
		return peers, nil
	}
	seen := map[Handle]bool{}
	for _, lp := range peers {
		seen[lp.Handle] = true
	}
	r, err := n.request(ctx, ctl{T: "query", Point: &p})
	if err != nil {
		return peers, err
	}
	for _, h := range r.Handles {
		if !seen[h] && h != n.handle {
			peers = append(peers, Peer{Handle: h})
		}
	}
	return peers, nil
}

// Dial connects to a session, accepting it only if allow approves its key.
func (n *Node) Dial(ctx context.Context, p Peer, allow func(ed25519.PublicKey) bool) (*Conn, error) {
	ctls := clientTLS(n.cert, allow)
	if p.Local {
		addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p.port}
		qc, err := n.tr.Dial(ctx, addr, ctls, quicConf(false))
		if err != nil {
			return nil, err
		}
		return &Conn{Conn: qc, PeerKey: peerKey(qc.ConnectionState().TLS), Path: "local"}, nil
	}
	srv := n.server()
	if srv == nil {
		return nil, ErrNoServer
	}

	pu := &punch{answer: make(chan sigPayload, 1), found: make(chan *net.UDPAddr, 1)}
	rand.Read(pu.nonce[:])
	n.mu.Lock()
	n.punches[p.Handle] = pu
	n.mu.Unlock()
	nonce := hex.EncodeToString(pu.nonce[:])
	offer, _ := json.Marshal(sigPayload{Role: "offer", Nonce: nonce, Machine: n.machine, Cands: n.candidates()})

	var ans sigPayload
	errc := make(chan ctl, 1)
	n.mu.Lock()
	n.nextReq++
	req := n.nextReq
	n.reqs[req] = errc
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.reqs, req)
		n.mu.Unlock()
	}()
	pkt := ctlPacket(ctl{T: "sig", Req: req, To: &p.Handle, Payload: offer})
	got := false
	for i := 0; i < 3 && !got; i++ {
		n.tr.WriteTo(pkt, srv)
		select {
		case ans = <-pu.answer:
			got = true
		case e := <-errc:
			return nil, fmt.Errorf("peer %s: %s", p.Handle, e.Msg)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !got {
		return nil, fmt.Errorf("peer %s did not answer", p.Handle)
	}

	if !n.cfg.DisablePunch {
		go n.punchLoop(pu, ans, 3*time.Second)
		select {
		case addr := <-pu.found:
			dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			qc, err := n.tr.Dial(dctx, addr, ctls, quicConf(false))
			cancel()
			if err == nil {
				path := "direct " + addr.String()
				if addr.IP.IsLoopback() {
					path = "local"
				}
				return &Conn{Conn: qc, PeerKey: peerKey(qc.ConnectionState().TLS), Path: path}, nil
			}
			n.log.Printf("direct dial %s at %s failed, using relay: %v", p.Handle, addr, err)
		case <-time.After(1500 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	qc, err := n.rtr.Dial(dctx, relayAddr(p.Handle), ctls, quicConf(true))
	if err != nil {
		return nil, fmt.Errorf("relay dial %s: %w", p.Handle, err)
	}
	return &Conn{Conn: qc, PeerKey: peerKey(qc.ConnectionState().TLS), Path: "relay"}, nil
}

func (n *Node) accept(ln *quic.Listener, relayed bool) {
	for {
		qc, err := ln.Accept(n.ctx)
		if err != nil {
			return
		}
		path := "relay"
		if !relayed {
			path = "direct " + qc.RemoteAddr().String()
			if ua, ok := qc.RemoteAddr().(*net.UDPAddr); ok && ua.IP.IsLoopback() {
				path = "local"
			}
		}
		c := &Conn{Conn: qc, PeerKey: peerKey(qc.ConnectionState().TLS), Path: path, Inbound: true}
		if n.cfg.OnConn != nil {
			go n.cfg.OnConn(c)
		} else {
			qc.CloseWithError(0, "not accepting")
		}
	}
}

// ---- the relay path as a PacketConn, so QUIC runs over it unchanged ----

type relayAddr Handle

func (a relayAddr) Network() string { return "peerbus-relay" }
func (a relayAddr) String() string  { return "relay:" + Handle(a).String() }

type relayPkt struct {
	from Handle
	data []byte
}

type relayConn struct {
	n        *Node
	in       chan relayPkt
	done     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	deadline time.Time
}

func (c *relayConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.mu.Lock()
	d := c.deadline
	c.mu.Unlock()
	var timeout <-chan time.Time
	if !d.IsZero() {
		t := time.NewTimer(time.Until(d))
		defer t.Stop()
		timeout = t.C
	}
	select {
	case pk := <-c.in:
		return copy(p, pk.data), relayAddr(pk.from), nil
	case <-timeout:
		return 0, nil, os.ErrDeadlineExceeded
	case <-c.done:
		return 0, nil, net.ErrClosed
	}
}

func (c *relayConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	ra, ok := addr.(relayAddr)
	if !ok {
		return 0, fmt.Errorf("not a relay address: %v", addr)
	}
	srv := c.n.server()
	if srv == nil {
		return 0, ErrNoServer
	}
	if _, err := c.n.tr.WriteTo(relayPacket(Handle(ra), p), srv); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *relayConn) Close() error                     { c.once.Do(func() { close(c.done) }); return nil }
func (c *relayConn) LocalAddr() net.Addr              { return relayAddr(c.n.handle) }
func (c *relayConn) SetDeadline(t time.Time) error    { return c.SetReadDeadline(t) }
func (c *relayConn) SetWriteDeadline(time.Time) error { return nil }
func (c *relayConn) SetReadBuffer(int) error          { return nil }
func (c *relayConn) SetWriteBuffer(int) error         { return nil }
func (c *relayConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}
