package pagetools

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/cdp"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// navigateTimeout is how long navigate waits for the page to load.
const navigateTimeout = 30 * time.Second

// NavigateIn is navigate's input: a URL, or an action (back, forward or
// reload).
type NavigateIn struct {
	URL    string `json:"url,omitempty"`
	Action string `json:"action,omitempty"`
}

// checkURL refuses what pages must not be sent to: any scheme but http
// and https, and URLs carrying a user name or password.
func checkURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	switch {
	case err != nil:
		return "", &Error{Status: 400, Msg: "url: not a URL"}
	case u.Scheme != "http" && u.Scheme != "https":
		return "", &Error{Status: 400, Msg: "url: only http and https URLs can be opened"}
	case u.User != nil:
		return "", &Error{Status: 400, Msg: "url: URLs with a user name or password (user:pass@) aren't opened; sign in on the page"}
	case u.Host == "":
		return "", &Error{Status: 400, Msg: "url: no host"}
	case len(raw) > 8192:
		return "", &Error{Status: 400, Msg: "url: too long"}
	}
	return u.String(), nil
}

// Navigate opens a URL in the active tab, or goes back, forward or
// reloads, and waits for the page to load (or 30 s). It answers with the
// page's URL and title. The tab's refs go stale.
func (t *Tools) Navigate(ctx context.Context, s sessions.Session, who Caller, in NavigateIn) (string, error) {
	var target string
	switch {
	case in.URL != "" && in.Action != "":
		return "", &Error{Status: 400, Msg: "give a url or an action, not both"}
	case in.URL != "":
		u, err := checkURL(in.URL)
		if err != nil {
			return "", err
		}
		target = u
	case in.Action == "back", in.Action == "forward", in.Action == "reload":
	case in.Action != "":
		return "", &Error{Status: 400, Msg: "action: want back, forward or reload"}
	default:
		return "", &Error{Status: 400, Msg: "give a url, or an action: back, forward or reload"}
	}
	return t.run(ctx, s, who, func(c *call) (string, error) {
		ctx := c.lease.Ctx
		if err := c.refresh(ctx); err != nil {
			return "", err
		}
		before, beforeURL := c.loader, c.url
		var want string // the loader the navigation makes, when known
		var what string
		switch {
		case target != "":
			var r struct {
				LoaderID  string `json:"loaderId"`
				ErrorText string `json:"errorText"`
			}
			if err := c.renderer(ctx, c.t.slow, "Page.navigate", map[string]any{"url": target}, &r); err != nil {
				return "", err
			}
			if r.ErrorText != "" {
				return "", &Error{Status: 502, Msg: "couldn't open the page: " + r.ErrorText}
			}
			want = r.LoaderID
			what = "Opened " + origin(target)
		case in.Action == "reload":
			if err := c.renderer(ctx, c.t.slow, "Page.reload", nil, nil); err != nil {
				return "", err
			}
			what = "Reloaded the page"
		default:
			var h struct {
				CurrentIndex int `json:"currentIndex"`
				Entries      []struct {
					ID int `json:"id"`
				} `json:"entries"`
			}
			if err := c.tgt.Call(ctx, "Page.getNavigationHistory", nil, &h); err != nil {
				return "", err
			}
			i := h.CurrentIndex - 1
			what = "Went back"
			if in.Action == "forward" {
				i, what = h.CurrentIndex+1, "Went forward"
			}
			if i < 0 || i >= len(h.Entries) {
				return "", &Error{Status: 409, Msg: "there's no page to go " + in.Action + " to"}
			}
			if err := c.renderer(ctx, c.t.slow, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[i].ID}, nil); err != nil {
				return "", err
			}
		}
		pi, loaded, err := c.waitLoad(ctx, before, beforeURL, want, target == "" || want != "")
		if err != nil {
			return "", err
		}
		c.st.refs.dropStale(c.tgt.ID, c.loader)
		c.note(what)
		trailer := ""
		if !loaded {
			trailer = "The page was still loading after 30 s."
		}
		return c.result("page", header(&page{url: pi.URL, title: pi.Title}), trailer), nil
	})
}

// waitLoad polls the active tab until a new document (want, if known;
// else any but before, or a new URL, when newDoc) has loaded, or
// navigateTimeout passes. It polls Page.getFrameTree and
// document.readyState: no events, no enable.
func (c *call) waitLoad(ctx context.Context, before, beforeURL, want string, newDoc bool) (pageInfo, bool, error) {
	deadline := time.Now().Add(navigateTimeout)
	for {
		err := c.refresh(ctx)
		if err != nil {
			return pageInfo{}, false, err
		}
		changed := !newDoc || c.loader != before || c.url != beforeURL
		if want != "" {
			changed = c.loader == want
		}
		var pi pageInfo
		if changed {
			err := c.eval(ctx, jsPageInfo, &pi)
			if errors.Is(err, cdp.ErrWorldGone) {
				pi.Ready = "" // still navigating: look again
			} else if err != nil {
				return pageInfo{}, false, err
			}
			if pi.Ready == "complete" {
				return pi, true, nil
			}
		}
		if time.Now().After(deadline) {
			if !changed {
				pi, err = c.info(ctx)
			}
			return pi, false, err
		}
		select {
		case <-ctx.Done():
			return pageInfo{}, false, ctx.Err()
		case <-time.After(c.t.poll):
		}
	}
}
