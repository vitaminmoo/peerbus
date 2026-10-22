package tui

import (
	"os"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestDump writes a full-colour render for the website when
// PEERBUS_TUI_DUMP_FILE names a file.
func TestDump(t *testing.T) {
	path := os.Getenv("PEERBUS_TUI_DUMP_FILE")
	if path == "" {
		t.Skip("set PEERBUS_TUI_DUMP_FILE to write a render")
	}
	lipgloss.SetColorProfile(termenv.TrueColor)
	m := sample()
	m.Update(tea.WindowSizeMsg{Width: 96, Height: 18})
	m.say("delivered to yourname/notes#4be1c2", false)
	if err := os.WriteFile(path, []byte(m.View()), 0o644); err != nil {
		t.Fatal(err)
	}
}
