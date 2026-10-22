// Command peerbus is the Claude Code channel server, the rendezvous server,
// and the CLI for message passing between Claude Code sessions across
// accounts, machines and people.
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
	"strings"
	"syscall"
	"time"

	"github.com/vitaminmoo/peerbus/internal/channel"
	"github.com/vitaminmoo/peerbus/internal/config"
	"github.com/vitaminmoo/peerbus/internal/inbox"
	"github.com/vitaminmoo/peerbus/internal/mesh"
	"github.com/vitaminmoo/peerbus/internal/proto"
	"github.com/vitaminmoo/peerbus/internal/transport"
	"github.com/vitaminmoo/peerbus/internal/tui"
)

const version = "0.2.0"

const usage = `peerbus: message passing between Claude Code sessions

Identity and pairing:
  peerbus id [-name NAME] [-machine NAME]         show or set your name and this machine's
  peerbus invite [-self] [-name NAME] [-ttl 15m]  print an invite code and wait for it to be redeemed
  peerbus pair [-name NAME] CODE                  redeem an invite code
  peerbus peer list | rm | enable | disable | rename | set NAME k=v...

Using:
  peerbus peers [-filter TEXT] [-json]            sessions reachable right now
  peerbus send [-thread ID] TO TEXT...
  peerbus tui                                     every held message on all your machines: approve, route, decline
                                                  and the history of every message, yours and friends'
  peerbus inbox [-watch]                          this machine's messages waiting for your approval
  peerbus approve [-to SESSION] ID                deliver a held message (interactive only)
  peerbus deny ID

Running:
  peerbus channel                                 the MCP channel server Claude Code spawns
  peerbus mcp-config                              print the MCP server entry for Claude Code
  peerbus rendezvous [-listen HOST:PORT]          run a rendezvous/relay server (UDP)
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
	case "mcp-config":
		err = runMCPConfig()
	case "peers":
		err = runPeers(ctx, args)
	case "send":
		err = runSend(ctx, args)
	case "tui":
		err = runTUI(ctx, args)
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

func debugLog() *log.Logger {
	if os.Getenv("PEERBUS_DEBUG") != "" {
		return log.New(os.Stderr, "peerbus ", log.LstdFlags)
	}
	return nil
}

// control starts a short-lived mesh for the CLI. With server empty it only
// finds sessions on this machine. It returns after the first round of
// discovery, or after wait.
func control(ctx context.Context, server string, wait time.Duration) (*mesh.Mesh, error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, err
	}
	m, err := mesh.Start(ctx, mesh.Config{
		Store:    idStore(),
		StateDir: config.StateDir(),
		Server:   server,
		STUN:     cfg.STUN,
		Kind:     proto.KindControl,
		Info:     proto.SessionInfo{SID: proto.NewID(8), Name: "cli", Host: cfg.MachineName()},
		Log:      debugLog(),
	})
	if err != nil {
		return nil, err
	}
	select {
	case <-m.Ready():
	case <-time.After(wait):
	case <-ctx.Done():
	}
	return m, nil
}

func runPeers(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	filter := fs.String("filter", "", "substring filter")
	asJSON := fs.Bool("json", false, "print JSON")
	local := fs.Bool("local", false, "this machine only")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	server := cfg.Server()
	if *local {
		server = ""
	}
	m, err := control(ctx, server, 8*time.Second)
	if err != nil {
		return err
	}
	defer m.Close()
	peers := m.Views()
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
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	m, err := control(ctx, cfg.Server(), 8*time.Second)
	if err != nil {
		return err
	}
	defer m.Close()
	res, err := m.Send(ctx, pos[0], strings.Join(pos[1:], " "), *thread)
	if err != nil {
		return err
	}
	fmt.Println(channel.DescribeSend(res))
	fmt.Println("(Sent from the CLI: replies can't reach you here.)")
	return nil
}

func inboxStore() inbox.Store { return inbox.Store{Dir: config.StateDir()} }

func printHeld(h proto.HeldView) {
	switch {
	case h.Outbound:
		fmt.Printf("[%s] %s  yours, to %s  (waiting for your go-ahead)\n", h.ID, time.Unix(h.HeldAt, 0).Format("Jan 02 15:04"), h.To)
	case h.Routed:
		fmt.Printf("[%s] %s  from %s  to you (pick a session: approve -to SESSION)\n", h.ID, time.Unix(h.HeldAt, 0).Format("Jan 02 15:04"), h.From)
	default:
		fmt.Printf("[%s] %s  from %s  to %s\n", h.ID, time.Unix(h.HeldAt, 0).Format("Jan 02 15:04"), h.From, h.To)
	}
	for _, line := range strings.Split(strings.TrimRight(h.Text, "\n"), "\n") {
		fmt.Printf("    %s\n", line)
	}
}

func runTUI(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: peerbus tui")
	}
	if err := requireTerminal("tui"); err != nil {
		return err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	ps, err := idStore().Load()
	if err != nil {
		return err
	}
	m, err := control(ctx, cfg.Server(), 5*time.Second)
	if err != nil {
		return err
	}
	defer m.Close()
	return tui.Run(ctx, m, ps.SelfName())
}

func runInbox(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ExitOnError)
	watch := fs.Bool("watch", false, "keep running and show new held messages (with a desktop notification when notify-send exists)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	st := inboxStore()
	state, err := st.Load()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, h := range state.Held {
		seen[h.ID] = true
		printHeld(h.View())
	}
	if len(state.Held) == 0 && !*watch {
		fmt.Println("No held messages.")
	}
	if !*watch {
		return nil
	}
	last := st.ModTime()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
		if mod := st.ModTime(); mod.Equal(last) {
			continue
		} else {
			last = mod
		}
		state, err := st.Load()
		if err != nil {
			continue
		}
		for _, h := range state.Held {
			if seen[h.ID] {
				continue
			}
			seen[h.ID] = true
			printHeld(h.View())
			if path, err := exec.LookPath("notify-send"); err == nil {
				exec.Command(path, "peerbus: message from "+h.Msg.From, firstLine(h.Msg.Text)).Run()
			}
		}
	}
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// localSession finds the session on this machine that a held message
// belongs to, or the one named by to.
func localSession(m *mesh.Mesh, h *inbox.Held, to string) (*mesh.Link, error) {
	locals := m.Locals()
	var names []string
	for _, l := range locals {
		names = append(names, l.Address())
	}
	pick := func(match func(*mesh.Link) bool) *mesh.Link {
		for _, l := range locals {
			if match(l) {
				return l
			}
		}
		return nil
	}
	if h.Outbound {
		if l := pick(func(l *mesh.Link) bool { return l.Info().SID == h.Target }); l != nil {
			return l, nil
		}
		return nil, errors.New("the session that wrote this message isn't running any more")
	}
	if to != "" {
		t := strings.TrimSpace(to)
		if l := pick(func(l *mesh.Link) bool {
			return l.Address() == t || l.Info().Name == t || strings.HasPrefix(l.Address(), t)
		}); l != nil {
			return l, nil
		}
		return nil, fmt.Errorf("no session %q on this machine; running: %s", to, strings.Join(names, ", "))
	}
	if h.Target != "" {
		if l := pick(func(l *mesh.Link) bool { return l.Info().SID == h.Target }); l != nil {
			return l, nil
		}
		if len(names) == 0 {
			return nil, errors.New("that session isn't running any more, and no other peerbus session is running on this machine")
		}
		return nil, fmt.Errorf("that session isn't running any more; pick another with -to: %s", strings.Join(names, ", "))
	}
	if len(locals) == 1 {
		return locals[0], nil
	}
	if len(names) == 0 {
		return nil, errors.New("no peerbus session is running on this machine to deliver it to")
	}
	return nil, fmt.Errorf("pick a session with -to: %s", strings.Join(names, ", "))
}

func runApprove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	to := fs.String("to", "", "deliver to this session of yours instead of the one it was sent to")
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
	h, err := inboxStore().Find(pos[0])
	if err != nil {
		return err
	}
	printHeld(h.View())
	m, err := control(ctx, "", 3*time.Second)
	if err != nil {
		return err
	}
	defer m.Close()
	l, err := localSession(m, h, *to)
	if err != nil {
		return err
	}
	if h.Outbound {
		fmt.Printf("\nSend it to %s? [y/N] ", h.Msg.To)
	} else {
		fmt.Printf("\nDeliver to %s? Replies and follow-ups in this thread will flow without approval for %s. [y/N] ", l.Address(), inbox.Grant)
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("not approved")
	}
	var res proto.SendResult
	if err := l.Request(ctx, proto.Frame{Type: proto.TApprove, ID: h.ID}, &res); err != nil {
		return err
	}
	fmt.Println(channel.DescribeSend(res))
	return nil
}

func runDeny(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: peerbus deny ID")
	}
	st := inboxStore()
	h, err := st.Find(args[0])
	if err != nil {
		return err
	}
	m, err := control(ctx, "", 3*time.Second)
	if err != nil {
		return err
	}
	defer m.Close()
	// A running session tells the sender; without one we just drop it.
	if locals := m.Locals(); len(locals) > 0 {
		if err := locals[0].Request(ctx, proto.Frame{Type: proto.TDeny, ID: h.ID}, nil); err != nil {
			return err
		}
	} else if _, err := st.Take(h.ID); err != nil {
		return err
	}
	fmt.Println("Declined", h.ID)
	return nil
}
