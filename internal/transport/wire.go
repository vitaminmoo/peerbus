// Package transport connects peerbus sessions to each other over QUIC
// (TLS 1.3 with pinned Ed25519 keys), preferring the most direct path:
// same machine, then LAN or public addresses found by simultaneous hole
// punching, then a relay.
//
// A rendezvous Server does three things, all keyed by values only paired
// peers know: it tells clients their public address, lists the session
// handles registered under a meeting point to a client that names that
// point, and forwards signaling and relayed datagrams between registered
// handles. It never lists anything to someone who doesn't already know the
// point, and never forwards to a handle that isn't currently registered.
//
// Control traffic shares each node's UDP socket with QUIC: every control
// packet starts with a byte below 0x40, a bit QUIC always sets (RFC 9443).
package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const (
	pControl  = 0x01 // + JSON ctl
	pRelay    = 0x02 // + 32-byte handle + datagram (to server: target; from server: sender)
	pPunch    = 0x03 // + 16-byte nonce + 32-byte sender handle
	pPunchAck = 0x04 // + 16-byte nonce + 32-byte sender handle

	alpn = "peerbus/1"

	regTTL       = 30 * time.Second
	regRefresh   = 10 * time.Second
	maxPoints    = 256
	maxCtlPacket = 8 * 1024
)

// Handle names one registered session. It is the SHA-256 of a registration
// key that only the session and the server see, so knowing a handle lets you
// signal it but not take over its registration.
type Handle [32]byte

// Point is a meeting point: an unguessable value both sides of a pairing
// derive from their shared secret.
type Point [32]byte

func handleOf(key []byte) Handle { return Handle(sha256.Sum256(key)) }

func (h Handle) String() string { return hex.EncodeToString(h[:4]) }
func (p Point) String() string  { return hex.EncodeToString(p[:4]) }

func (h Handle) MarshalText() ([]byte, error)  { return hexText(h[:]), nil }
func (p Point) MarshalText() ([]byte, error)   { return hexText(p[:]), nil }
func (h *Handle) UnmarshalText(b []byte) error { return unhexText(h[:], b) }
func (p *Point) UnmarshalText(b []byte) error  { return unhexText(p[:], b) }

func hexText(b []byte) []byte {
	out := make([]byte, hex.EncodedLen(len(b)))
	hex.Encode(out, b)
	return out
}

func unhexText(dst, src []byte) error {
	if hex.DecodedLen(len(src)) != len(dst) {
		return fmt.Errorf("want %d hex bytes, got %d chars", len(dst), len(src))
	}
	_, err := hex.Decode(dst, src)
	return err
}

// ctl is every control message; T selects the kind.
//
//	client -> server: reg{Key, Points}, query{Req, Point}, sig{Req, To, Payload}
//	server -> client: regok{Handle, Observed}, peers{Req, Point, Handles}, sig{From, Payload}, err{Req, Msg}
type ctl struct {
	T        string          `json:"t"`
	Req      uint32          `json:"req,omitempty"`
	Key      string          `json:"key,omitempty"`
	Points   []Point         `json:"points,omitempty"`
	Point    *Point          `json:"point,omitempty"`
	Handle   *Handle         `json:"handle,omitempty"`
	Handles  []Handle        `json:"handles,omitempty"`
	To       *Handle         `json:"to,omitempty"`
	From     *Handle         `json:"from,omitempty"`
	Observed string          `json:"observed,omitempty"`
	Msg      string          `json:"msg,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

func ctlPacket(c ctl) []byte {
	b, _ := json.Marshal(c)
	return append([]byte{pControl}, b...)
}

func relayPacket(h Handle, data []byte) []byte {
	b := make([]byte, 0, 33+len(data))
	b = append(b, pRelay)
	b = append(b, h[:]...)
	return append(b, data...)
}

func punchPacket(kind byte, nonce [16]byte, h Handle) []byte {
	b := make([]byte, 0, 49)
	b = append(b, kind)
	b = append(b, nonce[:]...)
	return append(b, h[:]...)
}

// offer/answer payloads carried in sig messages.
type sigPayload struct {
	Role    string   `json:"role"` // offer | answer
	Nonce   string   `json:"nonce"`
	Machine string   `json:"machine"` // loopback candidates only count on the same machine
	Cands   []string `json:"cands"`
}
