// Package broker routes messages between peerbus sessions.
//
// Delivery policy:
//   - Same user (any account, any machine): delivered immediately.
//   - Different user: the two users must share a group, and the message is
//     held until the recipient's human approves it from a control client
//     (the CLI). Approval also grants the thread for a while, so replies and
//     follow-ups between the thread's participants flow without more prompts.
//   - A sender may address a user rather than a session ("alice/"); the
//     recipient's human then picks the session when approving.
//
// Session connections can never approve; only control connections can.
package broker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/vitaminmoo/peerbus/internal/config"
	"github.com/vitaminmoo/peerbus/internal/proto"
)

const (
	offlineVisible = 24 * time.Hour
	retention      = 7 * 24 * time.Hour
	idleSubTTL     = 12 * time.Hour
	queueCap       = 200
	rateWindow     = 10 * time.Second
	rateMax        = 30
	defaultGrant   = 120 * time.Minute
)

type State struct {
	Peers    map[string]*Peer                `json:"peers"`
	Held     []*Held                         `json:"held"`
	Queues   map[string][]*proto.Message     `json:"queues"`
	Threads  map[string]*Thread              `json:"threads"`
	IdleSubs map[string]map[string]time.Time `json:"idleSubs"` // watched sid -> subscriber sid -> expiry
}

type Peer struct {
	proto.SessionInfo
	User     string    `json:"user"`
	Online   bool      `json:"-"`
	LastSeen time.Time `json:"lastSeen"`
}

func (p *Peer) address() string { return proto.Address(p.User, p.Name, p.SID) }

type Held struct {
	ID        string        `json:"id"`
	Msg       proto.Message `json:"msg"`
	ToUser    string        `json:"toUser"`
	TargetSID string        `json:"targetSid,omitempty"` // empty when human-routed
	HeldAt    time.Time     `json:"heldAt"`
}

// Thread tracks which users may post into a conversation and which users
// currently accept it without approval.
type Thread struct {
	ID           string               `json:"id"`
	Participants []string             `json:"participants"`
	Approved     map[string]time.Time `json:"approved"` // user -> grant expiry
	LastActive   time.Time            `json:"lastActive"`
}

type Broker struct {
	cfgPath, statePath string
	Log                *log.Logger
	Now                func() time.Time

	mu     sync.Mutex
	cfg    *config.Broker
	cfgMod time.Time
	tokens map[string]string // token hash -> user
	st     *State
	bySID  map[string]*conn
	conns  map[*conn]struct{}
	saveT  *time.Timer
	closed bool
}

type conn struct {
	ws    *websocket.Conn
	out   chan proto.Frame
	done  chan struct{}
	once  sync.Once
	user  string
	kind  string // session | control
	sid   string
	watch bool
	sends []time.Time
}

func (c *conn) push(f proto.Frame) {
	select {
	case c.out <- f:
	default:
		c.close() // a client that can't keep up is dropped rather than blocking the broker
	}
}

func (c *conn) close() { c.once.Do(func() { close(c.done) }) }

func New(cfgPath, statePath string) (*Broker, error) {
	b := &Broker{
		cfgPath:   cfgPath,
		statePath: statePath,
		Log:       log.New(os.Stderr, "peerbus-broker ", log.LstdFlags),
		Now:       time.Now,
		bySID:     map[string]*conn{},
		conns:     map[*conn]struct{}{},
	}
	if err := b.loadConfig(); err != nil {
		return nil, err
	}
	b.st = &State{}
	if err := config.ReadJSON(statePath, b.st); err != nil && !errors.Is(err, os.ErrNotExist) {
		b.Log.Printf("ignoring unreadable state: %v", err)
		b.st = &State{}
	}
	if b.st.Peers == nil {
		b.st.Peers = map[string]*Peer{}
	}
	if b.st.Queues == nil {
		b.st.Queues = map[string][]*proto.Message{}
	}
	if b.st.Threads == nil {
		b.st.Threads = map[string]*Thread{}
	}
	if b.st.IdleSubs == nil {
		b.st.IdleSubs = map[string]map[string]time.Time{}
	}
	return b, nil
}

