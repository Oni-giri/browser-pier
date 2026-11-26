package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeXdotool records what would go to xdotool's stdin.
type fakeXdotool struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	broken bool
}

func (f *fakeXdotool) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return 0, errors.New("broken pipe")
	}
	return f.buf.Write(p)
}

func (f *fakeXdotool) Close() error { return nil }

func newTestLive(fakes ...*fakeXdotool) (*liveInput, *int) {
	starts := 0
	li := newLiveInput(1280, 800)
	li.start = func() (ioWriteCloser, error) {
		f := fakes[starts]
		starts++
		return f, nil
	}
	return li, &starts
}

func post(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return rec
}

func TestLiveEvents(t *testing.T) {
	x := &fakeXdotool{}
	li, _ := newTestLive(x)
	rec := post(li.events, `[{"t":"move","x":10,"y":20},{"t":"down","b":1},{"t":"up","b":1},{"t":"wheel","dy":2,"dx":-1},{"t":"keydown","k":"Shift_L"},{"t":"keydown","k":"U00E9"},{"t":"keyup","k":"U00E9"}]`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("events: %d %s", rec.Code, rec.Body)
	}
	want := "mousemove 10 20\nmousedown 1\nmouseup 1\nclick 5\nclick 5\nclick 6\nkeydown Shift_L\nkeydown U00E9\nkeyup U00E9\n"
	if got := x.buf.String(); got != want {
		t.Fatalf("sent\n%s\nwant\n%s", got, want)
	}

	// Releasing lets go of what's still held: Shift.
	x.buf.Reset()
	if rec := post(li.release, ""); rec.Code != http.StatusNoContent || x.buf.String() != "keyup Shift_L\n" {
		t.Fatalf("release: %d %q", rec.Code, x.buf.String())
	}
}

