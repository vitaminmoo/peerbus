// Command labnode drives the transport package inside the NAT lab
// (../testdata/natlab.sh). It isn't part of peerbus itself.
//
//	labnode server -listen IP:PORT
//	labnode node [-server ADDR] [-localdir D] -points a,b [-dial POINT] [-nopunch]
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/vitaminmoo/peerbus/internal/transport"
)

func point(name string) transport.Point { return transport.Point(sha256.Sum256([]byte(name))) }

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: labnode server|node ...")
	}
	var err error
	switch os.Args[1] {
	case "server":
		err = server(os.Args[2:])
	case "node":
		err = node(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
}

func server(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fs.String("listen", "0.0.0.0:47615", "")
	fs.Parse(args)
	a, err := net.ResolveUDPAddr("udp4", *listen)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", a)
	if err != nil {
		return err
	}
	fmt.Println("server on", conn.LocalAddr())
	return transport.NewServer(conn).Serve(context.Background())
}

func node(args []string) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	srv := fs.String("server", "", "rendezvous server")
	localDir := fs.String("localdir", "", "same-machine discovery dir")
	points := fs.String("points", "", "comma-separated meeting point names to register under")
	dialPoint := fs.String("dial", "", "meeting point to look up and dial")
	noPunch := fs.Bool("nopunch", false, "")
	stun := fs.String("stun", "", "STUN server host:port")
	fs.Parse(args)

	_, key, _ := ed25519.GenerateKey(rand.Reader)
	n, err := transport.Start(context.Background(), transport.Config{
		Key:          key,
		Server:       *srv,
		LocalDir:     *localDir,
		Allow:        func(ed25519.PublicKey) bool { return true }, // pinning is covered by unit tests
		DisablePunch: *noPunch,
		STUN:         strings.Fields(strings.ReplaceAll(*stun, ",", " ")),
		Log:          log.New(os.Stderr, "", log.Lmicroseconds),
		OnConn: func(c *transport.Conn) {
			fmt.Println("accepted", c.Path)
			for {
				s, err := c.AcceptStream(context.Background())
				if err != nil {
					return
				}
				go func() { io.Copy(s, s); s.Close() }()
			}
		},
	})
	if err != nil {
		return err
	}
	if *srv != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := n.WaitRegistered(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("register: %w", err)
		}
	}
	var ps []transport.Point
	for _, name := range strings.Split(*points, ",") {
		if name != "" {
			ps = append(ps, point(name))
		}
	}
	n.SetPoints(ps)
	if *stun != "" {
		for i := 0; i < 30 && n.STUNMapped() == ""; i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	fmt.Println("ready", n.Handle(), "observed", n.Observed(), "stun", n.STUNMapped())
	if *dialPoint == "" {
		select {}
	}

	start := time.Now()
	var peers []transport.Peer
	for time.Since(start) < 10*time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		peers, err = n.Lookup(ctx, point(*dialPoint))
		cancel()
		if len(peers) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(peers) == 0 {
		return fmt.Errorf("nobody under %q (%v)", *dialPoint, err)
	}
	t := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := n.Dial(ctx, peers[0], func(ed25519.PublicKey) bool { return true })
	if err != nil {
		return err
	}
	connect := time.Since(t)
	s, err := c.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	buf := make([]byte, 5)
	t = time.Now()
	for range 5 {
		s.Write([]byte("ping\n"))
		if _, err := io.ReadFull(s, buf); err != nil {
			return err
		}
	}
	fmt.Printf("RESULT path=%s connect=%s rtt=%s\n", c.Path, connect.Round(time.Microsecond), (time.Since(t) / 5).Round(time.Microsecond))
	return nil
}
