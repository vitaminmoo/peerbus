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

	// From the CLI to a session on this machine only.
	TList    = "list"
	TApprove = "approve"
	TDeny    = "deny"
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
	Routed   bool   `json:"routed"`   // sent to you as a person; you pick the session
	Outbound bool   `json:"outbound"` // yours, waiting for your own go-ahead
	HeldAt   int64  `json:"heldAt"`
	Text     string `json:"text"`
}
