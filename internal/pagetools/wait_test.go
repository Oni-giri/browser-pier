package pagetools

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/cdp/cdptest"
)

// waitIn runs wait_for on the fixture's session.
func (f *fixture) waitIn(in WaitIn) (string, error) {
	out, err := f.tools.WaitFor(context.Background(), f.session(), f.who, in)
	f.closedAfter()
	return out, err
}

// looks counts the isolated-world scripts with a marker the fake browser
// was sent.
func looks(f *fixture, marker string) int {
	n := 0
	for _, m := range f.fb.Received() {
		if strings.Contains(string(m.Params), "/*bp:"+marker+"*/") {
			n++
		}
	}
	return n
}

// waited reads the seconds out of "waited 1.4 s for …".
func waited(t *testing.T, out string) float64 {
	t.Helper()
	rest, ok := strings.CutPrefix(out, "waited ")
	num, _, ok2 := strings.Cut(rest, " s for ")
	v, err := strconv.ParseFloat(num, 64)
	if !ok || !ok2 || err != nil {
		t.Fatalf("not a wait's answer: %q", out)
	}
	return v
}

// TestWaitAlreadyTrue: a condition true at the first look is answered so,
// before any sleep.
func TestWaitAlreadyTrue(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	p.doc.kids = append(p.doc.kids, txt("Hello from the agent", box(0, 500, 100, 20)))
	f.fb.addTab("https://example.com/", p.doc, true)
	f.tools.poll = 5 * time.Second // a sleep would show
	start := time.Now()
	out, err := f.waitIn(WaitIn{Text: "hello FROM"})
	if err != nil || out != `already true when wait started: text "hello FROM" is on the page` {
		t.Fatalf("%v %q", err, out)
	}
	if d := time.Since(start); d > time.Second || looks(f, "hastext") != 1 {
		t.Errorf("took %s and %d looks", d, looks(f, "hastext"))
	}
	if acts := f.m.Activity(f.sess.ID); len(acts) == 0 || acts[len(acts)-1].What != "Waited for text: already true" {
		t.Errorf("activity: %v", acts)
	}
	checkSent(t, f.fb)
}

