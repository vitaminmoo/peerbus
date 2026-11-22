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
	"strconv"
	"strings"
	"time"

	"github.com/vitaminmoo/peerbus/internal/history"
	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/inbox"
	"github.com/vitaminmoo/peerbus/internal/proto"
	"github.com/vitaminmoo/peerbus/internal/push"
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
		m.checkTwin(l)
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
	case proto.THistory:
		if !l.isOwnControl() {
			l.reply(f.Req, nil, errors.New("not allowed"))
			return
		}
		if f.ID != "" {
			res, err := m.HistoryMessage(f.Meta["from"], f.ID)
			l.reply(f.Req, res, err)
			return
		}
		res, err := m.History()
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
	case proto.TPushRegister:
		if !l.isOwnControl() || m.cfg.Kind != proto.KindSession {
			l.reply(f.Req, nil, errors.New("not allowed"))
			return
		}
		l.reply(f.Req, nil, m.registerPhone(f.Meta["name"], f.Meta["device"], f.Meta["token"]))
	case proto.TFakeHeld:
		if !l.isLocalControl() || m.cfg.Kind != proto.KindSession {
			l.reply(f.Req, nil, errors.New("not allowed"))
			return
		}
		res, err := m.fakeHeld(f.Meta["from"], f.Text, f.To == "you")
		l.reply(f.Req, res, err)
	case proto.TDeleteThread:
		if !l.isOwnControl() || m.cfg.Kind != proto.KindSession {
			l.reply(f.Req, nil, errors.New("not allowed"))
			return
		}
		at, _ := strconv.ParseInt(f.Meta["at"], 10, 64)
		res, err := m.DeleteThread(f.Thread, at)
		l.reply(f.Req, res, err)
	case proto.TReload:
		if !l.isOwnControl() || m.cfg.Kind != proto.KindSession || m.cfg.OnReload == nil {
			l.reply(f.Req, nil, errors.New("not allowed"))
			return
		}
		res, start, err := m.cfg.OnReload()
		l.reply(f.Req, res, err)
		if err == nil && start != nil {
			go start()
		}
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
	// A session the peer can't see takes nothing from them, unless it has
	// a thread open with them: then they can answer, and what they send
	// outside that thread waits for the human whatever deliver says.
	hidden := peer != nil && !peer.Perms.MaySee(me.Host, me.Name)
	if hidden && !m.inbox.Shown(l.key, me.SID) {
		return proto.SendResult{}, errors.New("no such session")
	}
	toHuman := strings.HasSuffix(f.To, "/"+proto.Human)
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
		ReplyTo:  proto.CleanThread(f.ReplyTo),
	}
	if kind != proto.KindControl {
		msg.FromSID = info.SID
	}
	if toHuman {
		// For the person, to read: it never reaches this session's agent,
		// so it needn't wait for approval.
		msg.To = m.selfName() + "/" + proto.Human
		return m.noteForHuman(msg, peer, l.key)
	}
	res := proto.SendResult{ID: msg.ID, Thread: thread, To: msg.To}
	rec := proto.HistoryEntry{ID: msg.ID, Thread: thread, ReplyTo: msg.ReplyTo, From: msg.From, To: msg.To, Text: msg.Text, At: msg.SentAt}
	if peer != nil {
		rec.Peer = peer.Name
	}
	if peer == nil || (peer.Perms.Deliver == "auto" && !hidden) || m.inbox.Approved(l.key, thread) {
		m.cfg.OnMessage(msg, peer != nil)
		res.Status = "delivered"
		rec.Status = res.Status
		m.record(rec)
		return res, nil
	}
	h := &inbox.Held{Msg: msg, Peer: identity.Key(l.key), PeerName: peer.Name, Target: me.SID}
	if strings.HasSuffix(f.To, "/") {
		h.Target = "" // addressed to us as a person; the human picks the session
		rec.To = m.selfName() + "/"
	}
	if err := m.inbox.Add(h); err != nil {
		return proto.SendResult{}, fmt.Errorf("couldn't hold the message: %w", err)
	}
	rec.Status = "held"
	m.record(rec)
	m.pushNote(m.heldNote(h))
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
		case "approved", "declined":
			status := map[string]string{"approved": "delivered", "declined": "declined"}[f.Meta["status"]]
			m.record(proto.HistoryEntry{ID: f.Meta["msg_id"], From: m.selfFrom(), Status: status})
		}
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
	return m.Reply(ctx, to, text, thread, "")
}

