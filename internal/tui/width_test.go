package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestWidths(t *testing.T) {
	for _, w := range []int{60, 72, 95, 96, 120, 200} {
		for _, h := range []int{14, 30} {
			m := sample()
			m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			for _, mode := range []mode{browsing, routing, confirmingDeny} {
				m.mode = mode
				v := m.View()
				lines := strings.Split(v, "\n")
				for i, l := range lines {
					if lw := lipgloss.Width(l); lw > w {
						t.Errorf("w=%d h=%d mode=%d line %d is %d wide", w, h, mode, i, lw)
					}
				}
				if len(lines) > h {
					t.Errorf("w=%d h=%d mode=%d: %d lines", w, h, mode, len(lines))
				}
			}
		}
	}
}
