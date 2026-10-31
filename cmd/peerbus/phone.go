package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/vitaminmoo/peerbus/internal/pairing"
)

// runPhone pairs the Android app as one of your own devices: a self
// invite, shown as a QR code for the app to scan.
func runPhone(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("phone", flag.ExitOnError)
	ttl := fs.Duration("ttl", 15*time.Minute, "how long the invite stays valid")
	server := fs.String("server", "", "rendezvous server to put in the invite (default: yours)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if err := requireTerminal("phone"); err != nil {
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
	if hint == "" {
		// The phone never shares this machine's discovery directory.
		return errors.New("a phone reaches your sessions through a rendezvous server, and client.json turns it off")
	}
	st := idStore()
	inv, err := pairing.NewInvite(st, true, *ttl, hint)
	if err != nil {
		return err
	}
	code := inv.Encode()
	if err := printQR(os.Stdout, strings.ToUpper(code)); err != nil {
		return err
	}
	fmt.Printf("\nIn the peerbus app, scan this (valid until %s, single use), or paste:\n\n  %s\n\n", inv.Expires.Format("15:04"), code)
	fmt.Println("The phone gets your identity key and your whole peer list, like any of your machines.")
	fmt.Println("Waiting for it... (Ctrl-C to cancel)")
	if _, err := pairing.Host(ctx, st, inv, "", opts); err != nil {
		return err
	}
	fmt.Println("\nYour phone is paired. It shows held messages from every machine with a session running.")
	return nil
}

// printQR draws text as a QR code two modules per character cell, dark on
// an explicit light background so it scans in dark terminals too.
func printQR(w io.Writer, text string) error {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return err
	}
	const quiet = 3
	dark := func(x, y int) bool {
		return x >= 0 && y >= 0 && x < c.Size && y < c.Size && c.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < c.Size+quiet; y += 2 {
		b.WriteString("\x1b[30;107m")
		for x := -quiet; x < c.Size+quiet; x++ {
			switch top, bottom := dark(x, y), dark(x, y+1); {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	_, err = io.WriteString(w, b.String())
	return err
}
