// Package tui is `peerbus tui`: the human's view of every held message on
// every one of their machines. Messages for a session can be approved
// there or routed to another session; messages sent to you as a person get
// a session picked; your own send=ask messages get a go-ahead. Nothing here
// reaches an agent's context until you act. A second page shows the
// history of every message your sessions sent and received.
package tui

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vitaminmoo/peerbus/internal/mesh"
	"github.com/vitaminmoo/peerbus/internal/proto"
)

const (
	refreshEvery = 2 * time.Second
	askTimeout   = 4 * time.Second
)

// Run shows the TUI until the user quits.
func Run(ctx context.Context, m *mesh.Mesh, self string) error {
	p := tea.NewProgram(newModel(ctx, m, self), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

type section int

const (
	forSession section = iota // held for a particular session
	forYou                    // sent to you as a person: pick a session
	yours                     // ours, waiting on send=ask
)

var sectionTitles = [...]string{"held for a session", "for you · pick a session", "yours · waiting to send"}

type item struct {
	v       proto.HeldView
	machine string
	inbox   string       // which inbox holds it
	via     []*mesh.Link // sessions sharing that inbox
}

func (it item) section() section {
	switch {
	case it.v.Outbound:
		return yours
	case it.v.Routed:
		return forYou
	}
	return forSession
}

type session struct {
	link *mesh.Link
	info proto.SessionInfo
	addr string
}

type page int

const (
	inboxPage page = iota
	historyPage
)

type mode int

const (
	browsing mode = iota
	routing
	confirmingDeny
)

type model struct {
	ctx  context.Context
	mesh *mesh.Mesh
	self string

	items    []item
	sessions []session
	machines int
	cursor   int
	pick     int // routing: chosen session
	mode     mode

	page page

	// history page
	hist        []record
	hcursor     int // into shown()
	filter      filter
	histLoaded  bool
	histPolling bool
	full        map[string]string // whole texts of messages cut short, by key
	asked       map[string]bool   // whole texts requested

	seen    map[string]bool // held ids already shown, for notifications
	loaded  bool
	busy    bool // an action is in flight
	status  string
	bad     bool
	statusT time.Time
	frame   int

	width, height int
}

func newModel(ctx context.Context, m *mesh.Mesh, self string) *model {
	return &model{ctx: ctx, mesh: m, self: self, seen: map[string]bool{}, full: map[string]string{}, asked: map[string]bool{}}
}

type (
	tickMsg    struct{}
	animMsg    struct{}
	fetchedMsg struct {
		items    []item
		sessions []session
		machines int
	}
	doneMsg struct {
		text string
		err  error
	}
)

func (m *model) Init() tea.Cmd { return tea.Batch(m.fetch(), anim()) }

func tick() tea.Cmd { return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} }) }
func anim() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return animMsg{} })
}

// askAll sends f to every session in own and collects the answers by
// inbox: sessions on one machine share one, so the first answer per inbox
// wins, and byInbox lists the sessions that can act on each.
func askAll[T any](ctx context.Context, own []*mesh.Link, f proto.Frame, inboxOf func(T) string) (views map[string]T, byInbox map[string][]*mesh.Link) {
	views, byInbox = map[string]T{}, map[string][]*mesh.Link{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, l := range own {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, askTimeout)
			defer cancel()
			var v T
			if err := l.Request(ctx, f, &v); err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			id := inboxOf(v)
			byInbox[id] = append(byInbox[id], l)
			if _, ok := views[id]; !ok {
				views[id] = v
			}
		}()
	}
	wg.Wait()
	return views, byInbox
}

