// Package proto defines the frames peerbus sessions exchange with each other
// over a QUIC stream: one JSON object per line, the "type" field selecting
// the frame. The same frames serve the CLI when it talks to a session on
// this machine.
package proto

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

const (
	Version = 2
	MaxText = 64 * 1024
)

type Resource struct {
	Kind   string `json:"kind"` // mcp, program, repo, env, file, note, ...
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	Source string `json:"source,omitempty"` // auto, config, agent
}

// SessionInfo is what a session says about itself. What a peer receives is
// filtered by the sender's permissions for that peer.
type SessionInfo struct {
	SID       string     `json:"sid"` // Claude session id when known
	Name      string     `json:"name"`
	Host      string     `json:"host"`
	Account   string     `json:"account,omitempty"` // Claude config dir label
	Cwd       string     `json:"cwd,omitempty"`
	Status    string     `json:"status"` // idle, busy, shell, unknown...
	Resources []Resource `json:"resources,omitempty"`
	// Started is when the Claude Code process started (unix ms), which a
	// resume in a second process doesn't share. PID is that process, for
	// your own sessions only.
	Started int64 `json:"started,omitempty"`
	PID     int   `json:"pid,omitempty"`
}

// PeerView is one reachable session as seen from here.
type PeerView struct {
	Address   string     `json:"address"` // user/name#ref, usable as a send target
	User      string     `json:"user"`    // your name for the peer; your own name for your sessions
	Name      string     `json:"name"`
	Ref       string     `json:"ref"`
	Host      string     `json:"host"`
	Status    string     `json:"status"`
	Foreign   bool       `json:"foreign"`
	Via       string     `json:"via"` // local, direct, relay
	Cwd       string     `json:"cwd,omitempty"`
	Account   string     `json:"account,omitempty"`
	Resources []Resource `json:"resources,omitempty"`
}

type Message struct {
	ID       string `json:"id"`
	Thread   string `json:"thread"`
	From     string `json:"from"`     // sender's address, from the recipient's point of view
	FromUser string `json:"fromUser"` // recipient's name for the sender
	FromSID  string `json:"fromSid,omitempty"`
	To       string `json:"to"`
	Text     string `json:"text"`
	SentAt   int64  `json:"sentAt"`
}

// Frame is the union of every frame; unused fields stay empty.
type Frame struct {
	Type string `json:"type"`
	Req  string `json:"req,omitempty"`

	// hello / update
	V       int          `json:"v,omitempty"`
	Kind    string       `json:"kind,omitempty"` // hello/update: session|control|hidden; notice: held|status|idle
	Handle  string       `json:"handle,omitempty"`
	Session *SessionInfo `json:"session,omitempty"`

	// send / subscribe_idle / approve / deny
	To     string `json:"to,omitempty"`
	Text   string `json:"text,omitempty"`
	Thread string `json:"thread,omitempty"`
	ID     string `json:"id,omitempty"`

	// result / notice
	OK    bool              `json:"ok,omitempty"`
	Error string            `json:"error,omitempty"`
	Data  any               `json:"data,omitempty"`
	Meta  map[string]string `json:"meta,omitempty"`
}

// Frame types.
const (
	THello         = "hello"
	TUpdate        = "update"
	TSend          = "send"
	TSubscribeIdle = "subscribe_idle"
	TNotice        = "notice"
	TResult        = "result"

	// Between your own sessions on different machines: the sender's whole
	// peers.json in Data, for the receiver to merge.
	TPeers = "peers"

	// From the CLI to a session on this machine only.
	TList = "list"

	// From your own CLI or TUI, on any of your machines, to a session:
	// list, approve or deny that machine's held messages. Approve may name
	// another of your sessions (To, a session id) to deliver to.
	TInbox   = "inbox"
	TApprove = "approve"
	TDeny    = "deny"

	// Between your own sessions: a held message, approved and handed to the
	// session it was routed to. Data is the held message.
	TDeliver = "deliver"

	// From your own CLI or TUI to a session: the messages that machine's
	// sessions sent and received, newest first, long ones cut short. With ID
	// (and Meta["from"]), that one message in full.
	THistory = "history"
)

// Hello kinds.
const (
	KindSession = "session"
	KindControl = "control" // the CLI; never listed
	KindHidden  = "hidden"  // a session the peer may not see or address
)

// SendResult says what happened to a message.
type SendResult struct {
	Status string `json:"status"` // delivered, held, ask
	ID     string `json:"id"`
	Thread string `json:"thread"`
	To     string `json:"to"`
}

func NewID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Ref is the short form of a session id used in addresses.
func Ref(sid string) string {
	s := strings.ReplaceAll(sid, "-", "")
	if len(s) > 6 {
		s = s[:6]
	}
	return s
}

func Address(user, name, sid string) string {
	return user + "/" + name + "#" + Ref(sid)
}

// CleanThread keeps thread ids to something safe to put in channel metadata.
func CleanThread(t string) string {
	t = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return -1
	}, t)
	if len(t) > 32 {
		t = t[:32]
	}
	return t
}

// HeldView is a waiting message as the CLI shows it.
type HeldView struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	To       string `json:"to"`
	Thread   string `json:"thread"`
	Peer     string `json:"peer"`             // our name for the other person
	Target   string `json:"target,omitempty"` // session id it's for (inbound) or from (outbound)
	Routed   bool   `json:"routed"`           // sent to you as a person; you pick the session
	Outbound bool   `json:"outbound"`         // yours, waiting for your own go-ahead
	HeldAt   int64  `json:"heldAt"`
	Text     string `json:"text"`
}

// InboxView is one machine's held messages, as a session there reports
// them to your own control clients.
type InboxView struct {
	Machine string     `json:"machine"` // its name
	InboxID string     `json:"inboxId"` // the same from every session sharing this inbox
	Held    []HeldView `json:"held"`
}

// HistoryEntry is a message one of your sessions sent or received, or a
// later change in what happened to it (Status, and To once a message held
// for you as a person is routed). Entries with the same From and ID are one
// message.
type HistoryEntry struct {
	ID     string `json:"id"`
	Thread string `json:"thread,omitempty"`
	From   string `json:"from"`
	To     string `json:"to,omitempty"`
	Peer   string `json:"peer,omitempty"` // our name for the other person; empty between your own sessions
	Status string `json:"status"`         // delivered, held, ask (waiting on send=ask), declined, unsent (ask declined)
	At     int64  `json:"at"`
	Text   string `json:"text,omitempty"`
	Cut    bool   `json:"cut,omitempty"` // Text is only the start of the message
}

// HistoryView is one machine's message history, as a session there reports
// it to your own control clients.
type HistoryView struct {
	Machine  string         `json:"machine"`
	InboxID  string         `json:"inboxId"` // the same from every session sharing this history
	Messages []HistoryEntry `json:"messages"`
}
