// Command p2pspike checks whether go-libp2p gives peerbus the transport it
// needs: Noise-secured links that prefer local/direct paths, hole punch
// through NATs (DCUtR), and fall back to a relay that only forwards to peers
// that are connected and hold a reservation.
//
//	p2pspike relay -dir D -listen IP              run the relay, write its addrs to D/relay.json
//	p2pspike node  -dir D -name N [-dial M]       reserve on the relay, serve echo; optionally dial node M
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	ma "github.com/multiformats/go-multiaddr"
)

const echoProto = "/peerbus/spike/echo/1"

var t0 = time.Now()

func logf(format string, args ...any) {
	fmt.Printf("[%6.2fs] %s\n", time.Since(t0).Seconds(), fmt.Sprintf(format, args...))
}

type nodeInfo struct {
	ID    string   `json:"id"`
	Addrs []string `json:"addrs"`
}

func writeInfo(dir, name string, h host.Host) {
	info := nodeInfo{ID: h.ID().String()}
	for _, a := range h.Addrs() {
		info.Addrs = append(info.Addrs, a.String())
	}
	b, _ := json.Marshal(info)
	os.WriteFile(filepath.Join(dir, name+".json.tmp"), b, 0o600)
	os.Rename(filepath.Join(dir, name+".json.tmp"), filepath.Join(dir, name+".json"))
}

func readInfo(dir, name string, wait time.Duration) (peer.AddrInfo, error) {
	deadline := time.Now().Add(wait)
	for {
		b, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err == nil {
			var ni nodeInfo
			if err := json.Unmarshal(b, &ni); err != nil {
				return peer.AddrInfo{}, err
			}
			id, err := peer.Decode(ni.ID)
			if err != nil {
				return peer.AddrInfo{}, err
			}
			ai := peer.AddrInfo{ID: id}
			for _, s := range ni.Addrs {
				ai.Addrs = append(ai.Addrs, ma.StringCast(s))
			}
			return ai, nil
		}
		if time.Now().After(deadline) {
			return peer.AddrInfo{}, fmt.Errorf("no %s.json in %s", name, dir)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: p2pspike relay|node ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "relay":
		err = runRelay(os.Args[2:])
	case "node":
		err = runNode(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runRelay(args []string) error {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	dir := fs.String("dir", ".", "shared dir")
	ip := fs.String("listen", "0.0.0.0", "listen IP")
	unlimited := fs.Bool("unlimited", false, "lift the default relayed-connection limits")
	fs.Parse(args)

	priv, _, _ := crypto.GenerateEd25519Key(rand.Reader)
	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(
			fmt.Sprintf("/ip4/%s/udp/4001/quic-v1", *ip),
			fmt.Sprintf("/ip4/%s/tcp/4001", *ip),
		),
		libp2p.Security(noise.ID, noise.New),
		libp2p.ForceReachabilityPublic(),
		libp2p.DisableRelay(), // the relay itself doesn't use relays
	)
	if err != nil {
		return err
	}
	var opts []relay.Option
	if *unlimited {
		opts = append(opts, relay.WithInfiniteLimits())
	}
	if _, err := relay.New(h, opts...); err != nil {
		return err
	}
	writeInfo(*dir, "relay", h)
	logf("relay %s on %v", h.ID(), h.Addrs())
	select {}
}

func runNode(args []string) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	dir := fs.String("dir", ".", "shared dir")
	name := fs.String("name", "", "this node's name")
	dial := fs.String("dial", "", "node to dial")
	probe := fs.Bool("probe-unreserved", false, "also try dialing a random, unreserved peer id through the relay")
	watch := fs.Duration("watch", 20*time.Second, "how long to watch the connection after dialing")
	fs.Parse(args)
	ctx := context.Background()

	relayInfo, err := readInfo(*dir, "relay", 10*time.Second)
	if err != nil {
		return err
	}
	h, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/0.0.0.0/udp/0/quic-v1", "/ip4/0.0.0.0/tcp/0"),
		libp2p.Security(noise.ID, noise.New),
		libp2p.EnableRelay(),
		libp2p.EnableHolePunching(),
		libp2p.ForceReachabilityPrivate(),
	)
	if err != nil {
		return err
	}
	h.SetStreamHandler(echoProto, func(s network.Stream) {
		defer s.Close()
		logf("inbound stream from %s via %s (limited=%v)", short(s.Conn().RemotePeer()), s.Conn().RemoteMultiaddr(), s.Conn().Stat().Limited)
		r := bufio.NewReader(s)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			s.Write([]byte(line))
		}
	})

	start := time.Now()
	if err := h.Connect(ctx, relayInfo); err != nil {
		return fmt.Errorf("connect relay: %w", err)
	}
	res, err := client.Reserve(ctx, h, relayInfo)
	if err != nil {
		return fmt.Errorf("reserve: %w", err)
	}
	logf("%s = %s reserved on relay in %s (expires %s)", *name, short(h.ID()), time.Since(start).Round(time.Millisecond), res.Expiration.Format("15:04:05"))

	// Publish: our own listen addrs (what peerbus's rendezvous would exchange)
	// plus the circuit address through the relay.
	info := nodeInfo{ID: h.ID().String()}
	for _, a := range h.Addrs() {
		info.Addrs = append(info.Addrs, a.String())
	}
	info.Addrs = append(info.Addrs, circuitAddr(relayInfo, h.ID()).String())
	b, _ := json.Marshal(info)
	os.WriteFile(filepath.Join(*dir, *name+".json"), b, 0o600)

	if *probe {
		fake, _, _ := crypto.GenerateEd25519Key(rand.Reader)
		fakeID, _ := peer.IDFromPrivateKey(fake)
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := h.Connect(pctx, peer.AddrInfo{ID: fakeID, Addrs: []ma.Multiaddr{circuitAddr(relayInfo, fakeID)}})
		cancel()
		logf("probe dial to unreserved id %s: %v", short(fakeID), err)
	}

	if *dial == "" {
		select {}
	}
	target, err := readInfo(*dir, *dial, 15*time.Second)
	if err != nil {
		return err
	}
	return dialAndWatch(ctx, h, target, *watch)
}

