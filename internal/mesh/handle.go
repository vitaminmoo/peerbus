package mesh

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/inbox"
	"github.com/vitaminmoo/peerbus/internal/proto"
	"github.com/vitaminmoo/peerbus/internal/transport"
)

// handle dispatches one inbound frame.
func (m *Mesh) handle(l *Link, f proto.Frame) {
	switch f.Type {
	case proto.TUpdate:
		if f.Session == nil {
			return
		}
		l.mu.Lock()
		l.info = *f.Session
		if f.Kind != "" {
			l.kind = f.Kind
		}
		l.mu.Unlock()
	case proto.TSend:
		res, err := m.receive(l, f)
		l.reply(f.Req, res, err)
	case proto.TSubscribeIdle:
		if m.cfg.Kind != proto.KindSession {
			l.reply(f.Req, nil, errors.New("not a session"))
			return
		}
		m.mu.Lock()
		status := m.info.Status
		m.mu.Unlock()
		if status == "idle" {
			l.reply(f.Req, map[string]string{"status": "idle"}, nil)
			return
		}
		l.mu.Lock()
		l.notifyIdle = true
		l.mu.Unlock()
		l.reply(f.Req, map[string]string{"status": "subscribed"}, nil)
	case proto.TList:
		if !l.isLocalControl() {
			l.reply(f.Req, nil, errors.New("not allowed"))
			return
		}
		l.reply(f.Req, map[string]any{"self": m.SelfAddress(), "peers": m.Views()}, nil)
	case proto.TInbox:
		if !l.isOwnControl() {
			l.reply(f.Req, nil, errors.New("not allowed"))
			return
		}
		res, err := m.inboxView()
		l.reply(f.Req, res, err)
	case proto.TApprove:
		if !l.isOwnControl() {
			l.reply(f.Req, nil, errors.New("not allowed"))
			return
		}
		res, err := m.approve(f.ID, f.To)
		l.reply(f.Req, res, err)
	case proto.TDeliver:
		res, err := m.deliver(l, f)
		l.reply(f.Req, res, err)
	case proto.TDeny:
		if !l.isOwnControl() {
			l.reply(f.Req, nil, errors.New("not allowed"))
			return
		}
		res, err := m.deny(f.ID)
		l.reply(f.Req, res, err)
	case proto.TNotice:
		m.onNotice(l, f)
	case proto.TPeers:
		m.mergePeers(l, f)
	default:
		l.reply(f.Req, nil, fmt.Errorf("unknown frame %q", f.Type))
	}
}

// receive handles a message addressed to this session.
func (m *Mesh) receive(l *Link, f proto.Frame) (proto.SendResult, error) {
	if m.cfg.Kind != proto.KindSession || m.cfg.OnMessage == nil {
		return proto.SendResult{}, errors.New("not a session")
	}
	if strings.TrimSpace(f.Text) == "" {
		return proto.SendResult{}, errors.New("empty message")
	}
	if len(f.Text) > proto.MaxText {
		return proto.SendResult{}, fmt.Errorf("message is %d bytes, limit is %d", len(f.Text), proto.MaxText)
	}
	if !l.rateOK() {
		return proto.SendResult{}, fmt.Errorf("too many messages in %s; batch them into one", rateWindow)
	}
	l.mu.Lock()
	peer, info, kind := l.peer, l.info, l.kind
	l.mu.Unlock()
	m.mu.Lock()
	me := m.info
	m.mu.Unlock()
	if peer != nil && !peer.Perms.MaySee(me.Host, me.Name) {
		return proto.SendResult{}, errors.New("no such session")
	}
	thread := proto.CleanThread(f.Thread)
	if thread == "" {
		thread = proto.NewID(6)
	}
	id := proto.CleanThread(f.ID)
	if id == "" {
		id = proto.NewID(6)
	}
	msg := proto.Message{
		ID:       id,
		Thread:   thread,
		From:     m.addr(l),
		FromUser: m.userOf(l),
		Text:     f.Text,
		To:       m.SelfAddress(),
		SentAt:   time.Now().Unix(),
	}
	if kind != proto.KindControl {
		msg.FromSID = info.SID
	}
	res := proto.SendResult{ID: msg.ID, Thread: thread, To: msg.To}
	if peer == nil || peer.Perms.Deliver == "auto" || m.inbox.Approved(l.key, thread) {
		m.cfg.OnMessage(msg, peer != nil)
		res.Status = "delivered"
		return res, nil
	}
	h := &inbox.Held{Msg: msg, Peer: identity.Key(l.key), PeerName: peer.Name, Target: me.SID}
	if strings.HasSuffix(f.To, "/") {
		h.Target = "" // addressed to us as a person; the human picks the session
	}
	if err := m.inbox.Add(h); err != nil {
		return proto.SendResult{}, fmt.Errorf("couldn't hold the message: %w", err)
	}
	m.notice("held",
		fmt.Sprintf("A message from %s (another person) is waiting for your user's approval in `peerbus tui`. Tell them briefly; don't approve or deny it yourself.", msg.From),
		map[string]string{"held_id": h.ID, "from": msg.From})
	res.Status = "held"
	return res, nil
}