// TestWaitSaysHowLong: true at the 4th look, 250 ms apart, is about
// 0.75 s of waiting.
func TestWaitSaysHowLong(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	f.tools.poll = 250 * time.Millisecond
	var mu sync.Mutex
	n := 0
	f.fb.eval = func(_ *ftab, expr string) (any, error) {
		if !strings.Contains(expr, "/*bp:load*/") {
			return nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		n++
		if n < 4 {
			return map[string]any{"ready": "interactive", "count": 2, "idle": 9000}, nil
		}
		return map[string]any{"ready": "complete", "count": 3, "idle": 9000}, nil
	}
	out, err := f.waitIn(WaitIn{Load: true})
	if err != nil || !strings.HasSuffix(out, " s for the page to load and the network to go quiet") {
		t.Fatalf("%v %q", err, out)
	}
	if s := waited(t, out); s < 0.55 || s > 1.05 || n != 4 {
		t.Errorf("waited %v s, %d looks", s, n)
	}
	checkSent(t, f.fb)
}

// TestWaitForLoad: the load condition wants the document complete and
// 500 ms since the last request (or the load) ended, by the entries'
// times, and from when the entries went down.
func TestWaitForLoad(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	var mu sync.Mutex
	var answers []map[string]any
	f.fb.eval = func(_ *ftab, expr string) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		a := answers[0]
		if len(answers) > 1 {
			answers = answers[1:]
		}
		return a, nil
	}
	set := func(as ...map[string]any) {
		mu.Lock()
		answers = as
		mu.Unlock()
	}
	st := func(ready string, count int, idle float64) map[string]any {
		return map[string]any{"ready": ready, "count": count, "idle": idle}
	}
	// Quiet long since: already true.
	set(st("complete", 4, 2000))
	if out, err := f.waitIn(WaitIn{Load: true}); err != nil || out != "already true when wait started: the page is loaded and the network quiet" {
		t.Errorf("quiet: %v %q", err, out)
	}
	// A full list of entries: said, since later requests can't be seen.
	set(st("complete", 250, 2000))
	if out, err := f.waitIn(WaitIn{Load: true}); err != nil || !strings.HasSuffix(out, "network quiet (but the page's list of finished requests is full at 250, so later ones can't be seen)") {
		t.Errorf("full: %v %q", err, out)
	}
	// Requests that keep ending: not quiet until one is 500 ms old.
	set(st("complete", 4, 100), st("complete", 5, 30), st("complete", 6, 499), st("complete", 6, 510))
	if out, err := f.waitIn(WaitIn{Load: true}); err != nil || !strings.HasPrefix(out, "waited ") {
		t.Errorf("busy: %v %q", err, out)
	}
	// Still loading, then requests ending to the end: the timeout says which.
	set(st("loading", 0, 0))
	if out, err := f.waitIn(WaitIn{Load: true, TimeoutMS: 50}); err != nil || out != "timed out after 0.05 s waiting for the page to load and the network to go quiet; now: the document is still loading (readyState loading)" {
		t.Errorf("loading: %v %q", err, out)
	}
	set(st("complete", 9, 120))
	if out, err := f.waitIn(WaitIn{Load: true, TimeoutMS: 50}); err != nil || out != "timed out after 0.05 s waiting for the page to load and the network to go quiet; now: the document is complete, but requests keep ending (the last 0.1 s ago)" {
		t.Errorf("requests: %v %q", err, out)
	}
	// A readyState the page made up isn't echoed.
	set(st("<b>bad</b>", 0, 0))
	if out, err := f.waitIn(WaitIn{Load: true, TimeoutMS: 30}); err != nil || strings.Contains(out, "bad") {
		t.Errorf("odd readyState: %v %q", err, out)
	}
	// The entries went down (the page cleared them): their times can't
	// tell the quiet, so it counts 500 ms from then.
	set(st("complete", 9, 100), st("complete", 0, 5000))
	start := time.Now()
	out, err := f.waitIn(WaitIn{Load: true})
	if err != nil || !strings.HasPrefix(out, "waited ") || time.Since(start) < loadQuiet {
		t.Errorf("after a clear: %v %q after %s", err, out, time.Since(start))
	}
	// A page that stopped its own load (window.stop()): complete, and its
	// load event never comes. Not long enough to tell from the moment
	// before the event: said so.
	stopped := func(quiet float64) map[string]any {
		return map[string]any{"ready": "complete", "count": 3, "idle": 0, "stopped": true, "quiet": quiet}
	}
	set(stopped(5000))
	if out, err := f.waitIn(WaitIn{Load: true, TimeoutMS: 60}); err != nil || out != "timed out after 0.06 s waiting for the page to load and the network to go quiet; now: the document is complete, but its load event hasn't run" {
		t.Errorf("stopped, briefly: %v %q", err, out)
	}
	// Once it has lasted loadQuiet, the quiet counts from its last
	// request (PROJ-122).
	start = time.Now()
	out, err = f.waitIn(WaitIn{Load: true, TimeoutMS: 3000})
	if err != nil || !strings.HasPrefix(out, "waited ") || !strings.HasSuffix(out, "(its load event never ran, as when a page stops its own load: the quiet counts from its last request)") || time.Since(start) < loadQuiet {
		t.Errorf("stopped: %v %q after %s", err, out, time.Since(start))
	}
	set(stopped(100))
	if out, err := f.waitIn(WaitIn{Load: true, TimeoutMS: 700}); err != nil || out != "timed out after 0.7 s waiting for the page to load and the network to go quiet; now: the document is complete without its load event (the page stopped its load), but requests keep ending (the last 0.1 s ago)" {
		t.Errorf("stopped, busy: %v %q", err, out)
	}
	checkSent(t, f.fb)
}

