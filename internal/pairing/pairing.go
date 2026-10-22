// Package pairing turns a pasted invite code into a pinned peer on both
// sides.
//
// An invite carries the inviter's public key and a one-time secret. The
// inviter waits at a meeting point derived from the secret; the redeemer
// connects there over the transport, pinning the inviter's key from the
// code, and proves it holds the secret with an HMAC over the TLS exporter,
// which binds the proof to this one connection. The inviter then sends a
// fresh pair secret (or, when pairing your own device, your identity key,
// self secret and peer list), and both sides record each other.
package pairing

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/transport"
)

const (
	codePrefix    = "pb1-"
	inviteVersion = 1
	flagSelf      = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Invite is what an invite code encodes.
type Invite struct {
	Self    bool // pairs one of your own devices
	Key     ed25519.PublicKey
	Secret  identity.Secret
	Expires time.Time
	Server  string // rendezvous host:port hint; empty means the redeemer's default
}

func (i Invite) Encode() string {
	b := []byte{inviteVersion, 0}
	if i.Self {
		b[1] |= flagSelf
	}
	b = append(b, i.Key...)
	b = append(b, i.Secret[:]...)
	b = binary.BigEndian.AppendUint32(b, uint32(i.Expires.Unix()))
	b = append(b, byte(len(i.Server)))
	b = append(b, i.Server...)
	sum := sha256.Sum256(b)
	b = append(b, sum[:2]...) // catches copy-paste damage
	return codePrefix + strings.ToLower(b32.EncodeToString(b))
}

func Decode(code string) (Invite, error) {
	code = strings.Join(strings.Fields(code), "")
	if !strings.HasPrefix(code, codePrefix) {
		return Invite{}, errors.New("not a peerbus invite (should start with pb1-)")
	}
	b, err := b32.DecodeString(strings.ToUpper(code[len(codePrefix):]))
	if err != nil || len(b) < 2+32+32+4+1+2 {
		return Invite{}, errors.New("invite code is damaged")
	}
	body, check := b[:len(b)-2], b[len(b)-2:]
	if sum := sha256.Sum256(body); sum[0] != check[0] || sum[1] != check[1] {
		return Invite{}, errors.New("invite code is damaged (checksum mismatch)")
	}
	if body[0] != inviteVersion {
		return Invite{}, fmt.Errorf("invite version %d isn't supported", body[0])
	}
	inv := Invite{Self: body[1]&flagSelf != 0, Key: ed25519.PublicKey(append([]byte(nil), body[2:34]...))}
	copy(inv.Secret[:], body[34:66])
	inv.Expires = time.Unix(int64(binary.BigEndian.Uint32(body[66:70])), 0)
	n := int(body[70])
	if len(body) != 71+n {
		return Invite{}, errors.New("invite code is damaged")
	}
	inv.Server = string(body[71:])
	return inv, nil
}

type Options struct {
	Server   string // rendezvous host:port when the invite doesn't name one
	STUN     []string
	LocalDir string
	Log      *log.Logger
}

type Result struct {
	Name        string // what you call them
	Fingerprint string
	Self        bool
}

// NewInvite creates an invite for this identity.
func NewInvite(store identity.Store, self bool, ttl time.Duration, server string) (Invite, error) {
	key, err := store.Identity()
	if err != nil {
		return Invite{}, err
	}
	return Invite{
		Self:    self,
		Key:     key.Public().(ed25519.PublicKey),
		Secret:  identity.NewSecret(),
		Expires: time.Now().Add(ttl).Truncate(time.Second),
		Server:  server,
	}, nil
}

type hello struct {
	V     int    `json:"v"`
	Proof string `json:"proof"`
	Name  string `json:"name"` // what the redeemer suggests being called
}

type reply struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Name   string          `json:"name,omitempty"` // what the inviter suggests being called
	Secret identity.Secret `json:"secret,omitempty"`
	Self   *selfBundle     `json:"self,omitempty"`
}

type selfBundle struct {
	Identity string          `json:"identity"` // seed hex
	Peers    *identity.Peers `json:"peers"`
}

type ack struct {
	OK bool `json:"ok"`
}

func proof(c *transport.Conn, secret identity.Secret) ([]byte, error) {
	cs := c.ConnectionState().TLS
	ekm, err := cs.ExportKeyingMaterial("peerbus pair v1", nil, 32)
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, secret[:])
	m.Write(ekm)
	return m.Sum(nil), nil
}

