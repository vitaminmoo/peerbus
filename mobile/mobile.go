// Package mobile is peerbus for the Android app, bound with gomobile. The
// phone pairs as one of your own devices (`peerbus phone` on a computer
// shows the QR code) and then joins the mesh the way `peerbus tui` does: as
// a control client that dials your sessions on every machine, gathers
// their held messages, and approves, routes or declines them.
//
// In the background the phone stays off the mesh: your sessions push it
// sealed notes through Firebase (see internal/push), which Unseal opens.
//
// gomobile only passes simple types, so snapshots cross to Kotlin as JSON.
package mobile

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vitaminmoo/peerbus/internal/config"
	"github.com/vitaminmoo/peerbus/internal/held"
	"github.com/vitaminmoo/peerbus/internal/identity"
	"github.com/vitaminmoo/peerbus/internal/mesh"
	"github.com/vitaminmoo/peerbus/internal/pairing"
	"github.com/vitaminmoo/peerbus/internal/proto"
	"github.com/vitaminmoo/peerbus/internal/push"
)

const (
	fastEvery = 2 * time.Second  // the app is on screen, as in the TUI
	slowEvery = 15 * time.Second // only notifications are watching
)

// Listener hears from a running client. Calls come from Go threads.
type Listener interface {
	// Changed delivers the latest snapshot as JSON whenever it changes.
	Changed(snapshot string)
	// Held announces a message newly waiting for you.
	Held(id, title, text string)
}

type Client struct {
	machine string
	log     *log.Logger

	mu      sync.Mutex
	m       *mesh.Mesh
	cancel  context.CancelFunc
	snap    held.Snapshot
	hist    map[string]held.Record // by key, from the last History
	every   time.Duration
	kick    chan struct{}
	stopped chan struct{}
}

// Open uses dir (the app's private files directory) for the identity,
// peers, config and state. machine is what this phone is listed as.
func Open(dir, machine string) (*Client, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// config reads these on every call; the phone has no home directory.
	os.Setenv("PEERBUS_CONFIG_DIR", filepath.Join(dir, "config"))
	os.Setenv("PEERBUS_STATE_DIR", filepath.Join(dir, "state"))
	return &Client{
		machine: identity.CleanName(machine),
		log:     log.New(os.Stderr, "peerbus ", 0), // gomobile sends stderr to logcat
		every:   slowEvery,
	}, nil
}

func store() identity.Store { return identity.Store{Dir: config.Dir()} }

// Paired reports whether this phone holds your identity yet.
func (c *Client) Paired() bool {
	if !store().HasIdentity() {
		return false
	}
	ps, err := store().Load()
	return err == nil && !ps.Self.Secret.IsZero()
}

// Fingerprint is your identity's fingerprint, as `peerbus id` shows it.
func (c *Client) Fingerprint() string {
	if !c.Paired() {
		return ""
	}
	key, err := store().Identity()
	if err != nil {
		return ""
	}
	return identity.Fingerprint(key.Public().(ed25519.PublicKey))
}

// Name is the name you go by.
func (c *Client) Name() string {
	ps, err := store().Load()
	if err != nil {
		return ""
	}
	return ps.SelfName()
}

// Pair redeems an invite from `peerbus phone` (or `peerbus invite -self`).
// It blocks until paired, for up to two minutes.
func (c *Client) Pair(code string) error {
	inv, err := pairing.Decode(code)
	if err != nil {
		return err
	}
	if !inv.Self {
		return errors.New("that's an invite for a friend; on your computer, run: peerbus phone")
	}
	c.Stop()
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// The phone is only ever your device, so it may replace an earlier pairing.
	_, err = pairing.Redeem(ctx, store(), code, "", true, pairing.Options{Server: cfg.Server(), STUN: cfg.STUN, Log: c.log})
	if err != nil {
		return err
	}
	return config.UpdateClient(func(cl *config.Client) {
		if inv.Server != "" {
			cl.Rendezvous = inv.Server
		}
		cl.Machine = c.machine
	})
}

