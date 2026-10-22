package channel

import (
	"slices"
	"testing"
)

func TestChannelSpecs(t *testing.T) {
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"claude"}, nil},
		{[]string{"claude", "--resume", "deob"}, nil},
		{[]string{"claude", "--dangerously-load-development-channels", "server:peerbus", "--resume", "deob"}, []string{"server:peerbus"}},
		{[]string{"claude", "--channels", "plugin:x@y", "server:pb", "--continue"}, []string{"plugin:x@y", "server:pb"}},
		{[]string{"claude", "--channels=server:peerbus", "hello"}, []string{"server:peerbus"}},
	} {
		if got := channelSpecs(c.args); !slices.Equal(got, c.want) {
			t.Errorf("%q: got %q, want %q", c.args, got, c.want)
		}
	}
}
