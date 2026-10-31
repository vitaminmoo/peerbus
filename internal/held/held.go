// Package held is what every human-facing client of peerbus does with held
// messages: gather them from every one of your machines, then approve,
// route or decline them through a session on the machine holding each.
// `peerbus tui` and the Android app both use it, so they see and act on
// the same things.
package held

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/vitaminmoo/peerbus/internal/mesh"
	"github.com/vitaminmoo/peerbus/internal/proto"
)

// AskTimeout bounds how long one session gets to answer.
const AskTimeout = 4 * time.Second

type Section int

const (
	ToRead     Section = iota // sent to you, user/human: read, reply, dismiss
	ForSession                // held for a particular session
	ForYou                    // sent to you as a person: pick a session
	Yours                     // ours, waiting on send=ask
)

var SectionTitles = [...]string{"to you · read", "held for a session", "for you · pick a session", "yours · waiting to send"}

// Item is one held message and where it waits.
type Item struct {
	View    proto.HeldView
	Machine string
	Inbox   string       // which inbox holds it
	Via     []*mesh.Link // sessions sharing that inbox
}

func (it Item) Section() Section {
	switch {
	case it.View.Note:
		return ToRead
	case it.View.Outbound:
		return Yours
	case it.View.Routed:
		return ForYou
	}
	return ForSession
}

// Session is one of your sessions, on any machine.
type Session struct {
	Link *mesh.Link
	Info proto.SessionInfo
	Addr string
}

// Snapshot is every held message on every machine with a session online,
// and those sessions.
type Snapshot struct {
	Items    []Item
	Sessions []Session
	Machines int
}

// AskAll sends f to every session in own and collects the answers by
// inbox: sessions on one machine share one. The answer from the newest
// peerbus there wins (an older one misreads what it doesn't know), and
// byInbox lists the sessions that can act on each, newest first.
func AskAll[T any](ctx context.Context, own []*mesh.Link, f proto.Frame, inboxOf func(T) string, versionOf func(T) int) (views map[string]T, byInbox map[string][]*mesh.Link) {
	type answer struct {
		link *mesh.Link
		view T
	}
	answers := map[string][]answer{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, l := range own {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, AskTimeout)
			defer cancel()
			var v T
			if err := l.Request(ctx, f, &v); err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			id := inboxOf(v)
			answers[id] = append(answers[id], answer{l, v})
		}()
	}
	wg.Wait()
	views, byInbox = map[string]T{}, map[string][]*mesh.Link{}
	for id, as := range answers {
		// Newest first; between equals, the same session every time, so
		// what's shown doesn't change with who answered first.
		sort.Slice(as, func(i, j int) bool {
			if vi, vj := versionOf(as[i].view), versionOf(as[j].view); vi != vj {
				return vi > vj
			}
			return as[i].link.Address() < as[j].link.Address()
		})
		views[id] = as[0].view
		for _, a := range as {
			byInbox[id] = append(byInbox[id], a.link)
		}
	}
	return views, byInbox
}

// Fetch asks every session of ours for its inbox.
func Fetch(ctx context.Context, m *mesh.Mesh) Snapshot {
	own := m.Own()
	var sessions []Session
	for _, l := range own {
		sessions = append(sessions, Session{Link: l, Info: l.Info(), Addr: l.Address()})
	}
	views, byInbox := AskAll(ctx, own, proto.Frame{Type: proto.TInbox},
		func(v proto.InboxView) string { return v.InboxID }, inboxVersion)
	var items []Item
	for id, v := range views {
		for _, h := range v.Held {
			items = append(items, Item{View: h, Machine: v.Machine, Inbox: id, Via: byInbox[id]})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if a, b := items[i].Section(), items[j].Section(); a != b {
			return a < b
		}
		return items[i].View.HeldAt > items[j].View.HeldAt
	})
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Info.Host != sessions[j].Info.Host {
			return sessions[i].Info.Host < sessions[j].Info.Host
		}
		return sessions[i].Addr < sessions[j].Addr
	})
	return Snapshot{Items: items, Sessions: sessions, Machines: len(views)}
}