func (b *Broker) loadConfig() error {
	fi, err := os.Stat(b.cfgPath)
	if err != nil {
		return err
	}
	cfg, err := config.LoadBroker(b.cfgPath)
	if err != nil {
		return err
	}
	tokens := map[string]string{}
	for name, u := range cfg.Users {
		for _, h := range u.Tokens {
			tokens[h] = name
		}
	}
	b.cfg, b.tokens, b.cfgMod = cfg, tokens, fi.ModTime()
	return nil
}

func (b *Broker) ListenAddr() string { return b.cfg.Listen }

// Serve runs the broker on ln until ctx is done.
func (b *Broker) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: b.Handler()}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	go b.reloadLoop(ctx)
	err := srv.Serve(ln)
	// srv.Close doesn't reach hijacked WebSocket connections; close them here
	// and make this the last state write.
	b.mu.Lock()
	b.closed = true
	if b.saveT != nil {
		b.saveT.Stop()
		b.saveT = nil
	}
	for c := range b.conns {
		c.close()
	}
	b.saveLocked()
	b.mu.Unlock()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("/ws", b.serveWS)
	return mux
}

func (b *Broker) reloadLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fi, err := os.Stat(b.cfgPath)
			if err != nil {
				continue
			}
			b.mu.Lock()
			if fi.ModTime() != b.cfgMod {
				if err := b.loadConfig(); err != nil {
					b.Log.Printf("config reload failed, keeping old config: %v", err)
				} else {
					b.Log.Printf("config reloaded")
				}
			}
			b.mu.Unlock()
		}
	}
}

func (b *Broker) serveWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	defer ws.CloseNow()
	ctx := r.Context()

	c := &conn{ws: ws, out: make(chan proto.Frame, 256), done: make(chan struct{})}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var hello proto.Frame
	err = wsjson.Read(hctx, ws, &hello)
	cancel()
	if err != nil {
		return
	}
	welcome, err := b.hello(c, hello)
	if err != nil {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		wsjson.Write(wctx, ws, proto.Frame{Type: proto.TError, Error: err.Error()})
		cancel()
		ws.Close(websocket.StatusPolicyViolation, "hello rejected")
		return
	}
	defer b.disconnect(c)

	go b.writer(ctx, c)
	go b.pinger(ctx, c)
	c.push(welcome)
	b.afterHello(c)

	for {
		var f proto.Frame
		if err := wsjson.Read(ctx, ws, &f); err != nil {
			return
		}
		select {
		case <-c.done:
			return
		default:
		}
		b.handle(c, f)
	}
}

func (b *Broker) writer(ctx context.Context, c *conn) {
	for {
		select {
		case <-c.done:
			c.ws.Close(websocket.StatusGoingAway, "closed")
			return
		case <-ctx.Done():
			return
		case f := <-c.out:
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := wsjson.Write(wctx, c.ws, f)
			cancel()
			if err != nil {
				c.close()
				return
			}
		}
	}
}

func (b *Broker) pinger(ctx context.Context, c *conn) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				c.close()
				c.ws.CloseNow()
				return
			}
		}
	}
}

func (b *Broker) hello(c *conn, f proto.Frame) (proto.Frame, error) {
	if f.Type != proto.THello {
		return proto.Frame{}, errors.New("first frame must be hello")
	}
	if f.V != proto.Version {
		return proto.Frame{}, fmt.Errorf("protocol version %d not supported (broker speaks %d)", f.V, proto.Version)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	user, ok := b.tokens[config.HashToken(f.Token)]
	if !ok {
		return proto.Frame{}, errors.New("unknown token")
	}
	c.user, c.kind, c.watch = user, f.Kind, f.Watch
	welcome := proto.Frame{Type: proto.TWelcome, User: user}
	switch f.Kind {
	case "control":
	case "session":
		s := f.Session
		if s == nil || s.SID == "" || s.Name == "" {
			return proto.Frame{}, errors.New("session hello needs sid and name")
		}
		if p, ok := b.st.Peers[s.SID]; ok && p.User != user {
			return proto.Frame{}, errors.New("session id belongs to another user")
		}
		if old := b.bySID[s.SID]; old != nil {
			old.close() // same session reconnected; the newest connection wins
		}
		c.sid = s.SID
		p := &Peer{SessionInfo: *s, User: user, Online: true, LastSeen: b.Now()}
		b.st.Peers[s.SID] = p
		b.bySID[s.SID] = c
		welcome.Address = p.address()
	default:
		return proto.Frame{}, fmt.Errorf("unknown hello kind %q", f.Kind)
	}
	b.conns[c] = struct{}{}
	b.scheduleSaveLocked()
	return welcome, nil
}

// afterHello flushes anything queued for a session while it was offline.
func (b *Broker) afterHello(c *conn) {
	if c.kind != "session" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.st.Queues[c.sid]
	delete(b.st.Queues, c.sid)
	for _, m := range q {
		c.push(proto.Frame{Type: proto.TDeliver, Msg: m, Foreign: m.FromUser != c.user})
	}
	if len(q) > 0 {
		b.scheduleSaveLocked()
	}
}

func (b *Broker) disconnect(c *conn) {
	c.close()
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.conns, c)
	if c.kind != "session" || b.bySID[c.sid] != c {
		return
	}
	delete(b.bySID, c.sid)
	if p := b.st.Peers[c.sid]; p != nil {
		p.Online = false
		p.LastSeen = b.Now()
		b.fireIdleLocked(p, "disconnected")
	}
	b.scheduleSaveLocked()
}

