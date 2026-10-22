// Package client is the broker connection used by channel servers and the CLI.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/vitaminmoo/peerbus/internal/proto"
)

var ErrNotConnected = errors.New("not connected to the peerbus broker")

type Client struct {
	URL       string
	Hello     func() proto.Frame // built fresh for every (re)connect
	OnFrame   func(proto.Frame)  // deliver, notice and held frames
	OnState   func(up bool, err error)
	Autostart func() error // called when a local broker refuses connections
	Log       *log.Logger

	mu        sync.Mutex
	ws        *websocket.Conn
	welcome   proto.Frame
	pending   map[string]chan proto.Frame
	lastStart time.Time
}

func wsURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws", "wss":
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("broker url %q must be ws://, wss://, http:// or https://", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/ws"
	}
	return u.String(), nil
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log.Printf(format, args...)
	}
}

// dial opens one connection and completes the hello exchange.
func (c *Client) dial(ctx context.Context) (*websocket.Conn, proto.Frame, error) {
	u, err := wsURL(c.URL)
	if err != nil {
		return nil, proto.Frame{}, err
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, u, nil)
	if err != nil {
		return nil, proto.Frame{}, err
	}
	ws.SetReadLimit(1 << 20)
	if err := wsjson.Write(dctx, ws, c.Hello()); err != nil {
		ws.CloseNow()
		return nil, proto.Frame{}, err
	}
	var f proto.Frame
	if err := wsjson.Read(dctx, ws, &f); err != nil {
		ws.CloseNow()
		return nil, proto.Frame{}, err
	}
	if f.Type == proto.TError {
		ws.CloseNow()
		return nil, proto.Frame{}, fmt.Errorf("broker rejected us: %s", f.Error)
	}
	if f.Type != proto.TWelcome {
		ws.CloseNow()
		return nil, proto.Frame{}, fmt.Errorf("unexpected first frame %q", f.Type)
	}
	return ws, f, nil
}

func refused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "connection refused")
}

func (c *Client) maybeAutostart(err error) bool {
	if c.Autostart == nil || !refused(err) || time.Since(c.lastStart) < 5*time.Second {
		return false
	}
	c.lastStart = time.Now()
	if serr := c.Autostart(); serr != nil {
		c.logf("autostart broker: %v", serr)
		return false
	}
	return true
}

// Connect makes a single connection, starting a local broker if needed, and
// reads frames in the background until the connection drops.
func (c *Client) Connect(ctx context.Context) error {
	ws, w, err := c.dial(ctx)
	if err != nil && c.maybeAutostart(err) {
		for i := 0; i < 20 && err != nil && refused(err); i++ {
			time.Sleep(150 * time.Millisecond)
			ws, w, err = c.dial(ctx)
		}
	}
	if err != nil {
		return err
	}
	c.attach(ws, w)
	go c.readLoop(ctx, ws)
	return nil
}

// Run keeps a connection up until ctx is done, reconnecting with backoff.
func (c *Client) Run(ctx context.Context) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		ws, w, err := c.dial(ctx)
		if err != nil {
			if c.maybeAutostart(err) {
				backoff = 200 * time.Millisecond
			}
			if c.OnState != nil {
				c.OnState(false, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 15*time.Second)
			continue
		}
		backoff = 500 * time.Millisecond
		c.attach(ws, w)
		if c.OnState != nil {
			c.OnState(true, nil)
		}
		err = c.readLoop(ctx, ws)
		if c.OnState != nil {
			c.OnState(false, err)
		}
	}
}

func (c *Client) attach(ws *websocket.Conn, w proto.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ws, c.welcome = ws, w
	if c.pending == nil {
		c.pending = map[string]chan proto.Frame{}
	}
}

func (c *Client) readLoop(ctx context.Context, ws *websocket.Conn) error {
	defer func() {
		c.mu.Lock()
		if c.ws == ws {
			c.ws = nil
		}
		for req, ch := range c.pending {
			close(ch)
			delete(c.pending, req)
		}
		c.mu.Unlock()
		ws.CloseNow()
	}()
	for {
		var f proto.Frame
		if err := wsjson.Read(ctx, ws, &f); err != nil {
			return err
		}
		if f.Type == proto.TResult {
			c.mu.Lock()
			ch := c.pending[f.Req]
			delete(c.pending, f.Req)
			c.mu.Unlock()
			if ch != nil {
				ch <- f
			}
			continue
		}
		if c.OnFrame != nil {
			c.OnFrame(f)
		}
	}
}

// Welcome returns the welcome frame of the current connection.
func (c *Client) Welcome() (proto.Frame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.welcome, c.ws != nil
}

// Reconnect drops the current connection; Run dials again with a fresh hello.
func (c *Client) Reconnect() {
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws != nil {
		ws.Close(websocket.StatusNormalClosure, "reconnecting")
	}
}

func (c *Client) Close() {
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws != nil {
		ws.Close(websocket.StatusNormalClosure, "bye")
	}
}

// Send writes a frame without waiting for a result.
func (c *Client) Send(ctx context.Context, f proto.Frame) error {
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws == nil {
		return ErrNotConnected
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(wctx, ws, f)
}

// Request sends a frame and decodes the result's data into out (if non-nil).
func (c *Client) Request(ctx context.Context, f proto.Frame, out any) error {
	f.Req = proto.NewID(6)
	ch := make(chan proto.Frame, 1)
	c.mu.Lock()
	ws := c.ws
	if ws != nil {
		c.pending[f.Req] = ch
	}
	c.mu.Unlock()
	if ws == nil {
		return ErrNotConnected
	}
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := wsjson.Write(wctx, ws, f); err != nil {
		return err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return ErrNotConnected
		}
		if !r.OK {
			return errors.New(r.Error)
		}
		if out != nil && r.Data != nil {
			b, _ := json.Marshal(r.Data)
			return json.Unmarshal(b, out)
		}
		return nil
	case <-wctx.Done():
		c.mu.Lock()
		delete(c.pending, f.Req)
		c.mu.Unlock()
		return fmt.Errorf("broker did not answer: %w", wctx.Err())
	}
}