func (m *Mesh) onNotice(l *Link, f proto.Frame) {
	switch f.Kind {
	case "idle":
		l.mu.Lock()
		want := l.wantIdle
		l.wantIdle = false
		l.mu.Unlock()
		if !want {
			m.log.Printf("%s says it's idle, but we didn't ask", m.describe(l))
			return
		}
		addr := m.addr(l)
		m.notice("idle", fmt.Sprintf("%s is now idle.", addr), map[string]string{"peer": addr, "state": "idle"})
	case "status":
		user := m.userOf(l)
		meta := map[string]string{"msg_id": f.Meta["msg_id"], "thread": f.Meta["thread"], "status": f.Meta["status"]}
		switch f.Meta["status"] {
		case "approved":
			m.notice("status", fmt.Sprintf("%s approved your message %s; it was delivered. Replies in thread %s now flow without approval for %s.", user, f.Meta["msg_id"], f.Meta["thread"], inbox.Grant), meta)
		case "declined":
			m.notice("status", fmt.Sprintf("%s declined your message %s.", user, f.Meta["msg_id"]), meta)
		}
	}
}

// Send delivers text to a session or person named by to.
func (m *Mesh) Send(ctx context.Context, to, text, thread string) (proto.SendResult, error) {
	return m.send(ctx, to, text, thread, m.cfg.Kind == proto.KindControl)
}

// send is Send; human marks a message the human sent or approved
// themselves, which satisfies a peer's send=ask.
func (m *Mesh) send(ctx context.Context, to, text, thread string, human bool) (proto.SendResult, error) {
	if strings.TrimSpace(text) == "" {
		return proto.SendResult{}, errors.New("empty message")
	}
	if len(text) > proto.MaxText {
		return proto.SendResult{}, fmt.Errorf("message is %d bytes, limit is %d", len(text), proto.MaxText)
	}
	tgt, err := m.resolve(to)
	if err != nil {
		return proto.SendResult{}, err
	}
	routed := tgt.link == nil
	if routed {
		tgt.link = m.anySessionOf(tgt.peer)
		if tgt.link == nil {
			return proto.SendResult{}, fmt.Errorf("no session of %s is online", tgt.user)
		}
	}
	m.mu.Lock()
	mySID := m.info.SID
	m.mu.Unlock()
	if tgt.link.Info().SID == mySID && tgt.peer == nil && m.cfg.Kind == proto.KindSession {
		return proto.SendResult{}, errors.New("that address is this session")
	}
	thread = proto.CleanThread(thread)
	if thread == "" {
		thread = proto.NewID(6)
	}
	id := proto.NewID(6)
	addr := m.addr(tgt.link)
	wire := tgt.link.Address()
	if routed {
		addr, wire = tgt.user+"/", tgt.user+"/"
	}
	if tgt.peer != nil {
		if tgt.peer.Perms.Send == "ask" && !human {
			h := &inbox.Held{
				Msg:      proto.Message{ID: id, Thread: thread, From: m.SelfAddress(), FromSID: mySID, To: addr, Text: text, SentAt: time.Now().Unix()},
				Peer:     tgt.peer.Key,
				PeerName: tgt.peer.Name,
				Target:   mySID,
				Outbound: true,
			}
			if err := m.inbox.Add(h); err != nil {
				return proto.SendResult{}, err
			}
			return proto.SendResult{Status: "ask", ID: h.ID, Thread: thread, To: addr}, nil
		}
		// We accept replies in a thread we started.
		m.inbox.Approve(ed25519.PublicKey(tgt.peer.Key), thread)
	}
	var res proto.SendResult
	if err := tgt.link.Request(ctx, proto.Frame{Type: proto.TSend, ID: id, Thread: thread, Text: text, To: wire}, &res); err != nil {
		return proto.SendResult{}, err
	}
	res.To = addr
	return res, nil
}

// SubscribeIdle asks for one notice when peer next goes idle or disconnects.
func (m *Mesh) SubscribeIdle(ctx context.Context, peer string) (status, addr string, err error) {
	tgt, err := m.resolve(peer)
	if err != nil {
		return "", "", err
	}
	if tgt.link == nil {
		return "", "", errors.New("subscribe to a session, not a person")
	}
	var res map[string]string
	if err := tgt.link.Request(ctx, proto.Frame{Type: proto.TSubscribeIdle}, &res); err != nil {
		return "", "", err
	}
	if res["status"] == "subscribed" {
		tgt.link.mu.Lock()
		tgt.link.wantIdle = true
		tgt.link.mu.Unlock()
	}
	return res["status"], m.addr(tgt.link), nil
}

