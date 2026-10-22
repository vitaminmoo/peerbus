// Package history is the file behind the history page of `peerbus tui`:
// every message this machine's sessions sent or received, and what became
// of it. Every session on the machine appends to it; nothing in it reaches
// an agent.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/vitaminmoo/peerbus/internal/proto"
)

const (
	retention = 30 * 24 * time.Hour
	// Past compactAt bytes the file is rewritten without entries older than
	// retention, keeping the newest that fit in half of it.
	compactAt = 8 << 20
)

type Store struct{ Dir string }

func (s Store) path() string     { return filepath.Join(s.Dir, "history.jsonl") }
func (s Store) lockPath() string { return filepath.Join(s.Dir, ".history.lock") }

// Add appends an entry: a new message, or a change in one already logged.
func (s Store) Add(e proto.HistoryEntry) error {
	if e.At == 0 {
		e.At = time.Now().Unix()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	f, err := os.OpenFile(s.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	fi, serr := f.Stat()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && serr == nil && fi.Size() > compactAt {
		err = s.compact()
	}
	return err
}

// compact rewrites the file with recent entries only. The caller holds the
// lock.
func (s Store) compact() error {
	entries, err := s.load()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-retention).Unix()
	var lines [][]byte
	size := 0
	for i := len(entries) - 1; i >= 0 && entries[i].At >= cutoff; i-- {
		b, _ := json.Marshal(entries[i])
		if size += len(b) + 1; size > compactAt/2 {
			break
		}
		lines = append(lines, append(b, '\n'))
	}
	slices.Reverse(lines)
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, bytes.Join(lines, nil), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// load reads every entry, skipping lines it can't parse (such as one being
// written right now).
func (s Store) load() ([]proto.HistoryEntry, error) {
	f, err := os.Open(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 8*proto.MaxText)
	var out []proto.HistoryEntry
	for sc.Scan() {
		var e proto.HistoryEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// Messages returns one entry per message, oldest first, with later changes
// folded in.
func (s Store) Messages() ([]proto.HistoryEntry, error) {
	entries, err := s.load()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-retention).Unix()
	var out []proto.HistoryEntry
	at := map[string]int{}
	for _, e := range entries {
		if e.At < cutoff {
			continue
		}
		if i, ok := at[Key(e)]; ok {
			Merge(&out[i], e)
			continue
		}
		at[Key(e)] = len(out)
		out = append(out, e)
	}
	return out, nil
}

// Find returns one message in full.
func (s Store) Find(from, id string) (proto.HistoryEntry, error) {
	msgs, err := s.Messages()
	if err != nil {
		return proto.HistoryEntry{}, err
	}
	for _, e := range msgs {
		if e.From == from && e.ID == id {
			return e, nil
		}
	}
	return proto.HistoryEntry{}, errors.New("no such message in the history")
}

// Key tells messages apart. Message ids are picked by the sender, so a
// friend could reuse one of ours; pairing it with the sender's address
// keeps their messages from folding into ours.
func Key(e proto.HistoryEntry) string { return e.From + "\x00" + e.ID }

// Merge folds e, another entry for the same message (a later change, or
// the other end's record of it), into dst.
func Merge(dst *proto.HistoryEntry, e proto.HistoryEntry) {
	if dst.Text == "" || dst.Cut && !e.Cut && e.Text != "" {
		dst.Text, dst.Cut = e.Text, e.Cut
	}
	if dst.Thread == "" {
		dst.Thread = e.Thread
	}
	if dst.Peer == "" {
		dst.Peer = e.Peer
	}
	if e.To != "" && (dst.To == "" || e.Text == "") {
		dst.To = e.To // a change names where the message ended up
	}
	if dst.At == 0 || e.At != 0 && e.At < dst.At && e.Text != "" {
		dst.At = e.At
	}
	if rank(e.Status) >= rank(dst.Status) {
		dst.Status = e.Status
	}
}

// rank orders statuses so a message's record only moves forward, whatever
// order the machines' records arrive in.
func rank(status string) int {
	switch status {
	case "ask":
		return 0
	case "held":
		return 1
	case "delivered", "declined", "unsent":
		return 2
	}
	return -1
}
