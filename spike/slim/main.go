// Command slimspike is the hand-assembled counterpart to the libp2p spike:
// QUIC (TLS 1.3, pinned Ed25519 certs) over one UDP socket per node, with a
// small server that does three jobs, all keyed by unguessable registered ids:
// tell a client its public address (STUN's job), pass signaling between two
// registered ids, and forward datagrams between two registered ids (TURN's
// job). Nodes exchange candidate addresses, punch simultaneously, and use the
// first address that answers; otherwise the same QUIC stack runs over the
// relay through a virtual PacketConn.
//
//	slimspike server -dir D -listen IP:PORT
//	slimspike node   -dir D -name N [-dial M] [-nopunch] [-probe]
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// Every non-QUIC packet starts with a byte below 0x40 (QUIC always sets that
// bit), so control traffic and QUIC share one socket.
const (
	pControl  = 0x01 // + JSON
	pRelay    = 0x02 // + 32-byte id + datagram (to server: target id; from server: sender id)
	pPunch    = 0x03 // + 16-byte nonce + 32-byte sender id
	pPunchAck = 0x04 // + 16-byte nonce + 32-byte sender id

	alpn     = "peerbus/1"
	regTTL   = 30 * time.Second
	echoLine = "ping\n"
)

type ID [32]byte

func (id ID) String() string { return hex.EncodeToString(id[:3]) }

func parseID(s string) (ID, error) {
	var id ID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return id, fmt.Errorf("bad id %q", s)
	}
	copy(id[:], b)
	return id, nil
}

