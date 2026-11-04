// Package mcp is a minimal MCP server over stdio: newline-delimited JSON-RPC
// with initialize, ping, tools/list, tools/call, and server-sent
// notifications (which is all a Claude Code channel needs).
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const latestProtocol = "2025-06-18"

// drainPoll is how often a waiting Serve looks up to see if a Drain wants it.
const drainPoll = 250 * time.Millisecond

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

	// Resumed says this process took over a connection from an earlier one
	// (see Drain), so the client won't send initialize again. Pending is
	// what that process had read but not acted on.
	Resumed bool
	Pending []byte

	out      io.Writer
	wmu      sync.Mutex
	initOnce sync.Once
	ready    chan struct{}
	drains   chan drainRequest
	calls    atomic.Int64 // tool calls running
	unpark   chan struct{}
}

type drainRequest struct {
	pending chan<- []byte
	resume  <-chan struct{}
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
	s.initOnce.Do(func() {
		s.ready = make(chan struct{})
		s.drains = make(chan drainRequest)
	})
}

// Ready is closed once the client has sent notifications/initialized.
func (s *Server) Ready() <-chan struct{} {
	s.init()
	return s.ready
}

func (s *Server) markReady() {
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
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

// Serve reads requests from r until EOF. Only when r is a file (stdin) can
// a Drain stop it.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.init()
	s.out = w
	if s.Resumed {
		s.markReady()
		// This binary's tools may not be the ones the client knows.
		if err := s.Notify("notifications/tools/list_changed", map[string]any{}); err != nil {
			return err
		}
	}
	f, _ := r.(*os.File)
	buf := s.Pending
	s.Pending = nil
	chunk := make([]byte, 1<<16)
	for {
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			s.line(ctx, buf[:i+1])
			buf = buf[i+1:]
		}
		// Only part of a line is left, and nothing has been read that isn't
		// in buf: the one place a Drain can safely stop us.
		if f != nil {
			if err := s.waitInput(f, buf); err != nil {
				return err
			}
		}
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err == io.EOF {
			if len(buf) > 0 {
				s.line(ctx, buf)
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) line(ctx context.Context, line []byte) {
	var m rpcMsg
	if err := json.Unmarshal(line, &m); err != nil {
		s.write(rpcMsg{ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	s.dispatch(ctx, m)
}

// waitInput returns once f has something to read (or has ended), parking
// whenever a Drain asks. Reading only when poll says there's something
// means a parked Serve holds nothing it hasn't handed over.
func (s *Server) waitInput(f *os.File, buf []byte) error {
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	for {
		select {
		case d := <-s.drains:
			d.pending <- slices.Clone(buf)
			<-d.resume
		default:
		}
		n, err := unix.Poll(fds, int(drainPoll/time.Millisecond))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
}

// Drain stops reading requests and waits for running tool calls to answer,
// so that the process can be replaced without losing anything. It returns
// what had been read of a request not yet complete, for the next process's
// Pending. If that takes longer than wait, reading carries on and Drain
// reports an error. After a Drain, Resume undoes it.
func (s *Server) Drain(wait time.Duration) ([]byte, error) {
	s.init()
	deadline := time.After(wait)
	pending := make(chan []byte, 1)
	resume := make(chan struct{})
	select {
	case s.drains <- drainRequest{pending, resume}:
	case <-deadline:
		return nil, errors.New("the MCP server isn't reading")
	}
	buf := <-pending
	for s.calls.Load() > 0 {
		select {
		case <-deadline:
			close(resume)
			return nil, errors.New("tool calls are still running")
		case <-time.After(20 * time.Millisecond):
		}
	}
	s.unpark = resume
	return buf, nil
}

// Resume goes back to reading after a Drain.
func (s *Server) Resume() { close(s.unpark) }

// Freeze holds back all output until the returned function is called, so a
// process being replaced can't stop halfway through a message.
func (s *Server) Freeze() (unfreeze func()) {
	s.wmu.Lock()
	return s.wmu.Unlock
}

func (s *Server) dispatch(ctx context.Context, m rpcMsg) {
	isRequest := len(m.ID) > 0 && string(m.ID) != "null"
	if m.Method == "" {
		return // a response to something we never send
	}
	if !isRequest {
		if m.Method == "notifications/initialized" {
			s.markReady()
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
		caps := map[string]any{"tools": map[string]any{"listChanged": true}}
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
		s.calls.Add(1)
		go func() {
			defer s.calls.Add(-1)
			s.call(ctx, m)
		}()
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
