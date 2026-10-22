package channel

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/vitaminmoo/peerbus/internal/config"
	"github.com/vitaminmoo/peerbus/internal/proto"
)

// autoResources describes what this session can reach: its git repo, the MCP
// servers configured for it, and configured programs present on PATH.
func autoResources(cfg *config.Client, cwd string) []proto.Resource {
	var out []proto.Resource
	if r, ok := gitResource(cwd); ok {
		out = append(out, r)
	}
	for _, name := range mcpServers(cwd) {
		out = append(out, proto.Resource{Kind: "mcp", Name: name, Source: "auto"})
	}
	if cfg != nil {
		for _, prog := range cfg.Programs {
			if path, err := exec.LookPath(prog); err == nil {
				out = append(out, proto.Resource{Kind: "program", Name: prog, Detail: path, Source: "config"})
			}
		}
		for _, r := range cfg.Resources {
			out = append(out, proto.Resource{Kind: r.Kind, Name: r.Name, Detail: r.Detail, Source: "config"})
		}
	}
	return out
}

func git(cwd string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitResource(cwd string) (proto.Resource, bool) {
	top := git(cwd, "rev-parse", "--show-toplevel")
	if top == "" {
		return proto.Resource{}, false
	}
	name := git(cwd, "remote", "get-url", "origin")
	if name == "" {
		name = filepath.Base(top)
	}
	r := proto.Resource{Kind: "repo", Name: name, Source: "auto"}
	if br := git(cwd, "branch", "--show-current"); br != "" {
		r.Detail = "branch " + br
	}
	return r, true
}

// mcpServers lists MCP server names configured at user scope, for this
// project in .claude.json, and in the project's .mcp.json.
func mcpServers(cwd string) []string {
	var names []string
	for n := range mcpConfig(cwd) {
		if n != "peerbus" {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

type mcpEntry struct {
	Command string `json:"command"`
}

// mcpConfig is every MCP server Claude Code would start in cwd, by name.
func mcpConfig(cwd string) map[string]mcpEntry {
	type servers struct {
		MCPServers map[string]mcpEntry `json:"mcpServers"`
	}
	out := map[string]mcpEntry{}
	add := func(s servers) {
		for n, e := range s.MCPServers {
			if _, ok := out[n]; !ok {
				out[n] = e
			}
		}
	}
	var cj struct {
		servers
		Projects map[string]servers `json:"projects"`
	}
	if b, err := os.ReadFile(claudeJSONPath()); err == nil && json.Unmarshal(b, &cj) == nil {
		add(cj.servers)
		add(cj.Projects[cwd])
	}
	var pj servers
	if b, err := os.ReadFile(filepath.Join(cwd, ".mcp.json")); err == nil && json.Unmarshal(b, &pj) == nil {
		add(pj)
	}
	return out
}
