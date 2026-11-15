package pagetools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/cdp"

	"github.com/Oni-giri/browser-pier/internal/sessions"
)

const (
	// DefaultWait is how long wait_for waits unless asked; MaxWait caps it.
	DefaultWait = 10 * time.Second
	MaxWait     = 30 * time.Second
)

// resourceBuffer is how many Resource Timing entries Chrome keeps for a
// page unless it asks for more: a page with exactly this many has most
// likely filled its list, and its later requests aren't recorded.
const resourceBuffer = 250

// loadQuiet is how long the load condition wants the page to have
// finished no request (nor its load) before it counts as loaded.
const loadQuiet = 500 * time.Millisecond

// WaitIn is wait_for's input: one condition, and how long to wait.
type WaitIn struct {
	// Text waits for the page's visible text to contain it (ignoring case).
	Text string `json:"text,omitempty"`
	// Ref waits for an element to be State: visible (the default) or hidden.
	Ref   string `json:"ref,omitempty"`
	State string `json:"state,omitempty"`
	// URLChange waits for the tab's URL to change.
	URLChange bool `json:"url_change,omitempty"`
	// Load waits for the document to be complete and the page to have
	// finished no request for loadQuiet (PROJ-122).
	Load      bool `json:"load,omitempty"`
	TimeoutMS int  `json:"timeout_ms,omitempty"`
}

const (
	// jsHasText: the page's rendered text (innerText, which leaves out
	// what CSS hides) contains the text, ignoring case.
	jsHasText = `/*bp:hastext*/ ((want) => { const b = document.body; return !!b && b.innerText.toLowerCase().includes(want.toLowerCase()); })`
	// jsVisible: laid out, not display:none or visibility:hidden, with a
	// non-empty box: the snapshot's own rules.
	jsVisible = `/*bp:visible*/ function() {
  if (!this.isConnected) return false;
  if (this.checkVisibility && !this.checkVisibility({checkVisibilityCSS: true, visibilityProperty: true})) return false;
  const r = this.getBoundingClientRect();
  return r.width > 0 && r.height > 0;
}`
	jsHref = `/*bp:href*/ location.href`
	// jsLoad: the document's readyState, how many Resource Timing entries
	// it has, and how long ago (ms) the last of them finished, or its load
	// did. Resource Timing records a request when it ends, so this only
	// reads what the browser keeps anyway; nothing is hooked or observed.
	// A load not yet ended (readyState is complete just before the load
	// event's handlers run) is no quiet at all.
	jsLoad = `/*bp:load*/ (() => {
  const p = performance, rs = p.getEntriesByType('resource'), nav = p.getEntriesByType('navigation')[0];
  let last = nav ? nav.loadEventEnd : 0;
  for (const e of rs) if (e.responseEnd > last) last = e.responseEnd;
  return {ready: document.readyState, count: rs.length, idle: nav && !nav.loadEventEnd ? 0 : p.now() - last};
})()`
)

// waitCond is a condition in words, as BrowserPier says them: as it is
// (already true) and as awaited. The text is the agent's own, quoted.
func waitCond(in WaitIn) (is, awaited string) {
	switch {
	case in.URLChange:
		return "the URL changed", "the URL to change"
	case in.Load:
		return "the page is loaded and the network quiet", "the page to load and the network to go quiet"
	case in.Ref != "":
		state := in.State
		if state == "" {
			state = "visible"
		}
		return in.Ref + " is " + state, in.Ref + " to be " + state
	}
	q := strconv.Quote(capText(collapse(in.Text), maxField))
	return "text " + q + " is on the page", "text " + q + " to appear"
}