// Forget removes your identity and peers from this phone, after telling
// your machines to stop pushing to it (one session hearing is enough:
// peers.json carries it to the rest).
func (c *Client) Forget() error {
	if snap := c.snapshot(); c.Running() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		tellOne(ctx, snap, map[string]string{"device": c.pushReg().Device, "name": c.machine})
		cancel()
	}
	c.Stop()
	return os.RemoveAll(config.Dir())
}

// tellOne registers meta with the first session that takes it.
func tellOne(ctx context.Context, snap held.Snapshot, meta map[string]string) error {
	err := errors.New("none of your sessions is online")
	for _, s := range snap.Sessions {
		rctx, cancel := context.WithTimeout(ctx, held.AskTimeout)
		err = s.Link.Request(rctx, proto.Frame{Type: proto.TPushRegister, Meta: meta}, nil)
		cancel()
		if err == nil {
			return nil
		}
	}
	return err
}

// pushReg is this phone's push registration: a stable device id, and its
// current FCM token.
type pushReg struct {
	Device string `json:"device"`
	Token  string `json:"token,omitempty"`
}

func pushPath() string { return filepath.Join(config.Dir(), "push.json") }

func (c *Client) pushReg() pushReg {
	var r pushReg
	config.ReadJSON(pushPath(), &r)
	if r.Device == "" {
		b := make([]byte, 8)
		rand.Read(b)
		r.Device = hex.EncodeToString(b)
		config.WriteJSON(pushPath(), &r)
	}
	return r
}

// SetPushToken records the phone's FCM token. Sessions learn it the next
// time the phone reaches them, which is right away when it's on the mesh.
func (c *Client) SetPushToken(token string) error {
	r := c.pushReg()
	if r.Token == token {
		return nil
	}
	r.Token = token
	if err := config.WriteJSON(pushPath(), &r); err != nil {
		return err
	}
	c.Refresh()
	return nil
}

// PushToken is the FCM token sessions are told about, if any.
func (c *Client) PushToken() string { return c.pushReg().Token }

// Unseal opens a push from one of your sessions. It returns the note as
// JSON: {"k": "held"|"gone", "id", "title", "text"}.
func (c *Client) Unseal(sealed string) (string, error) {
	ps, err := store().Load()
	if err != nil {
		return "", err
	}
	n, err := push.Open(ps.Self.Secret, sealed)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(n)
	return string(b), nil
}

// register tells your machines this phone's push token, once per run and
// whenever it changes: one session is enough, as peers.json carries it to
// the rest. It returns the token they know.
func (c *Client) register(ctx context.Context, snap held.Snapshot, registered string) string {
	r := c.pushReg()
	if r.Token == "" || r.Token == registered || len(snap.Sessions) == 0 {
		return registered
	}
	if err := tellOne(ctx, snap, map[string]string{"device": r.Device, "token": r.Token, "name": c.machine}); err != nil {
		c.log.Printf("registering for pushes: %v", err)
		return registered
	}
	return r.Token
}

// Start joins the mesh and watches every machine's held messages until
// Stop.
func (c *Client) Start(l Listener) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m != nil {
		return nil
	}
	if !c.Paired() {
		return errors.New("not paired yet")
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	if cfg.Server() == "" {
		return errors.New("no rendezvous server configured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m, err := mesh.Start(ctx, mesh.Config{
		Store:    store(),
		StateDir: config.StateDir(),
		Server:   cfg.Server(),
		STUN:     cfg.STUN,
		Kind:     proto.KindControl,
		// The phone is you: it sends as user/human, where replies come back.
		Info: proto.SessionInfo{SID: proto.NewID(8), Name: proto.Human, Host: cfg.MachineName()},
		Log:  c.log,
	})
	if err != nil {
		cancel()
		return err
	}
	c.m, c.cancel = m, cancel
	c.snap = held.Snapshot{}
	c.kick = make(chan struct{}, 1)
	c.stopped = make(chan struct{})
	go c.watch(ctx, m, l, c.kick, c.stopped)
	return nil
}

