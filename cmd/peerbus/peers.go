package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/vitaminmoo/peerbus/internal/config"
	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/pairing"
)

func idStore() identity.Store { return identity.Store{Dir: config.Dir()} }

func pairingOptions() (pairing.Options, error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return pairing.Options{}, err
	}
	return pairing.Options{Server: cfg.Server(), STUN: cfg.STUN, LocalDir: config.LocalDir(), Log: debugLog()}, nil
}

// requireTerminal keeps trust changes with the human: a Claude session's
// Bash tool has no terminal. Like approve, it's a speed bump rather than an
// authentication boundary.
func requireTerminal(what string) error {
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return fmt.Errorf("%s must be run interactively in a terminal", what)
	}
	return nil
}

func confirm(prompt string) bool {
	fmt.Print(prompt + " [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func runID(args []string) error {
	fs := flag.NewFlagSet("id", flag.ExitOnError)
	name := fs.String("name", "", "set the name you suggest others call you")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	st := idStore()
	key, err := st.Identity()
	if err != nil {
		return err
	}
	if *name != "" {
		if err := requireTerminal("changing your name"); err != nil {
			return err
		}
		if err := st.Update(func(p *identity.Peers) error { p.Self.Name = identity.CleanName(*name); return nil }); err != nil {
			return err
		}
	}
	ps, err := st.Load()
	if err != nil {
		return err
	}
	enabled := 0
	for _, p := range ps.Peers {
		if p.Enabled {
			enabled++
		}
	}
	shown := ps.Self.Name
	if shown == "" {
		shown = os.Getenv("USER") + " (default; set with -name)"
	}
	fmt.Printf("Identity:    %s\nName:        %s\nStore:       %s\nPeers:       %d (%d enabled)\n",
		identity.Fingerprint(key.Public().(ed25519.PublicKey)), shown, st.Dir, len(ps.Peers), enabled)
	return nil
}

func runInvite(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	self := fs.Bool("self", false, "pair one of your own devices (it receives your identity)")
	name := fs.String("name", "", "what to call them (default: what they suggest)")
	ttl := fs.Duration("ttl", 15*time.Minute, "how long the invite stays valid")
	server := fs.String("server", "", "rendezvous server to put in the invite (default: yours)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if err := requireTerminal("invite"); err != nil {
		return err
	}
	opts, err := pairingOptions()
	if err != nil {
		return err
	}
	hint := *server
	if hint == "" {
		hint = opts.Server
	}
	st := idStore()
	inv, err := pairing.NewInvite(st, *self, *ttl, hint)
	if err != nil {
		return err
	}
	code := inv.Encode()
	if *self {
		fmt.Printf("On your other device, run (valid until %s, single use):\n\n  peerbus pair %s\n\n", inv.Expires.Format("15:04"), code)
		fmt.Println("That device will get your identity key and your whole peer list.")
	} else {
		fmt.Printf("Send this to them (valid until %s, single use):\n\n  peerbus pair %s\n\n", inv.Expires.Format("15:04"), code)
		fmt.Println("Anyone holding the code can pair as them until it's used, so send it somewhere private.")
	}
	fmt.Println("Waiting for them... (Ctrl-C to cancel)")
	res, err := pairing.Host(ctx, st, inv, *name, opts)
	if err != nil {
		return err
	}
	if res.Self {
		fmt.Println("\nYour other device is paired.")
		return nil
	}
	fmt.Printf("\nPaired with %q, fingerprint %s.\n", res.Name, res.Fingerprint)
	fmt.Println("Their messages are held for your approval until you change it: peerbus peer set", res.Name, "deliver=auto")
	return nil
}

func runPair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	name := fs.String("name", "", "what to call them (default: what they suggest)")
	force := fs.Bool("force", false, "for your own devices: replace this machine's identity and peers")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: peerbus pair [-name NAME] CODE")
	}
	if err := requireTerminal("pair"); err != nil {
		return err
	}
	opts, err := pairingOptions()
	if err != nil {
		return err
	}
	inv, err := pairing.Decode(strings.Join(pos, ""))
	if err != nil {
		return err
	}
	if inv.Self {
		fmt.Println("This invite makes this machine one of the inviter's own devices: it takes their identity and peer list.")
		if !confirm("Continue?") {
			return errors.New("not paired")
		}
	}
	fmt.Println("Pairing...")
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := pairing.Redeem(ctx, idStore(), strings.Join(pos, ""), *name, *force, opts)
	if err != nil {
		return err
	}
	if res.Self {
		fmt.Println("This machine is now one of your devices.")
		return nil
	}
	fmt.Printf("Paired with %q, fingerprint %s.\n", res.Name, res.Fingerprint)
	return nil
}