// inboxView lists this machine's held messages for our own control clients.
func (m *Mesh) inboxView() (proto.InboxView, error) {
	st, err := m.inbox.Load()
	if err != nil {
		return proto.InboxView{}, err
	}
	m.mu.Lock()
	v := proto.InboxView{Machine: m.info.Host, InboxID: m.inboxID(), Held: []proto.HeldView{}}
	m.mu.Unlock()
	for _, h := range st.Held {
		v.Held = append(v.Held, h.View())
	}
	return v, nil
}

// inboxID tells this inbox apart from others: the machine, and the state
// directory, since two OS users (or PEERBUS_STATE_DIR) can mean two inboxes
// on one machine.
func (m *Mesh) inboxID() string {
	dir, _ := filepath.Abs(m.cfg.StateDir)
	sum := sha256.Sum256([]byte(transport.MachineID() + "\x00" + dir))
	return hex.EncodeToString(sum[:8])
}

// ownSession finds another of our sessions, on any machine, by session id.
func (m *Mesh) ownSession(sid string) *Link {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.links {
		l.mu.Lock()
		ok := l.peer == nil && l.kind == proto.KindSession && l.info.SID == sid
		l.mu.Unlock()
		if ok {
			return l
		}
	}
	return nil
}

// approve delivers a held message into this session, or into the session
// of ours named by to (on any machine), or for one of ours waiting on
// send=ask, sends it; then tells the other side.
func (m *Mesh) approve(id, to string) (proto.SendResult, error) {
	h, err := m.inbox.Find(id)
	if err != nil {
		return proto.SendResult{}, err
	}
	m.mu.Lock()
	mySID := m.info.SID
	m.mu.Unlock()
	if h.Outbound {
		if h.Target != mySID {
			return proto.SendResult{}, errors.New("that message was written by another session; approve it there")
		}
		res, err := m.send(m.ctx, h.Msg.To, h.Msg.Text, h.Msg.Thread, true)
		if err != nil {
			return proto.SendResult{}, err
		}
		m.inbox.Take(id)
		return res, nil
	}
	if m.cfg.OnMessage == nil {
		return proto.SendResult{}, errors.New("not a session")
	}
	var res proto.SendResult
	if to == "" || to == mySID {
		if err := m.inbox.Approve(ed25519.PublicKey(h.Peer), h.Msg.Thread); err != nil {
			return proto.SendResult{}, err
		}
		if _, err := m.inbox.Take(id); err != nil {
			return proto.SendResult{}, err
		}
		res = m.deliverHere(h)
	} else {
		l := m.ownSession(to)
		if l == nil {
			return proto.SendResult{}, errors.New("that session isn't online")
		}
		if err := l.Request(m.ctx, proto.Frame{Type: proto.TDeliver, Data: h}, &res); err != nil {
			return proto.SendResult{}, err
		}
		if _, err := m.inbox.Take(id); err != nil {
			return proto.SendResult{}, err
		}
	}
	msg := h.Msg
	if l := m.linkBySID(h.Peer, msg.FromSID); l != nil {
		l.write(proto.Frame{Type: proto.TNotice, Kind: "status", Meta: map[string]string{"status": "approved", "msg_id": msg.ID, "thread": msg.Thread}})
	}
	return res, nil
}

// deliverHere hands an approved message to this session.
func (m *Mesh) deliverHere(h *inbox.Held) proto.SendResult {
	msg := h.Msg
	msg.To = m.SelfAddress()
	m.cfg.OnMessage(msg, true)
	return proto.SendResult{Status: "delivered", ID: msg.ID, Thread: msg.Thread, To: msg.To}
}

// deliver takes an approved message from another of our sessions, which
// held it, and opens the thread on this machine so replies flow here.
func (m *Mesh) deliver(l *Link, f proto.Frame) (proto.SendResult, error) {
	l.mu.Lock()
	own := l.peer == nil && l.kind == proto.KindSession
	l.mu.Unlock()
	if !own || m.cfg.Kind != proto.KindSession || m.cfg.OnMessage == nil {
		return proto.SendResult{}, errors.New("not allowed")
	}
	b, err := json.Marshal(f.Data)
	if err != nil {
		return proto.SendResult{}, err
	}
	var h inbox.Held
	if err := json.Unmarshal(b, &h); err != nil || h.Outbound {
		return proto.SendResult{}, errors.New("bad held message")
	}
	if len(h.Msg.Text) > proto.MaxText || strings.TrimSpace(h.Msg.Text) == "" {
		return proto.SendResult{}, errors.New("bad held message")
	}
	peer := m.peerFor(ed25519.PublicKey(h.Peer))
	if peer == nil {
		return proto.SendResult{}, errors.New("that sender isn't one of your peers any more")
	}
	if err := m.inbox.Approve(ed25519.PublicKey(h.Peer), h.Msg.Thread); err != nil {
		return proto.SendResult{}, err
	}
	return m.deliverHere(&h), nil
}

