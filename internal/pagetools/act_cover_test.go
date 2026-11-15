package pagetools

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// rect is a content quad for a w×h box at (x, y).
func rect(x, y, w, h float64) []float64 { return []float64{x, y, x + w, y, x + w, y + h, x, y + h} }

func TestPickQuadTakesTheLargestBox(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quads [][]float64
		x, y  float64
		ok    bool
	}{
		// A wrapped link: a 3 px sliver at the end of a line, then a long line.
		{"wrapped link", [][]float64{rect(597, 0, 3, 20), rect(0, 20, 360, 20)}, 180, 30, true},
		{"largest last of three", [][]float64{rect(0, 0, 10, 10), rect(0, 10, 20, 10), rect(0, 20, 300, 10)}, 150, 25, true},
		{"a tie keeps the first", [][]float64{rect(0, 0, 10, 10), rect(100, 0, 10, 10)}, 5, 5, true},
		{"malformed quads are skipped", [][]float64{{1, 2, 3}, rect(10, 10, 20, 20)}, 20, 20, true},
		{"an empty box still has a centre", [][]float64{rect(50, 50, 0, 0)}, 50, 50, true},
		{"no quads", nil, 0, 0, false},
		{"only malformed", [][]float64{{1, 2}}, 0, 0, false},
		{"an empty box at the corner", [][]float64{rect(0, 0, 0, 0)}, 0, 0, false},
	} {
		x, y, ok := pickQuad(tc.quads)
		if x != tc.x || y != tc.y || ok != tc.ok {
			t.Errorf("%s: got (%v, %v, %t), want (%v, %v, %t)", tc.name, x, y, ok, tc.x, tc.y, tc.ok)
		}
	}
}

func coverWait(ms int) *int { return &ms }

// The page's state the fake keeps, read under its lock.
func (f *fixture) behaviourOf(n *fnode) behaviour {
	f.fb.mu.Lock()
	defer f.fb.mu.Unlock()
	return *f.fb.behave[n]
}

func (f *fixture) inputOf(tb *ftab) fakeInput {
	f.fb.mu.Lock()
	defer f.fb.mu.Unlock()
	return *f.fb.input(tb)
}

func TestActRecentresOnlyWhenCovered(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	if _, err := f.act(Step{Action: "click", Ref: r["Sign in"], Pace: paceInstant}, Step{Action: "hover", Ref: r["Sign in"], Pace: paceInstant}, Step{Action: "check", Ref: r["Remember me"]}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*fnode{p.signin, p.remember} {
		if b := f.behaviourOf(n); b.centred != 0 {
			t.Errorf("%s: scrolled to the middle %d times though nothing covered it", n.name, b.centred)
		}
	}
	if b := f.behaviourOf(p.signin); b.hits != 2 {
		t.Errorf("%d hit tests for two uncovered steps", b.hits)
	}
	checkSent(t, f.fb)
}

func TestActWaitsForACoverUpToItsDeadline(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()

	// cover_wait_ms 0: one centre scroll, one more look, no wait.
	start := time.Now()
	_, err := f.act(Step{Action: "click", Ref: r["Buy now"], CoverWaitMS: coverWait(0), Pace: paceInstant})
	if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), `region "Promotion overlay"`) {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("cover_wait_ms 0 took %s", d)
	}
	if b := f.behaviourOf(p.buy); b.centred != 1 || b.hits != 2 {
		t.Errorf("cover_wait_ms 0: %d centre scrolls, %d hit tests; want 1 and 2", b.centred, b.hits)
	}
	if strings.Contains(err.Error(), "dialog") {
		t.Errorf("a dialog is named with none open: %v", err)
	}

	// 200 ms: looks every 10 ms (the fixture's poll) until the deadline,
	// releasing each cover it won't name, and clicks nothing.
	f.fb.mu.Lock()
	f.fb.behave[p.buy].hits, f.fb.behave[p.buy].centred = 0, 0
	f.fb.mu.Unlock()
	before := f.inputOf(tb).released
	start = time.Now()
	_, err = f.act(Step{Action: "click", Ref: r["Buy now"], CoverWaitMS: coverWait(200), Pace: paceInstant})
	d := time.Since(start)
	if !isStatus(err, http.StatusConflict) {
		t.Fatalf("got %v", err)
	}
	if d < 200*time.Millisecond || d > 700*time.Millisecond {
		t.Errorf("a 200 ms wait took %s", d)
	}
	b := f.behaviourOf(p.buy)
	if b.centred != 1 || b.hits < 8 {
		t.Errorf("a 200 ms wait: %d centre scrolls, %d hit tests", b.centred, b.hits)
	}
	if got := f.inputOf(tb).released - before; got != b.hits-1 {
		t.Errorf("released %d covers of %d hit tests; want all but the one named", got, b.hits)
	}
	if got := p.clicked(); len(got) != 0 {
		t.Errorf("clicked %v", got)
	}

	// A step can't ask for a negative wait, or more than 10 s.
	for _, ms := range []int{-1, 10001} {
		if _, err := f.act(Step{Action: "click", Ref: r["Buy now"], CoverWaitMS: coverWait(ms)}); !isStatus(err, http.StatusBadRequest) {
			t.Errorf("cover_wait_ms %d: %v", ms, err)
		}
	}
	checkSent(t, f.fb)
}

