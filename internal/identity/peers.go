package identity

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"
)

// Peers is peers.json: your own devices' shared state and everyone you've
// paired with.
type Peers struct {
	Self  Self    `json:"self"`
	Peers []*Peer `json:"peers"`
}

type Self struct {
	// Name is what you suggest others call you when pairing.
	Name string `json:"name,omitempty"`
	// Secret is shared by all your devices; your sessions meet under it.
	Secret Secret `json:"secret"`
}

type Peer struct {
	Name     string    `json:"name"` // your name for them
	Key      Key       `json:"key"`
	Secret   Secret    `json:"secret"`
	Enabled  bool      `json:"enabled"`
	Perms    Perms     `json:"perms"`
	PairedAt time.Time `json:"pairedAt"`
}

// Perms is what a peer may do; the receiving side enforces it.
type Perms struct {
	// Deliver is "hold" (their messages wait for your approval, which also
	// opens the thread for a while) or "auto".
	Deliver string `json:"deliver"`
	// Send is "auto" or "ask" (your sessions' messages to them wait for your
	// approval).
	Send string `json:"send"`
	// Sessions are globs over your session names that they may address.
	Sessions []string `json:"sessions"`
	// ShowDetails lets them see your sessions' cwd and Claude account.
	ShowDetails bool `json:"showDetails"`
	// ShowResources lets them see what your sessions advertise.
	ShowResources bool `json:"showResources"`
}

// DefaultPerms are what a new friend gets.
func DefaultPerms() Perms {
	return Perms{Deliver: "hold", Send: "auto", Sessions: []string{"*"}, ShowResources: true}
}

func (p Perms) Validate() error {
	if p.Deliver != "hold" && p.Deliver != "auto" {
		return fmt.Errorf("deliver must be hold or auto, not %q", p.Deliver)
	}
	if p.Send != "ask" && p.Send != "auto" {
		return fmt.Errorf("send must be ask or auto, not %q", p.Send)
	}
	for _, g := range p.Sessions {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("bad session glob %q", g)
		}
	}
	return nil
}

// MaySee reports whether a session name matches the peer's Sessions globs.
func (p Perms) MaySee(session string) bool {
	for _, g := range p.Sessions {
		if ok, _ := path.Match(g, session); ok {
			return true
		}
	}
	return false
}

// ByKey finds the peer with a key.
func (ps *Peers) ByKey(k ed25519.PublicKey) *Peer {
	for _, p := range ps.Peers {
		if p.Key.Equal(k) {
			return p
		}
	}
	return nil
}

func (ps *Peers) ByName(name string) *Peer {
	for _, p := range ps.Peers {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// UniqueName returns name, or name-2, name-3... if it's taken.
func (ps *Peers) UniqueName(name string) string {
	name = CleanName(name)
	if ps.ByName(name) == nil {
		return name
	}
	for i := 2; ; i++ {
		n := fmt.Sprintf("%s-%d", name, i)
		if ps.ByName(n) == nil {
			return n
		}
	}
}

// CleanName makes a name safe for addresses like name/session.
func CleanName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, strings.TrimSpace(name))
	if name == "" {
		return "peer"
	}
	return name
}

// Load reads peers.json; a missing file is an empty store.
func (s Store) Load() (*Peers, error) {
	b, err := os.ReadFile(s.peersPath())
	if errors.Is(err, os.ErrNotExist) {
		return &Peers{}, nil
	}
	if err != nil {
		return nil, err
	}
	var p Peers
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", s.peersPath(), err)
	}
	return &p, nil
}

// ModTime is when peers.json last changed, for reloading.
func (s Store) ModTime() time.Time {
	fi, err := os.Stat(s.peersPath())
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// Update applies fn to peers.json under an exclusive lock.
func (s Store) Update(fn func(*Peers) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	p, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(p); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.peersPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.peersPath())
}

// SelfName is what your sessions are listed under and what you suggest
// being called when pairing.
func (ps *Peers) SelfName() string {
	if ps.Self.Name != "" {
		return ps.Self.Name
	}
	if u := os.Getenv("USER"); u != "" {
		return CleanName(u)
	}
	h, _ := os.Hostname()
	return CleanName(h)
}

// Enabled lists the peers whose sessions may connect.
func (ps *Peers) Enabled() []*Peer {
	var out []*Peer
	for _, p := range ps.Peers {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out
}
