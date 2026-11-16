package pagetools

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/cdp"
)

// targetInfo is a tab, as Target.getTargets lists it.
type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	OpenerID string `json:"openerId"`
}

// listTabs lists the browser's tabs (page targets) and updates what the
// session knows of them: new ones get an ID (t1, t2…) and, once the tabs
// have been listed before, a notice for the agent; closed ones and their
// refs are forgotten.
func (c *call) listTabs(ctx context.Context) ([]targetInfo, error) {
	var r struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := c.browser(ctx, "Target.getTargets", nil, &r); err != nil {
		return nil, err
	}
	var pages []targetInfo
	open := map[string]bool{}
	for _, ti := range r.TargetInfos {
		if ti.Type != "page" {
			continue
		}
		pages = append(pages, ti)
		open[ti.TargetID] = true
		if _, ok := c.st.tabs[ti.TargetID]; ok {
			continue
		}
		c.st.next++
		// Ranked only once it has been in front (use, bringToFront).
		tb := &tabState{id: "t" + strconv.Itoa(c.st.next), target: ti.TargetID, opener: ti.OpenerID}
		c.st.tabs[ti.TargetID] = tb
		c.st.order = append(c.st.order, ti.TargetID)
		if c.st.listed {
			c.notices = append(c.notices, "a new tab opened: "+tb.id+" ("+quoteOdd(tabAddress(ti.URL))+")")
		}
	}
	for id := range c.st.tabs {
		if !open[id] {
			c.forgetTab(id)
		}
	}
	c.st.listed = true
	return pages, nil
}

func (c *call) forgetTab(id string) {
	delete(c.st.tabs, id)
	c.st.order = slices.DeleteFunc(c.st.order, func(t string) bool { return t == id })
	c.st.refs.dropTarget(id)
	c.t.dropBaseline(c.sess.ID, id)
	if c.st.active == id {
		c.st.active = ""
	}
}

// origin is a URL's scheme and host, or the URL itself (about:blank).
func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Collapsed, so a newline in it (an iframe's src of
		// "%zz&#10;# heading") can't start a line of its own (PROJ-95).
		return collapse(capText(raw, maxField))
	}
	return u.Scheme + "://" + u.Host
}

// quoteOdd quotes an origin that is an address with a space or a quote in
// it, so a line shows where the page's string ends; a plain one shows as
// it is.
func quoteOdd(s string) string {
	if strings.ContainsAny(s, ` "`) {
		return quote(s)
	}
	return s
}

// findActive finds the tab in front, which every tool acts on: the page
// whose document.visibilityState is "visible" (read in the isolated
// world). Exactly one is, with or without a window manager (see the
// spike); if several were, the one made or seen front most recently
// wins. The tab the last call used is checked first, so a call usually
// costs one look.
func (c *call) findActive() error {
	ctx := c.lease.Ctx
	pages, err := c.listTabs(ctx)
	if err != nil {
		return err
	}
	if len(pages) == 0 {
		return &Error{Status: 409, Msg: "the browser has no tab open; open one with tabs (action new)"}
	}
	// The last front tab first, then the others, most recently front first.
	cands := slices.Clone(pages)
	slices.SortStableFunc(cands, func(a, b targetInfo) int {
		ra, rb := c.rank(a.TargetID), c.rank(b.TargetID)
		switch {
		case ra > rb:
			return -1
		case ra < rb:
			return 1
		}
		return 0
	})
	stuck := ""
	for i, ti := range cands {
		tgt, err := c.attach(ctx, ti.TargetID)
		if err != nil {
			continue // closed meanwhile
		}
		timeout := c.t.quick
		if i > 0 {
			timeout = min(c.t.quick, 2*time.Second)
		}
		vis, err := c.visibility(ctx, tgt, timeout)
		if errors.Is(err, errUnresponsive) {
			if ti.TargetID == c.st.active || stuck == "" {
				stuck = ti.TargetID
			}
			tgt.Detach()
			continue
		}
		if err != nil && ctx.Err() != nil {
			return err
		}
		if vis == "visible" {
			return c.use(tgt)
		}
		tgt.Detach()
	}
	if stuck != "" && (stuck == c.st.active || c.st.active == "") {
		return errUnresponsive
	}
	// None in front (it happens while a window is being made): the last
	// one used, or the newest.
	pick := cands[0].TargetID
	tgt, err := c.attach(ctx, pick)
	if err != nil {
		return err
	}
	return c.use(tgt)
}

// rank orders tabs for findActive: the last active one first.
func (c *call) rank(target string) int64 {
	if target == c.st.active {
		return 1 << 62
	}
	if tb, ok := c.st.tabs[target]; ok {
		return tb.activated
	}
	return 0
}

// use makes tgt the call's tab.
func (c *call) use(tgt *cdp.Target) error {
	if c.tgt != nil && c.tgt != tgt {
		c.tgt.Detach()
	}
	c.tgt = tgt
	c.frame, c.loader, c.world = "", "", 0
	if c.st.active != tgt.ID {
		c.st.seq++
		if tb := c.st.tabs[tgt.ID]; tb != nil {
			tb.activated = c.st.seq
		}
	}
	c.st.active = tgt.ID
	c.tab = c.st.tabs[tgt.ID]
	return nil
}

const jsVisibility = `/*bp:visibility*/ document.visibilityState`

// visibility reads a tab's document.visibilityState in its isolated world.
func (c *call) visibility(ctx context.Context, tgt *cdp.Target, timeout time.Duration) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var r struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	err := tgt.Call(cctx, "Page.getFrameTree", nil, &r)
	var w int64
	if err == nil {
		w, err = tgt.IsolatedWorld(cctx, r.FrameTree.Frame.ID)
	}
	var v string
	if err == nil {
		err = tgt.Eval(cctx, w, jsVisibility, &v)
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return "", errUnresponsive
	}
	return v, err
}

// tabAddress is how a new-tab notice names the tab's page: its origin and
// path, capped, without the query or fragment (PROJ-110). The path tells
// one page of a site from another (a sign-in popup from a help page) for
// a few tokens; a sign-in popup's query is long OAuth state that says
// little, and tabs lists the whole address. The path stays escaped, so
// it can't hold a space, a quote or a line break.
func tabAddress(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return origin(raw)
	}
	path := u.EscapedPath()
	if path == "/" {
		path = ""
	}
	return capText(u.Scheme+"://"+u.Host+path, maxField)
}
