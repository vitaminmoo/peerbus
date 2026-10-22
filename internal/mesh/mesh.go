// Package mesh connects a session to every other session it may talk to:
// your own on this machine and elsewhere, and those of the people you've
// paired with. Each pairing has a shared secret; sessions register at the
// rendezvous server (and in a directory on this machine) under meeting
// points derived from those secrets, find each other there, and keep one
// QUIC connection per pair of sessions. Messages, status and notices travel
// over those connections; nothing is stored or routed by a server.
//
// Policy is enforced by the receiving side from its own peers.json: who may
// connect, which sessions a peer may see, and whether their messages are
// delivered or held for the human. Your own sessions on different machines
// send each other their peers.json, so every machine enforces the same list.
package mesh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/vitaminmoo/peerbus/internal/history"
	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/inbox"
	"github.com/vitaminmoo/peerbus/internal/proto"
	"github.com/vitaminmoo/peerbus/internal/transport"
)

// discoverEvery is how often sessions look for new peers; tests shorten it.
var discoverEvery = 5 * time.Second

const (
	pointsEvery  = time.Minute
	dialBackoff  = 30 * time.Second
	dialTimeout  = 20 * time.Second
	helloTimeout = 15 * time.Second
	requestWait  = 15 * time.Second
	rateWindow   = 10 * time.Second
	rateMax      = 30
)

type Config struct {
	Store    identity.Store
	StateDir string // inbox, history and same-machine discovery
	// Server is the rendezvous server; empty keeps us to this machine.
	Server string
	STUN   []string
	// Kind is proto.KindSession for a Claude session or proto.KindControl
	// for the CLI, which is never listed and accepts no connections.
	Kind string
	Info proto.SessionInfo
	Log  *log.Logger

	// OnMessage receives a message for this session.
	OnMessage func(m proto.Message, foreign bool)
	// OnNotice receives held/status/idle notices.
	OnNotice func(kind, text string, meta map[string]string)
}

type Mesh struct {
	cfg     Config
	log     *log.Logger
	key     ed25519.PrivateKey
	pub     ed25519.PublicKey
	node    *transport.Node
	inbox   inbox.Store
	history history.Store

	ctx       context.Context
	cancel    context.CancelFunc
	ready     chan struct{}
	readyOnce sync.Once
	twinOnce  sync.Once

	reloadMu sync.Mutex // one reloadPeers at a time: the loop and merges both call it

	mu       sync.Mutex
	info     proto.SessionInfo
	peers    *identity.Peers
	peersMod time.Time
	links    map[transport.Handle]*Link
	dialing  map[transport.Handle]time.Time // handle -> no redial before
}

// Start loads the identity, opens the transport and begins looking for
// sessions. It returns once the node is up; discovery continues in the
// background.
func Start(ctx context.Context, cfg Config) (*Mesh, error) {
	if cfg.Kind == "" {
		cfg.Kind = proto.KindSession
	}
	m := &Mesh{
		cfg:     cfg,
		log:     cfg.Log,
		inbox:   inbox.Store{Dir: cfg.StateDir},
		history: history.Store{Dir: cfg.StateDir},
		info:    cfg.Info,
		ready:   make(chan struct{}),
		links:   map[transport.Handle]*Link{},
		dialing: map[transport.Handle]time.Time{},
	}
	if m.log == nil {
		m.log = log.New(io.Discard, "", 0)
	}
	key, err := cfg.Store.Identity()
	if err != nil {
		return nil, err
	}
	m.key, m.pub = key, key.Public().(ed25519.PublicKey)
	if m.peers, err = cfg.Store.Load(); err != nil {
		return nil, err
	}
	m.peersMod = cfg.Store.ModTime()
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.node, err = transport.Start(m.ctx, transport.Config{
		Key:      key,
		Server:   cfg.Server,
		STUN:     cfg.STUN,
		LocalDir: filepath.Join(cfg.StateDir, "local"),
		Allow:    m.allow,
		OnConn:   m.accept,
		Log:      m.log,
	})
	if err != nil {
		m.cancel()
		return nil, err
	}
	m.setPoints()
	go m.loop()
	return m, nil
}

// Ready is closed once the first round of discovery has finished.
func (m *Mesh) Ready() <-chan struct{} { return m.ready }

func (m *Mesh) Close() error {
	m.cancel()
	m.mu.Lock()
	links := m.snapshot()
	m.mu.Unlock()
	for _, l := range links {
		l.close()
	}
	return m.node.Close()
}

// Node exposes the transport for diagnostics.
func (m *Mesh) Node() *transport.Node { return m.node }