// Reply is Send answering the message with id replyTo. With thread empty it
// continues that message's thread, if this session has seen it.
func (m *Mesh) Reply(ctx context.Context, to, text, thread, replyTo string) (proto.SendResult, error) {
	return m.send(ctx, to, text, thread, replyTo, "", m.cfg.Kind == proto.KindControl)
}

// send is Send; human marks a message the human sent or approved
// themselves, which satisfies a peer's send=ask. id is empty for a new
// message.
func (m *Mesh) send(ctx context.Context, to, text, thread, replyTo, id string, human bool) (proto.SendResult, error) {
	replyTo = proto.CleanThread(replyTo)
	if thread == "" && replyTo != "" {
		thread = m.threadOf(replyTo)
	}
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
	if tgt.human && tgt.peer == nil && m.cfg.Kind == proto.KindSession {
		// Our own person: this machine's inbox will do.
		msg := proto.Message{
			ID: orNewID(id), Thread: orNewThread(thread), From: m.SelfAddress(), FromUser: m.selfName(),
			FromSID: m.Info().SID, To: m.selfName() + "/" + proto.Human, Text: text, SentAt: time.Now().Unix(), ReplyTo: replyTo,
		}
		return m.noteForHuman(msg, nil, m.pub)
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
	if id == "" {
		id = proto.NewID(6)
	}
	unverified := !tgt.human && !routed && tgt.link.Info().Channels != proto.ChannelsOn
	addr := m.addr(tgt.link)
	wire := tgt.link.Address()
	switch {
	case tgt.human:
		addr, wire = tgt.user+"/"+proto.Human, tgt.user+"/"+proto.Human
	case routed:
		addr, wire = tgt.user+"/", tgt.user+"/"
	}
	rec := proto.HistoryEntry{ID: id, Thread: thread, ReplyTo: replyTo, From: m.selfFrom(), To: addr, Text: text}
	if tgt.peer != nil {
		rec.Peer = tgt.peer.Name
		if tgt.peer.Perms.Send == "ask" && !human {
			h := &inbox.Held{
				Msg:      proto.Message{ID: id, Thread: thread, ReplyTo: replyTo, From: m.SelfAddress(), FromSID: mySID, To: addr, Text: text, SentAt: time.Now().Unix()},
				Peer:     tgt.peer.Key,
				PeerName: tgt.peer.Name,
				Target:   mySID,
				Outbound: true,
			}
			if err := m.inbox.Add(h); err != nil {
				return proto.SendResult{}, err
			}
			m.pushNote(m.heldNote(h))
			rec.Status = "ask"
			m.record(rec)
			return proto.SendResult{Status: "ask", ID: h.ID, Thread: thread, To: addr, Unverified: unverified}, nil
		}
		// We accept replies in a thread we started.
		m.openThread(ed25519.PublicKey(tgt.peer.Key), tgt.peer, thread)
	}
	var res proto.SendResult
	if err := tgt.link.Request(ctx, proto.Frame{Type: proto.TSend, ID: id, Thread: thread, ReplyTo: replyTo, Text: text, To: wire}, &res); err != nil {
		return proto.SendResult{}, err
	}
	res.To = addr
	res.Unverified = unverified
	rec.Status = res.Status
	m.record(rec)
	return res, nil
}

// selfFrom is this session's address as a session we send to records it.
func (m *Mesh) selfFrom() string {
	if m.cfg.Kind == proto.KindControl {
		m.mu.Lock()
		name := m.info.Name
		m.mu.Unlock()
		return m.selfName() + "/" + name // cli, tui or phone
	}
	return m.SelfAddress()
}

// record adds to this machine's message history. Failing only costs the
// TUI's view of the message, so it's logged and otherwise ignored.
func (m *Mesh) record(e proto.HistoryEntry) {
	if err := m.history.Add(e); err != nil {
		m.log.Printf("history: %v", err)
	}
}

// History lists the messages this machine's sessions sent and received,
// newest first, as many as fit in one frame, long ones cut short.
func (m *Mesh) History() (proto.HistoryView, error) {
	const budget, preview = 96 << 10, 400
	msgs, err := m.history.Messages()
	if err != nil {
		return proto.HistoryView{}, err
	}
	deleted, err := m.history.Deleted()
	if err != nil {
		return proto.HistoryView{}, err
	}
	m.mu.Lock()
	v := proto.HistoryView{Machine: m.info.Host, InboxID: m.inboxID(), Messages: []proto.HistoryEntry{}, V: proto.ViewVersion, Deleted: deleted}
	m.mu.Unlock()
	size := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		e := msgs[i]
		if len(e.Text) > preview {
			e.Text, e.Cut = strings.ToValidUTF8(e.Text[:preview], ""), true
		}
		b, _ := json.Marshal(e)
		if size += len(b) + 1; size > budget {
			break
		}
		v.Messages = append(v.Messages, e)
	}
	return v, nil
}

