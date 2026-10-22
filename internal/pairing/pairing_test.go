package pairing

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/transport"
)

func server(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { transport.NewServer(conn).Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return conn.LocalAddr().String()
}

func store(t *testing.T, name string) identity.Store {
	t.Helper()
	s := identity.Store{Dir: t.TempDir()}
	if _, err := s.Identity(); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(p *identity.Peers) error { p.Self.Name = name; return nil }); err != nil {
		t.Fatal(err)
	}
	return s
}

func pubOf(t *testing.T, s identity.Store) ed25519.PublicKey {
	k, err := s.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return k.Public().(ed25519.PublicKey)
}

type hostResult struct {
	res Result
	err error
}

func host(t *testing.T, s identity.Store, inv Invite, name string, opts Options) chan hostResult {
	ch := make(chan hostResult, 1)
	go func() {
		r, err := Host(context.Background(), s, inv, name, opts)
		ch <- hostResult{r, err}
	}()
	return ch
}

func TestCodeRoundTrip(t *testing.T) {
	inv, err := NewInvite(store(t, "a"), true, time.Hour, "peerb.us:47615")
	if err != nil {
		t.Fatal(err)
	}
	code := inv.Encode()
	got, err := Decode("  " + code[:20] + "\n" + code[20:] + " ") // pasted with a line break
	if err != nil {
		t.Fatal(err)
	}
	if !got.Key.Equal(inv.Key) || got.Secret != inv.Secret || !got.Self || got.Server != inv.Server || !got.Expires.Equal(inv.Expires) {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, inv)
	}
	damaged := []byte(code)
	damaged[30] ^= 1
	if _, err := Decode(string(damaged)); err == nil {
		t.Fatal("damaged code accepted")
	}
	t.Logf("code is %d characters", len(code))
}

func TestFriendPairing(t *testing.T) {
	opts := Options{Server: server(t)}
	a, b := store(t, "alice"), store(t, "bob")
	inv, _ := NewInvite(a, false, time.Minute, "")
	hc := host(t, a, inv, "", opts)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Redeem(ctx, b, inv.Encode(), "", false, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "alice" || res.Fingerprint != identity.Fingerprint(pubOf(t, a)) {
		t.Fatalf("redeemer result %+v", res)
	}
	h := <-hc
	if h.err != nil || h.res.Name != "bob" {
		t.Fatalf("host result %+v %v", h.res, h.err)
	}

	pa, _ := a.Load()
	pb, _ := b.Load()
	bob, alice := pa.ByKey(pubOf(t, b)), pb.ByKey(pubOf(t, a))
	if bob == nil || alice == nil {
		t.Fatalf("not recorded: %+v / %+v", pa.Peers, pb.Peers)
	}
	if bob.Secret != alice.Secret || bob.Secret.IsZero() || bob.Secret == inv.Secret {
		t.Fatal("pair secrets differ, are empty, or reuse the invite secret")
	}
	if bob.Perms.Deliver != "hold" || !bob.Enabled {
		t.Fatalf("default perms %+v", bob.Perms)
	}
	if bob.Secret.Point(time.Now()) != alice.Secret.Point(time.Now()) {
		t.Fatal("meeting points differ")
	}

	// Used up: nobody is waiting any more.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if _, err := Redeem(ctx2, store(t, "carol"), inv.Encode(), "", false, opts); err == nil {
		t.Fatal("a used invite was redeemed again")
	}
}

func TestWrongInviterKey(t *testing.T) {
	opts := Options{Server: server(t)}
	a, b := store(t, "alice"), store(t, "bob")
	inv, _ := NewInvite(a, false, time.Minute, "")
	hc := host(t, a, inv, "", opts)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	forged := inv
	forged.Key = pubOf(t, store(t, "mallory"))
	if _, err := Redeem(ctx, b, forged.Encode(), "", false, opts); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("redeem against the wrong key: %v", err)
	}
	if _, err := Redeem(ctx, b, inv.Encode(), "", false, opts); err != nil {
		t.Fatal(err)
	}
	if h := <-hc; h.err != nil {
		t.Fatal(h.err)
	}
}

