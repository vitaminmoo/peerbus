package transport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// Same-machine discovery: each session writes a small file naming its port
// and meeting points, so sessions on one machine find each other with no
// server. The directory should be private to the OS user.

type localEntry struct {
	Handle Handle  `json:"handle"`
	Port   int     `json:"port"`
	PID    int     `json:"pid"`
	Points []Point `json:"points"`
}

func (n *Node) localPath() string {
	if n.cfg.LocalDir == "" {
		return ""
	}
	return filepath.Join(n.cfg.LocalDir, n.handle.String()+"-"+hexString(n.handle[4:8])+".json")
}

func hexString(b []byte) string { return string(hexText(b)) }

func (n *Node) writeLocal() {
	path := n.localPath()
	if path == "" {
		return
	}
	n.mu.Lock()
	e := localEntry{Handle: n.handle, Port: n.Port(), PID: os.Getpid(), Points: n.points}
	n.mu.Unlock()
	if err := os.MkdirAll(n.cfg.LocalDir, 0o700); err != nil {
		n.log.Printf("local discovery dir: %v", err)
		return
	}
	b, _ := json.Marshal(e)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, path)
	}
}

func (n *Node) removeLocal() {
	if path := n.localPath(); path != "" {
		os.Remove(path)
	}
}

func alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func (n *Node) lookupLocal(p Point) []Peer {
	if n.cfg.LocalDir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(n.cfg.LocalDir, "*.json"))
	var out []Peer
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var e localEntry
		if json.Unmarshal(b, &e) != nil || e.Handle == n.handle {
			continue
		}
		if !alive(e.PID) {
			os.Remove(f) // left behind by a session that died
			continue
		}
		if slices.Contains(e.Points, p) {
			out = append(out, Peer{Handle: e.Handle, Local: true, port: e.Port})
		}
	}
	return out
}
