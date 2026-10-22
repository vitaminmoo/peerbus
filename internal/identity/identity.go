// Package identity holds this user's key and the people they've paired with.
//
// Every device of one user shares a single Ed25519 identity key: pairing your
// own new device copies it there, so friends pin one key per person and never
// need to re-pair when you add a device. Each pairing (and the "self" group of
// your own devices) has a shared secret; meeting points at the rendezvous
// server are derived from it, so only the two sides can find each other.
package identity

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vitaminmoo/peerbus/internal/transport"
)

// Key is an Ed25519 public key that marshals as hex.
type Key ed25519.PublicKey

func (k Key) MarshalText() ([]byte, error) { return []byte(hex.EncodeToString(k)), nil }
func (k *Key) UnmarshalText(b []byte) error {
	v, err := hex.DecodeString(string(b))
	if err != nil || len(v) != ed25519.PublicKeySize {
		return fmt.Errorf("bad public key %q", b)
	}
	*k = v
	return nil
}

func (k Key) Equal(o ed25519.PublicKey) bool { return ed25519.PublicKey(k).Equal(o) }

// Fingerprint is a short, human-comparable form of a key.
func Fingerprint(k ed25519.PublicKey) string {
	sum := sha256.Sum256(k)
	h := hex.EncodeToString(sum[:8])
	return h[0:4] + " " + h[4:8] + " " + h[8:12] + " " + h[12:16]
}

// Secret is a 32-byte shared secret that marshals as hex.
type Secret [32]byte

func NewSecret() Secret {
	var s Secret
	rand.Read(s[:])
	return s
}

func (s Secret) MarshalText() ([]byte, error) { return []byte(hex.EncodeToString(s[:])), nil }
func (s *Secret) UnmarshalText(b []byte) error {
	v, err := hex.DecodeString(string(b))
	if err != nil || len(v) != 32 {
		return errors.New("bad secret")
	}
	copy(s[:], v)
	return nil
}

func (s Secret) IsZero() bool { return s == Secret{} }

const pointPeriod = time.Hour

func (s Secret) point(label string, slot int64) transport.Point {
	m := hmac.New(sha256.New, s[:])
	m.Write([]byte("peerbus " + label + " v1\x00"))
	binary.Write(m, binary.BigEndian, slot)
	var p transport.Point
	copy(p[:], m.Sum(nil))
	return p
}

// Point is where both sides look for each other right now. It rotates
// hourly, so the server can't link a pair's meetings across hours.
func (s Secret) Point(now time.Time) transport.Point {
	return s.point("rendezvous", now.Unix()/int64(pointPeriod.Seconds()))
}

// Points are what a session registers under: the previous, current and next
// hour, so peers with clocks up to an hour apart still meet.
func (s Secret) Points(now time.Time) []transport.Point {
	slot := now.Unix() / int64(pointPeriod.Seconds())
	return []transport.Point{s.point("rendezvous", slot-1), s.point("rendezvous", slot), s.point("rendezvous", slot+1)}
}

// InvitePoint is where an inviter waits for the invite to be redeemed.
func (s Secret) InvitePoint() transport.Point { return s.point("invite", 0) }

// Store is the directory holding identity.key and peers.json.
type Store struct{ Dir string }

func (s Store) keyPath() string   { return filepath.Join(s.Dir, "identity.key") }
func (s Store) peersPath() string { return filepath.Join(s.Dir, "peers.json") }
func (s Store) lockPath() string  { return filepath.Join(s.Dir, ".peers.lock") }

// Identity loads the identity key, creating it (and the self secret) on
// first use.
func (s Store) Identity() (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(s.keyPath())
	if err == nil {
		return parseKeyFile(b)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	if err := s.writeKey(key, false); err != nil {
		if errors.Is(err, os.ErrExist) { // another process won the race
			return s.Identity()
		}
		return nil, err
	}
	err = s.Update(func(p *Peers) error {
		if p.Self.Secret.IsZero() {
			p.Self.Secret = NewSecret()
		}
		return nil
	})
	return key, err
}

// HasIdentity reports whether an identity key exists yet.
func (s Store) HasIdentity() bool {
	_, err := os.Stat(s.keyPath())
	return err == nil
}

// ReplaceIdentity installs a key received while pairing one of your own
// devices.
func (s Store) ReplaceIdentity(key ed25519.PrivateKey) error { return s.writeKey(key, true) }

func parseKeyFile(b []byte) (ed25519.PrivateKey, error) {
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("identity.key is corrupt")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func (s Store) writeKey(key ed25519.PrivateKey, replace bool) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	data := []byte(hex.EncodeToString(key.Seed()) + "\n")
	if !replace {
		f, err := os.OpenFile(s.keyPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(data)
		return err
	}
	tmp := s.keyPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.keyPath())
}

// lock serializes writers of peers.json across processes.
func (s Store) lock() (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
