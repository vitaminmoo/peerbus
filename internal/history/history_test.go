package history

import (
	"strings"
	"testing"
	"time"

	"github.com/vitaminmoo/peerbus/internal/proto"
)

func TestMessages(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	now := time.Now().Unix()
	add := func(e proto.HistoryEntry) {
		t.Helper()
		if err := s.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	// Held for us as a person, then routed and delivered.
	add(proto.HistoryEntry{ID: "m1", From: "bob/x#b1", To: "alice/", Peer: "bob", Status: "held", Text: "hi", At: now - 10})
	// The same id from someone else is another message.
	add(proto.HistoryEntry{ID: "m1", From: "carol/y#c1", To: "alice/one#a1", Peer: "carol", Status: "delivered", Text: "spoof", At: now - 5})
	add(proto.HistoryEntry{ID: "m1", From: "bob/x#b1", To: "alice/two#a2", Status: "delivered", At: now})
	// A change can't move a message back.
	add(proto.HistoryEntry{ID: "m2", From: "alice/one#a1", To: "bob/x#b1", Peer: "bob", Status: "delivered", Text: "yo", At: now})
	add(proto.HistoryEntry{ID: "m2", From: "alice/one#a1", Status: "held", At: now})
	// Too old to keep.
	add(proto.HistoryEntry{ID: "m0", From: "bob/x#b1", Status: "delivered", Text: "old", At: now - int64(retention.Seconds()) - 1})

	msgs, err := s.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages: %+v", msgs)
	}
	if m := msgs[0]; m.Text != "hi" || m.Status != "delivered" || m.To != "alice/two#a2" || m.Peer != "bob" || m.At != now-10 {
		t.Fatalf("routed: %+v", m)
	}
	if m := msgs[1]; m.Text != "spoof" || m.From != "carol/y#c1" {
		t.Fatalf("other sender: %+v", m)
	}
	if m := msgs[2]; m.Status != "delivered" {
		t.Fatalf("went backwards: %+v", m)
	}
	if _, err := s.Find("bob/x#b1", "m1"); err != nil {
		t.Fatal(err)
	}
}

func TestCompact(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	big := strings.Repeat("x", 60<<10)
	for i := range compactAt/len(big) + 10 {
		if err := s.Add(proto.HistoryEntry{ID: proto.NewID(4), From: "a/b", Status: "delivered", Text: big, At: time.Now().Unix() - int64(retention.Seconds()) + int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(entries) * len(big); n > compactAt+len(big) {
		t.Fatalf("%d entries left (%d bytes)", len(entries), n)
	}
}