func selfName(ps *identity.Peers) string {
	if ps.Self.Name != "" {
		return ps.Self.Name
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	h, _ := os.Hostname()
	return h
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func readJSON(r *bufio.Reader, v any) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Host waits for inv to be redeemed once, then records the new peer. name
// overrides what the redeemer suggests being called.
func Host(ctx context.Context, store identity.Store, inv Invite, name string, opts Options) (Result, error) {
	key, err := store.Identity()
	if err != nil {
		return Result{}, err
	}
	if !key.Public().(ed25519.PublicKey).Equal(inv.Key) {
		return Result{}, errors.New("invite belongs to a different identity")
	}
	ctx, cancel := context.WithDeadline(ctx, inv.Expires)
	defer cancel()

	var used atomic.Bool
	done := make(chan Result, 1)
	host := func(c *transport.Conn) {
		defer c.CloseWithError(0, "")
		sctx, scancel := context.WithTimeout(ctx, 20*time.Second)
		defer scancel()
		s, err := c.AcceptStream(sctx)
		if err != nil {
			return
		}
		r := bufio.NewReader(s)
		defer finish(s, r)
		var h hello
		if readJSON(r, &h) != nil {
			return
		}
		want, err := proof(c, inv.Secret)
		got, _ := hex.DecodeString(h.Proof)
		if err != nil || !hmac.Equal(want, got) {
			writeJSON(s, reply{Error: "wrong invite secret"})
			return
		}
		if !used.CompareAndSwap(false, true) {
			writeJSON(s, reply{Error: "invite already used"})
			return
		}
		ps, err := store.Load()
		if err != nil {
			used.Store(false)
			writeJSON(s, reply{Error: "inviter couldn't read its peers"})
			return
		}
		rep := reply{OK: true, Name: selfName(ps)}
		var pairSecret identity.Secret
		if inv.Self {
			rep.Self = &selfBundle{Identity: hex.EncodeToString(key.Seed()), Peers: ps}
		} else {
			pairSecret = identity.NewSecret()
			rep.Secret = pairSecret
		}
		var a ack
		if writeJSON(s, rep) != nil || readJSON(r, &a) != nil || !a.OK {
			used.Store(false) // let a retry through
			return
		}
		res := Result{Fingerprint: identity.Fingerprint(c.PeerKey), Self: inv.Self}
		if inv.Self {
			res.Name = "(your device)"
		} else {
			err = store.Update(func(p *identity.Peers) error {
				n := name
				if n == "" {
					n = h.Name
				}
				res.Name = p.UniqueName(n)
				p.Peers = append(p.Peers, &identity.Peer{
					Name: res.Name, Key: identity.Key(c.PeerKey), Secret: pairSecret,
					Enabled: true, Perms: identity.DefaultPerms(), PairedAt: time.Now().UTC(),
				})
				return nil
			})
			if err != nil {
				return
			}
		}
		select {
		case done <- res:
		default:
		}
	}

	node, err := transport.Start(ctx, transport.Config{
		Key:      key,
		Server:   orDefault(inv.Server, opts.Server),
		STUN:     opts.STUN,
		LocalDir: opts.LocalDir,
		Log:      opts.Log,
		// Anyone may connect; the invite proof decides.
		Allow:  func(ed25519.PublicKey) bool { return true },
		OnConn: host,
	})
	if err != nil {
		return Result{}, err
	}
	defer node.Close()
	node.SetPoints([]transport.Point{inv.Secret.InvitePoint()})

	select {
	case res := <-done:
		return res, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{}, errors.New("invite expired before anyone redeemed it")
		}
		return Result{}, ctx.Err()
	}
}

// finish closes our side of s and waits for the peer to close theirs, so a
// last message isn't lost to the connection closing first.
func finish(s interface {
	io.Closer
	SetReadDeadline(time.Time) error
}, r io.Reader) {
	s.Close()
	s.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.Copy(io.Discard, r)
}

func orDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

// Redeem pairs with the inviter of code. name overrides what the inviter
// suggests being called. Pairing your own device replaces this machine's
// identity, so it refuses when this store already has peers unless force.
func Redeem(ctx context.Context, store identity.Store, code, name string, force bool, opts Options) (Result, error) {
	inv, err := Decode(code)
	if err != nil {
		return Result{}, err
	}
	if time.Now().After(inv.Expires) {
		return Result{}, fmt.Errorf("invite expired at %s", inv.Expires.Format(time.Kitchen))
	}
	ps, err := store.Load()
	if err != nil {
		return Result{}, err
	}
	var key ed25519.PrivateKey
	if inv.Self {
		if store.HasIdentity() && len(ps.Peers) > 0 && !force {
			return Result{}, errors.New("this machine already has an identity with peers; pairing it as one of your devices would replace them (use -force)")
		}
		_, key, _ = ed25519.GenerateKey(nil) // throwaway; we adopt the received identity
	} else {
		if key, err = store.Identity(); err != nil {
			return Result{}, err
		}
		if key.Public().(ed25519.PublicKey).Equal(inv.Key) {
			return Result{}, errors.New("that's your own invite; redeem it on the other side")
		}
		if p := ps.ByKey(inv.Key); p != nil {
			return Result{}, fmt.Errorf("already paired with them as %q", p.Name)
		}
	}

	ctx, cancel := context.WithDeadline(ctx, inv.Expires)
	defer cancel()
	node, err := transport.Start(ctx, transport.Config{
		Key:      key,
		Server:   orDefault(inv.Server, opts.Server),
		STUN:     opts.STUN,
		LocalDir: opts.LocalDir,
		Log:      opts.Log,
		Allow:    func(ed25519.PublicKey) bool { return false }, // we only dial out
	})
	if err != nil {
		return Result{}, err
	}
	defer node.Close()
	if orDefault(inv.Server, opts.Server) != "" {
		if err := node.WaitRegistered(ctx); err != nil {
			return Result{}, fmt.Errorf("rendezvous server: %w", err)
		}
	}

	var peer transport.Peer
	found := false
	for !found {
		peers, _ := node.Lookup(ctx, inv.Secret.InvitePoint())
		if len(peers) > 0 {
			peer, found = peers[0], true
			break
		}
		select {
		case <-ctx.Done():
			return Result{}, errors.New("the inviter isn't waiting (is `peerbus invite` still running?)")
		case <-time.After(500 * time.Millisecond):
		}
	}
	c, err := node.Dial(ctx, peer, func(k ed25519.PublicKey) bool { return k.Equal(inv.Key) })
	if err != nil {
		return Result{}, err
	}
	defer c.CloseWithError(0, "")
	s, err := c.OpenStreamSync(ctx)
	if err != nil {
		return Result{}, err
	}
	defer s.Close()
	pr, err := proof(c, inv.Secret)
	if err != nil {
		return Result{}, err
	}
	if err := writeJSON(s, hello{V: 1, Proof: hex.EncodeToString(pr), Name: selfName(ps)}); err != nil {
		return Result{}, err
	}
	r := bufio.NewReader(s)
	var rep reply
	if err := readJSON(r, &rep); err != nil {
		return Result{}, fmt.Errorf("inviter closed the connection: %w", err)
	}
	if !rep.OK {
		return Result{}, errors.New(rep.Error)
	}

	res := Result{Fingerprint: identity.Fingerprint(inv.Key), Self: inv.Self}
	if inv.Self {
		if rep.Self == nil || rep.Self.Peers == nil {
			return Result{}, errors.New("inviter sent no identity")
		}
		seed, err := hex.DecodeString(rep.Self.Identity)
		if err != nil || len(seed) != ed25519.SeedSize {
			return Result{}, errors.New("inviter sent a bad identity")
		}
		idKey := ed25519.NewKeyFromSeed(seed)
		if !idKey.Public().(ed25519.PublicKey).Equal(inv.Key) {
			return Result{}, errors.New("inviter sent an identity that doesn't match the invite")
		}
		if err := store.ReplaceIdentity(idKey); err != nil {
			return Result{}, err
		}
		if err := store.Update(func(p *identity.Peers) error { *p = *rep.Self.Peers; return nil }); err != nil {
			return Result{}, err
		}
		res.Name = "(your device)"
	} else {
		if rep.Secret.IsZero() {
			return Result{}, errors.New("inviter sent no pair secret")
		}
		err = store.Update(func(p *identity.Peers) error {
			n := name
			if n == "" {
				n = rep.Name
			}
			res.Name = p.UniqueName(n)
			p.Peers = append(p.Peers, &identity.Peer{
				Name: res.Name, Key: identity.Key(inv.Key), Secret: rep.Secret,
				Enabled: true, Perms: identity.DefaultPerms(), PairedAt: time.Now().UTC(),
			})
			return nil
		})
		if err != nil {
			return Result{}, err
		}
	}
	if err := writeJSON(s, ack{OK: true}); err != nil {
		return Result{}, err
	}
	finish(s, r)
	return res, nil
}
