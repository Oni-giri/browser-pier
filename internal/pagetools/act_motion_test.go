package pagetools

import (
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/motion"
)

// mouse returns a tab's mouse events so far, and forgets them.
func (f *fixture) mouse(tb *ftab) []mouseEvent {
	f.fb.mu.Lock()
	defer f.fb.mu.Unlock()
	in := f.fb.input(tb)
	out := in.mouse
	in.mouse, in.events = nil, nil
	return out
}

func (f *fixture) takePauses() []time.Duration {
	f.pauseMu.Lock()
	defer f.pauseMu.Unlock()
	out := f.pauses
	f.pauses = nil
	return out
}

// split is a click's moves, and its press and release.
func split(t *testing.T, ev []mouseEvent) (moves []mouseEvent, press, release mouseEvent) {
	t.Helper()
	for i, e := range ev {
		if e.typ == "mousePressed" {
			if i+1 >= len(ev) || ev[i+1].typ != "mouseReleased" {
				t.Fatalf("press not followed by its release: %v", ev)
			}
			return ev[:i], e, ev[i+1]
		}
		if e.typ != "mouseMoved" {
			t.Fatalf("unexpected %v", e)
		}
	}
	t.Fatalf("no press in %v", ev)
	return
}

func TestActClickGlidesAimsAndHolds(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	// Sign in is 100x30 at (10, 200): its middle is (60, 215).
	landed := map[[2]float64]bool{}
	for i := range 10 {
		f.takePauses()
		if _, err := f.act(Step{Action: "click", Ref: r["Sign in"]}); err != nil {
			t.Fatal(err)
		}
		moves, press, release := split(t, f.mouse(tb))
		if len(moves) < 10 {
			t.Fatalf("click %d: %d moves, want a glide", i, len(moves))
		}
		last := moves[len(moves)-1]
		if last.x != press.x || last.y != press.y || release.x != press.x || release.y != press.y {
			t.Fatalf("pressed at %v after moving to %v, released at %v", press, last, release)
		}
		if math.Abs(press.x-60) > 40 || math.Abs(press.y-215) > 12 {
			t.Fatalf("click %d at (%v, %v): beyond 40%% of the button from its middle", i, press.x, press.y)
		}
		// The first glide, in from an edge, isn't a straight line.
		if i == 0 {
			if dev := deviation(moves); dev <= 2 {
				t.Errorf("moves at most %v px off a straight line", dev)
			}
		}
		landed[[2]float64{press.x, press.y}] = true
		// A dwell before the press, and a hold of 60-200 ms.
		ps := f.takePauses()
		hold := ps[len(ps)-1]
		if hold < 60*time.Millisecond || hold > 200*time.Millisecond {
			t.Fatalf("held %v", hold)
		}
		if dwell := ps[len(ps)-2]; dwell < 40*time.Millisecond || dwell > time.Second {
			t.Fatalf("dwelt %v", dwell)
		}
	}
	if len(landed) < 5 {
		t.Fatalf("10 clicks landed on %d points", len(landed))
	}
	if got := p.clicked(); len(got) != 10 {
		t.Fatalf("clicks %v", got)
	}
}

// deviation is how far a path strays from the line between its ends.
func deviation(ev []mouseEvent) float64 {
	a, b := ev[0], ev[len(ev)-1]
	dx, dy := b.x-a.x, b.y-a.y
	d := math.Hypot(dx, dy)
	worst := 0.0
	for _, e := range ev {
		worst = math.Max(worst, math.Abs(dy*(e.x-a.x)-dx*(e.y-a.y))/d)
	}
	return worst
}

