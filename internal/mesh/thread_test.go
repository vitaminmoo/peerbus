package mesh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitaminmoo/peerbus/internal/history"
	"github.com/vitaminmoo/peerbus/internal/proto"
)

func TestReplyMetadata(t *testing.T) {
	srv := server(t)
	alice, bob := store(t, "alice"), store(t, "bob")
	pair(t, alice, bob)
	state := t.TempDir()
	one, ev1 := start(t, alice, state, srv, proto.KindSession, proto.SessionInfo{SID: "a1a1a1a1", Name: "one", Host: "h", Status: "idle"})
	two, ev2 := start(t, alice, state, srv, proto.KindSession, proto.SessionInfo{SID: "a2a2a2a2", Name: "two", Host: "h", Status: "idle"})
	b, _ := start(t, bob, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "b1b1b1b1", Name: "bobsess", Host: "b", Status: "idle"})
	waitFor(t, "everyone to meet", func() bool { return sees(one, "alice/two#a2a2a2") && sees(b, "alice/one#a1a1a1") })
	ctx := context.Background()

	first, err := one.Send(ctx, "two", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	m := ev2.msg(t)
	if m.ReplyTo != "" {
		t.Fatalf("a new message says it replies to %q", m.ReplyTo)
	}
	// Naming the message is enough: the thread comes from the history.
	res, err := two.Reply(ctx, m.From, "answer", "", m.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := ev1.msg(t)
	if r.ReplyTo != first.ID || r.Thread != first.Thread || res.Thread != first.Thread {
		t.Fatalf("reply %+v, result %+v, to %+v", r, res, first)
	}
	// A reply_to is cleaned on arrival, since it goes into channel metadata.
	link := b.resolveLink(t, "alice/two")
	if err := link.Request(ctx, proto.Frame{Type: proto.TSend, To: "alice/two#a2a2a2", Text: "sneaky", ReplyTo: `x" kind="evil`}, nil); err != nil {
		t.Fatal(err)
	}
	msgs, err := (history.Store{Dir: state}).Messages()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range msgs {
		if e.Text == "sneaky" {
			found = true
			if e.ReplyTo != "xkindevil" {
				t.Fatalf("reply_to kept as %q", e.ReplyTo)
			}
		}
	}
	if !found {
		t.Fatal("bob's message wasn't logged")
	}
}

// An agent may read only its own session's messages: the history file also
// has other sessions' and messages not yet approved.
func TestSessionMessagesAreTheSessionsOwn(t *testing.T) {
	srv := server(t)
	alice, bob := store(t, "alice"), store(t, "bob")
	pair(t, alice, bob)
	state := t.TempDir()
	one, _ := start(t, alice, state, srv, proto.KindSession, proto.SessionInfo{SID: "a1a1a1a1", Name: "one", Host: "h", Status: "idle"})
	two, ev2 := start(t, alice, state, srv, proto.KindSession, proto.SessionInfo{SID: "a2a2a2a2", Name: "two", Host: "h", Status: "idle"})
	three, _ := start(t, alice, state, srv, proto.KindSession, proto.SessionInfo{SID: "a3a3a3a3", Name: "three", Host: "h", Status: "idle"})
	b, _ := start(t, bob, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "b1b1b1b1", Name: "bobsess", Host: "b", Status: "idle"})
	waitFor(t, "everyone to meet", func() bool {
		return sees(one, "alice/two#a2a2a2") && sees(one, "alice/three#a3a3a3") && sees(b, "alice/one#a1a1a1")
	})
	ctx := context.Background()
	if _, err := two.Send(ctx, "three", "between two and three", ""); err != nil {
		t.Fatal(err)
	}
	if res, err := b.Send(ctx, "alice/one", "held for one", ""); err != nil || res.Status != "held" {
		t.Fatalf("bob's send: %+v %v", res, err)
	}
	if _, err := one.Send(ctx, "two", "from one", ""); err != nil {
		t.Fatal(err)
	}
	ev2.msg(t)
	if _, err := three.Send(ctx, "one", "to one", ""); err != nil {
		t.Fatal(err)
	}
	msgs, err := one.SessionMessages()
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, e := range msgs {
		texts = append(texts, e.Text)
	}
	if got := strings.Join(texts, "|"); got != "from one|to one" {
		t.Fatalf("session one sees: %s", got)
	}
}

