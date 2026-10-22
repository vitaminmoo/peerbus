package channel

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// registry is the subset of Claude Code's per-session registry file
// ($CLAUDE_CONFIG_DIR/sessions/<pid>.json) that we read. The file is an
// internal detail of Claude Code, so everything here degrades to defaults
// when it is missing or changes shape.
type registry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"`
}

func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// claudeJSONPath is where Claude Code keeps user and per-project MCP config.
func claudeJSONPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

func accountLabel() string {
	return filepath.Base(claudeConfigDir())
}

func parentPID(pid int) int {
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		// "pid (comm) state ppid ..."; comm may contain spaces and parens.
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 {
			if f := strings.Fields(s[i+1:]); len(f) >= 2 {
				ppid, _ := strconv.Atoi(f[1])
				return ppid
			}
		}
		return 0
	}
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	ppid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return ppid
}

// findRegistry walks up our process ancestry looking for the Claude Code
// process that spawned us and returns its registry file.
func findRegistry() string {
	if p := os.Getenv("PEERBUS_REGISTRY_FILE"); p != "" {
		return p
	}
	dir := filepath.Join(claudeConfigDir(), "sessions")
	pid := os.Getppid()
	for range 8 {
		if pid <= 1 {
			return ""
		}
		path := filepath.Join(dir, strconv.Itoa(pid)+".json")
		if _, err := os.Stat(path); err == nil {
			return path
		}
		pid = parentPID(pid)
	}
	return ""
}

func waitRegistry(ctx context.Context, d time.Duration) string {
	deadline := time.Now().Add(d)
	for {
		if p := findRegistry(); p != "" || time.Now().After(deadline) {
			return p
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func readRegistry(path string) (registry, bool) {
	var r registry
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &r) != nil {
		return r, false
	}
	return r, true
}