func (b *Broker) handle(c *conn, f proto.Frame) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var data any
	var err error
	switch {
	case f.Type == proto.TUpdate && c.kind == "session":
		b.updateLocked(c, f.Session)
	case f.Type == proto.TList:
		data = b.viewsLocked(c.user)
	case f.Type == proto.TSend:
		data, err = b.sendLocked(c, f)
	case f.Type == proto.TSubscribeIdle && c.kind == "session":
		data, err = b.subscribeIdleLocked(c, f)
	case f.Type == proto.TInbox && c.kind == "control":
		data = b.inboxLocked(c.user)
	case f.Type == proto.TApprove && c.kind == "control":
		data, err = b.approveLocked(c, f)
	case f.Type == proto.TDeny && c.kind == "control":
		data, err = b.denyLocked(c, f)
	default:
		err = fmt.Errorf("frame %q not allowed on a %s connection", f.Type, c.kind)
	}
	if f.Req != "" {
		if err != nil {
			c.push(proto.Frame{Type: proto.TResult, Req: f.Req, Error: err.Error()})
		} else {
			c.push(proto.Frame{Type: proto.TResult, Req: f.Req, OK: true, Data: data})
		}
	}
	b.scheduleSaveLocked()
}

func (b *Broker) updateLocked(c *conn, s *proto.SessionInfo) {
	p := b.st.Peers[c.sid]
	if p == nil || s == nil {
		return
	}
	if s.Name != "" {
		p.Name = s.Name
	}
	if s.Cwd != "" {
		p.Cwd = s.Cwd
	}
	if s.Resources != nil {
		p.Resources = s.Resources
	}
	p.ShareDetails = s.ShareDetails
	p.LastSeen = b.Now()
	if s.Status != "" && s.Status != p.Status {
		p.Status = s.Status
		if s.Status == "idle" {
			b.fireIdleLocked(p, "idle")
		}
	}
}

// visibleLocked reports which users viewer can see: itself plus everyone it
// shares a group with.
func (b *Broker) visibleLocked(viewer string) map[string]bool {
	vis := map[string]bool{viewer: true}
	for _, members := range b.cfg.Groups {
		if slices.Contains(members, viewer) {
			for _, m := range members {
				vis[m] = true
			}
		}
	}
	return vis
}

func (b *Broker) liveLocked(p *Peer) bool {
	return p.Online || b.Now().Sub(p.LastSeen) < offlineVisible
}

