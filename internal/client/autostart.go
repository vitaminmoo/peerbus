package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/vitaminmoo/peerbus/internal/config"
)

// AutostartLocal returns an Autostart func that launches `peerbus broker` in
// its own session when url points at this machine and a broker config
// exists here, or nil when autostart doesn't apply.
func AutostartLocal(url string) func() error {
	if !config.IsLocalURL(url) {
		return nil
	}
	return func() error {
		if _, err := os.Stat(config.BrokerPath()); err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
			return err
		}
		logf, err := os.OpenFile(filepath.Join(config.StateDir(), "broker.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer logf.Close()
		cmd := exec.Command(exe, "broker")
		cmd.Stdout, cmd.Stderr = logf, logf
		// Its own session, so it outlives the Claude Code session that started it.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
}
