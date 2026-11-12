package pagetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/sessions"
)

const (
	// DefaultWait is how long wait_for waits unless asked; MaxWait caps it.
	DefaultWait = 10 * time.Second
	MaxWait     = 30 * time.Second
)

// WaitIn is wait_for's input: one condition, and how long to wait.
type WaitIn struct {
	// Text waits for the page's visible text to contain it (ignoring case).
	Text string `json:"text,omitempty"`
	// Ref waits for an element to be State: visible (the default) or hidden.
	Ref   string `json:"ref,omitempty"`
	State string `json:"state,omitempty"`
	// URLChange waits for the tab's URL to change.
	URLChange bool `json:"url_change,omitempty"`
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
)

// WaitFor waits for one condition on the active tab, polling every 250 ms
// (no events, nothing enabled), for up to timeout_ms (10 s by default,
// 30 s at most). A handoff, a hold or a stop ends it at once. It answers
// "ok" or "timed out".
func (t *Tools) WaitFor(ctx context.Context, s sessions.Session, who Caller, in WaitIn) (string, error) {
	conds := 0
	for _, set := range []bool{in.Text != "", in.Ref != "", in.URLChange} {
		if set {
			conds++
		}
	}
	if conds != 1 {
		return "", &Error{Status: 400, Msg: "wait for one thing: text, a ref (visible or hidden), or url_change"}
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
		deadline := time.Now().Add(timeout)
		var start string
		what := "Waited for text"
		switch {
		case in.URLChange:
			what = "Waited for the URL to change"
			if err := c.eval(ctx, jsHref, &start); err != nil {
				return "", err
			}
		case in.Ref != "":
			state := in.State
			if state == "" {
				state = "visible"
			}
			what = "Waited for " + in.Ref + " to be " + state
		}
		for {
			ok, err := c.check1(ctx, in, start)
			if err != nil {
				return "", err
			}
			if ok {
				c.note(what + ": ok")
				return c.result("page", "", "ok"), nil
			}
			if time.Now().After(deadline) {
				c.note(what + ": timed out")
				return c.result("page", "", fmt.Sprintf("timed out after %s", timeout)), nil
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
func (c *call) check1(ctx context.Context, in WaitIn, startURL string) (bool, error) {
	switch {
	case in.URLChange:
		var href string
		err := c.eval(ctx, jsHref, &href)
		return err == nil && href != startURL, err
	case in.Text != "":
		arg, _ := json.Marshal(in.Text)
		var ok bool
		err := c.eval(ctx, jsHasText+"("+string(arg)+")", &ok)
		return ok, err
	}
	hidden := in.State == "hidden"
	n, err := c.resolve(ctx, in.Ref)
	var e *Error
	if errors.As(err, &e) && strings.HasPrefix(e.Msg, "stale ref") && hidden {
		return true, nil // gone counts as hidden
	}
	if err != nil {
		return false, err
	}
	var visible bool
	if err := c.callOn(ctx, n.obj, jsVisible, nil, &visible); err != nil {
		return false, err
	}
	return visible != hidden, nil
}