func TestLiveEventsRefuseAnythingElse(t *testing.T) {
	x := &fakeXdotool{}
	li, _ := newTestLive(x)
	for _, body := range []string{
		`[{"t":"move","x":1280,"y":0}]`,
		`[{"t":"down","b":9}]`,
		`[{"t":"keydown","k":"a\nexec sh"}]`,
		`[{"t":"keydown","k":"exec sh"}]`,
		`[{"t":"keydown","k":"exec"}]`,
		`[{"t":"keyup","k":"windowkill"}]`,
		`[{"t":"exec"}]`,
		`[{"t":"move","x":1,"y":1,"cmd":"x"}]`,
	} {
		if rec := post(li.events, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if x.buf.Len() != 0 {
		t.Fatalf("refused events still reached xdotool: %q", x.buf.String())
	}
}

func TestLiveRestartsXdotool(t *testing.T) {
	dead, fresh := &fakeXdotool{broken: true}, &fakeXdotool{}
	li, starts := newTestLive(dead, fresh)
	if rec := post(li.events, `[{"t":"move","x":1,"y":1}]`); rec.Code != http.StatusNoContent {
		t.Fatalf("events: %d", rec.Code)
	}
	if *starts != 2 || fresh.buf.String() != "mousemove 1 1\n" {
		t.Fatalf("starts %d, sent %q", *starts, fresh.buf.String())
	}
}

// A release written to xdotool keeps its key held until xdotool is known
// to have run it (drain), and isn't written twice meanwhile (the second
// release only checks xdotool is there, with a no-op).
func TestLiveReleaseStaysHeldUntilItRan(t *testing.T) {
	x := &fakeXdotool{}
	li, _ := newTestLive(x)
	post(li.events, `[{"t":"keydown","k":"Control_L"}]`)
	post(li.release, "")
	post(li.release, "")
	if got := x.buf.String(); got != "keydown Control_L\nkeyup Control_L\nsleep 0\n" {
		t.Fatalf("sent %q", got)
	}
	if !li.held["kControl_L"] || !li.released["kControl_L"] {
		t.Fatalf("held %v, released %v", li.held, li.released)
	}
	li.mu.Lock()
	err := li.drain()
	li.mu.Unlock()
	if err != nil || len(li.held) != 0 || len(li.released) != 0 {
		t.Fatalf("after a clean drain: %v, held %v, released %v", err, li.held, li.released)
	}
}

// A release counts as queued only once written. If xdotool can't be
// started, a key held across a drain stays held, a later release isn't
// skipped, and a drain doesn't drop it.
func TestLiveReleaseIsQueuedOnlyOnceWritten(t *testing.T) {
	li := newLiveInput(1280, 800)
	x := &fakeXdotool{}
	li.start = func() (ioWriteCloser, error) { return x, nil }
	post(li.events, `[{"t":"keydown","k":"Shift_L"}]`)
	li.mu.Lock()
	if err := li.drain(); err != nil {
		t.Fatal(err)
	}
	li.mu.Unlock()
	// Starting xdotool fails now (too many open files, say).
	li.start = func() (ioWriteCloser, error) { return nil, errors.New("too many open files") }
	if rec := post(li.events, `[{"t":"keyup","k":"Shift_L"}]`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("keyup with no xdotool: %d", rec.Code)
	}
	if rec := post(li.release, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("release with no xdotool: %d", rec.Code)
	}
	if !li.held["kShift_L"] || li.released["kShift_L"] {
		t.Fatalf("held %v, released %v", li.held, li.released)
	}
	var synced []string
	li.runSync = func(args ...string) error { synced = append(synced, strings.Join(args, " ")); return nil }
	li.mu.Lock()
	err := li.drain()
	li.mu.Unlock()
	if err != nil || !li.held["kShift_L"] || len(synced) != 0 {
		t.Fatalf("drain with nothing written: %v, held %v, synced %v", err, li.held, synced)
	}
	// Once xdotool starts again, the release goes out.
	y := &fakeXdotool{}
	li.start = func() (ioWriteCloser, error) { return y, nil }
	post(li.release, "")
	if got := y.buf.String(); got != "keyup Shift_L\n" {
		t.Fatalf("release sent %q", got)
	}
}

// A batch whose first write hits a dead xdotool and whose retry, in a new
// one, goes through keeps its own releases queued; the dead one's, which
// may not have run, go to the new one first.
func TestLiveRetryKeepsTheBatchsReleases(t *testing.T) {
	dead, fresh := &fakeXdotool{}, &fakeXdotool{}
	li, _ := newTestLive(dead, fresh)
	post(li.events, `[{"t":"keydown","k":"Control_L"},{"t":"keydown","k":"a"},{"t":"keyup","k":"a"}]`)
	if !li.released["ka"] {
		t.Fatalf("released %v", li.released)
	}
	dead.mu.Lock()
	dead.broken = true
	dead.mu.Unlock()
	if rec := post(li.events, `[{"t":"keyup","k":"Control_L"}]`); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if !li.released["kControl_L"] || !li.released["ka"] || !li.held["ka"] || len(li.lost) != 0 {
		t.Fatalf("held %v, released %v, lost %v", li.held, li.released, li.lost)
	}
	if got := fresh.buf.String(); got != "keyup a\nkeyup Control_L\n" {
		t.Fatalf("retry sent %q", got)
	}
}

// A drain with no xdotool running counts nothing marked released as
// done: whatever marked it, no xdotool can have run that release.
func TestLiveDrainWithNothingRunningKeepsReleasesUnconfirmed(t *testing.T) {
	li := newLiveInput(1280, 800)
	li.held["kShift_L"], li.released["kShift_L"] = true, true
	var synced []string
	li.runSync = func(args ...string) error { synced = append(synced, strings.Join(args, " ")); return nil }
	li.mu.Lock()
	err := li.drain()
	li.mu.Unlock()
	if err != nil || len(synced) != 1 || synced[0] != "keyup Shift_L" {
		t.Fatalf("drain: %v, synced %v", err, synced)
	}
}

// A hand-back release that finds xdotool dead sends a new one a release
// for every key held, not only those that had none queued in the dead
// one, which may never have run them.
func TestLiveReleaseAfterADeadXdotoolReleasesEverything(t *testing.T) {
	dead, fresh := &fakeXdotool{}, &fakeXdotool{}
	li, _ := newTestLive(dead, fresh)
	post(li.events, `[{"t":"keydown","k":"Control_L"},{"t":"keydown","k":"a"},{"t":"keyup","k":"Control_L"}]`)
	if !li.released["kControl_L"] || li.released["ka"] {
		t.Fatalf("released %v", li.released)
	}
	// xdotool crashes before running keyup Control_L.
	dead.mu.Lock()
	dead.broken = true
	dead.mu.Unlock()
	if rec := post(li.release, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if got := fresh.buf.String(); got != "keyup Control_L\nkeyup a\n" {
		t.Fatalf("the new xdotool got %q", got)
	}
	if !li.released["kControl_L"] || !li.released["ka"] {
		t.Fatalf("released %v", li.released)
	}
}

// With every held key's release already queued, a hand-back release still
// checks the xdotool that has them; a dead one's go to a new one.
func TestLiveReleaseChecksTheXdotoolWithTheQueuedReleases(t *testing.T) {
	dead, fresh := &fakeXdotool{}, &fakeXdotool{}
	li, _ := newTestLive(dead, fresh)
	post(li.events, `[{"t":"keydown","k":"Control_L"},{"t":"keyup","k":"Control_L"}]`)
	dead.mu.Lock()
	dead.broken = true
	dead.mu.Unlock()
	if rec := post(li.release, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if got := fresh.buf.String(); got != "keyup Control_L\nsleep 0\n" {
		t.Fatalf("the new xdotool got %q", got)
	}
	// Alive, it gets only the no-op, and nothing is released twice.
	x := &fakeXdotool{}
	li, _ = newTestLive(x)
	post(li.events, `[{"t":"keydown","k":"Control_L"},{"t":"keyup","k":"Control_L"}]`)
	post(li.release, "")
	if got := x.buf.String(); got != "keydown Control_L\nkeyup Control_L\nsleep 0\n" {
		t.Fatalf("sent %q", got)
	}
	// Nothing held: nothing sent, no xdotool started.
	li, starts := newTestLive()
	if rec := post(li.release, ""); rec.Code != http.StatusNoContent || *starts != 0 {
		t.Fatalf("nothing held: %d, %d starts", rec.Code, *starts)
	}
}

// A person's keys in the live view neither wait for the agent's shortcut
// gap nor arm it (PROJ-151).
func TestLiveKeysIgnoreTheShortcutGap(t *testing.T) {
	c := newClockedVisual()
	x := &fakeXdotool{}
	c.live, _ = newTestLive(x)
	c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+l", "pace": "instant"}`)
	rec := serveVisual(c.visual, http.MethodPost, "/input/events", `[{"t":"keydown","k":"Control_L"},{"t":"keydown","k":"l"},{"t":"keyup","k":"l"},{"t":"keyup","k":"Control_L"},{"t":"keydown","k":"a"},{"t":"keyup","k":"a"}]`)
	if rec.Code != http.StatusNoContent || !strings.HasSuffix(x.buf.String(), "keydown a\nkeyup a\n") {
		t.Fatalf("events: %d %q", rec.Code, x.buf.String())
	}
	if len(c.slept) != 0 {
		t.Fatalf("the live view waited %v", c.slept)
	}
	// Past the agent's gap, a person's ctrl+l doesn't make the agent wait.
	c.t = c.t.Add(time.Second)
	serveVisual(c.visual, http.MethodPost, "/input/events", `[{"t":"keydown","k":"Control_L"},{"t":"keydown","k":"l"},{"t":"keyup","k":"l"},{"t":"keyup","k":"Control_L"}]`)
	c.do(t, http.MethodPost, "/input/type", `{"text": "x", "pace": "instant"}`)
	if len(c.slept) != 0 {
		t.Fatalf("a person's ctrl+l made the agent wait %v", c.slept)
	}
}

// The clipboard box's paste is the person's input too: it doesn't wait for
// an agent's shortcut gap, nor make the agent's next keys wait.
func TestLivePasteIgnoresTheShortcutGap(t *testing.T) {
	c := newClockedVisual()
	newFakeX(c.visual)
	c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+l", "pace": "instant"}`)
	if rec := serveVisual(c.visual, http.MethodPost, "/input/paste", `{"text":"x"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("paste: %d %s", rec.Code, rec.Body)
	}
	if len(c.slept) != 0 {
		t.Fatalf("the paste waited %v", c.slept)
	}
	// Past the agent's gap, a person's paste doesn't arm it again.
	c.t = c.t.Add(time.Second)
	serveVisual(c.visual, http.MethodPost, "/input/paste", `{"text":"y"}`)
	c.do(t, http.MethodPost, "/input/type", `{"text": "x", "pace": "instant"}`)
	if len(c.slept) != 0 {
		t.Fatalf("the person's Ctrl+V made the agent wait %v", c.slept)
	}
}

// The live view's Type button is the person's input too: it types at once
// without waiting for an agent's shortcut gap, and doesn't arm it. Its text
// is checked as /input/type's.
func TestLiveTypeIgnoresTheShortcutGap(t *testing.T) {
	c := newClockedVisual()
	c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+l", "pace": "instant"}`)
	at := c.do(t, http.MethodPost, "/input/live-type", `{"text": "hello"}`)
	if len(c.slept) != 0 || at != xdotoolTakes {
		t.Fatalf("the person's text waited %v, started at %v", c.slept, at)
	}
	if got := strings.Join(c.calls[1].args, " "); got != "type --clearmodifiers --delay 12 -- hello" {
		t.Fatalf("typed with %q", got)
	}
	// Past the agent's gap, the person's text doesn't arm it again.
	c.t = c.t.Add(time.Second)
	c.do(t, http.MethodPost, "/input/live-type", `{"text": "x"}`)
	c.do(t, http.MethodPost, "/input/type", `{"text": "x", "pace": "instant"}`)
	if len(c.slept) != 0 {
		t.Fatalf("the person's text made the agent wait %v", c.slept)
	}
	for _, body := range []string{`{"text": ""}`, `{"text": "` + strings.Repeat("a", maxTypeLen+1) + `"}`, `{"text": "x", "pace": "human"}`, `{}`} {
		if rec := serveVisual(c.visual, http.MethodPost, "/input/live-type", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%.40s: %d", body, rec.Code)
		}
	}
}

// A key-up written to an xdotool that died before running it is lost: the
// next xdotool, started by any event (a mouse move), gets it first, and a
// drain then finds nothing held.
func TestLiveLostReleaseGoesToTheNextXdotool(t *testing.T) {
	dead, fresh := &fakeXdotool{}, &fakeXdotool{}
	li, _ := newTestLive(dead, fresh)
	post(li.events, `[{"t":"keydown","k":"Control_L"},{"t":"keyup","k":"Control_L"}]`)
	dead.mu.Lock()
	dead.broken = true
	dead.mu.Unlock()
	if rec := post(li.events, `[{"t":"move","x":5,"y":5}]`); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if got := fresh.buf.String(); got != "keyup Control_L\nmousemove 5 5\n" {
		t.Fatalf("the new xdotool got %q", got)
	}
	li.mu.Lock()
	err := li.drain()
	li.mu.Unlock()
	if err != nil || len(li.held) != 0 || len(li.lost) != 0 {
		t.Fatalf("drain: %v, held %v, lost %v", err, li.held, li.lost)
	}
}

// If no new xdotool can start, the lost key-up waits; a drain then runs it
// in an xdotool of its own instead of counting nothing as held.
func TestLiveDrainRunsALostRelease(t *testing.T) {
	li := newLiveInput(1280, 800)
	dead := &fakeXdotool{}
	li.start = func() (ioWriteCloser, error) { return dead, nil }
	post(li.events, `[{"t":"keydown","k":"Control_L"},{"t":"keyup","k":"Control_L"}]`)
	dead.mu.Lock()
	dead.broken = true
	dead.mu.Unlock()
	li.start = func() (ioWriteCloser, error) { return nil, errors.New("too many open files") }
	if rec := post(li.events, `[{"t":"move","x":5,"y":5}]`); rec.Code != http.StatusInternalServerError {
		t.Fatal(rec.Code)
	}
	var synced []string
	li.runSync = func(args ...string) error { synced = append(synced, strings.Join(args, " ")); return nil }
	li.mu.Lock()
	err := li.drain()
	li.mu.Unlock()
	if err != nil || len(synced) != 1 || synced[0] != "keyup Control_L" || len(li.held) != 0 {
		t.Fatalf("drain: %v, synced %v, held %v", err, synced, li.held)
	}
}

// At a hand-back (?drain=1) the release answers only once it has run: the
// xdotool is ended; a wedged one is killed, and what is held let go in an
// xdotool of its own, or the release fails. Without drain (blur, Escape),
// it only queues.
func TestLiveHandBackReleaseDrains(t *testing.T) {
	x := &closingXdotool{}
	li, _ := newTestLive()
	li.start = func() (ioWriteCloser, error) { return x, nil }
	post(li.events, `[{"t":"keydown","k":"Control_L"}]`)
	if rec := post(li.release, ""); rec.Code != http.StatusNoContent || x.closed != 0 {
		t.Fatalf("blur release: %d, closed %d", rec.Code, x.closed)
	}
	rec := httptest.NewRecorder()
	li.release(rec, httptest.NewRequest(http.MethodPost, "/input/release?drain=1", nil))
	if rec.Code != http.StatusNoContent || x.closed != 1 || len(li.held) != 0 {
		t.Fatalf("hand-back: %d, closed %d, held %v", rec.Code, x.closed, li.held)
	}

	// Wedged: killed, then let go in its own xdotool.
	li, _ = newTestLive()
	li.start = func() (ioWriteCloser, error) { return &killedXdotool{}, nil }
	var synced []string
	li.runSync = func(args ...string) error { synced = append(synced, strings.Join(args, " ")); return nil }
	post(li.events, `[{"t":"keydown","k":"Control_L"}]`)
	rec = httptest.NewRecorder()
	li.release(rec, httptest.NewRequest(http.MethodPost, "/input/release?drain=1", nil))
	if rec.Code != http.StatusNoContent || len(synced) != 1 || synced[0] != "keyup Control_L" {
		t.Fatalf("wedged hand-back: %d, synced %v", rec.Code, synced)
	}
	// And if that fails too, the hand-back is told.
	li, _ = newTestLive()
	li.start = func() (ioWriteCloser, error) { return &killedXdotool{}, nil }
	li.runSync = func(...string) error { return errors.New("xdotool: exit status 1") }
	post(li.events, `[{"t":"keydown","k":"Control_L"}]`)
	rec = httptest.NewRecorder()
	li.release(rec, httptest.NewRequest(http.MethodPost, "/input/release?drain=1", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed hand-back: %d", rec.Code)
	}
}