// HistoryMessage is one message from this machine's history in full.
func (m *Mesh) HistoryMessage(from, id string) (proto.HistoryEntry, error) {
	return m.history.Find(from, id)
}

// DeleteThread forgets a thread's messages up to at on this machine, and
// the notes to you waiting in its inbox. Approvals of the thread end with
// it. Messages waiting for a decision stay, in the inbox and the history, so
// nobody's message vanishes unanswered: approve or decline them first.
func (m *Mesh) DeleteThread(thread string, at int64) (proto.DeleteResult, error) {
	thread = proto.CleanThread(thread)
	if thread == "" {
		return proto.DeleteResult{}, errors.New("no thread given")
	}
	if now := time.Now().Unix(); at <= 0 || at > now {
		at = now
	}
	st, err := m.inbox.Load()
	if err != nil {
		return proto.DeleteResult{}, err
	}
	waiting := map[string]bool{}
	for _, h := range st.Held {
		if h.Msg.Thread != thread || h.Msg.SentAt > at {
			continue
		}
		if !h.Note {
			waiting[h.Msg.From+"\x00"+h.Msg.ID] = true
			continue
		}
		if _, err := m.inbox.Take(h.ID); err == nil {
			m.pushNote(push.Note{Kind: push.Gone, ID: h.ID})
		}
	}
	if err := m.inbox.RevokeThread(thread, at); err != nil {
		return proto.DeleteResult{}, err
	}
	removed, err := m.history.DeleteThread(thread, at, func(e proto.HistoryEntry) bool { return waiting[history.Key(e)] })
	if err != nil {
		return proto.DeleteResult{}, err
	}
	m.mu.Lock()
	host := m.info.Host
	m.mu.Unlock()
	return proto.DeleteResult{Machine: host, InboxID: m.inboxID(), Removed: removed, Kept: len(waiting), V: proto.ViewVersion}, nil
}

// SessionMessages are the messages this session itself sent or had
// delivered, oldest first: all an agent may see of the history, which also
// holds other sessions' messages and ones still waiting for approval. Ref,
// not the whole address, identifies it, since a session can be renamed.
func (m *Mesh) SessionMessages() ([]proto.HistoryEntry, error) {
	msgs, err := m.history.Messages()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	prefix, suffix := m.peers.SelfName()+"/", "#"+proto.Ref(m.info.SID)
	m.mu.Unlock()
	mine := func(addr string) bool { return strings.HasPrefix(addr, prefix) && strings.HasSuffix(addr, suffix) }
	var out []proto.HistoryEntry
	for _, e := range msgs {
		if mine(e.From) || mine(e.To) && e.Status == "delivered" && !e.Fake {
			out = append(out, e)
		}
	}
	return out, nil
}

// threadOf is the thread of a message this session sent or received.
func (m *Mesh) threadOf(id string) string {
	if m.cfg.Kind != proto.KindSession {
		return ""
	}
	msgs, err := m.SessionMessages()
	if err != nil {
		m.log.Printf("history: %v", err)
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].ID == id {
			return msgs[i].Thread
		}
	}
	return ""
}

