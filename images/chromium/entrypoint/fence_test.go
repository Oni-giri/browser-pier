package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// heldUp serves a request whose body arrives in two parts, the handler
// waiting between them, as a request held up on its way would. It
// returns once the handler is waiting, with a function that sends the
// rest and returns the answer.
func heldUp(t *testing.T, h http.Handler, method, target, body string, header http.Header) func() *httptest.ResponseRecorder {
	t.Helper()
	pr, pw := io.Pipe()
	req := httptest.NewRequest(method, target, pr)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		h.ServeHTTP(rec, req)
	}()
	cut := len(body) / 2
	if _, err := io.WriteString(pw, body[:cut]); err != nil { // the handler has read this much
		t.Fatal(err)
	}
	return func() *httptest.ResponseRecorder {
		_, _ = io.WriteString(pw, body[cut:])
		_ = pw.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Fatal("the handler never answered")
		}
		return rec
	}
}

func takeover(n string) http.Header { return http.Header{takeoverHeader: {n}} }

func fenceLift(t *testing.T, h http.Handler, fence string) uint64 {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/input/release", nil)
	req.Header.Set(fenceHeader, fence)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct{ Fence uint64 }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("fenced lift: %d %s", rec.Code, rec.Body)
	}
	return out.Fence
}

// A person's operation held up on its way, whose takeover ends (the lift
// raises the fence) before it acts, does nothing once it goes on: no
// input reaches xdotool, no xclip starts, nothing is pasted (PROJ-196).
// An agent's, naming no takeover, isn't fenced.
func TestFencedOperationsDoNothing(t *testing.T) {
	shortWait(t)
	for _, c := range []struct {
		name, method, target, body string
	}{
		{"live input", http.MethodPost, "/input/events", `[{"t":"keydown","k":"Control_L"},{"t":"down","b":1}]`},
		{"typing", http.MethodPost, "/input/type", `{"text":"typed by the person","pace":"instant"}`},
		{"clipboard set", http.MethodPost, "/input/clipboard", `{"text":"the person's secret"}`},
		{"paste", http.MethodPost, "/input/paste", `{"text":"the person's secret"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, calls := newTestVisual(false)
			x := &fakeXdotool{}
			v.live.start = func() (ioWriteCloser, error) { return x, nil }
			fx := newFakeX(v)
			mux := http.NewServeMux()
			v.register(mux)

			finish := heldUp(t, mux, c.method, c.target, c.body, takeover("3"))
			if got := fenceLift(t, mux, "3"); got != 3 {
				t.Fatalf("fence %d", got)
			}
			rec := finish()
			if rec.Code != http.StatusConflict {
				t.Errorf("the ended takeover's %s: %d %s", c.name, rec.Code, rec.Body)
			}
			fx.mu.Lock()
			holders := len(fx.holders)
			fx.mu.Unlock()
			if x.buf.Len() != 0 || len(*calls) != 0 || holders != 0 {
				t.Errorf("it acted: xdotool %q, commands %v, xclip holders %d", x.buf.String(), *calls, holders)
			}

			// The next takeover's goes through; so does an agent's.
			for _, h := range []http.Header{takeover("4"), {}} {
				req := httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
				for k, vs := range h {
					for _, v := range vs {
						req.Header.Add(k, v)
					}
				}
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code == http.StatusConflict {
					t.Errorf("%v refused: %s", h, rec.Body)
				}
			}
		})
	}
}

// An action under way when the fence goes up finishes first: the fence
// (and the lift with it) waits for it, so its effect is undone by the
// hand-back that follows, never left to land after it.
func TestFenceWaitsForAnActionUnderWay(t *testing.T) {
	li, _ := newTestLive(&fakeXdotool{})
	gen, person := uint64(5), true
	done, ok := li.fence.admit(gen, person)
	if !ok {
		t.Fatal("not admitted")
	}
	raised := make(chan uint64, 1)
	go func() { raised <- li.fence.raise(5, false) }()
	select {
	case <-raised:
		t.Fatal("the fence went up with an action under way")
	case <-time.After(50 * time.Millisecond):
	}
	done()
	if n := <-raised; n != 5 {
		t.Fatalf("fence %d", n)
	}
	if _, ok := li.fence.admit(5, true); ok {
		t.Error("admitted after the fence")
	}
}

// After the orchestrator restarts it fences every takeover seen so far,
// and learns the fence, to number its takeovers above it.
func TestFenceAll(t *testing.T) {
	v, _ := newTestVisual(false)
	v.live.start = func() (ioWriteCloser, error) { return &fakeXdotool{}, nil }
	mux := http.NewServeMux()
	v.register(mux)
	req := httptest.NewRequest(http.MethodPost, "/input/events", strings.NewReader(`[{"t":"move","x":1,"y":1}]`))
	req.Header.Set(takeoverHeader, "41")
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if got := fenceLift(t, mux, "all"); got != 41 {
		t.Fatalf("fence all = %d, want 41", got)
	}
	if got := fenceLift(t, mux, "7"); got != 41 {
		t.Fatalf("a lower fence lowered it: %d", got)
	}
	req = httptest.NewRequest(http.MethodPost, "/input/events", strings.NewReader(`[{"t":"move","x":1,"y":1}]`))
	req.Header.Set(takeoverHeader, "41")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("takeover 41 after fencing all: %d", rec.Code)
	}
}