func runPeer(args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	st := idStore()
	switch args[0] {
	case "list", "ls":
		ps, err := st.Load()
		if err != nil {
			return err
		}
		if len(ps.Peers) == 0 {
			fmt.Println("No peers yet. Pair with: peerbus invite")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tFINGERPRINT\tENABLED\tDELIVER\tSEND\tSESSIONS\tDETAILS\tRESOURCES\tPAIRED")
		for _, p := range ps.Peers {
			fmt.Fprintf(w, "%s\t%s\t%v\t%s\t%s\t%s\t%v\t%v\t%s\n", p.Name, identity.Fingerprint(ed25519.PublicKey(p.Key)), p.Enabled,
				p.Perms.Deliver, p.Perms.Send, strings.Join(p.Perms.Sessions, ","), p.Perms.ShowDetails, p.Perms.ShowResources, p.PairedAt.Local().Format("2006-01-02"))
		}
		return w.Flush()
	case "rm", "enable", "disable", "rename", "set":
	default:
		return fmt.Errorf("unknown peer command %q (list, rm, enable, disable, rename, set)", args[0])
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: peerbus peer %s NAME ...", args[0])
	}
	if err := requireTerminal("changing peers"); err != nil {
		return err
	}
	name := args[1]
	if args[0] == "rm" && !confirm(fmt.Sprintf("Forget %q? You'd have to pair again to talk to them.", name)) {
		return errors.New("not removed")
	}
	return st.Update(func(ps *identity.Peers) error {
		p := ps.ByName(name)
		if p == nil {
			return fmt.Errorf("no peer named %q", name)
		}
		switch args[0] {
		case "rm":
			for i, q := range ps.Peers {
				if q == p {
					ps.Peers = append(ps.Peers[:i], ps.Peers[i+1:]...)
					break
				}
			}
			fmt.Println("Removed", name)
		case "enable", "disable":
			p.Enabled = args[0] == "enable"
			fmt.Printf("%s %sd\n", name, args[0])
		case "rename":
			if len(args) != 3 {
				return errors.New("usage: peerbus peer rename OLD NEW")
			}
			n := identity.CleanName(args[2])
			if ps.ByName(n) != nil {
				return fmt.Errorf("%q is taken", n)
			}
			p.Name = n
			fmt.Printf("%s is now %s\n", name, n)
		case "set":
			if len(args) < 3 {
				return errors.New("usage: peerbus peer set NAME deliver=hold|auto send=ask|auto sessions=GLOB,... details=BOOL resources=BOOL")
			}
			perms := p.Perms
			for _, kv := range args[2:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("%q isn't key=value", kv)
				}
				var err error
				switch k {
				case "deliver":
					perms.Deliver = v
				case "send":
					perms.Send = v
				case "sessions":
					perms.Sessions = strings.Split(v, ",")
				case "details":
					perms.ShowDetails, err = strconv.ParseBool(v)
				case "resources":
					perms.ShowResources, err = strconv.ParseBool(v)
				default:
					return fmt.Errorf("unknown setting %q", k)
				}
				if err != nil {
					return fmt.Errorf("%s: %w", k, err)
				}
			}
			if err := perms.Validate(); err != nil {
				return err
			}
			p.Perms = perms
			fmt.Printf("%s: deliver=%s send=%s sessions=%s details=%v resources=%v\n", name, perms.Deliver, perms.Send,
				strings.Join(perms.Sessions, ","), perms.ShowDetails, perms.ShowResources)
		}
		return nil
	})
}