// TakeIdleWatches ends this session's idle subscriptions and says who they
// were on, for a process about to leave the mesh and come back.
func (m *Mesh) TakeIdleWatches() []string {
	m.mu.Lock()
	links := m.snapshot()
	m.mu.Unlock()
	var out []string
	for _, l := range links {
		l.mu.Lock()
		want := l.wantIdle
		l.wantIdle = false
		l.mu.Unlock()
		if want {
			out = append(out, m.addr(l))
		}
	}
	return out
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
	v := proto.InboxView{Machine: m.info.Host, InboxID: m.inboxID(), Held: []proto.HeldView{}, V: proto.ViewVersion}
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
	if h.Note {
		return proto.SendResult{}, errors.New("that message is for you to read; reply to it or dismiss it")
	}
	if h.Fake {
		// Nothing to deliver: say where it would have gone.
		addr := m.SelfAddress()
		if to != "" && to != mySID {
			l := m.ownSession(to)
			if l == nil {
				return proto.SendResult{}, errors.New("that session isn't online")
			}
			addr = l.Address()
		}
		if _, err := m.inbox.Take(id); err != nil {
			return proto.SendResult{}, err
		}
		m.record(proto.HistoryEntry{ID: h.Msg.ID, From: h.Msg.From, To: addr, Status: "delivered", Fake: true})
		m.pushNote(push.Note{Kind: push.Gone, ID: id})
		return proto.SendResult{Status: "delivered", ID: h.Msg.ID, Thread: h.Msg.Thread, To: addr}, nil
	}
	if h.Outbound {
		if h.Target != mySID {
			return proto.SendResult{}, errors.New("that message was written by another session; approve it there")
		}
		res, err := m.send(m.ctx, h.Msg.To, h.Msg.Text, h.Msg.Thread, h.Msg.ReplyTo, h.Msg.ID, true)
		if err != nil {
			return proto.SendResult{}, err
		}
		m.inbox.Take(id)
		m.pushNote(push.Note{Kind: push.Gone, ID: id})
		return res, nil
	}
	if m.cfg.OnMessage == nil {
		return proto.SendResult{}, errors.New("not a session")
	}
	var res proto.SendResult
	if to == "" || to == mySID {
		if err := m.openThread(ed25519.PublicKey(h.Peer), m.peerFor(ed25519.PublicKey(h.Peer)), h.Msg.Thread); err != nil {
			return proto.SendResult{}, err
		}
		if _, err := m.inbox.Take(id); err != nil {
			return proto.SendResult{}, err
		}
		res = m.deliverHere(h)
		if h.Target != mySID {
			// Routed here from where it was held for: a change names where
			// it ended up, as when it's handed to another session.
			m.record(proto.HistoryEntry{ID: h.Msg.ID, From: h.Msg.From, To: res.To, Status: "delivered"})
		}
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
		m.record(proto.HistoryEntry{ID: h.Msg.ID, From: h.Msg.From, To: res.To, Status: "delivered"})
	}
	m.pushNote(push.Note{Kind: push.Gone, ID: id})
	msg := h.Msg
	if l := m.linkBySID(h.Peer, msg.FromSID); l != nil {
		l.write(proto.Frame{Type: proto.TNotice, Kind: "status", Meta: map[string]string{"status": "approved", "msg_id": msg.ID, "thread": msg.Thread}})
	}
	return res, nil
}

// openThread lets messages in thread from the person with key flow to
// this session for inbox.Grant. peer is nil for a test message's made-up
// sender. A session their sessions setting hides from them shows itself
// to them for as long, or they couldn't answer it.
func (m *Mesh) openThread(key ed25519.PublicKey, peer *identity.Peer, thread string) error {
	m.mu.Lock()
	me := m.info
	links := m.snapshot()
	m.mu.Unlock()
	if peer == nil || m.cfg.Kind != proto.KindSession || peer.Perms.MaySee(me.Host, me.Name) {
		return m.inbox.Approve(key, thread, "")
	}
	if err := m.inbox.Approve(key, thread, me.SID); err != nil {
		return err
	}
	for _, l := range links {
		if sameUser(l, peer) {
			f := m.helloFor(peer)
			f.Type = proto.TUpdate
			l.write(f)
		}
	}
	return nil
}

