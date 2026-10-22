package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Cassette futurism, as on peerb.us: a beige plate, a four-colour stripe,
// hazard-striped section labels, amber phosphor for message text.
var (
	cBeige   = lipgloss.Color("#d6ceb6")
	cInk     = lipgloss.Color("#2c2a22")
	cAmber   = lipgloss.Color("#f3b44a")
	cAmberLo = lipgloss.Color("#a4803e")
	cOrange  = lipgloss.Color("#e2772b")
	cTeal    = lipgloss.Color("#3f9a8e")
	cRed     = lipgloss.Color("#c9443a")
	cMustard = lipgloss.Color("#d9a521")
	cGlitch  = lipgloss.Color("#7fe9d2")
	cDim     = lipgloss.Color("#8c8577")

	plate    = lipgloss.NewStyle().Background(cBeige).Foreground(cInk).Bold(true).Padding(0, 1)
	label    = lipgloss.NewStyle().Foreground(cDim).Bold(true)
	dim      = lipgloss.NewStyle().Foreground(cDim)
	amber    = lipgloss.NewStyle().Foreground(cAmber)
	selected = lipgloss.NewStyle().Background(cOrange).Foreground(cInk).Bold(true)
	good     = lipgloss.NewStyle().Foreground(cTeal).Bold(true)
	bad      = lipgloss.NewStyle().Foreground(cRed).Bold(true)
	keycap   = lipgloss.NewStyle().Background(cBeige).Foreground(cInk).Bold(true).Padding(0, 1)
	screen   = lipgloss.NewStyle().Border(lipgloss.ThickBorder()).BorderForeground(cBeige).Foreground(cAmber).Padding(0, 1)
)

var laneColour = [...]lipgloss.Color{cOrange, cMustard, cTeal}

// hazard draws n stripe cells in c.
func hazard(c lipgloss.Color, n int) string {
	return lipgloss.NewStyle().Foreground(c).Render(strings.Repeat("▚", n))
}

func stripe(w int) string {
	q := w / 4
	cols := []lipgloss.Color{cMustard, cOrange, cRed, cTeal}
	var b strings.Builder
	for i, c := range cols {
		n := q
		if i == 3 {
			n = w - 3*q
		}
		b.WriteString(lipgloss.NewStyle().Foreground(c).Render(strings.Repeat("▀", n)))
	}
	return b.String()
}

func (m *model) View() string {
	if m.width == 0 {
		return ""
	}
	w := m.width
	var out []string
	out = append(out, m.header(w), stripe(w))

	body := max(2, m.height-4) // header, stripe, status, footer
	box := func(w, h int) lipgloss.Style {
		return lipgloss.NewStyle().Width(w).Height(h).MaxWidth(w).MaxHeight(h)
	}
	if w >= 96 {
		lw := max(38, w*2/5)
		left := box(lw, body).Render(m.list(lw, body))
		right := box(w-lw-2, body).Render(m.detail(w-lw-2, body))
		out = append(out, lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right))
	} else {
		lh := max(1, min(body/2, max(6, body/3)))
		out = append(out, box(w, lh).Render(m.list(w, lh)), box(w, body-lh).Render(m.detail(w, body-lh)))
	}
	out = append(out, m.statusLine(w), m.footer(w))
	return strings.Join(out, "\n")
}

func (m *model) header(w int) string {
	title := hazard(cInk, 3) + " PEERBUS ▸ INBOX"
	left := plate.Render(title)
	sessions := len(m.sessions)
	live := good.Render("●") + " live"
	if !m.loaded {
		live = dim.Render(glitch(m.frame, 5)) + " tuning in"
	}
	right := dim.Render(fmt.Sprintf("%s · %d machine%s · %d session%s  ", m.self, m.machines, plural(m.machines), sessions, plural(sessions))) + live
	if lipgloss.Width(left)+lipgloss.Width(right)+1 > w {
		right = live
	}
	gap := max(1, w-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m *model) list(w, h int) string {
	var lines []string
	if !m.loaded {
		return "\n" + dim.Render("  "+glitch(m.frame, 12)+"  finding your sessions…")
	}
	if len(m.items) == 0 {
		return "\n" + amber.Render("  ░▒▓ ALL QUIET ▓▒░") + "\n\n" + dim.Render("  No held messages on any of your\n  machines. New ones appear here.")
	}
	cur := section(-1)
	for i, it := range m.items {
		if s := it.section(); s != cur {
			cur = s
			n := 0
			for _, x := range m.items {
				if x.section() == s {
					n++
				}
			}
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, hazard(laneColour[s], 2)+" "+label.Render(strings.ToUpper(sectionTitles[s]))+" "+dim.Render(fmt.Sprint(n)))
		}
		lines = append(lines, m.row(it, i == m.cursor, w))
	}
	// keep the cursor in view
	cl := 0
	for i, l := range lines {
		if strings.Contains(l, "▸") && i > 0 {
			cl = i
		}
	}
	if len(lines) > h && cl >= h {
		lines = lines[cl-h+1:]
	}
	return strings.Join(lines, "\n")
}