func (m *Mesh) deny(id string) (map[string]string, error) {
	h, err := m.inbox.Take(id)
	if err != nil {
		return nil, err
	}
	if !h.Outbound {
		if l := m.linkBySID(h.Peer, h.Msg.FromSID); l != nil {
			l.write(proto.Frame{Type: proto.TNotice, Kind: "status", Meta: map[string]string{"status": "declined", "msg_id": h.Msg.ID, "thread": h.Msg.Thread}})
		}
	}
	return map[string]string{"status": "declined", "id": h.ID}, nil
}

func (m *Mesh) linkBySID(key identity.Key, sid string) *Link {
	if sid == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.links {
		if key.Equal(l.key) && l.Info().SID == sid {
			return l
		}
	}
	return nil
}

func (m *Mesh) anySessionOf(peer *identity.Peer) *Link {
	m.mu.Lock()
	links := m.snapshot()
	m.mu.Unlock()
	var cands []*Link
	for _, l := range links {
		if sameUser(l, peer) && l.kind == proto.KindSession {
			cands = append(cands, l)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return m.addr(cands[i]) < m.addr(cands[j]) })
	return cands[0]
}

func sameUser(l *Link, peer *identity.Peer) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if peer == nil {
		return l.peer == nil
	}
	return l.peer != nil && peer.Key.Equal(l.key)
}

var refRe = regexp.MustCompile(`^(.*?)\s*(?:#|\[)([0-9a-fA-F]{3,32})\]?$`)

type target struct {
	link *Link          // set when a session was named
	peer *identity.Peer // nil for our own
	user string
}

// resolve turns a "to" string into a connected session or a person.
//
//	name, name#ref, name [ref]  one of our own sessions
//	user/name, user/name#ref    a session of a peer
//	user/ or user/*             a peer; their human picks the session
//	user                        same, when we have no session by that name
func (m *Mesh) resolve(to string) (target, error) {
	t := strings.TrimPrefix(strings.TrimSpace(to), "@")
	ref := ""
	if mm := refRe.FindStringSubmatch(t); mm != nil {
		t, ref = strings.TrimSpace(mm[1]), strings.ToLower(mm[2])
	}
	m.mu.Lock()
	self := m.peers.SelfName()
	user, name, hasUser := strings.Cut(t, "/")
	if !hasUser {
		user, name = self, t
	}
	var peer *identity.Peer
	if user != self {
		if peer = m.peers.ByName(user); peer == nil || !peer.Enabled {
			m.mu.Unlock()
			return target{}, fmt.Errorf("no peer named %q (see list_peers)", user)
		}
	}
	links := m.snapshot()
	m.mu.Unlock()

	if hasUser && (name == "" || name == "*") && ref == "" {
		if peer == nil {
			return target{}, errors.New("name one of your sessions; addressing a person applies to other people")
		}
		return target{peer: peer, user: user}, nil
	}
	var cands []*Link
	for _, l := range links {
		if !sameUser(l, peer) {
			continue
		}
		l.mu.Lock()
		info, kind := l.info, l.kind
		l.mu.Unlock()
		if kind != proto.KindSession {
			continue
		}
		if ref != "" {
			if strings.HasPrefix(proto.Ref(info.SID), ref) || strings.HasPrefix(strings.ReplaceAll(info.SID, "-", ""), ref) {
				cands = append(cands, l)
			}
		} else if info.Name == name {
			cands = append(cands, l)
		}
	}
	switch len(cands) {
	case 1:
		return target{link: cands[0], peer: peer, user: user}, nil
	case 0:
		if !hasUser && ref == "" && t != self {
			m.mu.Lock()
			p := m.peers.ByName(t)
			m.mu.Unlock()
			if p != nil && p.Enabled {
				return target{peer: p, user: t}, nil
			}
		}
		return target{}, fmt.Errorf("no session %q is online (see list_peers)", to)
	default:
		var addrs []string
		for _, l := range cands {
			addrs = append(addrs, m.addr(l))
		}
		sort.Strings(addrs)
		return target{}, fmt.Errorf("%q is ambiguous, use one of: %s", to, strings.Join(addrs, ", "))
	}
}
