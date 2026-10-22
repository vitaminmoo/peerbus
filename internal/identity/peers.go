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
// paired with. Your machines keep it in sync with each other (see Merge),
// so a friend paired on one reaches all of them.
type Peers struct {
	Self  Self    `json:"self"`
	Peers []*Peer `json:"peers"`
}

type Self struct {
	// Name is what you suggest others call you when pairing.
	Name string `json:"name,omitempty"`
	// NameUpdated is when Name last changed, for syncing.
	NameUpdated time.Time `json:"nameUpdated,omitempty"`
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
	// Updated is when this entry last changed on any of your machines; the
	// latest change wins when they sync.
	Updated time.Time `json:"updated,omitempty"`
	// Removed marks a forgotten peer. The entry stays for a while so the
	// removal reaches machines that still have them.
	Removed bool `json:"removed,omitempty"`
}

// tombstoneTTL is how long a removed peer's entry is kept for syncing.
const tombstoneTTL = 90 * 24 * time.Hour

// changed is when p last changed; entries from before syncing existed
// count from when they were paired.
func (p *Peer) changed() time.Time {
	if p.Updated.IsZero() {
		return p.PairedAt
	}
	return p.Updated
}

// Touch records that p changed now.
func (p *Peer) Touch() { p.Updated = time.Now().UTC() }

// Remove forgets p everywhere: it can't connect, and the removal syncs to
// your other machines.
func (p *Peer) Remove() {
	p.Removed, p.Enabled, p.Secret = true, false, Secret{}
	p.Touch()
}

// Perms is what a peer may do; the receiving side enforces it.
type Perms struct {
	// Deliver is "hold" (their messages wait for your approval, which also
	// opens the thread for a while) or "auto".
	Deliver string `json:"deliver"`
	// Send is "auto" or "ask" (your sessions' messages to them wait for your
	// approval).
	Send string `json:"send"`
	// Sessions are globs over your session names that they may address. A
	// glob containing "/" matches "host/name" instead, so "laptop/*" means
	// every session on one machine.
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

// MaySee reports whether a session matches the peer's Sessions globs.
func (p Perms) MaySee(host, session string) bool {
	for _, g := range p.Sessions {
		s := session
		if strings.Contains(g, "/") {
			s = host + "/" + session
		}
		if ok, _ := path.Match(g, s); ok {
			return true
		}
	}
	return false
}

// ByKey finds the peer with a key.
func (ps *Peers) ByKey(k ed25519.PublicKey) *Peer {
	if p := ps.entry(k); p != nil && !p.Removed {
		return p
	}
	return nil
}

// entry finds the entry with a key, removed or not.
func (ps *Peers) entry(k ed25519.PublicKey) *Peer {
	for _, p := range ps.Peers {
		if p.Key.Equal(k) {
			return p
		}
	}
	return nil
}

func (ps *Peers) ByName(name string) *Peer {
	for _, p := range ps.Peers {
		if p.Name == name && !p.Removed {
			return p
		}
	}
	return nil
}

// Current lists the peers that haven't been removed.
func (ps *Peers) Current() []*Peer {
	var out []*Peer
	for _, p := range ps.Peers {
		if !p.Removed {
			out = append(out, p)
		}
	}
	return out
}

// Add records a newly paired peer, replacing a removed entry for the same
// key.
func (ps *Peers) Add(p *Peer) {
	p.Touch()
	if old := ps.entry(ed25519.PublicKey(p.Key)); old != nil {
		*old = *p
		return
	}
	ps.Peers = append(ps.Peers, p)
}

// Merge folds in the peer list from another of your machines: for each peer
// the most recently changed entry wins, removals included. Names that clash
// with a different peer get a suffix. It reports whether anything changed.
func (ps *Peers) Merge(o *Peers) bool {
	changed := false
	if o.Self.NameUpdated.After(ps.Self.NameUpdated) && o.Self.Name != "" {
		ps.Self.Name, ps.Self.NameUpdated = o.Self.Name, o.Self.NameUpdated
		changed = true
	}
	for _, in := range o.Peers {
		if len(in.Key) != ed25519.PublicKeySize || in.changed().IsZero() {
			continue
		}
		cp := *in
		cp.Updated = in.changed()
		cp.Perms.Sessions = append([]string(nil), in.Perms.Sessions...)
		if err := cp.Perms.Validate(); err != nil && !cp.Removed {
			continue
		}
		mine := ps.entry(ed25519.PublicKey(cp.Key))
		if mine != nil && !cp.Updated.After(mine.changed()) {
			continue
		}
		if !cp.Removed {
			if other := ps.ByName(cp.Name); other != nil && other != mine {
				cp.Name = ps.UniqueName(cp.Name)
			}
		}
		if mine != nil {
			*mine = cp
		} else {
			ps.Peers = append(ps.Peers, &cp)
		}
		changed = true
	}
	return ps.prune() || changed
}

// prune drops removals old enough to have reached every machine.
func (ps *Peers) prune() bool {
	keep := ps.Peers[:0]
	for _, p := range ps.Peers {
		if !p.Removed || time.Since(p.changed()) < tombstoneTTL {
			keep = append(keep, p)
		}
	}
	pruned := len(keep) != len(ps.Peers)
	ps.Peers = keep
	return pruned
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
		if p.Enabled && !p.Removed {
			out = append(out, p)
		}
	}
	return out
}
