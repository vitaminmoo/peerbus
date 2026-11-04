package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/vitaminmoo/peerbus/internal/proto"
)

// compose is writing a message as you (user/human) to any session you can
// reach, yours or a friend's, or to a friend as a person.
type compose struct {
	to      string // empty while picking
	thread  string // set when replying
	replyTo string // the message replied to
	targets []proto.PeerView
	pick    int
	area    textarea.Model
}

// startCompose opens the writer: to a picked session, or straight to to
// when replying to the message replyTo in thread.
func (m *model) startCompose(to, thread, replyTo string) {
	a := textarea.New()
	a.Placeholder = "write as " + m.self + "/" + proto.Human + "…"
	a.ShowLineNumbers = false
	a.Prompt = "┃ "
	a.CharLimit = proto.MaxText
	a.Cursor.SetMode(cursor.CursorStatic)
	a.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	a.FocusedStyle.CursorLine = lipgloss.NewStyle()
	a.FocusedStyle.Text = amber
	a.Focus()
	c := &compose{to: to, thread: thread, replyTo: replyTo, area: a}
	if to == "" {
		for _, v := range m.mesh.Views() {
			if v.Address != m.self+"/"+proto.Human {
				c.targets = append(c.targets, v)
			}
		}
		if len(c.targets) == 0 {
			m.say("nobody is reachable to write to", true)
			return
		}
	}
	m.compose, m.mode = c, composing
}

// replyTo is who a reply to e goes to: whoever isn't us.
func (m *model) replyTo(e proto.HistoryEntry) string {
	if m.outbound(e) || e.From == m.self+"/"+proto.Human {
		return e.To
	}
	return e.From
}

func (m *model) composeKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	c := m.compose
	if k.String() == "esc" {
		m.mode, m.compose = browsing, nil
		return m, nil
	}
	if c.to == "" {
		switch k.String() {
		case "up", "k":
			c.pick = max(0, c.pick-1)
		case "down", "j":
			c.pick = min(len(c.targets)-1, c.pick+1)
		case "enter":
			c.to = c.targets[c.pick].Address
		}
		return m, nil
	}
	if k.String() == "enter" {
		text := strings.TrimSpace(c.area.Value())
		if text == "" || m.busy {
			return m, nil
		}
		m.busy = true
		m.say("sending…", false)
		to, thread, replyTo := c.to, c.thread, c.replyTo
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
			defer cancel()
			res, err := m.mesh.SendAsYou(ctx, to, text, thread, replyTo)
			if err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{text: describeSent(res)}
		}
	}
	var cmd tea.Cmd
	c.area, cmd = c.area.Update(k)
	return m, cmd
}

// describeSent says what became of a message you wrote.
func describeSent(res proto.SendResult) string {
	if res.Status == "held" {
		return "sent to " + res.To + "; it waits for them to approve it"
	}
	return "sent to " + res.To
}

func (m *model) composeView(w, h int) string {
	c := m.compose
	if c.to == "" {
		lines := []string{hazard(cTeal, 2) + " " + label.Render("WRITE TO"), dim.Render("as " + m.self + "/" + proto.Human + ", so replies come back to you"), ""}
		user := ""
		for i, v := range c.targets {
			if v.User != user {
				user = v.User
				lines = append(lines, label.Render("  "+strings.ToUpper(user)))
			}
			line := fmt.Sprintf("%-18s %-7s %s", truncate(v.Name, 18), v.Status, v.Host)
			if v.Version != "" {
				line += "  " + v.Version
			}
			if v.Channels != proto.ChannelsOn {
				line += "  channels unverified"
			}
			if v.Name == proto.Human {
				line = fmt.Sprintf("%-18s %s", v.Name, "the person, to read")
			}
			line = truncate(line, w-6)
			if i == c.pick {
				line = selected.Render(line)
			}
			lines = append(lines, "    "+line)
		}
		return strings.Join(lines, "\n")
	}
	title := "WRITE TO " + c.to
	if c.thread != "" {
		title = "REPLY TO " + c.to
	}
	c.area.SetWidth(max(10, w-2))
	c.area.SetHeight(max(3, min(10, h-6)))
	lines := []string{
		hazard(cTeal, 2) + " " + label.Render(truncate(title, w-4)),
		dim.Render("as " + m.self + "/" + proto.Human),
		"",
		c.area.View(),
	}
	return strings.Join(lines, "\n")
}