func (m *Mesh) snapshot() []*Link {
	out := make([]*Link, 0, len(m.links))
	for _, l := range m.links {
		out = append(out, l)
	}
	return out
}

// allow decides who may connect: our own devices and enabled peers. The
// CLI only dials out.
func (m *Mesh) allow(pub ed25519.PublicKey) bool {
	if m.cfg.Kind != proto.KindSession {
		return false
	}
	return m.peerFor(pub) != nil || pub.Equal(m.pub)
}

// peerFor returns the enabled peer with this key, or nil (also for our own
// key).
func (m *Mesh) peerFor(pub ed25519.PublicKey) *identity.Peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.peers.ByKey(pub); p != nil && p.Enabled {
		return p
	}
	return nil
}

func (m *Mesh) setPoints() {
	if m.cfg.Kind != proto.KindSession {
		return // the CLI finds sessions; sessions don't look for it
	}
	now := time.Now()
	m.mu.Lock()
	pts := m.peers.Self.Secret.Points(now)
	for _, p := range m.peers.Enabled() {
		pts = append(pts, p.Secret.Points(now)...)
	}
	m.mu.Unlock()
	m.node.SetPoints(pts)
}

func (m *Mesh) loop() {
	if m.cfg.Server != "" {
		// Lookups need the server's address resolved and our registration
		// acknowledged; give that a moment rather than missing the first round.
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		if err := m.node.WaitRegistered(ctx); err != nil && m.ctx.Err() == nil {
			m.log.Printf("rendezvous server %s: not reachable yet (%v); same-machine sessions still work", m.cfg.Server, err)
		}
		cancel()
	}
	m.discover()
	m.readyOnce.Do(func() { close(m.ready) })
	t := time.NewTicker(discoverEvery)
	pt := time.NewTicker(pointsEvery)
	defer t.Stop()
	defer pt.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.reloadPeers()
			m.discover()
		case <-pt.C:
			m.setPoints()
		}
	}
}

// reloadPeers picks up `peerbus peer` changes: new or re-enabled peers get
// looked for, disabled ones are cut off, and permission changes reach the
// other side through a fresh hello.
func (m *Mesh) reloadPeers() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	mod := m.cfg.Store.ModTime()
	if mod.Equal(m.peersMod) {
		return
	}
	ps, err := m.cfg.Store.Load()
	if err != nil {
		m.log.Printf("reloading peers: %v", err)
		return
	}
	m.mu.Lock()
	m.peers, m.peersMod = ps, mod
	links := m.snapshot()
	m.mu.Unlock()
	for _, l := range links {
		if l.peer == nil {
			continue
		}
		np := ps.ByKey(l.key)
		if np == nil || !np.Enabled {
			l.close()
			continue
		}
		l.mu.Lock()
		l.peer = np
		l.mu.Unlock()
	}
	m.setPoints()
	m.Update(m.Info())
	for _, l := range links {
		if m.syncsWith(l) {
			m.sendPeers(l)
		}
	}
}

// syncsWith reports whether l is another of our own sessions, which may
// keep its own copy of peers.json. One that shares ours (the same machine
// and OS user) merges to no change.
func (m *Mesh) syncsWith(l *Link) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return m.cfg.Kind == proto.KindSession && l.peer == nil && l.kind == proto.KindSession
}

func (m *Mesh) sendPeers(l *Link) {
	m.mu.Lock()
	ps := m.peers
	m.mu.Unlock()
	l.write(proto.Frame{Type: proto.TPeers, Data: ps})
}

var errUnchanged = errors.New("unchanged")

// mergePeers folds in the peer list from one of our sessions on another
// machine and applies the result here.
func (m *Mesh) mergePeers(l *Link, f proto.Frame) {
	if !m.syncsWith(l) {
		return
	}
	b, err := json.Marshal(f.Data)
	if err != nil {
		return
	}
	var in identity.Peers
	if err := json.Unmarshal(b, &in); err != nil {
		m.log.Printf("peers from %s: %v", m.describe(l), err)
		return
	}
	err = m.cfg.Store.Update(func(ps *identity.Peers) error {
		if !ps.Merge(&in) {
			return errUnchanged
		}
		return nil
	})
	switch {
	case err == nil:
		m.log.Printf("merged peers from %s", m.describe(l))
		m.reloadPeers()
	case !errors.Is(err, errUnchanged):
		m.log.Printf("merging peers from %s: %v", m.describe(l), err)
	}
}

