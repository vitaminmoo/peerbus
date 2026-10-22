package identity

import (
	"crypto/ed25519"
	"testing"
	"time"
)

func newPeer(t *testing.T, name string, updated time.Time) *Peer {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Peer{Name: name, Key: Key(pub), Secret: NewSecret(), Enabled: true, Perms: DefaultPerms(), PairedAt: updated, Updated: updated}
}

func clone(p *Peer) *Peer {
	c := *p
	c.Perms.Sessions = append([]string(nil), p.Perms.Sessions...)
	return &c
}

func TestMergeAddsAndUpdates(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	alice := newPeer(t, "alice", t0)
	mine := &Peers{Peers: []*Peer{alice}}

	// The other machine paired bob and later tightened alice's sessions.
	bob := newPeer(t, "bob", t0.Add(time.Minute))
	alice2 := clone(alice)
	alice2.Perms.Sessions = []string{"re-*"}
	alice2.Updated = t0.Add(2 * time.Minute)
	theirs := &Peers{Peers: []*Peer{alice2, bob}}

	if !mine.Merge(theirs) {
		t.Fatal("no change reported")
	}
	if mine.ByName("bob") == nil {
		t.Fatal("bob not added")
	}
	if got := mine.ByName("alice").Perms.Sessions; len(got) != 1 || got[0] != "re-*" {
		t.Fatalf("alice not updated: %v", got)
	}
	if mine.Merge(theirs) {
		t.Fatal("merging the same list again changed something")
	}
}

func TestMergeKeepsNewerLocalEdit(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	alice := newPeer(t, "alice", t0)
	stale := clone(alice)
	alice.Perms.Deliver = "auto"
	alice.Updated = t0.Add(time.Minute)
	mine := &Peers{Peers: []*Peer{alice}}
	if mine.Merge(&Peers{Peers: []*Peer{stale}}) {
		t.Fatal("an older entry overwrote a newer one")
	}
	if mine.ByName("alice").Perms.Deliver != "auto" {
		t.Fatal("local edit lost")
	}
}

func TestMergeRemovalWinsOverStaleCopy(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	alice := newPeer(t, "alice", t0)
	offline := &Peers{Peers: []*Peer{clone(alice)}} // a machine that was off when alice was removed

	here := &Peers{Peers: []*Peer{alice}}
	alice.Remove()
	if here.ByName("alice") != nil || here.ByKey(ed25519.PublicKey(alice.Key)) != nil || len(here.Enabled()) != 0 {
		t.Fatal("removed peer still visible")
	}

	// The stale copy doesn't bring alice back here...
	if here.Merge(offline) {
		t.Fatal("stale copy resurrected a removed peer")
	}
	// ...and the removal reaches the offline machine.
	if !offline.Merge(here) || offline.ByName("alice") != nil {
		t.Fatal("removal didn't sync")
	}
	if !offline.Peers[0].Secret.IsZero() {
		t.Fatal("removed peer kept its secret")
	}
}

func TestAddReplacesRemovedEntry(t *testing.T) {
	alice := newPeer(t, "alice", time.Now().Add(-time.Hour))
	ps := &Peers{Peers: []*Peer{alice}}
	alice.Remove()
	again := clone(alice)
	again.Removed, again.Enabled, again.Secret = false, true, NewSecret()
	ps.Add(again)
	if len(ps.Peers) != 1 || ps.ByName("alice") == nil {
		t.Fatalf("re-pairing: %+v", ps.Peers)
	}
}

func TestMergeRenamesClashingNames(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	mine := &Peers{Peers: []*Peer{newPeer(t, "sam", t0)}}
	other := newPeer(t, "sam", t0) // a different sam, paired on another machine
	if !mine.Merge(&Peers{Peers: []*Peer{other}}) {
		t.Fatal("no change")
	}
	if mine.ByName("sam") == nil || mine.ByName("sam-2") == nil {
		t.Fatalf("names: %s, %s", mine.Peers[0].Name, mine.Peers[1].Name)
	}
}

func TestMergeLegacyEntries(t *testing.T) {
	// Entries written before syncing existed have no Updated time.
	old := newPeer(t, "alice", time.Now().Add(-time.Hour))
	old.Updated = time.Time{}
	mine := &Peers{}
	if !mine.Merge(&Peers{Peers: []*Peer{old}}) || mine.ByName("alice") == nil {
		t.Fatal("legacy entry didn't sync")
	}
}

func TestMergePrunesOldRemovals(t *testing.T) {
	gone := newPeer(t, "alice", time.Now().Add(-2*tombstoneTTL))
	gone.Removed = true
	ps := &Peers{Peers: []*Peer{gone}}
	ps.Merge(&Peers{})
	if len(ps.Peers) != 0 {
		t.Fatal("old removal kept")
	}
}

func TestMergeSelfName(t *testing.T) {
	t0 := time.Now()
	mine := &Peers{Self: Self{Name: "old", NameUpdated: t0}}
	mine.Merge(&Peers{Self: Self{Name: "new", NameUpdated: t0.Add(time.Second)}})
	if mine.Self.Name != "new" {
		t.Fatalf("name: %s", mine.Self.Name)
	}
	mine.Merge(&Peers{Self: Self{Name: "older", NameUpdated: t0}})
	if mine.Self.Name != "new" {
		t.Fatalf("older name won: %s", mine.Self.Name)
	}
}

func TestMaySeeByMachine(t *testing.T) {
	p := Perms{Sessions: []string{"ghidra-*", "laptop/*"}}
	cases := []struct {
		host, name string
		want       bool
	}{
		{"server", "ghidra-1", true},
		{"laptop", "notes", true},
		{"server", "notes", false},
	}
	for _, c := range cases {
		if got := p.MaySee(c.host, c.name); got != c.want {
			t.Errorf("MaySee(%q, %q) = %v", c.host, c.name, got)
		}
	}
}
