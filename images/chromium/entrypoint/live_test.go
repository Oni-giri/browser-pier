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

// A release xdotool doesn't take keeps what is held, so the next one
// lets go of it: nothing stays down because one try failed.
func TestLiveReleaseThatFailsIsTriedAgain(t *testing.T) {
	x := &fakeXdotool{}
	dead1, dead2, fresh := &fakeXdotool{broken: true}, &fakeXdotool{broken: true}, &fakeXdotool{}
	li, _ := newTestLive(x, dead1, dead2, fresh)
	if rec := post(li.events, `[{"t":"down","b":1},{"t":"keydown","k":"Control_L"}]`); rec.Code != http.StatusNoContent {
		t.Fatalf("events: %d", rec.Code)
	}
	x.broken = true // xdotool goes away, and the next one too
	if rec := post(li.release, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a release xdotool didn't take: %d", rec.Code)
	}
	if rec := post(li.release, ""); rec.Code != http.StatusNoContent || fresh.buf.String() != "mouseup 1\nkeyup Control_L\n" {
		t.Fatalf("the release after: %d %q", rec.Code, fresh.buf.String())
	}
}