// discover looks under every meeting point we share with someone and dials
// the sessions we aren't connected to yet. It returns when this round's
// dials have finished.
func (m *Mesh) discover() {
	type want struct {
		secret identity.Secret
		key    ed25519.PublicKey
	}
	m.mu.Lock()
	wants := []want{{m.peers.Self.Secret, m.pub}}
	for _, p := range m.peers.Enabled() {
		wants = append(wants, want{p.Secret, ed25519.PublicKey(p.Key)})
	}
	m.mu.Unlock()
	now := time.Now()
	var wg sync.WaitGroup
	for _, w := range wants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(m.ctx, 3*time.Second)
			found, err := m.node.Lookup(ctx, w.secret.Point(now))
			cancel()
			if err != nil && len(found) == 0 && m.ctx.Err() == nil && !errors.Is(err, transport.ErrNoServer) {
				m.log.Printf("lookup: %v", err)
			}
			for _, p := range found {
				if !m.claimDial(p.Handle) {
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					m.dial(p, w.key)
				}()
			}
		}()
	}
	wg.Wait()
}

func (m *Mesh) claimDial(h transport.Handle) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.links[h] != nil || m.dialing[h].After(time.Now()) {
		return false
	}
	m.dialing[h] = time.Now().Add(dialBackoff)
	return true
}

// dial connects to a session found under a meeting point shared with key.
// Our own sessions register under every pair point too (to meet that peer),
// so either key may answer.
func (m *Mesh) dial(p transport.Peer, key ed25519.PublicKey) {
	ctx, cancel := context.WithTimeout(m.ctx, dialTimeout)
	defer cancel()
	c, err := m.node.Dial(ctx, p, func(k ed25519.PublicKey) bool { return k.Equal(key) || k.Equal(m.pub) })
	if err != nil {
		if m.ctx.Err() == nil {
			m.log.Printf("dial %s: %v", p.Handle, err)
		}
		return
	}
	l, err := m.handshake(c, true)
	if err != nil {
		m.log.Printf("hello to %s: %v", p.Handle, err)
		c.CloseWithError(0, err.Error())
		return
	}
	m.mu.Lock()
	delete(m.dialing, l.handle)
	m.mu.Unlock()
	m.register(l)
}

func (m *Mesh) accept(c *transport.Conn) {
	l, err := m.handshake(c, false)
	if err != nil {
		m.log.Printf("hello from %s: %v", c.RemoteAddr(), err)
		c.CloseWithError(0, err.Error())
		return
	}
	m.register(l)
}

// handshake opens the control stream and exchanges hellos. The dialer
// speaks first.
func (m *Mesh) handshake(c *transport.Conn, dialer bool) (*Link, error) {
	peer := m.peerFor(c.PeerKey)
	if peer == nil && !c.PeerKey.Equal(m.pub) {
		return nil, errors.New("not a paired key")
	}
	ctx, cancel := context.WithTimeout(m.ctx, helloTimeout)
	defer cancel()
	var err error
	l := &Link{m: m, conn: c, key: c.PeerKey, peer: peer, pending: map[string]chan proto.Frame{}, done: make(chan struct{})}
	if dialer {
		l.s, err = c.OpenStreamSync(ctx)
	} else {
		l.s, err = c.AcceptStream(ctx)
	}
	if err != nil {
		return nil, err
	}
	l.r = newReader(l.s)
	var h proto.Frame
	if dialer {
		if err := l.write(m.helloFor(peer)); err != nil {
			return nil, err
		}
		if h, err = l.readWithin(helloTimeout); err != nil {
			return nil, err
		}
	} else {
		if h, err = l.readWithin(helloTimeout); err != nil {
			return nil, err
		}
		if err := l.write(m.helloFor(peer)); err != nil {
			return nil, err
		}
	}
	if h.Type != proto.THello {
		return nil, fmt.Errorf("first frame was %q, not hello", h.Type)
	}
	if h.V != proto.Version {
		return nil, fmt.Errorf("peer speaks protocol %d, we speak %d; update peerbus on both sides", h.V, proto.Version)
	}
	hb, err := hex.DecodeString(h.Handle)
	if err != nil || len(hb) != len(l.handle) {
		return nil, errors.New("bad handle in hello")
	}
	copy(l.handle[:], hb)
	switch h.Kind {
	case proto.KindSession, proto.KindControl, proto.KindHidden:
	default:
		return nil, fmt.Errorf("unknown hello kind %q", h.Kind)
	}
	l.kind = h.Kind
	if h.Session != nil {
		l.info = *h.Session
	}
	return l, nil
}