func (b *Broker) viewsLocked(viewer string) []proto.PeerView {
	vis := b.visibleLocked(viewer)
	var out []proto.PeerView
	for _, p := range b.st.Peers {
		if !vis[p.User] || !b.liveLocked(p) {
			continue
		}
		v := proto.PeerView{
			Address:   p.address(),
			User:      p.User,
			Name:      p.Name,
			Ref:       proto.Ref(p.SID),
			Host:      p.Host,
			Status:    p.Status,
			LastSeen:  p.LastSeen.Unix(),
			Foreign:   p.User != viewer,
			Resources: p.Resources,
		}
		if !p.Online {
			v.Status = "offline"
		}
		if !v.Foreign || p.ShareDetails {
			v.Cwd, v.Account = p.Cwd, p.Account
		}
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

var refRe = regexp.MustCompile(`^(.*?)\s*(?:#|\[)([0-9a-fA-F]{3,32})\]?$`)

type target struct {
	peer *Peer  // set when a session was named
	user string // recipient user
}

// resolveLocked turns a "to" string into a session or a user.
//
//	name, name#ref, name [ref]  one of viewer's own sessions
//	user/name, user/name#ref    a session of a visible user
//	user/ or user/*             a visible user; their human picks the session
//	user                        same, when viewer has no session by that name
func (b *Broker) resolveLocked(viewer, to string) (target, error) {
	t := strings.TrimPrefix(strings.TrimSpace(to), "@")
	ref := ""
	if m := refRe.FindStringSubmatch(t); m != nil {
		t, ref = strings.TrimSpace(m[1]), strings.ToLower(m[2])
	}
	vis := b.visibleLocked(viewer)
	user, name, hasUser := strings.Cut(t, "/")
	if !hasUser {
		user, name = viewer, t
	}
	if !vis[user] {
		return target{}, fmt.Errorf("no visible user %q", user)
	}
	if hasUser && (name == "" || name == "*") && ref == "" {
		if user == viewer {
			return target{}, errors.New("name one of your sessions; routing to yourself as a user only applies to other people")
		}
		return target{user: user}, nil
	}

	var cands []*Peer
	for _, p := range b.st.Peers {
		if p.User != user || !b.liveLocked(p) {
			continue
		}
		if ref != "" {
			if strings.HasPrefix(proto.Ref(p.SID), ref) || strings.HasPrefix(strings.ReplaceAll(p.SID, "-", ""), ref) {
				cands = append(cands, p)
			}
		} else if p.Name == name {
			cands = append(cands, p)
		}
	}
	if len(cands) > 1 {
		var online []*Peer
		for _, p := range cands {
			if p.Online {
				online = append(online, p)
			}
		}
		if len(online) == 1 {
			cands = online
		}
	}
	switch len(cands) {
	case 1:
		return target{peer: cands[0], user: user}, nil
	case 0:
		if !hasUser && ref == "" && t != viewer && vis[t] {
			return target{user: t}, nil
		}
		return target{}, fmt.Errorf("no session %q visible (see list_peers)", to)
	default:
		var addrs []string
		for _, p := range cands {
			addrs = append(addrs, p.address())
		}
		sort.Strings(addrs)
		return target{}, fmt.Errorf("%q is ambiguous, use one of: %s", to, strings.Join(addrs, ", "))
	}
}

func (b *Broker) grant() time.Duration {
	if b.cfg.ThreadApprovalMinutes > 0 {
		return time.Duration(b.cfg.ThreadApprovalMinutes) * time.Minute
	}
	return defaultGrant
}

// threadLocked returns the thread a sender may post into. Unknown ids, or
// threads the sender isn't part of, start a fresh thread: clients can't pick
// thread ids, so they can't ride on someone else's approval.
func (b *Broker) threadLocked(id, user string) *Thread {
	if th := b.st.Threads[id]; th != nil && slices.Contains(th.Participants, user) {
		return th
	}
	th := &Thread{ID: proto.NewID(6), Participants: []string{user}, Approved: map[string]time.Time{}}
	b.st.Threads[th.ID] = th
	return th
}

func (b *Broker) senderAddress(c *conn) string {
	if p := b.st.Peers[c.sid]; c.kind == "session" && p != nil {
		return p.address()
	}
	return c.user + "/cli"
}

func (b *Broker) sendLocked(c *conn, f proto.Frame) (any, error) {
	if strings.TrimSpace(f.Text) == "" {
		return nil, errors.New("empty message")
	}
	if len(f.Text) > proto.MaxText {
		return nil, fmt.Errorf("message is %d bytes, limit is %d", len(f.Text), proto.MaxText)
	}
	now := b.Now()
	c.sends = slices.DeleteFunc(c.sends, func(t time.Time) bool { return now.Sub(t) > rateWindow })
	if len(c.sends) >= rateMax {
		return nil, fmt.Errorf("too many messages in %s; batch them into one", rateWindow)
	}
	c.sends = append(c.sends, now)

	tgt, err := b.resolveLocked(c.user, f.To)
	if err != nil {
		return nil, err
	}
	if tgt.peer != nil && tgt.peer.SID == c.sid {
		return nil, errors.New("that address is this session")
	}

	th := b.threadLocked(f.Thread, c.user)
	th.Approved[c.user] = now.Add(b.grant()) // a sender accepts replies in its own thread
	th.LastActive = now
	msg := &proto.Message{
		ID:       proto.NewID(6),
		Thread:   th.ID,
		From:     b.senderAddress(c),
		FromUser: c.user,
		FromSID:  c.sid,
		Text:     f.Text,
		SentAt:   now.Unix(),
	}
	res := proto.SendResult{ID: msg.ID, Thread: th.ID}
	if tgt.peer != nil {
		msg.To, res.To = tgt.peer.address(), tgt.peer.address()
	} else {
		msg.To, res.To = tgt.user+"/", tgt.user+"/"
	}

	switch {
	case tgt.user == c.user:
		res.Status = b.deliverLocked(tgt.peer, msg)
	case tgt.peer != nil && th.Approved[tgt.user].After(now):
		res.Status = b.deliverLocked(tgt.peer, msg)
	default:
		b.holdLocked(msg, tgt)
		res.Status = "held"
	}
	return res, nil
}

func (b *Broker) deliverLocked(p *Peer, m *proto.Message) string {
	if c := b.bySID[p.SID]; c != nil {
		c.push(proto.Frame{Type: proto.TDeliver, Msg: m, Foreign: m.FromUser != p.User})
		return "delivered"
	}
	q := append(b.st.Queues[p.SID], m)
	if len(q) > queueCap {
		q = q[len(q)-queueCap:]
	}
	b.st.Queues[p.SID] = q
	return "queued"
}

func (b *Broker) holdLocked(m *proto.Message, tgt target) {
	h := &Held{ID: proto.NewID(4), Msg: *m, ToUser: tgt.user, HeldAt: b.Now()}
	if tgt.peer != nil {
		h.TargetSID = tgt.peer.SID
	}
	b.st.Held = append(b.st.Held, h)
	view := b.heldViewLocked(h)
	for c := range b.conns {
		if c.user == tgt.user && c.kind == "control" && c.watch {
			c.push(proto.Frame{Type: proto.THeld, Held: &view})
		}
	}
	// The targeted session learns that something is waiting, but not what it
	// says: the content stays out of Claude's context until a human approves.
	if tgt.peer != nil {
		b.noticeLocked(tgt.peer.SID, "held",
			fmt.Sprintf("A message from %s (another user) is waiting for your user's approval. Tell your user; they can review it with `peerbus inbox` and approve with `peerbus approve %s`. Do not approve or deny it yourself.", m.From, h.ID),
			map[string]string{"held_id": h.ID, "from": m.From})
	}
}

func (b *Broker) heldViewLocked(h *Held) proto.HeldView {
	return proto.HeldView{
		ID:     h.ID,
		From:   h.Msg.From,
		To:     h.Msg.To,
		Routed: h.TargetSID == "",
		HeldAt: h.HeldAt.Unix(),
		Text:   h.Msg.Text,
	}
}

func (b *Broker) noticeLocked(sid, kind, text string, meta map[string]string) {
	if c := b.bySID[sid]; c != nil {
		c.push(proto.Frame{Type: proto.TNotice, Kind: kind, Text: text, Meta: meta})
	}
}

func (b *Broker) inboxLocked(user string) []proto.HeldView {
	var out []proto.HeldView
	for _, h := range b.st.Held {
		if h.ToUser == user {
			out = append(out, b.heldViewLocked(h))
		}
	}
	return out
}

func (b *Broker) takeHeldLocked(user, id string) (*Held, error) {
	for i, h := range b.st.Held {
		if h.ToUser == user && h.ID == id {
			b.st.Held = slices.Delete(b.st.Held, i, i+1)
			return h, nil
		}
	}
	return nil, fmt.Errorf("no held message %q", id)
}

func (b *Broker) approveLocked(c *conn, f proto.Frame) (any, error) {
	var h *Held
	for _, x := range b.st.Held {
		if x.ToUser == c.user && x.ID == f.ID {
			h = x
		}
	}
	if h == nil {
		return nil, fmt.Errorf("no held message %q", f.ID)
	}
	var p *Peer
	if f.To != "" {
		tgt, err := b.resolveLocked(c.user, f.To)
		if err != nil {
			return nil, err
		}
		if tgt.peer == nil || tgt.user != c.user {
			return nil, errors.New("approve --to must name one of your own sessions")
		}
		p = tgt.peer
	} else if h.TargetSID != "" {
		p = b.st.Peers[h.TargetSID]
	}
	if p == nil {
		return nil, errors.New("this message was sent to you as a user; pick a session with --to")
	}
	b.takeHeldLocked(c.user, h.ID)

	now := b.Now()
	th := b.st.Threads[h.Msg.Thread]
	if th == nil {
		th = &Thread{ID: h.Msg.Thread, Participants: []string{h.Msg.FromUser}, Approved: map[string]time.Time{}}
		b.st.Threads[th.ID] = th
	}
	if !slices.Contains(th.Participants, c.user) {
		th.Participants = append(th.Participants, c.user)
	}
	th.Approved[c.user] = now.Add(b.grant())
	th.LastActive = now

	m := h.Msg
	m.To = p.address()
	status := b.deliverLocked(p, &m)
	if m.FromSID != "" {
		b.noticeLocked(m.FromSID, "status",
			fmt.Sprintf("%s approved your message %s; it was %s to %s. Replies in thread %s now flow without approval for %s.", c.user, m.ID, status, m.To, th.ID, b.grant()),
			map[string]string{"msg_id": m.ID, "thread": th.ID, "status": "approved"})
	}
	return proto.SendResult{Status: status, ID: m.ID, Thread: th.ID, To: m.To}, nil
}

func (b *Broker) denyLocked(c *conn, f proto.Frame) (any, error) {
	h, err := b.takeHeldLocked(c.user, f.ID)
	if err != nil {
		return nil, err
	}
	if h.Msg.FromSID != "" {
		b.noticeLocked(h.Msg.FromSID, "status",
			fmt.Sprintf("%s declined your message %s to %s.", c.user, h.Msg.ID, h.Msg.To),
			map[string]string{"msg_id": h.Msg.ID, "status": "declined"})
	}
	return map[string]string{"status": "declined", "id": h.ID}, nil
}

func (b *Broker) subscribeIdleLocked(c *conn, f proto.Frame) (any, error) {
	tgt, err := b.resolveLocked(c.user, f.Peer)
	if err != nil {
		return nil, err
	}
	p := tgt.peer
	if p == nil {
		return nil, errors.New("subscribe to a session, not a user")
	}
	if !p.Online {
		return map[string]string{"status": "offline", "peer": p.address()}, nil
	}
	if p.Status == "idle" {
		return map[string]string{"status": "idle", "peer": p.address()}, nil
	}
	subs := b.st.IdleSubs[p.SID]
	if subs == nil {
		subs = map[string]time.Time{}
		b.st.IdleSubs[p.SID] = subs
	}
	subs[c.sid] = b.Now().Add(idleSubTTL)
	return map[string]string{"status": "subscribed", "peer": p.address()}, nil
}

func (b *Broker) fireIdleLocked(p *Peer, why string) {
	subs := b.st.IdleSubs[p.SID]
	delete(b.st.IdleSubs, p.SID)
	now := b.Now()
	for sid, exp := range subs {
		if exp.Before(now) {
			continue
		}
		b.noticeLocked(sid, "idle", fmt.Sprintf("%s is now %s.", p.address(), why),
			map[string]string{"peer": p.address(), "state": why})
	}
}

func (b *Broker) scheduleSaveLocked() {
	if b.saveT != nil || b.closed {
		return
	}
	b.saveT = time.AfterFunc(250*time.Millisecond, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.closed {
			return
		}
		b.saveT = nil
		b.saveLocked()
	})
}

func (b *Broker) saveLocked() {
	cut := b.Now().Add(-retention)
	for sid, p := range b.st.Peers {
		if !p.Online && p.LastSeen.Before(cut) {
			delete(b.st.Peers, sid)
			delete(b.st.Queues, sid)
		}
	}
	for id, th := range b.st.Threads {
		if th.LastActive.Before(cut) {
			delete(b.st.Threads, id)
		}
	}
	b.st.Held = slices.DeleteFunc(b.st.Held, func(h *Held) bool { return h.HeldAt.Before(cut) })
	if b.statePath == "" {
		return
	}
	if err := config.WriteJSON(b.statePath, b.st); err != nil {
		b.Log.Printf("saving state: %v", err)
	}
}
