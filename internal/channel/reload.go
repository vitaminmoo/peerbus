package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vitaminmoo/peerbus/internal/config"
	"github.com/vitaminmoo/peerbus/internal/proto"
)

// A reload replaces this process with the peerbus binary on disk, in place:
// same PID, same stdin and stdout, so Claude Code never notices its MCP
// server went anywhere and no /mcp reconnect is needed. The new process
// rejoins the mesh, picking up from a file named by resumeEnv.
const (
	resumeEnv = "PEERBUS_RESUME"
	// ResumeProbe is the subcommand that says a binary can take over, and
	// ResumeMagic what it prints first. Without the check, replacing
	// ourselves with an older binary would leave a server waiting forever
	// for a handshake that already happened.
	ResumeProbe = "resume-check"
	ResumeMagic = "peerbus-resume-1"

	probeWait = 5 * time.Second
	drainWait = 20 * time.Second
)

// resumeState is what the next process needs and can't work out itself.
type resumeState struct {
	SID       string           `json:"sid"`
	Started   int64            `json:"started"`
	Resources []proto.Resource `json:"resources,omitempty"` // from the advertise tool
	Pending   []byte           `json:"pending,omitempty"`   // a request read in part
}

// takeResume reads and removes the state an earlier process left, if this
// process is a reload. The handshake happened either way, so a state that
// can't be read only loses what was in it.
func takeResume(lg interface{ Printf(string, ...any) }) (resumeState, bool) {
	path := os.Getenv(resumeEnv)
	if path == "" {
		return resumeState{}, false
	}
	os.Unsetenv(resumeEnv)
	var st resumeState
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &st)
		os.Remove(path)
	}
	if err != nil {
		lg.Printf("reading what the last process left: %v", err)
	}
	return st, true
}

// diskBinary is the binary to reload into: what Claude Code would start
// now, which a symlink or PATH may point somewhere new, or failing that,
// wherever the running one was installed.
func diskBinary() (string, error) {
	if p, err := exec.LookPath(os.Args[0]); err == nil && filepath.IsAbs(p) {
		return p, nil
	}
	return os.Executable()
}

// probe checks the binary at path can run here and can take over.
func probe(path string) (version string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, ResumeProbe).Output()
	if err != nil {
		return "", fmt.Errorf("%s can't take over from a running session (is it older than this one?): %w", path, err)
	}
	magic, version, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if magic != ResumeMagic {
		return "", fmt.Errorf("%s can't take over from a running session", path)
	}
	return version, nil
}

// onReload answers a request to reload. Everything that can be refused is
// refused here, where the requester hears about it.
func (c *Channel) onReload() (proto.ReloadResult, func(), error) {
	select {
	case <-c.srv.Ready():
	default:
		return proto.ReloadResult{}, nil, errors.New("still connecting to Claude Code")
	}
	path, err := diskBinary()
	if err != nil {
		return proto.ReloadResult{}, nil, err
	}
	onDisk, err := os.Stat(path)
	if err != nil {
		return proto.ReloadResult{}, nil, err
	}
	if running, err := os.Stat("/proc/self/exe"); err == nil && os.SameFile(onDisk, running) {
		return proto.ReloadResult{Status: "current", Version: c.srv.Version}, nil, nil
	}
	version, err := probe(path)
	if err != nil {
		return proto.ReloadResult{}, nil, err
	}
	if !c.reloading.CompareAndSwap(false, true) {
		return proto.ReloadResult{Status: "reloading", Version: version}, nil, nil
	}
	return proto.ReloadResult{Status: "reloading", Version: version}, func() {
		defer c.reloading.Store(false)
		if err := c.reload(path); err != nil {
			c.log.Printf("reload: %v", err)
		}
	}, nil
}

// reload replaces this process, and returns only if it couldn't, having
// put things back. Order matters: tool calls finish on the mesh before it
// closes, and what arrived before it closed is told to Claude Code before
// the process goes.
func (c *Channel) reload(path string) error {
	pending, err := c.srv.Drain(drainWait)
	if err != nil {
		return err
	}
	defer c.srv.Resume()

	c.mu.Lock()
	st := resumeState{SID: c.info.SID, Started: c.info.Started, Resources: c.agentRes, Pending: pending}
	m := c.mesh
	c.mu.Unlock()
	watches := m.TakeIdleWatches()
	m.Close()
	for _, addr := range watches {
		c.onNotice("idle", fmt.Sprintf("peerbus reloaded, ending your subscription to %s. Subscribe again if you still want to know when it's idle.", addr),
			map[string]string{"peer": addr, "state": "reloaded"})
	}
	c.stopPump()

	stateFile, err := writeResume(st)
	if err == nil {
		env := append(slicesWithout(os.Environ(), resumeEnv), resumeEnv+"="+stateFile)
		unfreeze := c.srv.Freeze()
		err = syscall.Exec(path, os.Args, env) // only returns on failure
		unfreeze()
		os.Remove(stateFile)
	}
	go c.pump(c.ctx)
	c.join(c.ctx)
	return fmt.Errorf("couldn't replace this process, so it carries on: %w", err)
}

func writeResume(st resumeState) (string, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(config.StateDir(), "resume-*.json") // 0600
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func slicesWithout(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return out
}