// Whoever runs the rendezvous server can see invite meeting points and
// connect to a waiting inviter, but can't prove the secret.
func TestWrongProof(t *testing.T) {
	opts := Options{Server: server(t)}
	a, b := store(t, "alice"), store(t, "bob")
	inv, _ := NewInvite(a, false, time.Minute, "")
	hc := host(t, a, inv, "", opts)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, k, _ := ed25519.GenerateKey(nil)
	n, err := transport.Start(ctx, transport.Config{Key: k, Server: opts.Server, Allow: func(ed25519.PublicKey) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	n.WaitRegistered(ctx)
	var peers []transport.Peer
	for len(peers) == 0 {
		peers, _ = n.Lookup(ctx, inv.Secret.InvitePoint())
		time.Sleep(50 * time.Millisecond)
	}
	c, err := n.Dial(ctx, peers[0], func(k ed25519.PublicKey) bool { return k.Equal(inv.Key) })
	if err != nil {
		t.Fatal(err)
	}
	s, _ := c.OpenStreamSync(ctx)
	writeJSON(s, hello{V: 1, Proof: strings.Repeat("00", 32), Name: "mallory"})
	var rep reply
	if err := readJSON(bufio.NewReader(s), &rep); err != nil || rep.OK || !strings.Contains(rep.Error, "wrong invite secret") {
		t.Fatalf("bogus proof: %+v %v", rep, err)
	}
	c.CloseWithError(0, "")

	// The invite is still good for the real redeemer.
	if _, err := Redeem(ctx, b, inv.Encode(), "", false, opts); err != nil {
		t.Fatal(err)
	}
	if h := <-hc; h.err != nil || h.res.Name != "bob" {
		t.Fatalf("host %+v %v", h.res, h.err)
	}
}

func TestExpired(t *testing.T) {
	a := store(t, "alice")
	inv, _ := NewInvite(a, false, -time.Minute, "")
	if _, err := Redeem(context.Background(), store(t, "bob"), inv.Encode(), "", false, Options{}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired invite: %v", err)
	}
}

func TestSelfPairing(t *testing.T) {
	opts := Options{Server: server(t)}
	a, friend := store(t, "alice"), store(t, "bob")
	// alice already has a friend.
	finv, _ := NewInvite(friend, false, time.Minute, "")
	fh := host(t, friend, finv, "", opts)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := Redeem(ctx, a, finv.Encode(), "", false, opts); err != nil {
		t.Fatal(err)
	}
	<-fh

	laptop := identity.Store{Dir: t.TempDir()} // brand new device
	inv, _ := NewInvite(a, true, time.Minute, "")
	hc := host(t, a, inv, "", opts)
	res, err := Redeem(ctx, laptop, inv.Encode(), "", false, opts)
	if err != nil {
		t.Fatal(err)
	}
	if h := <-hc; h.err != nil || !res.Self {
		t.Fatalf("self pairing: %+v %v", h.res, h.err)
	}
	if !pubOf(t, laptop).Equal(pubOf(t, a)) {
		t.Fatal("laptop didn't adopt alice's identity")
	}
	pa, _ := a.Load()
	pl, _ := laptop.Load()
	if pl.Self.Secret != pa.Self.Secret || pl.ByKey(pubOf(t, friend)) == nil {
		t.Fatal("laptop didn't get the self secret and peers")
	}

	// A device with its own peers refuses to be overwritten without -force.
	inv2, _ := NewInvite(a, true, time.Minute, "")
	host(t, a, inv2, "", opts)
	if _, err := Redeem(ctx, friend, inv2.Encode(), "", false, opts); err == nil || !strings.Contains(err.Error(), "force") {
		t.Fatalf("self-pairing over existing peers: %v", err)
	}
}
