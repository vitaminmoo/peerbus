// Package config loads peerbus client and broker configuration.
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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

func ClientPath() string { return filepath.Join(Dir(), "client.json") }
func BrokerPath() string { return filepath.Join(Dir(), "broker.json") }

// Client is per OS user per machine, shared by every Claude account on it,
// so all of them act as the same peerbus user.
type Client struct {
	URL          string           `json:"url,omitempty"`   // legacy broker
	Token        string           `json:"token,omitempty"` // legacy broker
	Rendezvous   string           `json:"rendezvous,omitempty"`
	STUN         []string         `json:"stun,omitempty"`
	ShareDetails bool             `json:"shareDetails,omitempty"` // let other users see cwd/account
	Resources    []ClientResource `json:"resources,omitempty"`
	Programs     []string         `json:"programs,omitempty"` // advertised when found on PATH
}

type ClientResource struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

type Broker struct {
	Listen                string              `json:"listen"` // host:port
	Users                 map[string]*User    `json:"users"`
	Groups                map[string][]string `json:"groups"`
	ThreadApprovalMinutes int                 `json:"threadApprovalMinutes,omitempty"`
}

type User struct {
	Tokens []string `json:"tokens"` // sha256 hex of each token
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

func LoadClient() (*Client, error) {
	var c Client
	if err := ReadJSON(ClientPath(), &c); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no peerbus client config at %s (run: peerbus init, or peerbus join <url> <token>)", ClientPath())
		}
		return nil, err
	}
	return &c, nil
}

// LoadClientOrDefault returns the client config, or defaults when there is
// none, with rendezvous and STUN defaults filled in.
func LoadClientOrDefault() (*Client, error) {
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

// LocalDir is where sessions on this machine find each other.
func LocalDir() string { return filepath.Join(StateDir(), "local") }

func LoadBroker(path string) (*Broker, error) {
	var b Broker
	if err := ReadJSON(path, &b); err != nil {
		return nil, err
	}
	if b.Users == nil {
		b.Users = map[string]*User{}
	}
	if b.Groups == nil {
		b.Groups = map[string][]string{}
	}
	if b.Listen == "" {
		b.Listen = "127.0.0.1:" + strconv.Itoa(DefaultPort)
	}
	return &b, nil
}

func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func NewToken(user string) string {
	b := make([]byte, 24)
	rand.Read(b)
	return "pb_" + user + "_" + base64.RawURLEncoding.EncodeToString(b)
}

func IsLocalURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}