type ctl struct {
	T        string          `json:"t"`
	ID       string          `json:"id,omitempty"`
	To       string          `json:"to,omitempty"`
	From     string          `json:"from,omitempty"`
	Observed string          `json:"observed,omitempty"`
	Msg      string          `json:"msg,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

func ctlPacket(c ctl) []byte {
	b, _ := json.Marshal(c)
	return append([]byte{pControl}, b...)
}

var t0 = time.Now()

func logf(format string, args ...any) {
	fmt.Printf("[%6.2fs] %s\n", time.Since(t0).Seconds(), fmt.Sprintf(format, args...))
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: slimspike server|node ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "server":
		err = runServer(os.Args[2:])
	case "node":
		err = runNode(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---- server ----

type reg struct {
	addr *net.UDPAddr
	seen time.Time
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	dir := fs.String("dir", ".", "shared dir")
	listen := fs.String("listen", "0.0.0.0:3478", "UDP listen address")
	fs.Parse(args)
	laddr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return err
	}
	os.WriteFile(filepath.Join(*dir, "server.addr"), []byte(conn.LocalAddr().String()), 0o600)
	logf("server on %s", conn.LocalAddr())

	regs := map[ID]reg{}
	byAddr := map[string]ID{}
	live := func(id ID) (*net.UDPAddr, bool) {
		r, ok := regs[id]
		return r.addr, ok && time.Since(r.seen) < regTTL
	}
	buf := make([]byte, 65536)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return err
		}
		if n < 1 {
			continue
		}
		b := buf[:n]
		switch b[0] {
		case pControl:
			var c ctl
			if json.Unmarshal(b[1:], &c) != nil {
				continue
			}
			switch c.T {
			case "reg":
				id, err := parseID(c.ID)
				if err != nil {
					continue
				}
				regs[id] = reg{addr: addr, seen: time.Now()}
				byAddr[addr.String()] = id
				conn.WriteToUDP(ctlPacket(ctl{T: "regok", Observed: addr.String()}), addr)
			case "sig":
				from, ok := byAddr[addr.String()]
				if _, reg := live(from); !ok || !reg {
					continue
				}
				to, err := parseID(c.To)
				if err != nil {
					continue
				}
				dst, ok := live(to)
				if !ok {
					// Only someone who already knows the id learns it isn't online.
					conn.WriteToUDP(ctlPacket(ctl{T: "err", To: c.To, Msg: "not registered"}), addr)
					continue
				}
				conn.WriteToUDP(ctlPacket(ctl{T: "sig", From: hex.EncodeToString(from[:]), Payload: c.Payload}), dst)
			}
		case pRelay:
			if n < 33 {
				continue
			}
			from, ok := byAddr[addr.String()]
			if !ok {
				continue
			}
			var to ID
			copy(to[:], b[1:33])
			dst, ok := live(to)
			if !ok {
				continue
			}
			out := make([]byte, 0, n)
			out = append(out, pRelay)
			out = append(out, from[:]...)
			out = append(out, b[33:]...)
			conn.WriteToUDP(out, dst)
		}
	}
}

// ---- node ----

type peerFile struct {
	ID  string `json:"id"`
	Pub string `json:"pub"`
}

type sigPayload struct {
	Role  string   `json:"role"` // offer | answer
	Nonce string   `json:"nonce"`
	Cands []string `json:"cands"`
}

type punch struct {
	peer  ID
	nonce [16]byte
	found chan *net.UDPAddr
	once  sync.Once
}

type Node struct {
	name    string
	dir     string
	id      ID
	key     ed25519.PrivateKey
	cert    tls.Certificate
	trusted map[string]bool // hex ed25519 public keys (stands in for pairing)

	udp    *net.UDPConn
	tr     *quic.Transport
	srv    *net.UDPAddr
	relay  *relayConn
	rtr    *quic.Transport
	regOK  chan struct{}
	regOne sync.Once

	mu       sync.Mutex
	observed string
	answers  map[ID]chan sigPayload
	errs     map[ID]chan string
	punches  map[ID]*punch
}

func runNode(args []string) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	dir := fs.String("dir", ".", "shared dir")
	name := fs.String("name", "", "node name")
	dial := fs.String("dial", "", "node to dial")
	noPunch := fs.Bool("nopunch", false, "skip hole punching (force the relay path)")
	probe := fs.Bool("probe", false, "signal a random unregistered id")
	fs.Parse(args)
	ctx := context.Background()

	srvB, err := waitFile(filepath.Join(*dir, "server.addr"), 10*time.Second)
	if err != nil {
		return err
	}
	srv, err := net.ResolveUDPAddr("udp", string(srvB))
	if err != nil {
		return err
	}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	n := &Node{
		name: *name, dir: *dir, key: key, cert: makeCert(key), trusted: map[string]bool{},
		srv: srv, regOK: make(chan struct{}),
		answers: map[ID]chan sigPayload{}, errs: map[ID]chan string{}, punches: map[ID]*punch{},
	}
	rand.Read(n.id[:])
	if n.udp, err = net.ListenUDP("udp4", &net.UDPAddr{}); err != nil {
		return err
	}
	n.tr = &quic.Transport{Conn: n.udp}
	n.relay = &relayConn{n: n, in: make(chan relayPkt, 256), done: make(chan struct{})}
	n.rtr = &quic.Transport{Conn: n.relay}

	b, _ := json.Marshal(peerFile{ID: hex.EncodeToString(n.id[:]), Pub: hex.EncodeToString(pub)})
	writeFile(filepath.Join(*dir, *name+".peer"), b)
	n.loadTrusted(*dir)

	ln, err := n.tr.Listen(n.serverTLS(), quicConf(false))
	if err != nil {
		return err
	}
	rln, err := n.rtr.Listen(n.serverTLS(), quicConf(true))
	if err != nil {
		return err
	}
	go n.accept(ctx, ln, "direct")
	go n.accept(ctx, rln, "relay")
	go n.readLoop(ctx)
	go n.registerLoop(ctx)

	start := time.Now()
	select {
	case <-n.regOK:
	case <-time.After(5 * time.Second):
		return errors.New("server never acknowledged registration")
	}
	n.mu.Lock()
	logf("%s = %s registered in %s, observed as %s, local %s", *name, n.id, time.Since(start).Round(time.Microsecond), n.observed, n.udp.LocalAddr())
	n.mu.Unlock()

	if *probe {
		var fake ID
		rand.Read(fake[:])
		_, err := n.signal(fake, sigPayload{Role: "offer", Nonce: "00"}, 2*time.Second)
		logf("probe signal to unregistered id %s: %v", fake, err)
	}
	if *dial == "" {
		select {}
	}
	pb, err := waitFile(filepath.Join(*dir, *dial+".peer"), 15*time.Second)
	if err != nil {
		return err
	}
	var pf peerFile
	json.Unmarshal(pb, &pf)
	target, err := parseID(pf.ID)
	if err != nil {
		return err
	}
	n.loadTrusted(*dir)
	return n.dialAndPing(ctx, target, pf.Pub, *noPunch)
}

func waitFile(path string, d time.Duration) ([]byte, error) {
	deadline := time.Now().Add(d)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			return b, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func writeFile(path string, b []byte) {
	os.WriteFile(path+".tmp", b, 0o600)
	os.Rename(path+".tmp", path)
}

func (n *Node) loadTrusted(dir string) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.peer"))
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var pf peerFile
		if json.Unmarshal(b, &pf) == nil {
			n.trusted[pf.Pub] = true
		}
	}
}

func quicConf(relayed bool) *quic.Config {
	return &quic.Config{
		KeepAlivePeriod: 10 * time.Second, // also keeps NAT mappings open
		MaxIdleTimeout:  60 * time.Second,
		// Relayed datagrams carry a 33-byte header; stay at the initial size.
		DisablePathMTUDiscovery: relayed,
	}
}

func makeCert(key ed25519.PrivateKey) tls.Certificate {
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		panic(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// pinned accepts exactly the peer certificates whose Ed25519 key passes ok.
// TLS 1.3 still checks the CertificateVerify signature against that key, so
// the peer proves it holds the private key.
func pinned(ok func(pubHex string) bool) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("no peer certificate")
		}
		c, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		pub, isEd := c.PublicKey.(ed25519.PublicKey)
		if !isEd || !ok(hex.EncodeToString(pub)) {
			return errors.New("peer key is not pinned")
		}
		return nil
	}
}

func (n *Node) serverTLS() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{n.cert},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: pinned(func(p string) bool {
			n.mu.Lock()
			ok := n.trusted[p]
			n.mu.Unlock()
			if !ok {
				n.loadTrusted(n.dir) // stands in for "a pairing completed since startup"
				n.mu.Lock()
				ok = n.trusted[p]
				n.mu.Unlock()
			}
			return ok
		}),
		NextProtos: []string{alpn},
		MinVersion: tls.VersionTLS13,
	}
}

func (n *Node) clientTLS(expectPub string) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{n.cert},
		InsecureSkipVerify:    true, // replaced by the pinned-key check
		VerifyPeerCertificate: pinned(func(p string) bool { return p == expectPub }),
		NextProtos:            []string{alpn},
		ServerName:            "peerbus",
		MinVersion:            tls.VersionTLS13,
	}
}

func (n *Node) registerLoop(ctx context.Context) {
	pkt := ctlPacket(ctl{T: "reg", ID: hex.EncodeToString(n.id[:])})
	for {
		n.tr.WriteTo(pkt, n.srv)
		select {
		case <-ctx.Done():
			return
		case <-n.regOK:
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (n *Node) readLoop(ctx context.Context) {
	buf := make([]byte, 65536)
	for {
		k, addr, err := n.tr.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		b := buf[:k]
		ua, _ := addr.(*net.UDPAddr)
		switch b[0] {
		case pControl:
			if ua == nil || ua.String() != n.srv.String() {
				continue // control messages only come from the server
			}
			var c ctl
			if json.Unmarshal(b[1:], &c) != nil {
				continue
			}
			n.onControl(c)
		case pRelay:
			if ua == nil || ua.String() != n.srv.String() || k < 33 {
				continue
			}
			var from ID
			copy(from[:], b[1:33])
			data := append([]byte(nil), b[33:]...)
			select {
			case n.relay.in <- relayPkt{from: from, data: data}:
			default:
			}
		case pPunch, pPunchAck:
			if k < 49 {
				continue
			}
			var sender ID
			copy(sender[:], b[17:49])
			n.mu.Lock()
			p := n.punches[sender]
			n.mu.Unlock()
			if p == nil || string(p.nonce[:]) != string(b[1:17]) {
				continue
			}
			if b[0] == pPunch {
				n.tr.WriteTo(punchPacket(pPunchAck, p.nonce, n.id), ua)
			}
			p.once.Do(func() { p.found <- ua })
		}
	}
}

func punchPacket(kind byte, nonce [16]byte, id ID) []byte {
	b := []byte{kind}
	b = append(b, nonce[:]...)
	return append(b, id[:]...)
}

func (n *Node) onControl(c ctl) {
	switch c.T {
	case "regok":
		n.mu.Lock()
		n.observed = c.Observed
		n.mu.Unlock()
		n.regOne.Do(func() { close(n.regOK) })
	case "err":
		id, err := parseID(c.To)
		if err != nil {
			return
		}
		n.mu.Lock()
		ch := n.errs[id]
		n.mu.Unlock()
		if ch != nil {
			select {
			case ch <- c.Msg:
			default:
			}
		}
	case "sig":
		from, err := parseID(c.From)
		if err != nil {
			return
		}
		var sp sigPayload
		if json.Unmarshal(c.Payload, &sp) != nil {
			return
		}
		if sp.Role == "answer" {
			n.mu.Lock()
			ch := n.answers[from]
			n.mu.Unlock()
			if ch != nil {
				select {
				case ch <- sp:
				default:
				}
			}
			return
		}
		// An offer: answer with our candidates and punch toward theirs. The
		// dialer decides the path; we just keep both listeners open.
		p := n.newPunch(from, sp.Nonce)
		ans := sigPayload{Role: "answer", Nonce: sp.Nonce, Cands: n.candidates()}
		pl, _ := json.Marshal(ans)
		n.tr.WriteTo(ctlPacket(ctl{T: "sig", To: hex.EncodeToString(from[:]), Payload: pl}), n.srv)
		go n.punchLoop(p, sp.Cands, 3*time.Second)
	}
}

func (n *Node) newPunch(peer ID, nonceHex string) *punch {
	p := &punch{peer: peer, found: make(chan *net.UDPAddr, 1)}
	nb, _ := hex.DecodeString(nonceHex)
	copy(p.nonce[:], nb)
	n.mu.Lock()
	n.punches[peer] = p
	n.mu.Unlock()
	return p
}

func (n *Node) candidates() []string {
	port := n.udp.LocalAddr().(*net.UDPAddr).Port
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, net.JoinHostPort(ipn.IP.String(), fmt.Sprint(port)))
	}
	n.mu.Lock()
	if n.observed != "" {
		out = append(out, n.observed)
	}
	n.mu.Unlock()
	return out
}

func (n *Node) punchLoop(p *punch, cands []string, d time.Duration) {
	var addrs []*net.UDPAddr
	for _, c := range cands {
		if a, err := net.ResolveUDPAddr("udp4", c); err == nil {
			addrs = append(addrs, a)
		}
	}
	pkt := punchPacket(pPunch, p.nonce, n.id)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, a := range addrs {
			n.tr.WriteTo(pkt, a)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (n *Node) signal(to ID, sp sigPayload, wait time.Duration) (sigPayload, error) {
	ans := make(chan sigPayload, 1)
	errc := make(chan string, 1)
	n.mu.Lock()
	n.answers[to], n.errs[to] = ans, errc
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.answers, to)
		delete(n.errs, to)
		n.mu.Unlock()
	}()
	pl, _ := json.Marshal(sp)
	n.tr.WriteTo(ctlPacket(ctl{T: "sig", To: hex.EncodeToString(to[:]), Payload: pl}), n.srv)
	select {
	case a := <-ans:
		return a, nil
	case e := <-errc:
		return sigPayload{}, errors.New(e)
	case <-time.After(wait):
		return sigPayload{}, errors.New("no answer")
	}
}

func (n *Node) dialAndPing(ctx context.Context, target ID, targetPub string, noPunch bool) error {
	start := time.Now()
	var nonce [16]byte
	rand.Read(nonce[:])
	p := n.newPunch(target, hex.EncodeToString(nonce[:]))
	ans, err := n.signal(target, sigPayload{Role: "offer", Nonce: hex.EncodeToString(nonce[:]), Cands: n.candidates()}, 3*time.Second)
	if err != nil {
		return fmt.Errorf("signal %s: %w", target, err)
	}
	logf("answer from %s after %s, candidates %v", target, time.Since(start).Round(time.Microsecond), ans.Cands)

	var conn *quic.Conn
	path := "relay"
	if !noPunch {
		go n.punchLoop(p, ans.Cands, 3*time.Second)
		select {
		case addr := <-p.found:
			logf("punch answered from %s after %s", addr, time.Since(start).Round(time.Microsecond))
			dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			conn, err = n.tr.Dial(dctx, addr, n.clientTLS(targetPub), quicConf(false))
			cancel()
			if err != nil {
				logf("direct QUIC dial to %s failed: %v", addr, err)
				conn = nil
			} else {
				path = "direct " + addr.String()
			}
		case <-time.After(2 * time.Second):
			logf("no punch answer within 2s")
		}
	}
	if conn == nil {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err = n.rtr.Dial(dctx, relayAddr(target), n.clientTLS(targetPub), quicConf(true))
		cancel()
		if err != nil {
			return fmt.Errorf("relay dial: %w", err)
		}
	}
	logf("QUIC up to %s via %s after %s (TLS %s)", target, path, time.Since(start).Round(time.Microsecond), tls.VersionName(conn.ConnectionState().TLS.Version))

	s, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	r := bufio.NewReader(s)
	var rtts []string
	for range 5 {
		t := time.Now()
		if _, err := s.Write([]byte(echoLine)); err != nil {
			return err
		}
		if _, err := r.ReadString('\n'); err != nil {
			return err
		}
		rtts = append(rtts, time.Since(t).Round(time.Microsecond).String())
	}
	logf("RESULT path=%s rtts=%s", path, strings.Join(rtts, " "))
	conn.CloseWithError(0, "done")
	return nil
}

func (n *Node) accept(ctx context.Context, ln *quic.Listener, kind string) {
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		logf("accepted %s QUIC from %s", kind, conn.RemoteAddr())
		go func() {
			for {
				s, err := conn.AcceptStream(ctx)
				if err != nil {
					return
				}
				go func() {
					r := bufio.NewReader(s)
					for {
						line, err := r.ReadString('\n')
						if err != nil {
							return
						}
						s.Write([]byte(line))
					}
				}()
			}
		}()
	}
}

// ---- relay path as a PacketConn, so QUIC runs over it unchanged ----

type relayAddr ID

func (a relayAddr) Network() string { return "peerbus-relay" }
func (a relayAddr) String() string  { return "relay:" + ID(a).String() }

type relayPkt struct {
	from ID
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
	b := make([]byte, 0, 33+len(p))
	b = append(b, pRelay)
	b = append(b, ra[:]...)
	b = append(b, p...)
	if _, err := c.n.tr.WriteTo(b, c.n.srv); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *relayConn) Close() error                     { c.once.Do(func() { close(c.done) }); return nil }
func (c *relayConn) LocalAddr() net.Addr              { return relayAddr(c.n.id) }
func (c *relayConn) SetDeadline(t time.Time) error    { return c.SetReadDeadline(t) }
func (c *relayConn) SetWriteDeadline(time.Time) error { return nil }
func (c *relayConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}
