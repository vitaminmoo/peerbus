// Package mcp is a minimal MCP server over stdio: newline-delimited JSON-RPC
// with initialize, ping, tools/list, tools/call, and server-sent
// notifications (which is all a Claude Code channel needs).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

const latestProtocol = "2025-06-18"

type Tool struct {
	Name        string                                                          `json:"name"`
	Description string                                                          `json:"description"`
	InputSchema map[string]any                                                  `json:"inputSchema"`
	Handler     func(ctx context.Context, args json.RawMessage) (string, error) `json:"-"`
}

type Server struct {
	Name, Version, Instructions string
	Experimental                map[string]any // experimental capabilities, e.g. claude/channel
	Tools                       []Tool

	out      io.Writer
	wmu      sync.Mutex
	initOnce sync.Once
	ready    chan struct{}
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) init() {
	s.initOnce.Do(func() { s.ready = make(chan struct{}) })
}

// Ready is closed once the client has sent notifications/initialized.
func (s *Server) Ready() <-chan struct{} {
	s.init()
	return s.ready
}

func (s *Server) write(m rpcMsg) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.out.Write(append(b, '\n'))
	return err
}

// Notify sends a server notification. Call it only after Ready.
func (s *Server) Notify(method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return s.write(rpcMsg{Method: method, Params: b})
}

// Serve reads requests from r until EOF.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.init()
	s.out = w
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var m rpcMsg
			if jerr := json.Unmarshal(line, &m); jerr != nil {
				s.write(rpcMsg{ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			} else {
				s.dispatch(ctx, m)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) dispatch(ctx context.Context, m rpcMsg) {
	isRequest := len(m.ID) > 0 && string(m.ID) != "null"
	if m.Method == "" {
		return // a response to something we never send
	}
	if !isRequest {
		if m.Method == "notifications/initialized" {
			select {
			case <-s.ready:
			default:
				close(s.ready)
			}
		}
		return
	}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		pv := p.ProtocolVersion
		if pv == "" {
			pv = latestProtocol
		}
		caps := map[string]any{"tools": map[string]any{}}
		if len(s.Experimental) > 0 {
			caps["experimental"] = s.Experimental
		}
		s.write(rpcMsg{ID: m.ID, Result: map[string]any{
			"protocolVersion": pv,
			"capabilities":    caps,
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
			"instructions":    s.Instructions,
		}})
	case "ping":
		s.write(rpcMsg{ID: m.ID, Result: map[string]any{}})
	case "tools/list":
		s.write(rpcMsg{ID: m.ID, Result: map[string]any{"tools": s.Tools}})
	case "tools/call":
		go s.call(ctx, m)
	default:
		s.write(rpcMsg{ID: m.ID, Error: &rpcError{-32601, "method not found: " + m.Method}})
	}
}

func (s *Server) call(ctx context.Context, m rpcMsg) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		s.write(rpcMsg{ID: m.ID, Error: &rpcError{-32602, "bad params"}})
		return
	}
	for _, t := range s.Tools {
		if t.Name != p.Name {
			continue
		}
		args := p.Arguments
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		text, err := t.Handler(ctx, args)
		res := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		if err != nil {
			res["content"] = []map[string]any{{"type": "text", "text": err.Error()}}
			res["isError"] = true
		}
		s.write(rpcMsg{ID: m.ID, Result: res})
		return
	}
	s.write(rpcMsg{ID: m.ID, Error: &rpcError{-32602, fmt.Sprintf("unknown tool %q", p.Name)}})
}
