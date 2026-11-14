// Package cdptest has a fake browser for tests of CDP callers: it serves
// what a browser container's control socket does for CDP (/cdp/json/version
// and the browser's WebSocket) and answers each method with a handler the
// test gives, recording everything it was sent.
package cdptest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// Message is one method call the fake browser received.
type Message struct {
	ID        int64           `json:"id"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
}

// Error makes a handler answer with a CDP error.
type Error struct{ Message string }

func (e Error) Error() string { return e.Message }

// Handler answers one call: a result (marshalled to JSON) or an error.
// An Error is sent as CDP's error; NoReply leaves the call unanswered.
type Handler func(m Message) (any, error)

// NoReply, returned by a Handler, leaves the call without an answer, as a
// renderer blocked by a dialog does.
var NoReply = Error{"no reply"}

// Browser is the fake browser.
type Browser struct {
	mu       sync.Mutex
	handle   Handler
	received []Message
	conns    int // open WebSocket connections
	dials    int
	closed   chan struct{} // signalled when a connection closes
}

// New makes a fake browser answering with handle.
func New(handle Handler) *Browser {
	return &Browser{handle: handle, closed: make(chan struct{}, 64)}
}

// SetHandler replaces the handler.
func (b *Browser) SetHandler(h Handler) {
	b.mu.Lock()
	b.handle = h
	b.mu.Unlock()
}

// Received lists the calls so far, oldest first.
func (b *Browser) Received() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Message(nil), b.received...)
}

// Methods lists the methods called so far, oldest first.
func (b *Browser) Methods() []string {
	var out []string
	for _, m := range b.Received() {
		out = append(out, m.Method)
	}
	return out
}

// Reset forgets the calls so far.
func (b *Browser) Reset() {
	b.mu.Lock()
	b.received = nil
	b.mu.Unlock()
}

// Open counts the WebSocket connections open now; Dials those ever opened.
func (b *Browser) Open() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conns
}

func (b *Browser) Dials() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dials
}

// Closed is signalled each time a connection closes.
func (b *Browser) Closed() <-chan struct{} { return b.closed }

// Handler serves the control socket's CDP paths. Other paths answer 404,
// unless next is given.
func (b *Browser) Handler(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /cdp/json/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Browser":"Chrome/156","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/fake"}`)
	})
	mux.HandleFunc("GET /cdp/devtools/browser/fake", b.serveWS)
	if next != nil {
		mux.Handle("/", next)
	}
	return mux
}

func (b *Browser) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 20)
	b.mu.Lock()
	b.conns++
	b.dials++
	b.mu.Unlock()
	defer func() {
		_ = conn.CloseNow()
		b.mu.Lock()
		b.conns--
		b.mu.Unlock()
		select {
		case b.closed <- struct{}{}:
		default:
		}
	}()
	ctx := r.Context()
	var wmu sync.Mutex
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m Message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		b.mu.Lock()
		b.received = append(b.received, m)
		h := b.handle
		b.mu.Unlock()
		// Answer in order but without blocking the reader, so a call
		// left unanswered doesn't hold up the next.
		go func() {
			res, err := h(m)
			if errors.Is(err, NoReply) {
				return
			}
			out := map[string]any{"id": m.ID}
			if m.SessionID != "" {
				out["sessionId"] = m.SessionID
			}
			if err != nil {
				out["error"] = map[string]any{"code": -32000, "message": err.Error()}
			} else {
				if res == nil {
					res = map[string]any{}
				}
				out["result"] = res
			}
			raw, _ := json.Marshal(out)
			wmu.Lock()
			defer wmu.Unlock()
			_ = conn.Write(ctx, websocket.MessageText, raw)
		}()
	}
}

// Serve serves the handler on a Unix socket in a temporary folder and
// returns its path; it stops when the test ends.
func Serve(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cdp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h} //nolint:gosec // a test server on a Unix socket
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return sock
}
