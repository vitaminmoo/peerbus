package push

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vitaminmoo/peerbus/internal/identity"
)

func TestSeal(t *testing.T) {
	mine, theirs := identity.NewSecret(), identity.NewSecret()
	long := strings.Repeat("é", maxText) // two bytes each: clipped mid-way
	sealed := Seal(mine, Note{Kind: Held, ID: "3fa9", Title: "From bob", Text: long})
	if Seal(mine, Note{Kind: Held, ID: "3fa9"}) == Seal(mine, Note{Kind: Held, ID: "3fa9"}) {
		t.Fatal("sealing twice gave the same ciphertext")
	}
	if len(sealed) > 3000 {
		t.Fatalf("sealed note is %d bytes; FCM allows 4 KB", len(sealed))
	}
	n, err := Open(mine, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != Held || n.ID != "3fa9" || n.Title != "From bob" || !utf8.ValidString(n.Text) || !strings.HasSuffix(n.Text, "…") {
		t.Fatalf("opened %+v", n)
	}
	if _, err := Open(theirs, sealed); err == nil {
		t.Fatal("opened with another identity's secret")
	}
	if _, err := Open(mine, sealed[:len(sealed)-4]+"AAA="); err == nil {
		t.Fatal("opened a tampered note")
	}
}
