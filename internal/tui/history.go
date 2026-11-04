package tui

import (
	"fmt"
	"slices"
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
	historyMsg  struct {
		records []record
		gen     int
	}
	fullMsg struct {
		key, text string
		err       error
	}
	deletedMsg struct {
		thread string // a ThreadKey
		kept   int
		err    error
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
	gen := m.hgen
	return func() tea.Msg { return historyMsg{records: held.History(m.ctx, m.mesh), gen: gen} }
}

func (m *model) applyHistory(h historyMsg) {
	if h.gen != m.hgen {
		return // from before a deletion
	}
	keep, _ := m.currentRecord()
	m.hist, m.histLoaded = h.records, true
	if m.threadCount(m.hthread) == 0 {
		m.hthread = "" // it was deleted, perhaps from elsewhere
	}
	m.placeHistoryCursor(keep)
}

// shown is what the history page lists: with a conversation open, its
// messages oldest first; otherwise each conversation under the current
// filter, by its newest message, newest first.
func (m *model) shown() []record {
	var out []record
	if m.hthread != "" {
		for _, r := range m.hist {
			if r.ThreadKey() == m.hthread {
				out = append(out, r)
			}
		}
		slices.Reverse(out)
		return out
	}
	seen := map[string]bool{}
	for _, r := range m.hist {
		if k := r.ThreadKey(); m.filter.keeps(r) && !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// threadCount is how many messages a conversation has.
func (m *model) threadCount(key string) int {
	n := 0
	for _, r := range m.hist {
		if r.ThreadKey() == key {
			n++
		}
	}
	return n
}

// deleteThread forgets the conversation everywhere it can reach.
func (m *model) deleteThread(r record) tea.Cmd {
	m.busy = true
	m.say("deleting…", false)
	key := r.ThreadKey()
	return func() tea.Msg {
		kept, err := held.DeleteThread(m.ctx, m.mesh, r.Entry.Thread)
		return deletedMsg{thread: key, kept: kept, err: err}
	}
}

func (m *model) applyDeleted(d deletedMsg) {
	m.busy, m.mode = false, browsing
	if d.err != nil {
		m.say("couldn't delete the thread: "+d.err.Error(), true)
		return
	}
	switch d.kept {
	case 0:
		m.say("thread deleted", false)
	default:
		m.say(fmt.Sprintf("thread deleted, except %d waiting for a decision in the inbox", d.kept), false)
	}
	// Off the page right away; the next fetch brings back anything that stayed.
	m.hgen++
	m.hist = slices.DeleteFunc(slices.Clone(m.hist), func(r record) bool { return r.ThreadKey() == d.thread })
	if m.hthread == d.thread {
		m.hthread = ""
	}
	m.hcursor = max(0, min(m.hcursor, len(m.shown())-1))
}

func (m *model) currentRecord() (record, bool) {
	s := m.shown()
	if m.hcursor < 0 || m.hcursor >= len(s) {
		return record{}, false
	}
	return s[m.hcursor], true
}

// placeHistoryCursor keeps the cursor on the message keep, or on its
// conversation when listing those (a new message changes which one stands
// for it), or within the list when it's gone.
func (m *model) placeHistoryCursor(keep record) {
	s := m.shown()
	for i, r := range s {
		if r.Key() == keep.Key() || m.hthread == "" && keep.Entry.ID != "" && r.ThreadKey() == keep.ThreadKey() {
			m.hcursor = i
			return
		}
	}
	m.hcursor = max(0, min(m.hcursor, len(s)-1))
}

func (m *model) historyKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mode == confirmingDelete {
		r, ok := m.currentRecord()
		if ok && !m.busy && (k.String() == "y" || k.String() == "Y") {
			return m, m.deleteThread(r)
		}
		m.mode = browsing
		m.say("not deleted", false)
		return m, nil
	}
	n := len(m.shown())
	switch k.String() {
	case "q":
		return m, tea.Quit
	case "enter":
		if r, ok := m.currentRecord(); ok && m.hthread == "" {
			m.hthread = r.ThreadKey()
			m.hcursor = max(0, len(m.shown())-1) // the newest, where it was
		}
	case "esc":
		if m.hthread != "" {
			keep, _ := m.currentRecord()
			m.hthread = ""
			m.placeHistoryCursor(keep)
		}
	case "d", "x":
		if _, ok := m.currentRecord(); ok && !m.busy {
			m.mode = confirmingDelete
		}
	case "up", "k":
		m.hcursor = max(0, m.hcursor-1)
	case "down", "j":
		m.hcursor = max(0, min(n-1, m.hcursor+1))
	case "home", "g":
		m.hcursor = 0
	case "end", "G":
		m.hcursor = max(0, n-1)
	case "n":
		m.startCompose("", "", "")
	case "r":
		if r, ok := m.currentRecord(); ok {
			m.startCompose(m.replyTo(r.Entry), r.Entry.Thread, r.Entry.ID)
		}
	case "f":
		if m.hthread == "" {
			keep, _ := m.currentRecord()
			m.filter = (m.filter + 1) % filter(len(filterTitles))
			m.placeHistoryCursor(keep)
		}
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
