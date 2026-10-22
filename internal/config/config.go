// Package config locates peerbus's files and loads the optional client
// settings.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const (
	DefaultPort       = 47615
	DefaultRendezvous = "rv.peerb.us:47615"
)

// DefaultSTUN is used to learn our public address when the rendezvous
// server shares our NAT.
var DefaultSTUN = []string{"stun.cloudflare.com:3478"}

// Dir holds identity.key, peers.json and client.json.
func Dir() string {
	if d := os.Getenv("PEERBUS_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "peerbus")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "peerbus")
}

// StateDir holds the inbox and the same-machine discovery directory.
func StateDir() string {
	if d := os.Getenv("PEERBUS_STATE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "peerbus")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "peerbus")
}

// LocalDir is where sessions on this machine find each other.
func LocalDir() string { return filepath.Join(StateDir(), "local") }

func ClientPath() string { return filepath.Join(Dir(), "client.json") }

// Client is optional, per OS user per machine, shared by every Claude
// account on it.
type Client struct {
	// Rendezvous is the server that introduces sessions on different
	// machines. "none" keeps sessions to this machine.
	Rendezvous string   `json:"rendezvous,omitempty"`
	STUN       []string `json:"stun,omitempty"`
	// Machine is what this machine's sessions are listed under, and what
	// "host/session" permission globs match. Defaults to the hostname.
	Machine   string           `json:"machine,omitempty"`
	Resources []ClientResource `json:"resources,omitempty"`
	Programs  []string         `json:"programs,omitempty"` // advertised when found on PATH
}

type ClientResource struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

// LoadClient returns the client config, or defaults when there is none.
func LoadClient() (*Client, error) {
	var c Client
	if err := ReadJSON(ClientPath(), &c); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if c.Rendezvous == "" {
		c.Rendezvous = DefaultRendezvous
	}
	if c.STUN == nil {
		c.STUN = DefaultSTUN
	}
	return &c, nil
}

// UpdateClient applies fn to client.json as stored, without the defaults
// LoadClient fills in.
func UpdateClient(fn func(*Client)) error {
	var c Client
	if err := ReadJSON(ClientPath(), &c); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fn(&c)
	return WriteJSON(ClientPath(), &c)
}

// MachineName is Machine, or the hostname when it isn't set.
func (c *Client) MachineName() string {
	if c.Machine != "" {
		return c.Machine
	}
	h, _ := os.Hostname()
	return h
}

// Server is the rendezvous address to use, or empty for none.
func (c *Client) Server() string {
	switch c.Rendezvous {
	case "none", "off", "-":
		return ""
	}
	return c.Rendezvous
}

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func WriteJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
