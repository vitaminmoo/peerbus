package held

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/vitaminmoo/peerbus/internal/history"
	"github.com/vitaminmoo/peerbus/internal/mesh"
	"github.com/vitaminmoo/peerbus/internal/proto"
)

// Record is one message, merged from every machine that logged it.
type Record struct {
	Entry    proto.HistoryEntry
	Machines []string
	Local    bool         // in this machine's history, read directly
	Via      []*mesh.Link // sessions on the other machines that logged it
}

func (r Record) Key() string { return history.Key(r.Entry) }

// History reads this machine's history and asks every session of ours for
// theirs, then merges the machines' records of each message, newest first.
func History(ctx context.Context, m *mesh.Mesh) []Record {
	views, byInbox := AskAll(ctx, m.Own(), proto.Frame{Type: proto.THistory},
		func(v proto.HistoryView) string { return v.InboxID }, func(v proto.HistoryView) int { return v.V })
	localID := ""
	if v, err := m.History(); err == nil {
		views[v.InboxID], localID = v, v.InboxID
	}
	ids := make([]string, 0, len(views))
	for id := range views {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// A machine that was offline for a deletion still has the thread. Don't
	// show its copy, and pass the deletion on.
	deleted := map[string]int64{}
	for _, id := range ids {
		for _, t := range views[id].Deleted {
			deleted[t.Thread] = max(deleted[t.Thread], t.At)
		}
	}
	var recs []Record
	at := map[string]int{}
	for _, id := range ids {
		v := views[id]
		missing := missingTombstones(v, deleted)
		for _, t := range missing {
			passOnDeletion(m, t, id == localID, byInbox[id], v.V)
		}
		for _, e := range v.Messages {
			if slices.ContainsFunc(missing, func(t proto.Tombstone) bool { return t.Thread == e.Thread && e.At <= t.At }) {
				continue
			}
			k := history.Key(e)
			i, ok := at[k]
			if ok {
				history.Merge(&recs[i].Entry, e)
			} else {
				i, at[k] = len(recs), len(recs)
				recs = append(recs, Record{Entry: e})
			}
			r := &recs[i]
			r.Machines = append(r.Machines, v.Machine)
			if id == localID {
				r.Local = true
			} else {
				r.Via = append(r.Via, byInbox[id]...)
			}
		}
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Entry.At > recs[j].Entry.At })
	return recs
}

// missingTombstones are the deletions a machine hasn't heard of.
func missingTombstones(v proto.HistoryView, deleted map[string]int64) []proto.Tombstone {
	var out []proto.Tombstone
	for thread, at := range deleted {
		if !slices.ContainsFunc(v.Deleted, func(t proto.Tombstone) bool { return t.Thread == thread && t.At >= at }) {
			out = append(out, proto.Tombstone{Thread: thread, At: at})
		}
	}
	return out
}

// passOnDeletion tells a machine about a deletion, in the background: a
// failure only means trying again at the next look at the history. A session
// too old to report deletions couldn't take this one either.
func passOnDeletion(m *mesh.Mesh, t proto.Tombstone, local bool, via []*mesh.Link, version int) {
	if local {
		m.DeleteThread(t.Thread, t.At)
		return
	}
	if len(via) == 0 || version < 2 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), AskTimeout)
		defer cancel()
		via[0].Request(ctx, deleteFrame(t.Thread, t.At), nil)
	}()
}

func deleteFrame(thread string, at int64) proto.Frame {
	return proto.Frame{Type: proto.TDeleteThread, Thread: thread, Meta: map[string]string{"at": strconv.FormatInt(at, 10)}}
}

// DeleteThread forgets a thread on this machine and every machine with a
// session online; the rest hear of it the next time a client sees both them
// and a machine that knows. It returns how many of the thread's messages
// stay because they're waiting for a decision in an inbox.
func DeleteThread(ctx context.Context, m *mesh.Mesh, thread string) (kept int, err error) {
	at := time.Now().Unix()
	local, err := m.DeleteThread(thread, at)
	if err != nil {
		return 0, err
	}
	views, _ := AskAll(ctx, m.Own(), deleteFrame(thread, at),
		func(v proto.DeleteResult) string { return v.InboxID }, func(v proto.DeleteResult) int { return v.V })
	views[local.InboxID] = local
	for _, v := range views {
		kept += v.Kept
	}
	return kept, nil
}

// ThreadKey groups records into conversations. A message logged without a
// thread is a conversation of its own.
func (r Record) ThreadKey() string {
	if r.Entry.Thread != "" {
		return r.Entry.Thread
	}
	return r.Key()
}

// ReloadOutcome is what became of one session asked to reload.
type ReloadOutcome struct {
	Addr, Host string
	Status     string // current, reloaded, pending (no word yet), failed
	Version    string // of the binary it reloaded into
	Err        error
}

// Reload asks every session of ours, on every machine, to replace itself
// with the peerbus binary on its disk, and waits up to wait for those that
// agreed to come back. A session that hasn't is still reported, since a
// remote one can take a while to be found again.
func Reload(ctx context.Context, m *mesh.Mesh, wait time.Duration) []ReloadOutcome {
	own := m.Own()
	out := make([]ReloadOutcome, len(own))
	var wg sync.WaitGroup
	for i, l := range own {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := ReloadOutcome{Addr: l.Address(), Host: l.Info().Host}
			ctx, cancel := context.WithTimeout(ctx, 2*AskTimeout)
			defer cancel()
			var res proto.ReloadResult
			if o.Err = l.Request(ctx, proto.Frame{Type: proto.TReload}, &res); o.Err != nil {
				o.Status = "failed"
			} else {
				o.Status, o.Version = res.Status, res.Version
			}
			out[i] = o
		}()
	}
	wg.Wait()
	deadline := time.Now().Add(wait)
	for {
		waiting := false
		for i, l := range own {
			if out[i].Status != "reloading" {
				continue
			}
			if back(m, l) {
				out[i].Status = "reloaded"
			} else {
				waiting = true
			}
		}
		if !waiting || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for i := range out {
		if out[i].Status == "reloading" {
			out[i].Status = "pending"
		}
	}
	return out
}

// back reports whether the session behind old is connected again.
func back(m *mesh.Mesh, old *mesh.Link) bool {
	sid := old.Info().SID
	return slices.ContainsFunc(m.Own(), func(l *mesh.Link) bool { return l != old && l.Info().SID == sid })
}

// Full fetches the whole message when the history only had its start.
func (r Record) Full(ctx context.Context, m *mesh.Mesh) (proto.HistoryEntry, error) {
	var e proto.HistoryEntry
	err := errors.New("none of the machines that logged it has a session online")
	if r.Local {
		e, err = m.HistoryMessage(r.Entry.From, r.Entry.ID)
	}
	for _, l := range r.Via {
		if err == nil {
			break
		}
		ctx, cancel := context.WithTimeout(ctx, AskTimeout)
		err = l.Request(ctx, proto.Frame{Type: proto.THistory, ID: r.Entry.ID, Meta: map[string]string{"from": r.Entry.From}}, &e)
		cancel()
	}
	return e, err
}