func TestActPointerCarriesOnAndEntersFromAnEdge(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	edge := func(e mouseEvent) bool { return e.x < 3 || e.x > 1277 || e.y > 797 }
	near := func(a, b mouseEvent) bool { return math.Hypot(a.x-b.x, a.y-b.y) < 40 }

	if _, err := f.act(Step{Action: "hover", Ref: r["Email"]}); err != nil {
		t.Fatal(err)
	}
	first := f.mouse(tb)
	if !edge(first[0]) {
		t.Fatalf("the first move %v doesn't come in from an edge", first[0])
	}
	end := first[len(first)-1]
	if _, err := f.act(Step{Action: "click", Ref: r["Sign in"]}); err != nil {
		t.Fatal(err)
	}
	if next := f.mouse(tb); !near(next[0], end) {
		t.Fatalf("the next glide starts at %v, not where the pointer stopped (%v)", next[0], end)
	}
	// Once a person has had the browser, the pointer is wherever they
	// left it: it comes in again.
	if _, err := f.m.SetNeedsHelp(f.sess.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.SetNeedsHelp(f.sess.ID, false); err != nil {
		t.Fatal(err)
	}
	r = f.refs()
	if _, err := f.act(Step{Action: "click", Ref: r["Sign in"]}); err != nil {
		t.Fatal(err)
	}
	if again := f.mouse(tb); near(again[0], end) {
		t.Fatalf("after a handoff the glide starts at %v, where it was before", again[0])
	}
}

func TestActInstantPaceIsAsBefore(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	f.takePauses()
	if _, err := f.act(Step{Action: "click", Ref: r["Sign in"], Pace: "instant"}, Step{Action: "type", Ref: r["Email"], Text: text("ab"), Pace: "instant"}); err != nil {
		t.Fatal(err)
	}
	ev := f.mouse(tb)
	if len(ev) != 3 || ev[0] != (mouseEvent{"mouseMoved", 60, 215}) || ev[1] != (mouseEvent{"mousePressed", 60, 215}) || ev[2] != (mouseEvent{"mouseReleased", 60, 215}) {
		t.Fatalf("instant click %v", ev)
	}
	if ps := f.takePauses(); len(ps) != 0 {
		t.Fatalf("instant pace paused %v", ps)
	}
	if *p.email.value != "ab" {
		t.Fatalf("typed %q", *p.email.value)
	}
}

func TestActTypesWithATypistsRhythm(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	const typed = "the quick brown fox, jumps é!"
	f.takePauses()
	if _, err := f.act(Step{Action: "type", Ref: r["Email"], Text: text(typed)}); err != nil {
		t.Fatal(err)
	}
	if *p.email.value != typed {
		t.Fatalf("typed %q", *p.email.value)
	}
	// Each key after the first waits for its gap; each one with a key on
	// the keyboard (not é, which is inserted) is held.
	ps := f.takePauses()
	var gaps []float64
	for i, ch := range []rune(typed) {
		if i > 0 {
			gaps, ps = append(gaps, ps[0].Seconds()), ps[1:]
		}
		if _, ok := usKeys[ch]; ok {
			if ps[0] < 60*time.Millisecond || ps[0] > 200*time.Millisecond {
				t.Fatalf("key %d held %v", i, ps[0])
			}
			ps = ps[1:]
		}
	}
	if len(ps) != 0 {
		t.Fatalf("pauses left over: %v", ps)
	}
	mean, sd := stats(gaps)
	if sd/mean < 0.25 || mean < 0.12 || mean > 0.45 {
		t.Fatalf("gaps mean %.3f s, SD %.3f: want a typist's uneven rhythm", mean, sd)
	}
	f.fb.mu.Lock()
	ev := strings.Join(f.fb.input(tb).events, "|")
	f.fb.mu.Unlock()
	if !strings.Contains(ev, "rawKeyDown t|char t|keyUp t|rawKeyDown h") {
		t.Fatalf("events %s", ev)
	}
}

func stats(xs []float64) (mean, sd float64) {
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	for _, x := range xs {
		sd += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(sd / float64(len(xs)))
}

func TestActPaceIsChecked(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	if _, err := f.act(Step{Action: "click", Ref: r["Sign in"], Pace: "slow"}); !isStatus(err, 400) {
		t.Fatalf("a bad pace: %v", err)
	}
	long := strings.Repeat("a", 300)
	_, err := f.act(Step{Action: "type", Ref: r["Email"], Text: &long}, Step{Action: "type", Ref: r["Password"], Text: &long})
	if !isStatus(err, 400) || !strings.Contains(err.Error(), "instant") {
		t.Fatalf("600 characters at human pace: %v", err)
	}
	if len(p.clicked()) != 0 || *p.email.value != "" {
		t.Fatal("a refused call acted")
	}
	// At instant pace, or as a fill, the same is fine.
	if _, err := f.act(Step{Action: "type", Ref: r["Email"], Text: &long, Pace: "instant"}, Step{Action: "type", Ref: r["Password"], Text: &long}); err != nil {
		t.Fatal(err)
	}
}

// The pointer takes a while to get there: if something covers the element
// meanwhile, the click doesn't go out.
func TestActClickChecksAgainAfterTheGlide(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	f.fb.onInput = func(typ string) {
		if typ == "mouseMoved" {
			f.fb.behave[p.signin].cover = p.overlay
		}
	}
	_, err := f.act(Step{Action: "click", Ref: r["Sign in"]})
	if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "covered") {
		t.Fatalf("got %v", err)
	}
	for _, e := range f.mouse(tb) {
		if e.typ != "mouseMoved" {
			t.Fatalf("%v went out", e)
		}
	}
	if len(p.clicked()) != 0 {
		t.Fatal("clicked")
	}
}

// Each session has its own source of pacing.
func TestEachSessionHasItsOwnMotion(t *testing.T) {
	tools := New(nil, nil)
	a := &state{mot: tools.newMotion()}
	b := &state{mot: tools.newMotion()}
	pa := a.mot.Path(motion.Point{}, motion.Point{X: 300, Y: 100}, 40)
	pb := b.mot.Path(motion.Point{}, motion.Point{X: 300, Y: 100}, 40)
	if pa[len(pa)/2] == pb[len(pb)/2] {
		t.Fatal("two sessions' sources gave the same path")
	}
}
