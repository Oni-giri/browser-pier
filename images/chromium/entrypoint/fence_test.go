package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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
			v.live.start = func() (*xdo, error) { return x.proc(), nil }
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
	go func() { raised <- li.fence.raise(5) }()
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

// queuedX is an xdotool that runs its input slowly, a line every step,
// as a busy display might: what is written is queued, not yet run. Its
// runs share one log of what ran.
type queuedX struct {
	mu   sync.Mutex
	ran  []string
	step time.Duration
}

func (q *queuedX) log() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.ran)
}

func (q *queuedX) start() (*xdo, error) {
	lines := make(chan string, 1024)
	exited := make(chan struct{})
	killed := make(chan struct{})
	var killOnce, closeOnce sync.Once
	go func() {
		defer close(exited)
		for l := range lines {
			select {
			case <-killed:
				return
			case <-time.After(q.step):
			}
			q.mu.Lock()
			q.ran = append(q.ran, l)
			q.mu.Unlock()
		}
	}()
	return &xdo{
		stdin:  queuedStdin{lines: lines, close: func() { closeOnce.Do(func() { close(lines) }) }},
		exited: exited,
		kill:   func() { killOnce.Do(func() { close(killed) }) },
	}, nil
}

type queuedStdin struct {
	lines chan string
	close func()
}

func (s queuedStdin) Write(p []byte) (int, error) {
	for _, l := range strings.Split(strings.TrimSuffix(string(p), "\n"), "\n") {
		s.lines <- l
	}
	return len(p), nil
}

func (s queuedStdin) Close() error { s.close(); return nil }

// The fence covers input already queued in xdotool: the lift answers only
// once everything the live xdotool was given has run (a burst of wheel
// turns, then Ctrl+C), lets go of the keys after it, and nothing of the
// takeover runs after the answer (PROJ-196).
func TestFenceRunsQueuedInputFirst(t *testing.T) {
	li, _ := newTestLive()
	q := &queuedX{step: 10 * time.Millisecond}
	li.start = q.start
	mux := http.NewServeMux()
	mux.HandleFunc("POST /input/events", li.events)
	mux.HandleFunc("POST /input/release", li.release)
	burst := `[{"t":"wheel","dy":5},{"t":"wheel","dy":5},{"t":"keydown","k":"Control_L"},{"t":"keydown","k":"c"},{"t":"keyup","k":"c"}]`
	req := httptest.NewRequest(http.MethodPost, "/input/events", strings.NewReader(burst))
	req.Header.Set(takeoverHeader, "3")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("events: %d %s", rec.Code, rec.Body)
	}
	queued := 13 // ten wheel clicks, three keys
	if got := len(q.log()); got >= queued {
		t.Fatalf("the input already ran (%d lines): the test needs it queued", got)
	}
	fenceLift(t, mux, "3")
	atAnswer := q.log()
	want := queued + 1 // and the lift's keyup Control_L
	if len(atAnswer) != want || atAnswer[len(atAnswer)-1] != "keyup Control_L" || atAnswer[queued-1] != "keyup c" {
		t.Fatalf("at the lift's answer, xdotool had run %q, want the queued input, then the release", atAnswer)
	}
	time.Sleep(100 * time.Millisecond)
	if after := q.log(); len(after) != len(atAnswer) {
		t.Errorf("ran after the lift answered: %q", after[len(atAnswer):])
	}
}