func TestActNamesAnOpenDialogFenced(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	title := "Cookie settings"
	f.fb.behave[p.buy].dialog = &title
	_, err := f.act(Step{Action: "click", Ref: r["Buy now"]})
	if !isStatus(err, http.StatusConflict) {
		t.Fatalf("got %v", err)
	}
	msg := err.Error()
	i := strings.Index(msg, `dialog "Cookie settings" is open`)
	if i < 0 || !strings.Contains(msg[:i], "; <untrusted-content-") || !strings.Contains(msg[i:], "</untrusted-content-") {
		t.Errorf("the dialog isn't named, fenced:\n%s", msg)
	}
	if !strings.Contains(msg, `region "Promotion overlay"`) {
		t.Errorf("the cover isn't named:\n%s", msg)
	}
	// A long title is cut to 60 characters; an empty one is still news.
	long := strings.Repeat("Cookies ", 20)
	f.fb.behave[p.buy].dialog = &long
	if _, err := f.act(Step{Action: "click", Ref: r["Buy now"]}); err == nil || !strings.Contains(err.Error(), `dialog "`+strings.TrimSpace(long[:59])+`…" is open`) {
		t.Errorf("long title: %v", err)
	}
	empty := " "
	f.fb.behave[p.buy].dialog = &empty
	if _, err := f.act(Step{Action: "click", Ref: r["Buy now"]}); err == nil || !strings.Contains(err.Error(), "a dialog is open") {
		t.Errorf("untitled dialog: %v", err)
	}
}

func TestActAimsAgainWhenTheTargetMoves(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	// Covered for a few looks; it moves while covered, then the cover goes.
	f.fb.behave[p.buy].onHit = func(hits int) {
		switch hits {
		case 3:
			p.buy.box = box(500, 400, 100, 30)
		case 5:
			f.fb.behave[p.buy].cover = nil
		}
	}
	out, err := f.act(Step{Action: "click", Ref: r["Buy now"], CoverWaitMS: coverWait(2000), Pace: paceInstant})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `clicked `+r["Buy now"]) {
		t.Errorf("got %s", out)
	}
	in := f.inputOf(tb)
	if len(in.mouse) == 0 {
		t.Fatal("no mouse events")
	}
	for _, e := range in.mouse {
		if e.x != 550 || e.y != 415 {
			t.Errorf("%s at (%v, %v), not the moved button's centre (550, 415)", e.typ, e.x, e.y)
		}
	}
	if got := p.clicked(); len(got) != 1 || got[0] != "Buy now" {
		t.Errorf("clicks %v", got)
	}
}