// register adds a link, keeping one connection per remote session. When both
// sides dialed at once, both keep the connection whose dialer has the lower
// handle, which they compute identically.
func (m *Mesh) register(l *Link) {
	m.mu.Lock()
	if m.ctx.Err() != nil {
		// Closing: Close cancels before it takes the lock to close links,
		// so a dial finishing now must not slip in after.
		m.mu.Unlock()
		l.close()
		return
	}
	if old := m.links[l.handle]; old != nil {
		if bytes.Compare(m.dialerOf(old), m.dialerOf(l)) <= 0 {
			m.mu.Unlock()
			l.close()
			return
		}
		delete(m.links, old.handle)
		m.mu.Unlock()
		old.close()
		m.mu.Lock()
	}
	m.links[l.handle] = l
	m.mu.Unlock()
	m.log.Printf("%s %s via %s", m.describe(l), map[bool]string{true: "connected", false: "accepted"}[!l.conn.Inbound], l.conn.Path)
	go l.readLoop()
	m.checkTwin(l)
	if m.syncsWith(l) {
		m.sendPeers(l)
	}
}

func (m *Mesh) dialerOf(l *Link) []byte {
	if l.conn.Inbound {
		return l.handle[:]
	}
	h := m.node.Handle()
	return h[:]
}

func (m *Mesh) unlink(l *Link) {
	m.mu.Lock()
	if m.links[l.handle] == l {
		delete(m.links, l.handle)
	}
	m.mu.Unlock()
	l.close()
	l.mu.Lock()
	want := l.wantIdle
	l.wantIdle = false
	l.mu.Unlock()
	if want && l.kind == proto.KindSession {
		addr := m.addr(l)
		m.notice("idle", fmt.Sprintf("%s is now disconnected.", addr), map[string]string{"peer": addr, "state": "disconnected"})
	}
	m.log.Printf("%s disconnected", m.describe(l))
}

func (m *Mesh) describe(l *Link) string {
	if l.kind == proto.KindControl {
		return m.userOf(l) + "/cli"
	}
	return m.addr(l)
}

// Info is what this session currently says about itself.
func (m *Mesh) Info() proto.SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.info
}

// Update publishes new session details to every connection and fires idle
// notices when the session has just gone idle.
func (m *Mesh) Update(info proto.SessionInfo) {
	m.mu.Lock()
	old := m.info
	m.info = info
	links := m.snapshot()
	m.mu.Unlock()
	wentIdle := info.Status == "idle" && old.Status != "idle"
	if info.SID != old.SID || info.Started != old.Started {
		for _, l := range links {
			m.checkTwin(l)
		}
	}
	for _, l := range links {
		f := m.helloFor(l.peer)
		f.Type = proto.TUpdate
		l.write(f)
		if wentIdle {
			l.mu.Lock()
			fire := l.notifyIdle
			l.notifyIdle = false
			l.mu.Unlock()
			if fire {
				l.write(proto.Frame{Type: proto.TNotice, Kind: "idle", Meta: map[string]string{"state": "idle"}})
			}
		}
	}
}

// helloFor describes this session to peer, filtered by what we let them see.
func (m *Mesh) helloFor(peer *identity.Peer) proto.Frame {
	m.mu.Lock()
	info := m.info
	m.mu.Unlock()
	h := m.node.Handle()
	f := proto.Frame{Type: proto.THello, V: proto.Version, Kind: m.cfg.Kind, Handle: hex.EncodeToString(h[:])}
	if peer != nil && m.cfg.Kind == proto.KindSession {
		if !peer.Perms.MaySee(info.Host, info.Name) {
			f.Kind = proto.KindHidden
			info = proto.SessionInfo{SID: info.SID, Name: info.Name}
		} else {
			if !peer.Perms.ShowDetails {
				info.Cwd, info.Account = "", ""
			}
			if !peer.Perms.ShowResources {
				info.Resources = nil
			}
		}
	}
	if peer != nil {
		info.PID = 0
	}
	f.Session = &info
	return f
}