// A lift or a clipboard emptying for takeover 3, held up until the next
// takeover has begun, does nothing to it: its held keys stay held, its
// clipboard stays (PROJ-196).
func TestStaleCleanupLeavesTheNextTakeoverAlone(t *testing.T) {
	shortWait(t)
	v, _ := newTestVisual(false)
	x := &fakeXdotool{}
	v.live.start = func() (*xdo, error) { return x.proc(), nil }
	fx := newFakeX(v)
	mux := http.NewServeMux()
	v.register(mux)
	do := func(method, target, body, header, value string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if header != "" {
			req.Header.Set(header, value)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	// Takeover 3 holds Shift, and ends.
	do(http.MethodPost, "/input/events", `[{"t":"keydown","k":"Shift_L"}]`, takeoverHeader, "3")
	fenceLift(t, mux, "3")
	// Takeover 4 holds Control, and puts text on the clipboard.
	do(http.MethodPost, "/input/events", `[{"t":"keydown","k":"Control_L"}]`, takeoverHeader, "4")
	if rec := do(http.MethodPost, "/input/clipboard", `{"text":"four's"}`, takeoverHeader, "4"); rec.Code != http.StatusNoContent {
		t.Fatalf("takeover 4's set: %d %s", rec.Code, rec.Body)
	}
	x.mu.Lock()
	x.buf.Reset()
	x.mu.Unlock()
	fx.mu.Lock()
	holders := len(fx.holders)
	fx.mu.Unlock()

	// Takeover 3's lift and emptying arrive late.
	if got := fenceLift(t, mux, "3"); got < 3 {
		t.Errorf("the late lift's fence %d", got)
	}
	if rec := do(http.MethodDelete, "/input/clipboard", "", fenceHeader, "3"); rec.Code != http.StatusNoContent {
		t.Errorf("the late emptying: %d %s", rec.Code, rec.Body)
	}
	x.mu.Lock()
	sent := x.buf.String()
	x.mu.Unlock()
	if sent != "" {
		t.Errorf("the late lift sent %q: takeover 4's keys", sent)
	}
	fx.mu.Lock()
	later := len(fx.holders)
	fx.mu.Unlock()
	if later != holders {
		t.Errorf("the late emptying took the clipboard (%d xclips started)", later-holders)
	}
	if rec := do(http.MethodGet, "/input/clipboard", "", takeoverHeader, "4"); rec.Body.String() != "four's" {
		t.Errorf("takeover 4's clipboard holds %q", rec.Body)
	}
	// Its own lift lets go of Control.
	fenceLift(t, mux, "4")
	x.mu.Lock()
	defer x.mu.Unlock()
	if !strings.Contains(x.buf.String(), "keyup Control_L") {
		t.Errorf("takeover 4's lift sent %q", x.buf.String())
	}
}

// stalledX is an xdotool that stopped reading its input: a write blocks
// until it is killed.
func stalledX() (*xdo, error) {
	killed := make(chan struct{})
	var once sync.Once
	return &xdo{stdin: stalledStdin{killed}, exited: killed, kill: func() { once.Do(func() { close(killed) }) }}, nil
}

type stalledStdin struct{ killed chan struct{} }

func (s stalledStdin) Write([]byte) (int, error) {
	<-s.killed
	return 0, errors.New("broken pipe")
}

func (s stalledStdin) Close() error { return nil }

// A stalled xdotool doesn't hold the fence: a write to it is cut off
// after writeWait (xdotool is killed), so the person's action under way
// ends and the lift goes through (PROJ-196).
func TestStalledXdotoolDoesntHoldTheFence(t *testing.T) {
	was := writeWait
	writeWait = 100 * time.Millisecond
	t.Cleanup(func() { writeWait = was })
	li, _ := newTestLive()
	li.start = stalledX
	mux := http.NewServeMux()
	mux.HandleFunc("POST /input/events", li.events)
	mux.HandleFunc("POST /input/release", li.release)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/input/events", strings.NewReader(`[{"t":"move","x":1,"y":1}]`))
		req.Header.Set(takeoverHeader, "3")
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.Sleep(20 * time.Millisecond) // the write is stuck
	lifted := make(chan struct{})
	go func() {
		defer close(lifted)
		req := httptest.NewRequest(http.MethodPost, "/input/release", nil)
		req.Header.Set(fenceHeader, "3")
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-lifted:
	case <-time.After(3 * time.Second):
		t.Fatal("the lift is held by a stalled xdotool")
	}
}
