package pagetools

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// maxTabsListed caps tabs' list.
const maxTabsListed = 20

// manyTabs is how many open tabs make the tools tell the agent to close
// some (PROJ-160). Each tab holds its page's memory; BrowserPier closes
// none by itself.
const manyTabs = 10

// TabsIn is tabs' input.
type TabsIn struct {
	// Action is list (the default), switch, close or new.
	Action string `json:"action,omitempty"`
	// ID is the tab to switch to or close (t1, t2…); close takes the
	// active tab without one.
	ID string `json:"id,omitempty"`
	// URL is what new opens; about:blank without one.
	URL string `json:"url,omitempty"`
}

// Tabs lists the browser's tabs, or switches, closes or opens one. The
// active tab is the browser's foreground tab: switch and new bring theirs
// to the front (Target.activateTarget), so the tools, the live view and a
// handoff all see the same one. Closing the active tab brings the one
// most recently in front before it back.
func (t *Tools) Tabs(ctx context.Context, s sessions.Session, who Caller, in TabsIn) (string, error) {
	url := "about:blank"
	switch in.Action {
	case "", "list":
	case "switch":
		if in.ID == "" {
			return "", &Error{Status: 400, Msg: "switch: say which tab, by its id (t1, t2…) from tabs list"}
		}
	case "close":
	case "new":
		if in.URL != "" {
			u, err := checkURL(in.URL)
			if err != nil {
				return "", err
			}
			url = u
		}
	default:
		return "", &Error{Status: 400, Msg: "action: list, switch, close or new"}
	}
	return t.run(ctx, s, who, func(c *call) (string, error) {
		ctx := c.lease.Ctx
		c.tabsTool = true
		switch in.Action {
		case "switch":
			tb, err := c.tabByID(in.ID)
			if err != nil {
				return "", err
			}
			if err := c.bringToFront(ctx, tb.target); err != nil {
				return "", err
			}
			c.note("Switched to " + tb.id)
			return c.tabResult(ctx, "switched to "+tb.id)
		case "new":
			var r struct {
				TargetID string `json:"targetId"`
			}
			if url != "about:blank" {
				t.openingDirectly(ctx, s, url)
			}
			if err := c.browser(ctx, "Target.createTarget", map[string]any{"url": url}, &r); err != nil {
				return "", err
			}
			if _, err := c.listTabs(ctx); err != nil {
				return "", err
			}
			// Ours, not news.
			if tb := c.st.tabs[r.TargetID]; tb != nil {
				c.notices = slices.DeleteFunc(c.notices, func(n string) bool { return strings.Contains(n, " "+tb.id+" ") })
			}
			if err := c.bringToFront(ctx, r.TargetID); err != nil {
				return "", err
			}
			if url != "about:blank" {
				if _, _, err := c.waitLoad(ctx, "", "", "", false, time.Now().Add(navigateTimeout)); err != nil {
					return "", err
				}
			}
			c.note("Opened a tab (" + origin(url) + ")")
			return c.tabResult(ctx, "opened "+c.tab.id)
		case "close":
			tb := c.tab
			if in.ID != "" {
				var err error
				if tb, err = c.tabByID(in.ID); err != nil {
					return "", err
				}
			}
			if len(c.st.tabs) <= 1 {
				return "", &Error{Status: http.StatusConflict, Msg: "that's the last tab, and closing it would close the browser; open another first, or stop the browser with stop_browser"}
			}
			wasActive := tb.target == c.tgt.ID
			if err := c.browser(ctx, "Target.closeTarget", map[string]any{"targetId": tb.target}, nil); err != nil {
				return "", err
			}
			c.forgetTab(tb.target)
			c.note("Closed " + tb.id)
			if !wasActive {
				return c.tabResult(ctx, "closed "+tb.id)
			}
			// The tab most recently in front before it comes back.
			next := ""
			for id, other := range c.st.tabs {
				if next == "" || other.activated > c.st.tabs[next].activated {
					next = id
				}
			}
			c.tgt = nil
			if err := c.bringToFront(ctx, next); err != nil {
				return "", err
			}
			return c.tabResult(ctx, "closed "+tb.id)
		}
		c.note("Listed tabs")
		return c.list(ctx)
	})
}

// manyTabsLine is BrowserPier's line telling the agent how many tabs are
// open, once there are manyTabs or more: on every tabs result, and on
// another tool's result when the count has grown since the last one. It
// is built from the count alone, never from a page's title or address,
// so it goes after the fence. The count is that of the tabs the call's
// Target.getTargets listed, less any it closed: no CDP call of its own.
func (c *call) manyTabsLine() string {
	n := len(c.st.tabs)
	grown := n > c.st.tabsSaid
	c.st.tabsSaid = n
	if n < manyTabs || !grown && !c.tabsTool {
		return ""
	}
	return fmt.Sprintf("%d tabs are open; close the ones you're done with (tabs, action close)", n)
}

// tabByID finds a tab by the ID the tools gave it.
func (c *call) tabByID(id string) (*tabState, error) {
	for _, tb := range c.st.tabs {
		if tb.id == id {
			return tb, nil
		}
	}
	return nil, &Error{Status: http.StatusNotFound, Msg: fmt.Sprintf("no tab %s; list them with tabs", id)}
}

// bringToFront activates a tab, waits (briefly) until the page sees it is
// in front, and makes it the call's tab.
func (c *call) bringToFront(ctx context.Context, target string) error {
	if err := c.browser(ctx, "Target.activateTarget", map[string]any{"targetId": target}, nil); err != nil {
		return err
	}
	tgt, err := c.attach(ctx, target)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		vis, err := c.visibility(ctx, tgt, c.t.quick)
		if err != nil && ctx.Err() != nil {
			return err
		}
		if vis == "visible" || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	c.st.seq++
	if tb := c.st.tabs[target]; tb != nil {
		tb.activated = c.st.seq
	}
	return c.use(tgt)
}

// tabResult is a tab action's answer: what was done, and the active tab
// now.
func (c *call) tabResult(ctx context.Context, done string) (string, error) {
	pi, err := c.info(ctx)
	if err != nil {
		return "", err
	}
	c.noteTitle(pi.Title) // the active tab's only, never the list's
	return c.result("tabs", done+"; the active tab is now "+c.tab.id+"\n"+header(&page{url: pi.URL, title: pi.Title}), ""), nil
}

// list lists the tabs, oldest first, with the active one marked.
func (c *call) list(ctx context.Context) (string, error) {
	pages, err := c.listTabs(ctx)
	if err != nil {
		return "", err
	}
	byTarget := map[string]targetInfo{}
	for _, p := range pages {
		byTarget[p.TargetID] = p
	}
	var lines []string
	for _, id := range c.st.order {
		tb, p := c.st.tabs[id], byTarget[id]
		if tb == nil || len(lines) == maxTabsListed {
			continue
		}
		l := tb.id + " " + capText(p.URL, maxHeader) + " " + quote(capText(strings.Join(strings.Fields(p.Title), " "), maxField))
		if id == c.tgt.ID {
			l += " (active)"
		}
		lines = append(lines, l)
	}
	trailer := ""
	if n := len(c.st.order); n > maxTabsListed {
		trailer = fmt.Sprintf("…and %d more tabs.", n-maxTabsListed)
	}
	return c.result("tabs", strings.Join(lines, "\n"), trailer), nil
}
