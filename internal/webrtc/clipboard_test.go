package webrtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// fakeClipboard is a browser container's clipboard endpoints, recording
// what reached them in order.
type fakeClipboard struct {
	mu    sync.Mutex
	calls []string // "release", "type", "set", "paste", "read", "empty"
	sent  []string // bodies of sets, pastes and types
	held  string
	// readErr, if set, is the read's refusal (status and text).
	readStatus int
	readErr    string
	// onEmpty, if set, runs as the clipboard is emptied.
	onEmpty func()
}

func (c *fakeClipboard) routes(mux *http.ServeMux) {
	record := func(what string, f func(body []byte, w http.ResponseWriter)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			c.mu.Lock()
			c.calls = append(c.calls, what)
			c.mu.Unlock()
			f(b, w)
		}
	}
	setText := func(b []byte) {
		var t struct{ Text string }
		_ = json.Unmarshal(b, &t)
		c.mu.Lock()
		c.sent = append(c.sent, string(b))
		c.held = t.Text
		c.mu.Unlock()
	}
	mux.HandleFunc("POST /input/release", record("release", func(_ []byte, w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }))
	mux.HandleFunc("POST /input/events", record("events", func(_ []byte, w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }))
	mux.HandleFunc("POST /input/type", record("type", func(b []byte, w http.ResponseWriter) {
		c.mu.Lock()
		c.sent = append(c.sent, string(b))
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /input/clipboard", record("set", func(b []byte, w http.ResponseWriter) { setText(b); w.WriteHeader(http.StatusNoContent) }))
	mux.HandleFunc("POST /input/paste", record("paste", func(b []byte, w http.ResponseWriter) { setText(b); w.WriteHeader(http.StatusNoContent) }))
	mux.HandleFunc("GET /input/clipboard", record("read", func(_ []byte, w http.ResponseWriter) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.readStatus != 0 {
			http.Error(w, c.readErr, c.readStatus)
			return
		}
		_, _ = io.WriteString(w, c.held)
	}))
	mux.HandleFunc("DELETE /input/clipboard", record("empty", func(_ []byte, w http.ResponseWriter) {
		c.mu.Lock()
		f := c.onEmpty
		c.mu.Unlock()
		if f != nil {
			f()
		}
		c.mu.Lock()
		c.held = ""
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (c *fakeClipboard) took() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.calls
	c.calls = nil
	return out
}

// talk is a viewer's data channel, with its replies.
type talk struct {
	t       *testing.T
	v       *viewer
	replies chan map[string]any
}

func newTalk(t *testing.T, v *viewer) *talk {
	return &talk{t: t, v: v, replies: make(chan map[string]any, 20)}
}

// send sends a message and returns its replies.
func (k *talk) send(msg string) []map[string]any {
	k.t.Helper()
	k.v.message([]byte(msg), func(r string) {
		var m map[string]any
		if err := json.Unmarshal([]byte(r), &m); err != nil {
			k.t.Errorf("reply %q: %v", r, err)
		}
		k.replies <- m
	})
	var got []map[string]any
	for len(k.replies) > 0 {
		got = append(got, <-k.replies)
	}
	return got
}

func set(id int, text string, paste bool) string {
	b, _ := json.Marshal(map[string]any{"type": "clipboard-set", "id": id, "text": text, "paste": paste})
	return string(b)
}

// The clipboard box reads and sets the browser's clipboard, only for the
// viewer with control, and none of the text is logged (PROJ-194).
func TestViewerClipboardBox(t *testing.T) {
	const sent, pasted, copied = "sent-Secret-42", "pasted-Secret-43", "copied-Secret-44"
	var c fakeClipboard
	var logs syncBuffer
	h, m, s := offerFixtureWith(t, c.routes, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	k := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: m.Layout().ControlSocket("google")})

	// Without control: refused, with the box told, and nothing reaches
	// the browser.
	for _, msg := range []string{`{"type":"clipboard","id":1}`, set(2, sent, false), set(3, sent, true)} {
		r := k.send(msg)
		if len(r) != 2 || r[0]["type"] != "clipboard-error" || r[1]["type"] != "control" || r[1]["on"] != false {
			t.Errorf("%s without control: %v", msg, r)
		}
	}
	if got := c.took(); len(got) != 0 {
		t.Fatalf("without control the browser got %v", got)
	}

	if r := k.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take: %v", r)
	}
	// Send to browser: held there; read back.
	if r := k.send(set(4, sent, false)); len(r) != 1 || r[0]["type"] != "clipboard-set" || r[0]["id"] != float64(4) {
		t.Errorf("set: %v", r)
	}
	if r := k.send(`{"type":"clipboard","id":5}`); len(r) != 1 || r[0]["type"] != "clipboard" || r[0]["id"] != float64(5) || r[0]["text"] != sent {
		t.Errorf("read: %v", r)
	}
	// Paste into page.
	if r := k.send(set(6, pasted, true)); len(r) != 1 || r[0]["type"] != "clipboard-set" {
		t.Errorf("paste: %v", r)
	}
	if got := c.took(); strings.Join(got, " ") != "set read paste" {
		t.Errorf("the browser got %v", got)
	}
	c.mu.Lock()
	if len(c.sent) != 2 || c.sent[0] != fmt.Sprintf(`{"text":%q}`, sent) || c.sent[1] != fmt.Sprintf(`{"text":%q}`, pasted) {
		t.Errorf("sent %v", c.sent)
	}
	c.mu.Unlock()

	// Refused here: nothing, or over the cap (in characters).
	for _, msg := range []string{set(7, "", false), set(8, strings.Repeat("é", maxClipboardSet+1), true)} {
		if r := k.send(msg); len(r) != 1 || r[0]["type"] != "clipboard-error" || r[0]["id"] == nil {
			t.Errorf("%.40s: %v", msg, r)
		}
	}
	if r := k.send(set(9, strings.Repeat("é", maxClipboardSet), false)); len(r) != 1 || r[0]["type"] != "clipboard-set" {
		t.Errorf("exactly the cap: %v", r)
	}
	if got := c.took(); strings.Join(got, " ") != "set" {
		t.Errorf("the browser got %v", got)
	}

	// A read the browser refuses (too big, not UTF-8): an error, saying
	// why, not what.
	c.mu.Lock()
	c.held, c.readStatus, c.readErr = copied, http.StatusRequestEntityTooLarge, "the clipboard holds more than 32 kB of text"
	c.mu.Unlock()
	if r := k.send(`{"type":"clipboard","id":10}`); len(r) != 1 || r[0]["type"] != "clipboard-error" || r[0]["id"] != float64(10) || !strings.Contains(fmt.Sprint(r[0]["msg"]), "32 kB") {
		t.Errorf("a refused read: %v", r)
	}
	c.mu.Lock()
	c.readStatus = 0
	c.mu.Unlock()
	if r := k.send(`{"type":"clipboard","id":11}`); len(r) != 1 || r[0]["text"] != copied {
		t.Errorf("read: %v", r)
	}

	// The activity log says what happened, not how long it was: that
	// would tell agents a password's length.
	acts := fmt.Sprint(m.Activity(s.ID))
	if !strings.Contains(acts, "Clipboard set") || !strings.Contains(acts, "Pasted from the clipboard box") || strings.Contains(acts, "characters") {
		t.Errorf("activity: %s", acts)
	}
	k.send(`{"type":"release"}`)
	// None of the texts is logged, in the activity log or the orchestrator's.
	all := logs.String() + fmt.Sprint(m.Activity(s.ID))
	for _, secret := range []string{sent, pasted, copied, "éééé"} {
		if strings.Contains(all, secret) {
			t.Errorf("%s is logged:\n%s", secret, all)
		}
	}
}

// The phone's Type button types at once, as before PROJ-194: a field that
// refuses pasting still takes it.
func TestViewerTypesText(t *testing.T) {
	var c fakeClipboard
	h, m, s := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	k := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: m.Layout().ControlSocket("google")})
	k.send(`{"type":"take"}`)
	if r := k.send(`{"type":"text","text":"héllo"}`); len(r) != 0 {
		t.Errorf("text: %v", r)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.Join(c.calls, " ") != "type" || len(c.sent) != 1 || c.sent[0] != `{"pace":"instant","text":"héllo"}` {
		t.Errorf("the browser got %v %v", c.calls, c.sent)
	}
}

// However the person's control ends, the browser's clipboard is emptied,
// and before agents can use the browser again (PROJ-194).
func TestClipboardEmptiedWhenControlEnds(t *testing.T) {
	var c fakeClipboard
	h, m, s := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	// While the clipboard empties, no agent gets in.
	var mu sync.Mutex
	var during []error
	c.onEmpty = func() {
		_, done, err := m.AgentInput(context.Background(), s.ID)
		if err == nil {
			done()
		}
		mu.Lock()
		during = append(during, err)
		mu.Unlock()
	}
	for _, end := range []struct {
		name string
		end  func(k *talk)
		want string // what reaches the browser, in order
	}{
		{"hand back", func(k *talk) {
			if r := k.send(`{"type":"release"}`); len(r) != 1 || r[0]["on"] != false {
				t.Errorf("release: %v", r)
			}
		}, "set release empty"},
		// A handoff's Done, Can't do this, expiry or cancel, or the
		// profile page's Hand back, all release control in the manager.
		{"control taken away", func(k *talk) {
			m.ReleaseControl(s.ID)
			// The viewer learns it at its next message, refused.
			if r := k.send(set(2, "after", false)); len(r) != 2 || r[0]["type"] != "clipboard-error" || r[1]["on"] != false {
				t.Errorf("a set after losing control: %v", r)
			}
		}, "set empty"},
		{"the viewer goes", func(k *talk) { k.v.handBack() }, "set release empty"},
	} {
		t.Run(end.name, func(t *testing.T) {
			mu.Lock()
			during = nil
			mu.Unlock()
			k := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: m.Layout().ControlSocket("google")})
			if r := k.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
				t.Fatalf("take: %v", r)
			}
			if r := k.send(set(1, "a secret", false)); len(r) != 1 || r[0]["type"] != "clipboard-set" {
				t.Fatalf("set: %v", r)
			}
			end.end(k)
			if got := strings.Join(c.took(), " "); got != end.want {
				t.Errorf("the browser got %q, want %q", got, end.want)
			}
			mu.Lock()
			if len(during) != 1 || !errors.Is(during[0], sessions.ErrHumanDriving) {
				t.Errorf("agent input while the clipboard emptied: %v", during)
			}
			mu.Unlock()
			if got, _ := m.Live("google"); got.HumanControl {
				t.Error("still under a person's control")
			}
			c.mu.Lock()
			if c.held != "" {
				t.Errorf("the clipboard holds %q", c.held)
			}
			c.mu.Unlock()
		})
	}
}

// syncBuffer is a bytes buffer for a logger used from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
