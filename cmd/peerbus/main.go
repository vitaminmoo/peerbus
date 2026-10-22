// Command peerbus is the broker, the Claude Code channel server, and the CLI
// for message passing between Claude Code sessions across accounts, machines
// and people.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/vitaminmoo/peerbus/internal/broker"
	"github.com/vitaminmoo/peerbus/internal/channel"
	"github.com/vitaminmoo/peerbus/internal/client"
	"github.com/vitaminmoo/peerbus/internal/config"
	"github.com/vitaminmoo/peerbus/internal/proto"
	"github.com/vitaminmoo/peerbus/internal/transport"
)

const version = "0.1.0"

const usage = `peerbus: message passing between Claude Code sessions

Identity and pairing:
  peerbus id [-name NAME]                         show your identity fingerprint
  peerbus invite [-self] [-name NAME] [-ttl 15m]  print an invite code and wait for it to be redeemed
  peerbus pair [-name NAME] CODE                  redeem an invite code
  peerbus peer list | rm | enable | disable | rename | set NAME k=v...

Legacy broker setup:
  peerbus init [-user NAME] [-listen HOST:PORT]   create a broker config and a client config for this machine
  peerbus join URL TOKEN                          point this machine at an existing broker
  peerbus user add NAME | token NAME | list       manage broker users (prints tokens once)
  peerbus group add GROUP USER... | rm GROUP USER... | list
  peerbus mcp-config                              print the MCP server entry for Claude Code

Running:
  peerbus broker [-listen HOST:PORT]              run the broker (local brokers autostart)
  peerbus rendezvous [-listen HOST:PORT]          run the rendezvous/relay server (UDP)
  peerbus channel                                 the MCP channel server Claude Code spawns

Using:
  peerbus peers [-filter TEXT] [-json]
  peerbus send [-thread ID] TO TEXT...
  peerbus inbox [-watch]                          third-party messages waiting for you
  peerbus approve [-to SESSION] ID                deliver a held message (interactive only)
  peerbus deny ID
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "broker":
		err = runBroker(ctx, args)
	case "rendezvous":
		err = runRendezvous(ctx, args)
	case "channel":
		err = channel.Run(ctx, version)
	case "id":
		err = runID(args)
	case "invite":
		err = runInvite(ctx, args)
	case "pair":
		err = runPair(ctx, args)
	case "peer":
		err = runPeer(args)
	case "init":
		err = runInit(args)
	case "join":
		err = runJoin(args)
	case "user":
		err = runUser(args)
	case "group":
		err = runGroup(args)
	case "mcp-config":
		err = runMCPConfig()
	case "peers":
		err = runPeers(ctx, args)
	case "send":
		err = runSend(ctx, args)
	case "inbox":
		err = runInbox(ctx, args)
	case "approve":
		err = runApprove(ctx, args)
	case "deny":
		err = runDeny(ctx, args)
	case "version", "-v", "--version":
		fmt.Println("peerbus", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "peerbus:", err)
		os.Exit(1)
	}
}

// parse lets flags appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func runBroker(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("broker", flag.ExitOnError)
	cfgPath := fs.String("config", config.BrokerPath(), "broker config")
	statePath := fs.String("state", filepath.Join(config.StateDir(), "broker-state.json"), "broker state file")
	listen := fs.String("listen", "", "override the config's listen address")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	b, err := broker.New(*cfgPath, *statePath)
	if err != nil {
		return err
	}
	addr := b.ListenAddr()
	if *listen != "" {
		addr = *listen
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	b.Log.Printf("peerbus %s listening on %s", version, ln.Addr())
	pidFile := filepath.Join(filepath.Dir(*statePath), "broker.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintln(os.Getpid())), 0o600); err == nil {
		defer os.Remove(pidFile)
	}
	return b.Serve(ctx, ln)
}

func runRendezvous(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rendezvous", flag.ExitOnError)
	listen := fs.String("listen", fmt.Sprintf("0.0.0.0:%d", config.DefaultPort), "UDP listen address")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	addr, err := net.ResolveUDPAddr("udp4", *listen)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return err
	}
	srv := transport.NewServer(conn)
	srv.Log = log.New(os.Stderr, "", 0)
	srv.Log.Printf("peerbus %s rendezvous on udp %s", version, conn.LocalAddr())
	return srv.Serve(ctx)
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	user := fs.String("user", os.Getenv("USER"), "your peerbus user name")
	listen := fs.String("listen", fmt.Sprintf("127.0.0.1:%d", config.DefaultPort), "broker listen address")
	force := fs.Bool("force", false, "overwrite existing configs")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if err := validName(*user); err != nil {
		return err
	}
	for _, p := range []string{config.BrokerPath(), config.ClientPath()} {
		if _, err := os.Stat(p); err == nil && !*force {
			return fmt.Errorf("%s already exists (use -force to overwrite)", p)
		}
	}
	token := config.NewToken(*user)
	bc := &config.Broker{
		Listen: *listen,
		Users:  map[string]*config.User{*user: {Tokens: []string{config.HashToken(token)}}},
		Groups: map[string][]string{},
	}
	if err := config.WriteJSON(config.BrokerPath(), bc); err != nil {
		return err
	}
	if err := config.WriteJSON(config.ClientPath(), &config.Client{URL: "ws://" + *listen, Token: token}); err != nil {
		return err
	}
	fmt.Printf("Wrote %s and %s (user %q).\n", config.BrokerPath(), config.ClientPath(), *user)
	fmt.Println("Every Claude account on this machine shares the client config, so they all act as the same peerbus user.")
	fmt.Println("Next: add peerbus to Claude Code (see `peerbus mcp-config`).")
	return nil
}

func validName(n string) error {
	if n == "" || strings.ContainsAny(n, "/#[] \t") {
		return fmt.Errorf("invalid name %q (no spaces, '/', '#', '[' or ']')", n)
	}
	return nil
}

func runJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite an existing client config")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: peerbus join URL TOKEN")
	}
	if _, err := os.Stat(config.ClientPath()); err == nil && !*force {
		return fmt.Errorf("%s already exists (use -force to overwrite)", config.ClientPath())
	}
	c := &config.Client{URL: pos[0], Token: pos[1]}
	if old, err := config.LoadClient(); err == nil {
		c.ShareDetails, c.Resources, c.Programs = old.ShareDetails, old.Resources, old.Programs
	}
	if err := config.WriteJSON(config.ClientPath(), c); err != nil {
		return err
	}
	fmt.Println("Wrote", config.ClientPath())
	return nil
}

func loadBrokerConfig() (*config.Broker, error) {
	bc, err := config.LoadBroker(config.BrokerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no broker config at %s on this machine (run peerbus init where the broker runs)", config.BrokerPath())
	}
	return bc, err
}

func runUser(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: peerbus user add NAME | token NAME | list")
	}
	bc, err := loadBrokerConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		names := make([]string, 0, len(bc.Users))
		for n := range bc.Users {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			fmt.Printf("%s (%d tokens)\n", n, len(bc.Users[n].Tokens))
		}
		return nil
	case "add", "token":
		if len(args) != 2 {
			return fmt.Errorf("usage: peerbus user %s NAME", args[0])
		}
		name := args[1]
		if err := validName(name); err != nil {
			return err
		}
		u := bc.Users[name]
		if args[0] == "add" && u != nil {
			return fmt.Errorf("user %q exists; use `peerbus user token %s` for another token", name, name)
		}
		if args[0] == "token" && u == nil {
			return fmt.Errorf("no user %q", name)
		}
		if u == nil {
			u = &config.User{}
			bc.Users[name] = u
		}
		token := config.NewToken(name)
		u.Tokens = append(u.Tokens, config.HashToken(token))
		if err := config.WriteJSON(config.BrokerPath(), bc); err != nil {
			return err
		}
		fmt.Printf("Token for %s (shown once):\n  %s\n\nOn their machine:\n  peerbus join ws://%s %s\n", name, token, bc.Listen, token)
		if args[0] == "add" {
			fmt.Printf("\nThey can't see or message anyone until you share a group: peerbus group add GROUP %s %s\n", os.Getenv("USER"), name)
		}
		return nil
	}
	return fmt.Errorf("unknown user command %q", args[0])
}

func runGroup(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: peerbus group add GROUP USER... | rm GROUP USER... | list")
	}
	bc, err := loadBrokerConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		for g, m := range bc.Groups {
			fmt.Printf("%s: %s\n", g, strings.Join(m, ", "))
		}
		return nil
	case "add", "rm":
		if len(args) < 3 {
			return fmt.Errorf("usage: peerbus group %s GROUP USER...", args[0])
		}
		g := args[1]
		for _, u := range args[2:] {
			if args[0] == "add" {
				if bc.Users[u] == nil {
					return fmt.Errorf("no user %q", u)
				}
				if !slices.Contains(bc.Groups[g], u) {
					bc.Groups[g] = append(bc.Groups[g], u)
				}
			} else {
				bc.Groups[g] = slices.DeleteFunc(bc.Groups[g], func(x string) bool { return x == u })
			}
		}
		if len(bc.Groups[g]) == 0 {
			delete(bc.Groups, g)
		}
		if err := config.WriteJSON(config.BrokerPath(), bc); err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", g, strings.Join(bc.Groups[g], ", "))
		return nil
	}
	return fmt.Errorf("unknown group command %q", args[0])
}

func runMCPConfig() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	cfg := map[string]any{"mcpServers": map[string]any{"peerbus": map[string]any{"command": exe, "args": []string{"channel"}}}}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Printf("%s\n\nAdd it for a Claude account (repeat per CLAUDE_CONFIG_DIR):\n  claude mcp add -s user peerbus -- %s channel\n\nThen start sessions with the channel enabled (custom channels need the development flag during the research preview):\n  claude --dangerously-load-development-channels server:peerbus\n", b, exe)
	return nil
}

// control connects as a control client (the human's side).
func control(ctx context.Context, watch bool, onFrame func(proto.Frame)) (*client.Client, error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, err
	}
	c := &client.Client{
		URL: cfg.URL,
		Hello: func() proto.Frame {
			return proto.Frame{Type: proto.THello, V: proto.Version, Token: cfg.Token, Kind: "control", Watch: watch}
		},
		OnFrame:   onFrame,
		Autostart: client.AutostartLocal(cfg.URL),
	}
	if err := c.Connect(ctx); err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", cfg.URL, err)
	}
	return c, nil
}

func runPeers(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	filter := fs.String("filter", "", "substring filter")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := control(ctx, false, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	var peers []proto.PeerView
	if err := c.Request(ctx, proto.Frame{Type: proto.TList}, &peers); err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(peers, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Print(channel.FormatPeers(peers, "", *filter))
	return nil
}

func runSend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	thread := fs.String("thread", "", "thread id to continue")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return errors.New("usage: peerbus send [-thread ID] TO TEXT...")
	}
	c, err := control(ctx, false, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	var res proto.SendResult
	if err := c.Request(ctx, proto.Frame{Type: proto.TSend, To: pos[0], Text: strings.Join(pos[1:], " "), Thread: *thread}, &res); err != nil {
		return err
	}
	fmt.Println(channel.DescribeSend(res))
	fmt.Println("(Sent from the CLI: replies can't reach you here.)")
	return nil
}

func printHeld(h proto.HeldView) {
	to := h.To
	if h.Routed {
		to += " (you pick the session: approve -to SESSION)"
	}
	fmt.Printf("[%s] %s  from %s  to %s\n", h.ID, time.Unix(h.HeldAt, 0).Format("Jan 02 15:04"), h.From, to)
	for _, line := range strings.Split(strings.TrimRight(h.Text, "\n"), "\n") {
		fmt.Printf("    %s\n", line)
	}
}

func runInbox(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ExitOnError)
	watch := fs.Bool("watch", false, "keep running and show new held messages (with a desktop notification when notify-send exists)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := control(ctx, *watch, func(f proto.Frame) {
		if f.Type != proto.THeld || f.Held == nil {
			return
		}
		printHeld(*f.Held)
		if path, err := exec.LookPath("notify-send"); err == nil {
			exec.Command(path, "peerbus: message from "+f.Held.From, firstLine(f.Held.Text)).Run()
		}
	})
	if err != nil {
		return err
	}
	defer c.Close()
	var held []proto.HeldView
	if err := c.Request(ctx, proto.Frame{Type: proto.TInbox}, &held); err != nil {
		return err
	}
	if len(held) == 0 && !*watch {
		fmt.Println("No held messages.")
	}
	for _, h := range held {
		printHeld(h)
	}
	if *watch {
		<-ctx.Done()
	}
	return nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func runApprove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	to := fs.String("to", "", "deliver to this session of yours instead of the requested one")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: peerbus approve [-to SESSION] ID")
	}
	// Approval is the human's decision. Refusing without a terminal keeps a
	// Claude session's Bash tool from approving third-party messages on its
	// own; it's a speed bump, not an authentication boundary.
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return errors.New("approve must be run interactively in a terminal")
	}
	c, err := control(ctx, false, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	var held []proto.HeldView
	if err := c.Request(ctx, proto.Frame{Type: proto.TInbox}, &held); err != nil {
		return err
	}
	i := slices.IndexFunc(held, func(h proto.HeldView) bool { return h.ID == pos[0] })
	if i < 0 {
		return fmt.Errorf("no held message %q", pos[0])
	}
	printHeld(held[i])
	dest := held[i].To
	if *to != "" {
		dest = *to
	}
	fmt.Printf("\nDeliver to %s? Replies and follow-ups in this thread will flow without approval for a while. [y/N] ", dest)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("not approved")
	}
	var res proto.SendResult
	if err := c.Request(ctx, proto.Frame{Type: proto.TApprove, ID: pos[0], To: *to}, &res); err != nil {
		return err
	}
	fmt.Println(channel.DescribeSend(res))
	return nil
}

func runDeny(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: peerbus deny ID")
	}
	c, err := control(ctx, false, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Request(ctx, proto.Frame{Type: proto.TDeny, ID: args[0]}, nil); err != nil {
		return err
	}
	fmt.Println("Declined", args[0])
	return nil
}
