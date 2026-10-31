package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vitaminmoo/peerbus/internal/held"
	"github.com/vitaminmoo/peerbus/internal/proto"
)

// historyEvery is how often the history page refreshes while it's shown.
const historyEvery = 3 * time.Second

type filter int

const (
	allMessages    filter = iota
	ownMessages           // between your own sessions
	friendMessages        // to or from someone you've paired with
)

var filterTitles = [...]string{"all messages", "between your sessions", "with friends"}

func (f filter) keeps(r record) bool {
	switch f {
	case ownMessages:
		return r.Entry.Peer == ""
	case friendMessages:
		return r.Entry.Peer != ""
	}
	return true
}

// record is one message, merged from every machine that logged it.
type record = held.Record

type (
	histTickMsg struct{}
	historyMsg  struct{ records []record }
	fullMsg     struct {
		key, text string
		err       error
	}
)

func histTick() tea.Cmd {
	return tea.Tick(historyEvery, func(time.Time) tea.Msg { return histTickMsg{} })
}

// turnPage switches between the inbox and the history.
func (m *model) turnPage() tea.Cmd {
	if m.page == historyPage {
		m.page = inboxPage
		return nil
	}
	m.page = historyPage
	if m.histPolling {
		return m.wantFull()
	}
	m.histPolling = true
	return m.fetchHistory()
}

// fetchHistory gathers every machine's history.
func (m *model) fetchHistory() tea.Cmd {
	return func() tea.Msg { return historyMsg{records: held.History(m.ctx, m.mesh)} }
}

func (m *model) applyHistory(h historyMsg) {
	keep := ""
	if r, ok := m.currentRecord(); ok {
		keep = r.Key()
	}
	m.hist, m.histLoaded = h.records, true
	m.placeHistoryCursor(keep)
}

// shown is the history under the current filter.
func (m *model) shown() []record {
	var out []record
	for _, r := range m.hist {
		if m.filter.keeps(r) {
			out = append(out, r)
		}
	}
	return out
}

func (m *model) currentRecord() (record, bool) {
	s := m.shown()
	if m.hcursor < 0 || m.hcursor >= len(s) {
		return record{}, false
	}
	return s[m.hcursor], true
}

// placeHistoryCursor keeps the cursor on the message with key, or within
// the list when it's gone.
func (m *model) placeHistoryCursor(key string) {
	s := m.shown()
	for i, r := range s {
		if r.Key() == key {
			m.hcursor = i
			return
		}
	}
	m.hcursor = max(0, min(m.hcursor, len(s)-1))
}

func (m *model) historyKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.shown())
	switch k.String() {
	case "q":
		return m, tea.Quit
	case "up", "k":
		m.hcursor = max(0, m.hcursor-1)
	case "down", "j":
		m.hcursor = max(0, min(n-1, m.hcursor+1))
	case "home", "g":
		m.hcursor = 0
	case "end", "G":
		m.hcursor = max(0, n-1)
	case "n":
		m.startCompose("", "")
	case "r":
		if r, ok := m.currentRecord(); ok {
			m.startCompose(m.replyTo(r.Entry), r.Entry.Thread)
		}
	case "f":
		keep := ""
		if r, ok := m.currentRecord(); ok {
			keep = r.Key()
		}
		m.filter = (m.filter + 1) % filter(len(filterTitles))
		m.placeHistoryCursor(keep)
	}
	return m, m.wantFull()
}

// wantFull fetches the whole text of the message under the cursor when the
// history only had its start.
func (m *model) wantFull() tea.Cmd {
	r, ok := m.currentRecord()
	if !ok || !r.Entry.Cut || m.asked[r.Key()] {
		return nil
	}
	m.asked[r.Key()] = true
	return func() tea.Msg {
		e, err := r.Full(m.ctx, m.mesh)
		return fullMsg{key: r.Key(), text: e.Text, err: err}
	}
}

// outbound reports whether one of our sessions sent the message.
func (m *model) outbound(e proto.HistoryEntry) bool {
	return strings.HasPrefix(e.From, m.self+"/")
}

// shortAddr drops the ref, and our own name, from an address for the list.
func (m *model) shortAddr(a string) string {
	if a == m.self+"/" || a == m.self+"/"+proto.Human {
		return "you"
	}
	a, _, _ = strings.Cut(a, "#")
	return strings.TrimPrefix(a, m.self+"/")
}

// statusText says what became of a message, from where you sit.
func (m *model) statusText(e proto.HistoryEntry) string {
	out := m.outbound(e)
	switch e.Status {
	case "delivered":
		return good.Render("delivered")
	case "held":
		if out {
			return amber.Render("held") + dim.Render(" · waiting for "+e.Peer+" to approve it")
		}
		return amber.Render("held") + dim.Render(" · waiting in your inbox (tab)")
	case "ask":
		return amber.Render("not sent") + dim.Render(" · waiting for your go-ahead (send=ask)")
	case "declined":
		if out {
			return bad.Render("declined") + dim.Render(" by "+e.Peer)
		}
		return bad.Render("declined") + dim.Render(" by you")
	case "unsent":
		return bad.Render("not sent") + dim.Render(" · you declined it")
	}
	return dim.Render(e.Status)
}
