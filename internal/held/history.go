package held

import (
	"context"
	"errors"
	"sort"

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
	var recs []Record
	at := map[string]int{}
	for _, id := range ids {
		v := views[id]
		for _, e := range v.Messages {
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