func TestActAimsAtAWrappedLinksLargestBox(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	f.fb.behave[p.signin].quads = [][]float64{rect(597, 200, 3, 20), rect(0, 220, 360, 20)}
	if _, err := f.act(Step{Action: "click", Ref: r["Sign in"], Pace: paceInstant}); err != nil {
		t.Fatal(err)
	}
	in := f.inputOf(tb)
	for _, e := range in.mouse {
		if e.x != 180 || e.y != 230 {
			t.Errorf("%s at (%v, %v), not the second line's centre", e.typ, e.x, e.y)
		}
	}
	// At human pace the hand scatters, but within the second line's box.
	f.fb.mu.Lock()
	f.fb.input(tb).mouse = nil
	f.fb.mu.Unlock()
	if _, err := f.act(Step{Action: "click", Ref: r["Sign in"]}); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.inputOf(tb).mouse {
		if e.typ != "mouseMoved" && (e.x < 0 || e.x > 360 || e.y < 220 || e.y > 240) {
			t.Errorf("%s at (%v, %v), outside the second line (0,220)-(360,240)", e.typ, e.x, e.y)
		}
	}
}

// At human pace, something that covers the point while the pointer goes
// there makes click aim again, within the step's cover wait, once.
func TestActAimsAgainWhenCoveredOnTheWay(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	// Hit tests: 1 aim, 2 the scattered point, 3 the check before the
	// press, which a passing overlay covers; then aim, scatter and check
	// again.
	f.fb.behave[p.signin].onHit = func(hits int) {
		f.fb.behave[p.signin].cover = nil
		if hits == 3 {
			f.fb.behave[p.signin].cover = p.overlay
		}
	}
	if _, err := f.act(Step{Action: "click", Ref: r["Sign in"]}); err != nil {
		t.Fatal(err)
	}
	if got := p.clicked(); len(got) != 1 || got[0] != "Sign in" {
		t.Errorf("clicks %v", got)
	}
	if b := f.behaviourOf(p.signin); b.hits != 6 || b.centred != 0 {
		t.Errorf("%d hit tests, %d centre scrolls; want 6 and 0", b.hits, b.centred)
	}

	// Covered on the way and still after the wait: refused, scrolled to
	// the middle once, nothing clicked.
	f.fb.mu.Lock()
	f.fb.behave[p.signin].hits = 0
	f.fb.behave[p.signin].onHit = func(hits int) {
		f.fb.behave[p.signin].cover = nil
		if hits >= 3 {
			f.fb.behave[p.signin].cover = p.overlay
		}
	}
	f.fb.mu.Unlock()
	start := time.Now()
	_, err := f.act(Step{Action: "click", Ref: r["Sign in"], CoverWaitMS: coverWait(100)})
	if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), `region "Promotion overlay"`) {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %s", d)
	}
	if b := f.behaviourOf(p.signin); b.centred != 1 {
		t.Errorf("%d centre scrolls", b.centred)
	}
	if got := p.clicked(); len(got) != 1 {
		t.Errorf("clicks %v", got)
	}
}

func TestAHandoffEndsACoverWaitAtOnce(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	done := make(chan error, 1)
	go func() {
		_, err := f.tools.Act(context.Background(), f.session(), f.who, ActIn{Steps: []Step{{Action: "click", Ref: r["Buy now"], CoverWaitMS: coverWait(10000), Pace: paceInstant}}})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	if _, err := f.m.SetNeedsHelp(f.sess.ID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "handoff") {
			t.Errorf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the cover wait went on through the handoff")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the handoff waited %s for the call", d)
	}
	if got := p.clicked(); len(got) != 0 {
		t.Errorf("clicked %v", got)
	}
	f.closedAfter()
}