// Stop leaves the mesh.
func (c *Client) Stop() {
	c.mu.Lock()
	m, cancel, stopped := c.m, c.cancel, c.stopped
	c.m, c.cancel = nil, nil
	c.mu.Unlock()
	if m == nil {
		return
	}
	cancel()
	<-stopped
	m.Close()
}

// Running reports whether the client is on the mesh.
func (c *Client) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m != nil
}

// SetForeground refreshes every 2 seconds while the app is on screen, and
// less often while only notifications are watching.
func (c *Client) SetForeground(on bool) {
	c.mu.Lock()
	if on {
		c.every = fastEvery
	} else {
		c.every = slowEvery
	}
	c.mu.Unlock()
	if on {
		c.Refresh()
	}
}

// Refresh asks every machine again now.
func (c *Client) Refresh() {
	c.mu.Lock()
	kick := c.kick
	c.mu.Unlock()
	if kick != nil {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
}

func (c *Client) watch(ctx context.Context, m *mesh.Mesh, l Listener, kick <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	seen := map[string]bool{}
	registered := ""
	loaded, last := false, ""
	live := false
	for {
		snap := held.Fetch(ctx, m)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-m.Ready():
			live = true
		default:
		}
		c.mu.Lock()
		c.snap = snap
		every := c.every
		c.mu.Unlock()
		registered = c.register(ctx, snap, registered)
		// Only what arrives while the app is open: what was waiting before
		// was pushed when it arrived, and announcing it again on every
		// launch would bring back notifications you'd cleared.
		for _, it := range snap.Items {
			if seen[it.View.ID] {
				continue
			}
			seen[it.View.ID] = true
			if loaded {
				title, text := announce(it, snap)
				l.Held(it.View.ID, title, text)
			}
		}
		loaded = loaded || live
		if js := encode(snap, m.Views(), live, c.Name()); js != last {
			last = js
			l.Changed(js)
		}
		select {
		case <-ctx.Done():
			return
		case <-kick:
		case <-time.After(every):
		}
	}
}

func announce(it held.Item, snap held.Snapshot) (title, text string) {
	first, _, _ := strings.Cut(it.View.Text, "\n")
	if it.View.Fake {
		first = "(test) " + first
	}
	switch it.Section() {
	case held.ToRead:
		return "From " + it.View.From, first
	case held.Yours:
		return "Waiting to send to " + it.View.To, snap.Label(it.View.Target) + ": " + first
	case held.ForYou:
		return "From " + it.View.From + " · pick a session", first
	}
	return "From " + it.View.From + " → " + snap.Label(it.View.Target), first
}

// Approve delivers a held message where it was sent, or to session toSID
// when that isn't empty, or sends one of yours.
func (c *Client) Approve(id, toSID string) (string, error) {
	snap := c.snapshot()
	it, ok := snap.Find(id)
	if !ok {
		return "", errors.New("that message isn't waiting any more")
	}
	var to *held.Session
	if toSID != "" {
		se, ok := snap.Session(toSID)
		if !ok {
			return "", errors.New("that session isn't online any more")
		}
		to = &se
	}
	a, err := snap.Approve(it, to)
	if err != nil {
		return "", err
	}
	return c.run(a)
}

// Deny declines a held message; the sender is told.
func (c *Client) Deny(id string) (string, error) {
	snap := c.snapshot()
	it, ok := snap.Find(id)
	if !ok {
		return "", errors.New("that message isn't waiting any more")
	}
	a, err := snap.Deny(it)
	if err != nil {
		return "", err
	}
	return c.run(a)
}

func (c *Client) snapshot() held.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap
}

