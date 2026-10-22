// Package inbox is the file behind `peerbus inbox`: messages from other
// people that wait for this machine's human, messages of ours that wait for
// their go-ahead, and the thread approvals that let follow-ups flow without
// asking again. Every session on the machine and the CLI share it.
package inbox

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/proto"
)

const (
	retention = 7 * 24 * time.Hour
	// Grant is how long an approval keeps a thread open.
	Grant = 2 * time.Hour
)

// Held is a message waiting for a human.
type Held struct {
	ID       string        `json:"id"`
	Msg      proto.Message `json:"msg"`
	Peer     identity.Key  `json:"peer"`                // the other person
	PeerName string        `json:"peerName"`            // our name for them when it was held
	Target   string        `json:"targetSid,omitempty"` // session it is for (inbound) or from (outbound); empty: the human picks
	Outbound bool          `json:"outbound,omitempty"`  // ours, waiting for our own approval (send=ask)
	HeldAt   time.Time     `json:"heldAt"`
}

// View is what the CLI shows.
func (h *Held) View() proto.HeldView {
	return proto.HeldView{
		ID: h.ID, From: h.Msg.From, To: h.Msg.To, Thread: h.Msg.Thread,
		Routed: h.Target == "" && !h.Outbound, Outbound: h.Outbound,
		HeldAt: h.HeldAt.Unix(), Text: h.Msg.Text,
	}
}

type grant struct {
	Peer   identity.Key `json:"peer"`
	Thread string       `json:"thread"`
	Until  time.Time    `json:"until"`
}

type State struct {
	Held   []*Held `json:"held"`
	Grants []grant `json:"grants"`
}

type Store struct{ Dir string }

func (s Store) path() string     { return filepath.Join(s.Dir, "inbox.json") }
func (s Store) lockPath() string { return filepath.Join(s.Dir, ".inbox.lock") }

func (s Store) Load() (*State, error) {
	b, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path(), err)
	}
	return &st, nil
}

// ModTime is when the inbox last changed, for `inbox -watch`.
func (s Store) ModTime() time.Time {
	fi, err := os.Stat(s.path())
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func (s Store) Update(fn func(*State) error) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	st, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	now := time.Now()
	st.Held = slices.DeleteFunc(st.Held, func(h *Held) bool { return now.Sub(h.HeldAt) > retention })
	st.Grants = slices.DeleteFunc(st.Grants, func(g grant) bool { return g.Until.Before(now) })
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// Add stores a held message, giving it an id.
func (s Store) Add(h *Held) error {
	return s.Update(func(st *State) error {
		for {
			h.ID = proto.NewID(2)
			if slices.IndexFunc(st.Held, func(x *Held) bool { return x.ID == h.ID }) < 0 {
				break
			}
		}
		if h.HeldAt.IsZero() {
			h.HeldAt = time.Now()
		}
		st.Held = append(st.Held, h)
		return nil
	})
}

// Find returns a held message without removing it.
func (s Store) Find(id string) (*Held, error) {
	st, err := s.Load()
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(st.Held, func(h *Held) bool { return h.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("no held message %q", id)
	}
	return st.Held[i], nil
}

// Take removes and returns a held message.
func (s Store) Take(id string) (*Held, error) {
	var out *Held
	err := s.Update(func(st *State) error {
		i := slices.IndexFunc(st.Held, func(h *Held) bool { return h.ID == id })
		if i < 0 {
			return fmt.Errorf("no held message %q", id)
		}
		out = st.Held[i]
		st.Held = slices.Delete(st.Held, i, i+1)
		return nil
	})
	return out, err
}

// Approve opens a thread with a peer for Grant.
func (s Store) Approve(peer ed25519.PublicKey, thread string) error {
	return s.Update(func(st *State) error {
		until := time.Now().Add(Grant)
		for i, g := range st.Grants {
			if g.Peer.Equal(peer) && g.Thread == thread {
				st.Grants[i].Until = until
				return nil
			}
		}
		st.Grants = append(st.Grants, grant{Peer: identity.Key(peer), Thread: thread, Until: until})
		return nil
	})
}

// Approved reports whether messages from peer in thread flow without asking.
func (s Store) Approved(peer ed25519.PublicKey, thread string) bool {
	st, err := s.Load()
	if err != nil {
		return false
	}
	now := time.Now()
	for _, g := range st.Grants {
		if g.Peer.Equal(peer) && g.Thread == thread && g.Until.After(now) {
			return true
		}
	}
	return false
}