// fetch asks every session of ours for its inbox.
func (m *model) fetch() tea.Cmd {
	return func() tea.Msg {
		own := m.mesh.Own()
		var sessions []session
		for _, l := range own {
			sessions = append(sessions, session{link: l, info: l.Info(), addr: l.Address()})
		}
		views, byInbox := askAll(m.ctx, own, proto.Frame{Type: proto.TInbox}, func(v proto.InboxView) string { return v.InboxID })
		var items []item
		for id, v := range views {
			for _, h := range v.Held {
				items = append(items, item{v: h, machine: v.Machine, inbox: id, via: byInbox[id]})
			}
		}
		sort.Slice(items, func(i, j int) bool {
			if a, b := items[i].section(), items[j].section(); a != b {
				return a < b
			}
			return items[i].v.HeldAt > items[j].v.HeldAt
		})
		sort.Slice(sessions, func(i, j int) bool {
			if sessions[i].info.Host != sessions[j].info.Host {
				return sessions[i].info.Host < sessions[j].info.Host
			}
			return sessions[i].addr < sessions[j].addr
		})
		return fetchedMsg{items: items, sessions: sessions, machines: len(views)}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case animMsg:
		m.frame++
		return m, anim()
	case tickMsg:
		return m, m.fetch()
	case fetchedMsg:
		m.applyFetch(msg)
		return m, tick()
	case histTickMsg:
		if m.page != historyPage {
			m.histPolling = false
			return m, nil
		}
		return m, m.fetchHistory()
	case historyMsg:
		m.applyHistory(msg)
		if m.page != historyPage {
			m.histPolling = false
			return m, nil
		}
		return m, tea.Batch(histTick(), m.wantFull())
	case fullMsg:
		if msg.err != nil {
			delete(m.asked, msg.key)
			m.say("couldn't load the whole message: "+msg.err.Error(), true)
		} else {
			m.full[msg.key] = msg.text
		}
	case doneMsg:
		m.busy = false
		if msg.err != nil {
			m.say(msg.err.Error(), true)
		} else {
			m.say(msg.text, false)
		}
		m.mode = browsing
		return m, m.fetch()
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *model) applyFetch(f fetchedMsg) {
	var keep string
	if it, ok := m.current(); ok {
		keep = it.v.ID
	}
	for _, it := range f.items {
		if !m.seen[it.v.ID] {
			m.seen[it.v.ID] = true
			if m.loaded && !it.v.Outbound {
				notify(it)
			}
		}
	}
	m.items, m.sessions, m.machines, m.loaded = f.items, f.sessions, f.machines, true
	m.cursor, m.pick = 0, min(m.pick, max(0, len(m.sessions)-1))
	found := false
	for i, it := range m.items {
		if it.v.ID == keep {
			m.cursor, found = i, true
		}
	}
	if m.mode != browsing && !found {
		m.mode = browsing // what we were acting on is gone
	}
}

func notify(it item) {
	if path, err := exec.LookPath("notify-send"); err == nil {
		first, _, _ := strings.Cut(it.v.Text, "\n")
		go exec.Command(path, "peerbus: message from "+it.v.From, first).Run()
	}
}

func (m *model) current() (item, bool) {
	if m.cursor < 0 || m.cursor >= len(m.items) {
		return item{}, false
	}
	return m.items[m.cursor], true
}

func (m *model) current2() item { it, _ := m.current(); return it }

func (m *model) say(s string, bad bool) { m.status, m.bad, m.statusT = s, bad, time.Now() }

func (m *model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if k.String() == "tab" && m.mode == browsing {
		return m, m.turnPage()
	}
	if m.page == historyPage {
		return m.historyKey(k)
	}
	switch m.mode {
	case routing:
		switch k.String() {
		case "esc", "q":
			m.mode = browsing
		case "up", "k":
			m.pick = max(0, m.pick-1)
		case "down", "j":
			m.pick = min(len(m.sessions)-1, m.pick+1)
		case "enter", "a":
			if m.pick >= 0 && m.pick < len(m.sessions) {
				return m, m.approve(m.current2(), m.sessions[m.pick])
			}
		}
		return m, nil
	case confirmingDeny:
		switch k.String() {
		case "y", "Y":
			return m, m.deny(m.current2())
		default:
			m.mode = browsing
			m.say("not declined", false)
		}
		return m, nil
	}
	switch k.String() {
	case "q":
		return m, tea.Quit
	case "up", "k":
		m.cursor = max(0, m.cursor-1)
	case "down", "j":
		m.cursor = min(len(m.items)-1, m.cursor+1)
	case "a", "enter":
		it, ok := m.current()
		if !ok || m.busy {
			break
		}
		if it.section() == forYou {
			m.startRouting(it)
			break
		}
		return m, m.approve(it, session{})
	case "r":
		it, ok := m.current()
		if !ok || m.busy {
			break
		}
		if it.section() == yours {
			m.say("your own messages go where you sent them; approve or decline", true)
			break
		}
		m.startRouting(it)
	case "d", "x":
		if _, ok := m.current(); ok && !m.busy {
			m.mode = confirmingDeny
		}
	}
	return m, nil
}

func (m *model) startRouting(it item) {
	if len(m.sessions) == 0 {
		m.say("none of your sessions are online to route to", true)
		return
	}
	m.mode, m.pick = routing, 0
	for i, s := range m.sessions {
		if s.info.SID == it.v.Target {
			m.pick = i
		}
	}
}

// approve acts on it: delivered where it was sent, or to s when routing,
// or for one of ours, sent.
func (m *model) approve(it item, to session) tea.Cmd {
	var via *mesh.Link
	frame := proto.Frame{Type: proto.TApprove, ID: it.v.ID}
	switch {
	case it.v.Outbound:
		via = m.sessionLink(it.v.Target)
		if via == nil {
			m.say("the session that wrote this isn't running any more; decline it", true)
			return nil
		}
	case to.link != nil:
		frame.To = to.info.SID
	default:
		if m.sessionLink(it.v.Target) == nil {
			m.say("that session is gone; press r to route it to another", true)
			return nil
		}
		frame.To = it.v.Target
	}
	if via == nil {
		via = pickVia(it, it.v.Target)
	}
	if via == nil {
		m.say("no session on "+it.machine+" is online to hand it over", true)
		return nil
	}
	m.busy = true
	m.say("delivering…", false)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
		defer cancel()
		var res proto.SendResult
		if err := via.Request(ctx, frame, &res); err != nil {
			return doneMsg{err: err}
		}
		if it.v.Outbound {
			return doneMsg{text: "sent to " + res.To}
		}
		return doneMsg{text: "delivered to " + res.To}
	}
}

func (m *model) deny(it item) tea.Cmd {
	via := pickVia(it, it.v.Target)
	if via == nil {
		m.mode = browsing
		m.say("no session on "+it.machine+" is online to decline it", true)
		return nil
	}
	m.busy = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		if err := via.Request(ctx, proto.Frame{Type: proto.TDeny, ID: it.v.ID}, nil); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: "declined; " + it.v.Peer + " is told"}
	}
}

// pickVia chooses a session on the holding machine, preferring sid.
func pickVia(it item, sid string) *mesh.Link {
	for _, l := range it.via {
		if l.Info().SID == sid {
			return l
		}
	}
	if len(it.via) > 0 {
		return it.via[0]
	}
	return nil
}

func (m *model) sessionLink(sid string) *mesh.Link {
	for _, s := range m.sessions {
		if s.info.SID == sid {
			return s.link
		}
	}
	return nil
}

func (m *model) sessionLabel(sid string) string {
	for _, s := range m.sessions {
		if s.info.SID == sid {
			return s.info.Name + "@" + s.info.Host
		}
	}
	return "(gone)"
}

func ago(unix int64) string {
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
