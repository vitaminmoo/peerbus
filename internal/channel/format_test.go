package channel

import (
	"strings"
	"testing"

	"github.com/vitaminmoo/peerbus/internal/proto"
)

func TestFormatPeersChannels(t *testing.T) {
	on := proto.PeerView{Address: "a/on#1", Name: "on", Status: "idle", Host: "h", Via: "local", Channels: proto.ChannelsOn, Version: "1.2.3"}
	old := proto.PeerView{Address: "b/old#2", Name: "old", Status: "idle", Host: "h", Via: "relay", Foreign: true, Channels: proto.ChannelsUnknown}
	person := proto.PeerView{Address: "b/human", Name: proto.Human, Status: "person", Foreign: true}

	got := FormatPeers([]proto.PeerView{on, person}, "", "")
	if !strings.Contains(got, "a/on#1  idle  host=h  via=local  version=1.2.3\n") {
		t.Errorf("verified session:\n%s", got)
	}
	if strings.Contains(got, "unverified") {
		t.Errorf("nothing here is unverified, a person included:\n%s", got)
	}

	got = FormatPeers([]proto.PeerView{on, old}, "", "")
	if !strings.Contains(got, "b/old#2  idle  host=h  via=relay  version=unknown  channels=unverified\n") {
		t.Errorf("unverified session:\n%s", got)
	}
	if !strings.Contains(got, unverifiedNote) {
		t.Errorf("no explanation:\n%s", got)
	}
}

func TestDescribeSendUnverified(t *testing.T) {
	res := proto.SendResult{Status: "delivered", To: "b/old#2", Thread: "t", ID: "m"}
	if s := DescribeSend(res); strings.Contains(s, "may not see") {
		t.Errorf("verified: %s", s)
	}
	res.Unverified = true
	if s := DescribeSend(res); !strings.Contains(s, "Delivered to b/old#2") || !strings.Contains(s, "may not see") {
		t.Errorf("unverified: %s", s)
	}
}
