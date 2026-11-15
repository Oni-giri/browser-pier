package pagetools

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
	// Taken out of the document during the wait: it might be put back,
	// so the wait goes on, and the timeout says it's gone.
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.fb.mu.Lock()
		f.fb.removed[p.signin] = true
		f.fb.mu.Unlock()
	}()
	out, err = f.waitIn(WaitIn{Ref: r["Sign in"], TimeoutMS: 150})
	if err != nil || out != "timed out after 0.15 s waiting for "+r["Sign in"]+" to be visible; now: "+r["Sign in"]+" is gone (the page changed)" {
		t.Errorf("removed: %v %q", err, out)
	}
	// Gone before the wait: stale, as every tool says.
	if _, err := f.waitIn(WaitIn{Ref: r["Sign in"]}); !isStatus(err, 409) {
		t.Errorf("stale at the start: %v", err)
	}
	// Another document during the wait: the ref can't come back.
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.fb.mu.Lock()
		f.fb.go_(tb, "https://example.com/next")
		f.fb.mu.Unlock()
	}()
	f.fb.mu.Lock()
	p.buy.box = nil
	f.fb.mu.Unlock()
	start := time.Now()
	out, err = f.waitIn(WaitIn{Ref: r["Buy now"], TimeoutMS: 5000})
	if err != nil || !strings.HasPrefix(out, "gave up after ") || !strings.HasSuffix(out, " s waiting for "+r["Buy now"]+" to be visible; now: "+r["Buy now"]+" is gone (the page changed)") || time.Since(start) > 2*time.Second {
		t.Errorf("navigated: %v %q after %s", err, out, time.Since(start))
	}
	checkSent(t, f.fb)
}
