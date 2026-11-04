package mesh

import (
	"context"
	"testing"

	"github.com/vitaminmoo/peerbus/internal/proto"
)

// Friends see channel state and version even without ShowDetails, and a
// session that doesn't say (an older peerbus) counts as unknown.
func TestChannelStateAndVersion(t *testing.T) {
	srv := server(t)
	alice, bob := store(t, "alice"), store(t, "bob")
	pair(t, alice, bob)
	a, _ := start(t, alice, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "a1a1a1a1", Name: "one", Host: "h", Status: "idle"})
	start(t, bob, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "b1b1b1b1", Name: "on", Host: "b", Status: "idle", Channels: proto.ChannelsOn, Version: "1.2.3", Cwd: "/secret"})
	start(t, bob, t.TempDir(), srv, proto.KindSession, proto.SessionInfo{SID: "b2b2b2b2", Name: "old", Host: "b", Status: "idle"})
	waitFor(t, "alice to see bob's sessions", func() bool { return sees(a, "bob/on#b1b1b1") && sees(a, "bob/old#b2b2b2") })

	checked := 0
	for _, v := range a.Views() {
		switch v.Address {
		case "bob/on#b1b1b1":
			checked++
			if v.Channels != proto.ChannelsOn || v.Version != "1.2.3" || v.Cwd != "" {
				t.Errorf("verified: %+v", v)
			}
		case "bob/old#b2b2b2":
			checked++
			if v.Channels != proto.ChannelsUnknown || v.Version != "" {
				t.Errorf("older: %+v", v)
			}
		case "bob/human":
			checked++
			if v.Channels != "" {
				t.Errorf("a person has no channels: %+v", v)
			}
		}
	}
	if checked != 3 {
		t.Errorf("checked %d of 3 views: %+v", checked, a.Views())
	}

	ctx := context.Background()
	res, err := a.Send(ctx, "bob/on#b1b1b1", "hi", "")
	if err != nil || res.Unverified {
		t.Errorf("to a verified session: %+v %v", res, err)
	}
	res, err = a.Send(ctx, "bob/old#b2b2b2", "hi", "")
	if err != nil || !res.Unverified {
		t.Errorf("to an unverified session: %+v %v", res, err)
	}
}
