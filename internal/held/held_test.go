package held

import (
	"testing"

	"github.com/vitaminmoo/peerbus/internal/proto"
)

func TestInboxVersion(t *testing.T) {
	note := proto.HeldView{ID: "n", Note: true}
	plain := proto.HeldView{ID: "p", Routed: true}
	for _, c := range []struct {
		v    proto.InboxView
		want int
	}{
		{proto.InboxView{V: 1}, 1},
		{proto.InboxView{Held: []proto.HeldView{plain}}, 0},
		{proto.InboxView{Held: []proto.HeldView{plain, note}}, 1}, // reads notes, from before V
		{proto.InboxView{Held: []proto.HeldView{{Fake: true}}}, 1},
	} {
		if got := inboxVersion(c.v); got != c.want {
			t.Errorf("%+v: %d, want %d", c.v, got, c.want)
		}
	}
}