func (m *model) row(it item, sel bool, w int) string {
	var who string
	switch it.section() {
	case forSession:
		who = it.v.Peer + " → " + m.sessionLabel(it.v.Target)
	case forYou:
		who = it.v.Peer + " → you"
	case yours:
		who = m.sessionLabel(it.v.Target) + " → " + it.v.Peer
	}
	age := ago(it.v.HeldAt)
	marker := "  "
	if sel {
		marker = "▸ "
	}
	room := w - lipgloss.Width(marker) - len(age) - 2
	if lipgloss.Width(who) > room {
		who = truncate(who, room)
	}
	line := marker + who + strings.Repeat(" ", max(1, w-lipgloss.Width(marker)-lipgloss.Width(who)-len(age)-1)) + age + " "
	if sel {
		return selected.Render(line)
	}
	return line
}

func (m *model) detail(w, h int) string {
	it, ok := m.current()
	if !ok {
		return ""
	}
	if m.mode == routing {
		return m.picker(it, w)
	}
	field := func(k, v string) string { return label.Render(fmt.Sprintf("%-7s", k)) + v }
	var lines []string
	switch it.section() {
	case forSession:
		lines = append(lines, field("FROM", it.v.From), field("TO", it.v.To+dim.Render("  "+m.sessionLabel(it.v.Target))))
	case forYou:
		lines = append(lines, field("FROM", it.v.From), field("TO", "you, as a person "+dim.Render("· pick a session")))
	case yours:
		lines = append(lines, field("FROM", it.v.From), field("TO", it.v.To), field("", dim.Render("held by your send=ask setting for "+it.v.Peer)))
	}
	lines = append(lines, field("HELD", fmt.Sprintf("on %s · %s ago · thread %s", it.machine, ago(it.v.HeldAt), it.v.Thread)), "")
	text := lipgloss.NewStyle().Width(w - 4).Render(it.v.Text)
	tl := strings.Split(text, "\n")
	if room := h - len(lines) - 4; len(tl) > room && room > 1 {
		tl = append(tl[:room-1], dim.Render(fmt.Sprintf("… %d more lines", len(tl)-room+1)))
	}
	lines = append(lines, screen.Width(w-2).Render(strings.Join(tl, "\n")))
	if m.mode == confirmingDeny {
		lines = append(lines, "", bad.Render("Decline this message? ")+dim.Render(it.v.Peer+" is told.  ")+keycap.Render("y")+" yes  "+keycap.Render("any")+" no")
	}
	return strings.Join(lines, "\n")
}

func (m *model) picker(it item, w int) string {
	lines := []string{
		hazard(cTeal, 2) + " " + label.Render("ROUTE TO ONE OF YOUR SESSIONS"),
		dim.Render("from " + it.v.From),
		"",
	}
	host := ""
	for i, s := range m.sessions {
		if s.info.Host != host {
			host = s.info.Host
			lines = append(lines, label.Render("  "+strings.ToUpper(host)))
		}
		dot := good.Render("●")
		if s.info.Status != "idle" {
			dot = lipgloss.NewStyle().Foreground(cOrange).Render("●")
		}
		line := fmt.Sprintf("%-18s %-6s %s", truncate(s.info.Name, 18), s.info.Status, s.info.Cwd)
		line = truncate(line, w-6)
		if i == m.pick {
			lines = append(lines, "  "+dot+" "+selected.Render(line))
		} else {
			lines = append(lines, "  "+dot+" "+line)
		}
	}
	return strings.Join(lines, "\n")
}

func (m *model) statusLine(w int) string {
	if m.status == "" {
		return ""
	}
	s := good
	if m.bad {
		s = bad
	}
	return s.Render(truncate(" "+m.status, w))
}

func (m *model) footer(w int) string {
	k := func(key, what string) string { return keycap.Render(key) + " " + dim.Render(what) + "  " }
	var s string
	switch m.mode {
	case routing:
		s = k("↑↓", "pick") + k("enter", "deliver there") + k("esc", "back")
	case confirmingDeny:
		s = k("y", "decline") + k("any", "keep")
	default:
		s = k("↑↓", "move") + k("a", "approve") + k("r", "route") + k("d", "decline") + k("q", "quit")
	}
	return truncate(s, w)
}

// glitch is a row of flickering block glyphs, like the invite codes on the
// site.
func glitch(frame, n int) string {
	g := []rune("░▒▓█▖▗▘▝▚▞")
	var b strings.Builder
	for i := range n {
		b.WriteRune(g[(frame*7+i*13+i*i)%len(g)])
	}
	return lipgloss.NewStyle().Foreground(cGlitch).Render(b.String())
}

func truncate(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