// TestWaitTimeoutSaysWhy: a ref that never shows is on the page but
// hidden, or gone; another document ends the wait at once.
func TestWaitTimeoutSaysWhy(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	f.fb.mu.Lock()
	p.signin.box = nil
	f.fb.mu.Unlock()
	out, err := f.waitIn(WaitIn{Ref: r["Sign in"], TimeoutMS: 60})
	if err != nil || out != "timed out after 0.06 s waiting for "+r["Sign in"]+" to be visible; now: "+r["Sign in"]+" is on the page but hidden" {
		t.Errorf("hidden: %v %q", err, out)
	}
	// Taken out of the document during the wait (after its second look):
	// it might be put back, so the wait goes on, and the timeout says
	// it's gone.
	looks := 0
	f.fb.mu.Lock()
	f.fb.onFunction = func(marker string) {
		if marker == "visible" {
			if looks++; looks == 2 {
				f.fb.removed[p.signin] = true
			}
		}
	}
	f.fb.mu.Unlock()
	out, err = f.waitIn(WaitIn{Ref: r["Sign in"], TimeoutMS: 150})
	if err != nil || out != "timed out after 0.15 s waiting for "+r["Sign in"]+" to be visible; now: "+r["Sign in"]+" is gone (the page changed)" || looks != 2 {
		t.Errorf("removed: %v %q after %d looks", err, out, looks)
	}
	// Gone before the wait: stale, as every tool says.
	if _, err := f.waitIn(WaitIn{Ref: r["Sign in"]}); !isStatus(err, 409) {
		t.Errorf("stale at the start: %v", err)
	}
	// Another document during the wait, on its second look, between
	// resolving the ref and asking whether it's visible: its object dies
	// with the document, which is no refusal, and the ref can't come back.
	looks = 0
	f.fb.mu.Lock()
	f.fb.onFunction = func(marker string) {
		if marker == "connected" {
			if looks++; looks == 2 {
				f.fb.go_(tb, "https://example.com/next")
			}
		}
	}
	p.buy.box = nil
	f.fb.mu.Unlock()
	start := time.Now()
	out, err = f.waitIn(WaitIn{Ref: r["Buy now"], TimeoutMS: 5000})
	if err != nil || !strings.HasPrefix(out, "gave up after ") || !strings.HasSuffix(out, " s waiting for "+r["Buy now"]+" to be visible; now: "+r["Buy now"]+" is gone (the page changed)") || time.Since(start) > 2*time.Second {
		t.Errorf("navigated: %v %q after %s", err, out, time.Since(start))
	}
	checkSent(t, f.fb)
}

// TestObjectGoneIsNoRefusal: an element's object that dies with its
// document between resolving a ref and the code run on it ("Could not
// find object with given id") is a page that changed, never "the browser
// refused" (PROJ-122): a wait looks again, act says the ref is stale.
func TestObjectGoneIsNoRefusal(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	// fail makes the next n runs of the script with this marker answer
	// as a dead object does.
	var mu sync.Mutex
	left := map[string]int{}
	fail := func(marker string, n int) {
		mu.Lock()
		defer mu.Unlock()
		left[marker] = n
	}
	f.fb.mu.Lock()
	f.fb.call = func(tb *ftab, m cdptest.Message) (any, bool, error) {
		if m.Method != "Runtime.callFunctionOn" {
			return nil, false, nil
		}
		var p struct {
			Fn string `json:"functionDeclaration"`
		}
		_ = json.Unmarshal(m.Params, &p)
		mu.Lock()
		defer mu.Unlock()
		for marker, n := range left {
			if n > 0 && strings.Contains(p.Fn, "/*bp:"+marker+"*/") {
				left[marker]--
				return nil, true, cdptest.Error{Message: "Could not find object with given id"}
			}
		}
		return nil, false, nil
	}
	f.fb.mu.Unlock()

	// visible: one dead look, then the next sees it.
	fail("visible", 1)
	out, err := f.waitIn(WaitIn{Ref: r["Sign in"], TimeoutMS: 2000})
	if err != nil || !strings.HasPrefix(out, "waited ") {
		t.Errorf("visible after a dead look: %v %q", err, out)
	}
	// Dead every look until the timeout: it says so, as a change.
	fail("visible", 1000)
	out, err = f.waitIn(WaitIn{Ref: r["Sign in"], TimeoutMS: 60})
	if err != nil || out != "timed out after 0.06 s waiting for "+r["Sign in"]+" to be visible; now: "+r["Sign in"]+" is gone (the page changed)" {
		t.Errorf("dead until the timeout: %v %q", err, out)
	}
	fail("visible", 0)
	// Dead at the connected check, during a wait: as stale.
	fail("connected", 1)
	if _, err := f.waitIn(WaitIn{Ref: r["Sign in"]}); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "stale ref") {
		t.Errorf("wait, dead at connected: %v", err)
	}
	fail("connected", 1)
	out, err = f.waitIn(WaitIn{Ref: r["Sign in"], State: "hidden"})
	if err != nil || !strings.HasPrefix(out, "already true") {
		t.Errorf("hidden, dead at connected: %v %q", err, out)
	}
	// Act: dead at the connected check is a stale ref; dead later in a
	// step is a page that changed, not a refusal.
	fail("connected", 1)
	if _, err := f.act(Step{Action: "click", Ref: r["Sign in"]}); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "stale ref") {
		t.Errorf("act, dead at connected: %v", err)
	}
	fail("hittest", 1)
	if _, err := f.act(Step{Action: "click", Ref: r["Sign in"]}); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "the page changed") || strings.Contains(err.Error(), "refused") {
		t.Errorf("act, dead at the hit test: %v", err)
	}
	checkSent(t, f.fb)
}
