package pagetools

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/cdp"
	"github.com/Oni-giri/browser-pier/internal/fence"
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
				return "", &Error{Status: 502, Msg: "couldn't open the page: " + fence.Wrap("browser error", r.ErrorText)}
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
			// Browser-side: it answers even while the page is stuck.
			hctx, cancel := context.WithTimeout(ctx, c.t.quick)
			err := c.tgt.Call(hctx, "Page.getNavigationHistory", nil, &h)
			cancel()
			if err != nil {
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
		body := header(&page{url: pi.URL, title: pi.Title})
		if loaded && target != "" && !sameAddress(target, pi.URL) {
			// Inside the fence: the address is the agent's own, but the
			// line sits among the page's.
			body += "\nRedirected from: " + capText(target, maxHeader)
		}
		return c.result("page", body, trailer), nil
	})
}

// sameAddress reports whether the page landed where it was sent: the same
// URL once both are normalised as a browser would show them, ignoring the
// fragment, a trailing slash, the case of the scheme and host, a default
// port and percent-encoding. Only a real move (a redirect, server or
// script) makes navigate say "Redirected from".
func sameAddress(requested, final string) bool {
	a, errA := url.Parse(requested)
	b, errB := url.Parse(final)
	if errA != nil || errB != nil {
		return requested == final
	}
	// A name typed in Unicode comes back as punycode, which the standard
	// library can't convert: taken as the same name.
	punycode := nonASCII(hostOf(a)) && strings.Contains(hostOf(b), "xn--")
	if hostOf(a) != hostOf(b) && !punycode {
		return false
	}
	return addressRest(a) == addressRest(b)
}

// hostOf is a URL's host in lower case, with its port unless it is the
// scheme's default.
func hostOf(u *url.URL) string {
	scheme, host, p := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	defaultPort := (scheme == "http" && p == "80") || (scheme == "https" && p == "443")
	if p != "" && !defaultPort {
		return net.JoinHostPort(host, p)
	}
	return host
}

func nonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// addressRest is a URL's scheme, path (without a trailing slash) and
// query, percent-decoded.
func addressRest(u *url.URL) string {
	query := u.RawQuery
	if q, err := url.PathUnescape(query); err == nil {
		query = q
	}
	return strings.ToLower(u.Scheme) + " " + strings.TrimRight(u.Path, "/") + "?" + query
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