// inboxVersion is how well a session reads its machine's inbox. Sessions
// from before views carried a version still show it: one that reports a
// note or a test message reads them, which the oldest can't.
func inboxVersion(v proto.InboxView) int {
	if v.V > 0 {
		return v.V
	}
	for _, h := range v.Held {
		if h.Note || h.Fake {
			return 1
		}
	}
	return 0
}

// Find is the item with this held message id.
func (s Snapshot) Find(id string) (Item, bool) {
	for _, it := range s.Items {
		if it.View.ID == id {
			return it, true
		}
	}
	return Item{}, false
}

// Session is the session with this id.
func (s Snapshot) Session(sid string) (Session, bool) {
	for _, se := range s.Sessions {
		if se.Info.SID == sid {
			return se, true
		}
	}
	return Session{}, false
}

// Label names a session for people: name@machine, or (gone).
func (s Snapshot) Label(sid string) string {
	if se, ok := s.Session(sid); ok {
		return se.Info.Name + "@" + se.Info.Host
	}
	return "(gone)"
}

// Action is a decision ready to send to the machine holding the message.
// Approve and Deny check what they can up front, so a client can say
// what's wrong straight away; Run does the network part.
type Action struct {
	via   *mesh.Link
	frame proto.Frame
	item  Item
}

// Approve prepares delivering it where it was sent, or to the session to
// when routing, or, for one of ours, sending it.
func (s Snapshot) Approve(it Item, to *Session) (Action, error) {
	if it.View.Note {
		return Action{}, errors.New("that's for you to read; reply or dismiss it")
	}
	var via *mesh.Link
	frame := proto.Frame{Type: proto.TApprove, ID: it.View.ID}
	switch {
	case it.View.Outbound:
		se, ok := s.Session(it.View.Target)
		if !ok {
			return Action{}, errors.New("the session that wrote this isn't running any more; decline it")
		}
		via = se.Link
	case to != nil && to.Link != nil:
		frame.To = to.Info.SID
	default:
		if it.Section() == ForYou {
			return Action{}, errors.New("pick a session to deliver it to")
		}
		if _, ok := s.Session(it.View.Target); !ok {
			return Action{}, errors.New("that session is gone; route it to another")
		}
		frame.To = it.View.Target
	}
	if via == nil {
		via = PickVia(it)
	}
	if via == nil {
		return Action{}, errors.New("no session on " + it.Machine + " is online to hand it over")
	}
	return Action{via: via, frame: frame, item: it}, nil
}

// Deny prepares declining it; the sender is told.
func (s Snapshot) Deny(it Item) (Action, error) {
	via := PickVia(it)
	if via == nil {
		return Action{}, errors.New("no session on " + it.Machine + " is online to decline it")
	}
	return Action{via: via, frame: proto.Frame{Type: proto.TDeny, ID: it.View.ID}, item: it}, nil
}

// Run carries out the action and says what happened.
func (a Action) Run(ctx context.Context) (string, error) {
	if a.frame.Type == proto.TDeny {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := a.via.Request(ctx, a.frame, nil); err != nil {
			return "", err
		}
		switch {
		case a.item.View.Note:
			return "dismissed", nil
		case a.item.View.Fake:
			return "declined (a test message)", nil
		}
		return "declined; " + a.item.View.Peer + " is told", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var res proto.SendResult
	if err := a.via.Request(ctx, a.frame, &res); err != nil {
		return "", err
	}
	if a.item.View.Outbound {
		return "sent to " + res.To, nil
	}
	return "delivered to " + res.To, nil
}

// PickVia chooses the session on the holding machine to act through: the
// newest peerbus there (Via's order), which reads the inbox right. It
// needn't be the message's own session; approving names that one.
func PickVia(it Item) *mesh.Link {
	if len(it.Via) > 0 {
		return it.Via[0]
	}
	return nil
}