func circuitAddr(relayInfo peer.AddrInfo, target peer.ID) ma.Multiaddr {
	return ma.StringCast(fmt.Sprintf("/p2p/%s/p2p-circuit/p2p/%s", relayInfo.ID, target))
}

func short(id peer.ID) string {
	s := id.String()
	return s[len(s)-6:]
}

func describeConns(h host.Host, p peer.ID) string {
	var parts []string
	for _, c := range h.Network().ConnsToPeer(p) {
		kind := "direct"
		if c.Stat().Limited {
			kind = "RELAYED"
		}
		parts = append(parts, fmt.Sprintf("%s %s", kind, c.RemoteMultiaddr()))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " | ")
}

func dialAndWatch(ctx context.Context, h host.Host, target peer.AddrInfo, watch time.Duration) error {
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := h.Connect(cctx, target)
	cancel()
	if err != nil {
		return fmt.Errorf("connect %s: %w", short(target.ID), err)
	}
	logf("connected to %s in %s: %s", short(target.ID), time.Since(start).Round(time.Millisecond), describeConns(h, target.ID))

	sctx := network.WithAllowLimitedConn(ctx, "spike")
	s, err := h.NewStream(sctx, target.ID, echoProto)
	if err != nil {
		return fmt.Errorf("stream: %w", err)
	}
	rtt := func(s network.Stream) (time.Duration, error) {
		t := time.Now()
		if _, err := s.Write([]byte("ping\n")); err != nil {
			return 0, err
		}
		if _, err := bufio.NewReader(s).ReadString('\n'); err != nil {
			return 0, err
		}
		return time.Since(t), nil
	}
	d, err := rtt(s)
	logf("echo over %s: rtt %s err=%v", map[bool]string{true: "RELAYED", false: "direct"}[s.Conn().Stat().Limited], d, err)
	s.Close()

	last := ""
	deadline := time.Now().Add(watch)
	for time.Now().Before(deadline) {
		if cur := describeConns(h, target.ID); cur != last {
			logf("conns to %s: %s", short(target.ID), cur)
			last = cur
		}
		time.Sleep(200 * time.Millisecond)
	}
	// A stream without WithAllowLimitedConn only opens on a direct connection.
	s2, err := h.NewStream(ctx, target.ID, echoProto)
	if err != nil {
		logf("RESULT no direct connection after %s: %v", watch, err)
		return nil
	}
	d, err = rtt(s2)
	logf("RESULT direct path %s, rtt %s err=%v", s2.Conn().RemoteMultiaddr(), d, err)
	s2.Close()
	return nil
}