// deliverHere hands an approved message to this session.
func (m *Mesh) deliverHere(h *inbox.Held) proto.SendResult {
	msg := h.Msg
	msg.To = m.SelfAddress()
	m.cfg.OnMessage(msg, true)
	m.record(proto.HistoryEntry{ID: msg.ID, Thread: msg.Thread, ReplyTo: msg.ReplyTo, From: msg.From, To: msg.To, Peer: h.PeerName, Text: msg.Text, At: msg.SentAt, Status: "delivered"})
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
	if err := m.openThread(ed25519.PublicKey(h.Peer), peer, h.Msg.Thread); err != nil {
		return proto.SendResult{}, err
	}
	return m.deliverHere(&h), nil
}

func (m *Mesh) deny(id string) (map[string]string, error) {
	h, err := m.inbox.Take(id)
	if err != nil {
		return nil, err
	}
	if h.Note {
		// Read and dismissed; the sender already knows it arrived.
		m.pushNote(push.Note{Kind: push.Gone, ID: h.ID})
		return map[string]string{"status": "dismissed", "id": h.ID}, nil
	}
	status := "declined"
	if h.Outbound {
		status = "unsent" // ours, and we said no
	}
	m.record(proto.HistoryEntry{ID: h.Msg.ID, From: h.Msg.From, Status: status})
	m.pushNote(push.Note{Kind: push.Gone, ID: h.ID})
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
	link  *Link          // set when a session was named
	peer  *identity.Peer // nil for our own
	user  string
	human bool // user/human: the person, to read it
}

// resolve turns a "to" string into a connected session or a person.
//
//	name, name#ref, name [ref]  one of our own sessions
//	user/name, user/name#ref    a session of a peer
//	user/ or user/*             a peer; their human picks the session
//	user                        same, when we have no session by that name
//	human, user/human           the person themselves, to read it
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

	if name == proto.Human {
		return target{peer: peer, user: user, human: true}, nil
	}
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
	cands = newestPerSession(cands)
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

// heldNote is what your phone shows for a message held here, worded as the
// app's own list words it.
func (m *Mesh) heldNote(h *inbox.Held) push.Note {
	m.mu.Lock()
	here := m.info.Name + "@" + m.info.Host
	m.mu.Unlock()
	n := push.Note{Kind: push.Held, ID: h.ID, Text: h.Msg.Text}
	switch {
	case h.Note:
		n.Title = "From " + h.Msg.From
	case h.Outbound:
		n.Title, n.Text = "Waiting to send to "+h.Msg.To, here+": "+h.Msg.Text
	case h.Target == "":
		n.Title = "From " + h.Msg.From + " · pick a session"
	default:
		n.Title = "From " + h.Msg.From + " → " + here
	}
	return n
}

// pushNote seals n for the phones registered on this machine and has the
// rendezvous server forward it. It runs in the background: a lost note
// only costs a notification, and the app shows the message when opened.
func (m *Mesh) pushNote(n push.Note) {
	if m.cfg.Kind != proto.KindSession || m.cfg.Server == "" {
		return
	}
	m.reloadPeers() // a phone may have just registered with another session here
	m.mu.Lock()
	secret, phone := m.peers.Self.Secret, m.peers.Self.Pushable()
	m.mu.Unlock()
	if phone == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		switch err := m.node.Push(ctx, phone.Token, push.Seal(secret, n)); {
		case errors.Is(err, transport.ErrUnregistered):
			// The app is gone or has a new token it hasn't told us yet.
			m.dropPhoneToken(phone.Token)
		case err != nil:
			m.log.Printf("push to %s: %v", phone.Name, err)
		}
	}()
}

// registerPhone records your phone's push token in peers.json, from where
// it syncs to your other machines. An empty token stops pushes.
func (m *Mesh) registerPhone(name, device, token string) error {
	if device == "" {
		return errors.New("no device")
	}
	return m.updatePhone(func(old *identity.Phone) (*identity.Phone, bool) {
		if token == "" && (old == nil || old.Device != device) {
			return nil, false // a phone we already replaced
		}
		if old != nil && old.Device == device && old.Token == token && old.Name == name {
			return nil, false
		}
		if name == "" && old != nil {
			name = old.Name
		}
		return &identity.Phone{Name: identity.CleanName(name), Device: device, Token: token}, true
	})
}