func (c *Client) run(a held.Action) (string, error) {
	text, err := a.Run(context.Background())
	c.Refresh()
	return text, err
}

type snapshotJSON struct {
	Live     bool          `json:"live"` // the first round of discovery is done
	Self     string        `json:"self"`
	Machines int           `json:"machines"`
	Items    []itemJSON    `json:"items"`
	Sessions []sessionJSON `json:"sessions"`
	// People are who you can write to: sessions, yours and friends', and
	// friends as people.
	People []proto.PeerView `json:"people"`
}

type itemJSON struct {
	proto.HeldView
	Section     int    `json:"section"`
	Machine     string `json:"machine"`
	TargetLabel string `json:"targetLabel"`
}

type sessionJSON struct {
	SID    string `json:"sid"`
	Name   string `json:"name"`
	Host   string `json:"host"`
	Status string `json:"status"`
	Cwd    string `json:"cwd"`
	Addr   string `json:"addr"`
}

func encode(snap held.Snapshot, views []proto.PeerView, live bool, self string) string {
	out := snapshotJSON{Live: live, Self: self, Machines: snap.Machines, Items: []itemJSON{}, Sessions: []sessionJSON{}, People: []proto.PeerView{}}
	for _, v := range views {
		if v.Name == proto.Human && !v.Foreign {
			continue // that's you
		}
		v.Resources = nil
		out.People = append(out.People, v)
	}
	for _, it := range snap.Items {
		out.Items = append(out.Items, itemJSON{HeldView: it.View, Section: int(it.Section()), Machine: it.Machine, TargetLabel: snap.Label(it.View.Target)})
	}
	for _, s := range snap.Sessions {
		out.Sessions = append(out.Sessions, sessionJSON{SID: s.Info.SID, Name: s.Info.Name, Host: s.Info.Host, Status: s.Info.Status, Cwd: s.Info.Cwd, Addr: s.Addr})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// Send writes text as you (user/human) to an address from the snapshot's
// people, continuing thread when it isn't empty. Replies come back to
// read in the inbox.
func (c *Client) Send(to, text, thread string) (string, error) {
	c.mu.Lock()
	m := c.m
	c.mu.Unlock()
	if m == nil {
		return "", errors.New("not on the mesh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := m.SendAsYou(ctx, to, text, thread)
	if err != nil {
		return "", err
	}
	c.Refresh()
	if res.Status == "held" {
		return "sent to " + res.To + "; it waits for them to approve it", nil
	}
	return "sent to " + res.To, nil
}

type recordJSON struct {
	proto.HistoryEntry
	Key      string   `json:"key"`
	Machines []string `json:"machines"`
}

// History is every message your sessions sent and received in the last
// 30 days, on every machine with a session online, newest first, as JSON:
// a list of history entries with "key" and "machines".
func (c *Client) History() (string, error) {
	c.mu.Lock()
	m := c.m
	c.mu.Unlock()
	if m == nil {
		return "", errors.New("not on the mesh")
	}
	recs := held.History(context.Background(), m)
	out := make([]recordJSON, 0, len(recs))
	byKey := make(map[string]held.Record, len(recs))
	for _, r := range recs {
		byKey[r.Key()] = r
		out = append(out, recordJSON{HistoryEntry: r.Entry, Key: r.Key(), Machines: r.Machines})
	}
	c.mu.Lock()
	c.hist = byKey
	c.mu.Unlock()
	b, _ := json.Marshal(out)
	return string(b), nil
}

// FullText is the whole of a message the history cut short.
func (c *Client) FullText(key string) (string, error) {
	c.mu.Lock()
	m, r, ok := c.m, c.hist[key], c.hist != nil
	c.mu.Unlock()
	if m == nil || !ok || r.Entry.ID == "" {
		return "", errors.New("that message isn't in the history any more")
	}
	e, err := r.Full(context.Background(), m)
	return e.Text, err
}
