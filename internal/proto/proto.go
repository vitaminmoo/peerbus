// Package proto defines the wire protocol between peerbus clients (channel
// servers, the CLI) and the broker. Every frame is one JSON object over a
// WebSocket; the "type" field selects the frame.
package proto

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

const (
	Version = 1
	MaxText = 64 * 1024
)

type Resource struct {
	Kind   string `json:"kind"` // mcp, program, repo, env, file, note, ...
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	Source string `json:"source,omitempty"` // auto, config, agent
}

// SessionInfo is what a session reports about itself.
type SessionInfo struct {
	SID          string     `json:"sid"` // Claude session id when known
	Name         string     `json:"name"`
	Host         string     `json:"host"`
	Account      string     `json:"account"` // Claude config dir label
	Cwd          string     `json:"cwd"`
	Status       string     `json:"status"` // idle, busy, shell, unknown...
	Resources    []Resource `json:"resources"`
	ShareDetails bool       `json:"shareDetails"` // show cwd/account to other users
}

// PeerView is what the broker shows about a session.
type PeerView struct {
	Address   string     `json:"address"` // user/name#ref, usable as a send target
	User      string     `json:"user"`
	Name      string     `json:"name"`
	Ref       string     `json:"ref"`
	Host      string     `json:"host"`
	Status    string     `json:"status"` // includes "offline"
	LastSeen  int64      `json:"lastSeen"`
	Foreign   bool       `json:"foreign"`
	Cwd       string     `json:"cwd,omitempty"`
	Account   string     `json:"account,omitempty"`
	Resources []Resource `json:"resources"`
}

type Message struct {
	ID       string `json:"id"`
	Thread   string `json:"thread"`
	From     string `json:"from"` // address of the sender
	FromUser string `json:"fromUser"`
	FromSID  string `json:"fromSid,omitempty"` // empty for CLI senders
	To       string `json:"to"`
	Text     string `json:"text"`
	SentAt   int64  `json:"sentAt"`
}

// HeldView is a third-party message waiting for its recipient's approval.
type HeldView struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Routed bool   `json:"routed"` // sender addressed the user; a human picks the session
	HeldAt int64  `json:"heldAt"`
	Text   string `json:"text"`
}

// Frame is the union of every frame; unused fields stay empty.
type Frame struct {
	Type string `json:"type"`
	Req  string `json:"req,omitempty"`

	// hello
	V       int          `json:"v,omitempty"`
	Token   string       `json:"token,omitempty"`
	Kind    string       `json:"kind,omitempty"` // hello: session|control; notice: held|status|idle
	Session *SessionInfo `json:"session,omitempty"`
	Watch   bool         `json:"watch,omitempty"`

	// send / approve / subscribe_idle
	To     string `json:"to,omitempty"`
	Text   string `json:"text,omitempty"`
	Thread string `json:"thread,omitempty"`
	ID     string `json:"id,omitempty"`
	Peer   string `json:"peer,omitempty"`

	// broker -> client
	User    string            `json:"user,omitempty"`
	Address string            `json:"address,omitempty"`
	OK      bool              `json:"ok,omitempty"`
	Error   string            `json:"error,omitempty"`
	Data    any               `json:"data,omitempty"`
	Msg     *Message          `json:"msg,omitempty"`
	Foreign bool              `json:"foreign,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`
	Held    *HeldView         `json:"held,omitempty"`
}

// Frame types.
const (
	THello         = "hello"
	TUpdate        = "update"
	TList          = "list"
	TSend          = "send"
	TSubscribeIdle = "subscribe_idle"
	TInbox         = "inbox"
	TApprove       = "approve"
	TDeny          = "deny"

	TWelcome = "welcome"
	TResult  = "result"
	TDeliver = "deliver"
	TNotice  = "notice"
	THeld    = "held"
	TError   = "error"
)

// Result data for a send.
type SendResult struct {
	Status string `json:"status"` // delivered, queued, held
	ID     string `json:"id"`
	Thread string `json:"thread"`
	To     string `json:"to"`
}

func NewID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

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
