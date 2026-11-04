package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// serve runs a server on real pipes, the way it runs on stdin and stdout.
func serve(t *testing.T, s *Server) (in *os.File, out *bufio.Reader) {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inW.Close(); outR.Close() })
	go s.Serve(context.Background(), inR, outW)
	return inW, bufio.NewReader(outR)
}

func readID(t *testing.T, out *bufio.Reader) string {
	t.Helper()
	line, err := out.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatal(err)
	}
	return string(m.ID)
}

func TestDrainHandsOverAPartialLine(t *testing.T) {
	s := &Server{}
	in, out := serve(t, s)
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" + `{"jsonrpc":"2.0","id":2,"me`)
	if id := readID(t, out); id != "1" {
		t.Fatalf("answered %s", id)
	}
	time.Sleep(2 * drainPoll) // let the partial line be read
	pending, err := s.Drain(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(pending) != `{"jsonrpc":"2.0","id":2,"me` {
		t.Fatalf("pending %q", pending)
	}
	// Nothing is read while drained: it's left for the next process.
	in.WriteString(`thod":"ping"}` + "\n")
	time.Sleep(2 * drainPoll)
	s.Resume()
	// Resuming the same server finishes the line it was holding.
	if id := readID(t, out); id != "2" {
		t.Fatalf("after resume answered %s", id)
	}
}

func TestDrainWaitsForToolCalls(t *testing.T) {
	release := make(chan struct{})
	s := &Server{Tools: []Tool{{Name: "slow", Handler: func(context.Context, json.RawMessage) (string, error) {
		<-release
		return "done", nil
	}}}}
	in, out := serve(t, s)
	in.WriteString(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow"}}` + "\n")
	for s.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	// Still running: the drain gives up and reading carries on.
	if _, err := s.Drain(4 * drainPoll); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("drain: %v", err)
	}
	in.WriteString(`{"jsonrpc":"2.0","id":8,"method":"ping"}` + "\n")
	if id := readID(t, out); id != "8" {
		t.Fatalf("answered %s", id)
	}
	close(release)
	if id := readID(t, out); id != "7" {
		t.Fatalf("answered %s", id)
	}
	for s.calls.Load() > 0 {
		time.Sleep(time.Millisecond)
	}
	if _, err := s.Drain(5 * time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestResumedNeedsNoHandshake(t *testing.T) {
	s := &Server{Resumed: true, Pending: []byte(`{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n")}
	_, out := serve(t, s)
	line, err := out.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "notifications/tools/list_changed") {
		t.Fatalf("first thing said: %s", line)
	}
	select {
	case <-s.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("not ready without a handshake")
	}
	if id := readID(t, out); id != "3" {
		t.Fatalf("pending request answered as %s", id)
	}
}