// seconds says a duration in seconds, as BrowserPier's answers do:
// "1.4 s"; whole seconds and short timeouts as they are ("10 s", "0.15 s").
func seconds(d time.Duration, exact bool) string {
	if exact {
		return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + " s"
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + " s"
}

// waitCheck is one look at a wait's condition: whether it holds and, when
// it doesn't, the page's state in BrowserPier's words (never page text),
// for a timeout's answer. Gone says it never can: the ref's document is
// no longer in the tab.
type waitCheck struct {
	ok   bool
	now  string
	gone bool
	// note qualifies a met condition, in BrowserPier's words.
	note string
}

// waiter is one wait's state between looks.
type waiter struct {
	in       WaitIn
	startURL string
	// count is how many resource entries the last look saw, and quietFrom
	// when they last went down (the page cleared them, or a new document
	// started a list of its own), if they did: the quiet counts from then
	// too, as the entries' times can't tell it.
	count     int
	quietFrom time.Time
}

// WaitFor waits for one condition on the active tab, polling every 250 ms
// (no events, nothing enabled), for up to timeout_ms (10 s by default,
// 30 s at most). A handoff, a hold or a stop ends it at once. It answers
// "already true when wait started: …" when the first look, before any
// sleep, finds it so (the agent waited for what was already there, often
// its own text echoed), else "waited 1.4 s for …", or "timed out after
// 10 s waiting for …" with, for a ref, what it is now (PROJ-122).
func (t *Tools) WaitFor(ctx context.Context, s sessions.Session, who Caller, in WaitIn) (string, error) {
	conds := 0
	for _, set := range []bool{in.Text != "", in.Ref != "", in.URLChange, in.Load} {
		if set {
			conds++
		}
	}
	if conds != 1 {
		return "", &Error{Status: 400, Msg: "wait for one thing: text, a ref (visible or hidden), url_change or load"}
	}
	if len([]rune(in.Text)) > maxQuery {
		return "", &Error{Status: 400, Msg: fmt.Sprintf("text: at most %d characters", maxQuery)}
	}
	switch in.State {
	case "", "visible", "hidden":
	default:
		return "", &Error{Status: 400, Msg: "state: visible or hidden"}
	}
	if in.State != "" && in.Ref == "" {
		return "", &Error{Status: 400, Msg: "state goes with a ref"}
	}
	timeout := DefaultWait
	if in.TimeoutMS != 0 {
		timeout = time.Duration(in.TimeoutMS) * time.Millisecond
	}
	if timeout <= 0 || timeout > MaxWait {
		return "", &Error{Status: 400, Msg: fmt.Sprintf("timeout_ms: want 1 to %d", MaxWait.Milliseconds())}
	}
	return t.run(ctx, s, who, func(c *call) (string, error) {
		ctx := c.lease.Ctx
		w := &waiter{in: in}
		what := "Waited for text"
		switch {
		case in.URLChange:
			what = "Waited for the URL to change"
			if err := c.eval(ctx, jsHref, &w.startURL); err != nil {
				return "", err
			}
		case in.Load:
			what = "Waited for the page to load"
		case in.Ref != "":
			what = "Waited for " + in.Ref + " to be " + cmp.Or(in.State, "visible")
		}
		is, awaited := waitCond(in)
		start := time.Now()
		deadline := start.Add(timeout)
		for first := true; ; first = false {
			ck, err := c.check1(ctx, w, first)
			if err != nil {
				return "", err
			}
			switch {
			case ck.ok && first:
				c.note(what + ": already true")
				return c.result("page", "", "already true when wait started: "+is+ck.note), nil
			case ck.ok:
				took := seconds(time.Since(start), false)
				c.note(what + ": ok after " + took)
				return c.result("page", "", "waited "+took+" for "+awaited+ck.note), nil
			case ck.gone:
				took := seconds(time.Since(start), false)
				c.note(what + ": gone after " + took)
				return c.result("page", "", "gave up after "+took+" waiting for "+awaited+"; now: "+ck.now), nil
			case time.Now().After(deadline):
				c.note(what + ": timed out")
				out := "timed out after " + seconds(timeout, true) + " waiting for " + awaited
				if ck.now != "" {
					out += "; now: " + ck.now
				}
				return c.result("page", "", out), nil
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(c.t.poll):
			}
		}
	})
}

// check1 checks a wait's condition once. The isolated world dies with its
// document, so it is made again after a navigation (refresh).
func (c *call) check1(ctx context.Context, w *waiter, first bool) (waitCheck, error) {
	in := w.in
	switch {
	case in.URLChange:
		var href string
		err := c.eval(ctx, jsHref, &href)
		if errors.Is(err, cdp.ErrWorldGone) {
			return waitCheck{}, nil // still navigating: look again
		}
		return waitCheck{ok: err == nil && href != w.startURL}, err
	case in.Text != "":
		arg, _ := json.Marshal(in.Text)
		var ok bool
		err := c.eval(ctx, jsHasText+"("+string(arg)+")", &ok)
		if errors.Is(err, cdp.ErrWorldGone) {
			return waitCheck{}, nil
		}
		return waitCheck{ok: ok}, err
	case in.Load:
		return c.checkLoad(ctx, w)
	}
	hidden := in.State == "hidden"
	n, err := c.resolve(ctx, in.Ref)
	var e *Error
	if errors.As(err, &e) && strings.HasPrefix(e.Msg, "stale ref") {
		if hidden {
			return waitCheck{ok: true}, nil // gone counts as hidden
		}
		if first {
			return waitCheck{}, err // stale before the wait: as every tool says
		}
		// Gone during the wait. Another document in the tab never brings
		// it back; a node taken out of this one may be put back.
		k, lerr := c.st.refs.lookup(in.Ref)
		moved := lerr != nil || k.loader != c.loader || k.frame != c.frame
		return waitCheck{now: in.Ref + " is gone (the page changed)", gone: moved}, nil
	}
	if err != nil {
		return waitCheck{}, err
	}
	var visible bool
	if err := c.callOn(ctx, n.obj, jsVisible, nil, &visible); err != nil {
		return waitCheck{}, err
	}
	ck := waitCheck{ok: visible != hidden}
	if !ck.ok && !hidden {
		ck.now = in.Ref + " is on the page but hidden"
	}
	return ck, nil
}

// checkLoad is the load condition: the document is complete, and neither
// its load nor any request ended in the last loadQuiet, by the page's
// Resource Timing entries, read from the isolated world. A request still
// under way has no entry yet, so a long poll that never ends doesn't hold
// the wait, and one slower than loadQuiet isn't waited for.
func (c *call) checkLoad(ctx context.Context, w *waiter) (waitCheck, error) {
	var st struct {
		Ready string  `json:"ready"`
		Count int     `json:"count"`
		Idle  float64 `json:"idle"`
	}
	err := c.eval(ctx, jsLoad, &st)
	if errors.Is(err, cdp.ErrWorldGone) {
		w.count, w.quietFrom = 0, time.Now()
		return waitCheck{now: "the next page is still coming"}, nil
	}
	if err != nil {
		return waitCheck{}, err
	}
	if st.Count < w.count {
		w.quietFrom = time.Now()
	}
	w.count = st.Count
	if st.Ready != "complete" {
		ready := "loading"
		if st.Ready == "interactive" {
			ready = "interactive"
		}
		return waitCheck{now: "the document is still loading (readyState " + ready + ")"}, nil
	}
	idle := max(time.Duration(st.Idle*float64(time.Millisecond)), 0)
	if !w.quietFrom.IsZero() {
		idle = min(idle, time.Since(w.quietFrom))
	}
	if idle >= loadQuiet {
		ck := waitCheck{ok: true}
		if st.Count == resourceBuffer {
			ck.note = fmt.Sprintf(" (but the page's list of finished requests is full at %d, so later ones can't be seen)", resourceBuffer)
		}
		return ck, nil
	}
	return waitCheck{now: "the document is complete, but requests keep ending (the last " + seconds(idle, false) + " ago)"}, nil
}