// dropPhoneToken stops pushes to a token the push service has forgotten,
// unless the phone has since registered a new one.
func (m *Mesh) dropPhoneToken(token string) {
	err := m.updatePhone(func(old *identity.Phone) (*identity.Phone, bool) {
		if old == nil || old.Token != token {
			return nil, false
		}
		p := *old
		p.Token = ""
		return &p, true
	})
	if err != nil {
		m.log.Printf("dropping the phone's push token: %v", err)
	}
}

// updatePhone applies change to Self.Phone in peers.json and syncs it.
func (m *Mesh) updatePhone(change func(old *identity.Phone) (*identity.Phone, bool)) error {
	err := m.cfg.Store.Update(func(ps *identity.Peers) error {
		p, ok := change(ps.Self.Phone)
		if !ok {
			return errUnchanged
		}
		p.Updated = time.Now().UTC()
		ps.Self.Phone = p
		return nil
	})
	if errors.Is(err, errUnchanged) {
		return nil
	}
	if err == nil {
		m.reloadPeers() // and tell your other machines
	}
	return err
}

// fakeHeld holds a made-up message from a made-up person, for this
// session (or, for you, as a person), so you can try approving it in
// `peerbus tui` or the app. It goes through the inbox, history and pushes
// like a real one; approving it delivers nothing.
func (m *Mesh) fakeHeld(from, text string, forYou bool) (proto.SendResult, error) {
	from = identity.CleanName(from)
	text = strings.TrimSpace(text)
	if text == "" || len(text) > proto.MaxText {
		return proto.SendResult{}, errors.New("give the message some text")
	}
	m.mu.Lock()
	me := m.info
	m.mu.Unlock()
	msg := proto.Message{
		ID: proto.NewID(6), Thread: proto.NewID(6),
		From: from + "/test#000000", FromUser: from,
		To: m.SelfAddress(), Text: text, SentAt: time.Now().Unix(),
	}
	h := &inbox.Held{Msg: msg, Peer: identity.Key(make([]byte, ed25519.PublicKeySize)), PeerName: from, Target: me.SID, Fake: true}
	if forYou {
		h.Target, h.Msg.To = "", m.selfName()+"/"
	}
	if err := m.inbox.Add(h); err != nil {
		return proto.SendResult{}, err
	}
	m.record(proto.HistoryEntry{ID: msg.ID, Thread: msg.Thread, From: msg.From, To: h.Msg.To, Peer: from, Text: text, At: msg.SentAt, Status: "held", Fake: true})
	m.pushNote(m.heldNote(h))
	return proto.SendResult{Status: "held", ID: h.ID, Thread: msg.Thread, To: h.Msg.To}, nil
}

// SendAsYou sends text from your own TUI or phone. It isn't held by your
// send=ask setting: you're the one sending it.
func (m *Mesh) SendAsYou(ctx context.Context, to, text, thread, replyTo string) (proto.SendResult, error) {
	return m.send(ctx, to, text, thread, replyTo, "", true)
}

// noteForHuman keeps a message for the person in this machine's inbox, to
// read in `peerbus tui` and on their phone, and pushes it to the phone.
// key is the sender's identity: a friend's, or ours.
func (m *Mesh) noteForHuman(msg proto.Message, peer *identity.Peer, key ed25519.PublicKey) (proto.SendResult, error) {
	h := &inbox.Held{Msg: msg, Peer: identity.Key(key), Note: true}
	rec := proto.HistoryEntry{ID: msg.ID, Thread: msg.Thread, ReplyTo: msg.ReplyTo, From: msg.From, To: msg.To, Text: msg.Text, At: msg.SentAt, Status: "delivered"}
	if peer != nil {
		h.PeerName, rec.Peer = peer.Name, peer.Name
	}
	if err := m.inbox.Add(h); err != nil {
		return proto.SendResult{}, fmt.Errorf("couldn't keep the message: %w", err)
	}
	m.record(rec)
	m.pushNote(m.heldNote(h))
	return proto.SendResult{Status: "delivered", ID: msg.ID, Thread: msg.Thread, To: msg.To}, nil
}

func orNewID(id string) string {
	if id == "" {
		return proto.NewID(6)
	}
	return id
}

func orNewThread(thread string) string {
	if thread = proto.CleanThread(thread); thread == "" {
		return proto.NewID(6)
	}
	return thread
}
