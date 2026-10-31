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
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vitaminmoo/peerbus/internal/held"
	"github.com/vitaminmoo/peerbus/internal/mesh"
)

const refreshEvery = 2 * time.Second

// Run shows the TUI until the user quits.
func Run(ctx context.Context, m *mesh.Mesh, self string) error {
	p := tea.NewProgram(newModel(ctx, m, self), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

type (
	section = held.Section
	item    = held.Item
	session = held.Session
)

const (
	toRead     = held.ToRead
	forSession = held.ForSession
	forYou     = held.ForYou
	yours      = held.Yours
)

var sectionTitles = held.SectionTitles

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
	composing
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
	compose  *compose

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

// fetch asks every session of ours for its inbox.
func (m *model) fetch() tea.Cmd {
	return func() tea.Msg {
		snap := held.Fetch(m.ctx, m.mesh)
		return fetchedMsg{items: snap.Items, sessions: snap.Sessions, machines: snap.Machines}
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
			if m.mode == composing {
				return m, nil // keep what was written
			}
		} else {
			m.say(msg.text, false)
		}
		m.mode, m.compose = browsing, nil
		return m, m.fetch()
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *model) applyFetch(f fetchedMsg) {
	var keep string
	if it, ok := m.current(); ok {
		keep = it.View.ID
	}
	for _, it := range f.items {
		if !m.seen[it.View.ID] {
			m.seen[it.View.ID] = true
			if m.loaded && !it.View.Outbound {
				notify(it)
			}
		}
	}
	m.items, m.sessions, m.machines, m.loaded = f.items, f.sessions, f.machines, true
	m.cursor, m.pick = 0, min(m.pick, max(0, len(m.sessions)-1))
	found := false
	for i, it := range m.items {
		if it.View.ID == keep {
			m.cursor, found = i, true
		}
	}
	if m.mode != browsing && !found {
		m.mode = browsing // what we were acting on is gone
	}
}

func notify(it item) {
	if path, err := exec.LookPath("notify-send"); err == nil {
		first, _, _ := strings.Cut(it.View.Text, "\n")
		go exec.Command(path, "peerbus: message from "+it.View.From, first).Run()
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
	if m.mode == composing {
		return m.composeKey(k)
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
		if it.Section() == forYou {
			m.startRouting(it)
			break
		}
		return m, m.approve(it, session{})
	case "n":
		m.startCompose("", "")
	case "r":
		it, ok := m.current()
		if !ok || m.busy {
			break
		}
		if it.Section() == toRead {
			m.startCompose(it.View.From, it.View.Thread)
			break
		}
		if it.Section() == yours {
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
		if s.Info.SID == it.View.Target {
			m.pick = i
		}
	}
}

func (m *model) snapshot() held.Snapshot {
	return held.Snapshot{Items: m.items, Sessions: m.sessions, Machines: m.machines}
}

// approve acts on it: delivered where it was sent, or to s when routing,
// or for one of ours, sent.
func (m *model) approve(it item, to session) tea.Cmd {
	var dest *session
	if to.Link != nil {
		dest = &to
	}
	a, err := m.snapshot().Approve(it, dest)
	if err != nil {
		m.say(err.Error(), true)
		return nil
	}
	m.busy = true
	m.say("delivering…", false)
	return m.run(a)
}

func (m *model) deny(it item) tea.Cmd {
	a, err := m.snapshot().Deny(it)
	if err != nil {
		m.mode = browsing
		m.say(err.Error(), true)
		return nil
	}
	m.busy = true
	return m.run(a)
}

func (m *model) run(a held.Action) tea.Cmd {
	return func() tea.Msg {
		text, err := a.Run(m.ctx)
		return doneMsg{text: text, err: err}
	}
}

func (m *model) sessionLabel(sid string) string { return m.snapshot().Label(sid) }

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