func TestDeleteThread(t *testing.T) {
	srv := server(t)
	alice, bob := store(t, "alice"), store(t, "bob")
	pair(t, alice, bob)
	state := t.TempDir()
	one, _ := start(t, alice, state, srv, proto.KindSession, proto.SessionInfo{SID: "a1a1a1a1", Name: "one", Host: "h", Status: "idle"})
	two, ev2 := start(t, alice, state, srv, proto.KindSession, proto.SessionInfo{SID: "a2a2a2a2", Name: "two", Host: "h", Status: "idle"})
	b, _ := start(t, bob, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "b1b1b1b1", Name: "bobsess", Host: "b", Status: "idle"})
	waitFor(t, "everyone to meet", func() bool { return sees(one, "alice/two#a2a2a2") && sees(b, "alice/one#a1a1a1") })
	ctx := context.Background()
	hist := history.Store{Dir: state}

	gone, err := one.Send(ctx, "two", "forget me", "")
	if err != nil {
		t.Fatal(err)
	}
	ev2.msg(t)
	other, err := one.Send(ctx, "two", "keep me", "")
	if err != nil {
		t.Fatal(err)
	}
	ev2.msg(t)
	// A note to you in the thread goes too; a friend's message waiting for a
	// decision stays, and so does its record.
	if _, err := two.Send(ctx, "human", "a note", gone.Thread); err != nil {
		t.Fatal(err)
	}
	held, err := b.Send(ctx, "alice/one", "may I?", gone.Thread)
	if err != nil || held.Status != "held" {
		t.Fatalf("bob: %+v %v", held, err)
	}
	if err := one.inbox.Approve(pub(t, bob), gone.Thread, ""); err != nil {
		t.Fatal(err)
	}

	// Only you may delete: not your sessions, not your friends.
	for _, l := range []*Link{two.resolveLink(t, "one"), b.resolveLink(t, "alice/one")} {
		if err := l.Request(ctx, proto.Frame{Type: proto.TDeleteThread, Thread: gone.Thread}, nil); err == nil {
			t.Fatal("a delete from something other than your own CLI was allowed")
		}
	}
	if msgs, _ := hist.Messages(); len(msgs) != 4 {
		t.Fatalf("deleted anyway: %+v", msgs)
	}

	res, err := one.DeleteThread(gone.Thread, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 2 || res.Kept != 1 {
		t.Fatalf("result: %+v", res)
	}
	msgs, _ := hist.Messages()
	if len(msgs) != 2 || msgs[0].Thread != other.Thread || msgs[1].Text != "may I?" {
		t.Fatalf("after: %+v", msgs)
	}
	st, _ := one.inbox.Load()
	if len(st.Held) != 1 || st.Held[0].Msg.Text != "may I?" {
		t.Fatalf("inbox after: %+v", st.Held)
	}
	if one.inbox.Approved(pub(t, bob), gone.Thread) {
		t.Fatal("the thread is still open to bob")
	}
	// The text isn't left behind in the file.
	if b, _ := readFile(hist); strings.Contains(b, "forget me") {
		t.Fatalf("history still holds it:\n%s", b)
	}
	// A late change to a deleted message doesn't bring back an empty row.
	one.record(proto.HistoryEntry{ID: gone.ID, From: one.SelfAddress(), Status: "declined"})
	if msgs, _ := hist.Messages(); len(msgs) != 2 {
		t.Fatalf("a change brought it back: %+v", msgs)
	}
	// A new message in the thread stands on its own.
	if _, err := one.Send(ctx, "two", "starting over", gone.Thread); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := hist.Messages(); len(msgs) != 3 {
		t.Fatalf("after a new message: %+v", msgs)
	}
	v, err := one.History()
	if err != nil || len(v.Deleted) != 1 || v.Deleted[0].Thread != gone.Thread {
		t.Fatalf("view: %+v %v", v.Deleted, err)
	}
}

func readFile(s history.Store) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir, "history.jsonl"))
	return string(b), err
}
