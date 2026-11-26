package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
// to have run it (drain), and isn't written twice meanwhile.
func TestLiveReleaseStaysHeldUntilItRan(t *testing.T) {
	x := &fakeXdotool{}
	li, _ := newTestLive(x)
	post(li.events, `[{"t":"keydown","k":"Control_L"}]`)
	post(li.release, "")
	post(li.release, "")
	if got := x.buf.String(); got != "keydown Control_L\nkeyup Control_L\n" {
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
// one, goes through keeps its own releases queued; only the dead one's
// are forgotten.
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
	if !li.released["kControl_L"] || li.released["ka"] || !li.held["ka"] {
		t.Fatalf("held %v, released %v", li.held, li.released)
	}
	if got := fresh.buf.String(); got != "keyup Control_L\n" {
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