// checkTwin steps this session off the mesh when l is another process
// running the same Claude session (a second resume of one conversation)
// that was there first. Two copies would share one address, so the newer
// one leaves and tells its agent why.
func (m *Mesh) checkTwin(l *Link) {
	if m.cfg.Kind != proto.KindSession {
		return
	}
	l.mu.Lock()
	info, kind, peer := l.info, l.kind, l.peer
	l.mu.Unlock()
	m.mu.Lock()
	me := m.info
	m.mu.Unlock()
	if peer != nil || kind != proto.KindSession || info.SID == "" || info.SID != me.SID {
		return
	}
	// Older wins; a twin that doesn't say when it started runs an older
	// peerbus and was most likely there first. Equal starts fall back to
	// the handle, which both sides compare the same way.
	older := info.Started == 0 || info.Started < me.Started
	if info.Started == me.Started {
		h := m.node.Handle()
		older = bytes.Compare(l.handle[:], h[:]) < 0
	}
	if !older {
		return
	}
	m.twinOnce.Do(func() {
		addr := m.addr(l)
		m.log.Printf("%s is already running in another process (pid %d); leaving the mesh", addr, info.PID)
		go m.Close()
		started := "an unknown time"
		if info.Started != 0 {
			started = time.UnixMilli(info.Started).Format("2006-01-02 15:04:05")
		}
		pid, find := "pid unknown", "Find that process"
		if info.PID != 0 {
			pid = fmt.Sprintf("pid %d", info.PID)
			find = fmt.Sprintf("Find that process (for example `ps -o pid,tty,lstart,args -p %d`)", info.PID)
		}
		m.notice("duplicate",
			fmt.Sprintf("peerbus is off in this session: %s is already running in another Claude Code process (%s on %s, started %s, status %s). This is probably the same conversation resumed twice, and the older copy may be sitting on a question. %s and warn your user that it's still open. Once it's closed, reconnect peerbus here with /mcp.",
				addr, pid, info.Host, started, info.Status, find),
			map[string]string{"peer": addr, "pid": fmt.Sprint(info.PID), "host": info.Host})
	})
}

func (m *Mesh) selfName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peers.SelfName()
}

// SelfAddress is this session's address as its own user sees it.
func (m *Mesh) SelfAddress() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return proto.Address(m.peers.SelfName(), m.info.Name, m.info.SID)
}

func (m *Mesh) userOf(l *Link) string {
	l.mu.Lock()
	p := l.peer
	l.mu.Unlock()
	if p != nil {
		return p.Name
	}
	return m.selfName()
}

// addr is the remote session's address from our point of view.
func (m *Mesh) addr(l *Link) string {
	l.mu.Lock()
	info, kind := l.info, l.kind
	l.mu.Unlock()
	if kind == proto.KindControl {
		return m.userOf(l) + "/cli"
	}
	return proto.Address(m.userOf(l), info.Name, info.SID)
}

func (m *Mesh) notice(kind, text string, meta map[string]string) {
	if m.cfg.OnNotice != nil {
		m.cfg.OnNotice(kind, text, meta)
	}
}

// Views lists the sessions connected right now that we're allowed to see.
func (m *Mesh) Views() []proto.PeerView {
	m.mu.Lock()
	links := m.snapshot()
	m.mu.Unlock()
	var out []proto.PeerView
	for _, l := range links {
		l.mu.Lock()
		kind, info, peer, path := l.kind, l.info, l.peer, l.conn.Path
		l.mu.Unlock()
		if kind != proto.KindSession {
			continue
		}
		v := proto.PeerView{
			User:      m.userOf(l),
			Name:      info.Name,
			Ref:       proto.Ref(info.SID),
			Host:      info.Host,
			Status:    info.Status,
			Foreign:   peer != nil,
			Via:       via(path),
			Cwd:       info.Cwd,
			Account:   info.Account,
			Resources: info.Resources,
		}
		v.Address = proto.Address(v.User, v.Name, info.SID)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Foreign != out[j].Foreign {
			return !out[i].Foreign
		}
		return out[i].Address < out[j].Address
	})
	return out
}

func via(path string) string {
	switch {
	case path == "local", path == "relay":
		return path
	default:
		return "direct"
	}
}

// Own are the connected sessions of ours, on every machine.
func (m *Mesh) Own() []*Link {
	m.mu.Lock()
	links := m.snapshot()
	m.mu.Unlock()
	var out []*Link
	for _, l := range links {
		l.mu.Lock()
		ok := l.peer == nil && l.kind == proto.KindSession
		l.mu.Unlock()
		if ok {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return m.addr(out[i]) < m.addr(out[j]) })
	return out
}

// Locals are the connected sessions that run on this machine under our
// identity: what the CLI talks to.
func (m *Mesh) Locals() []*Link {
	m.mu.Lock()
	links := m.snapshot()
	m.mu.Unlock()
	var out []*Link
	for _, l := range links {
		if l.peer == nil && l.conn.Path == "local" && l.kind == proto.KindSession {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return m.addr(out[i]) < m.addr(out[j]) })
	return out
}
