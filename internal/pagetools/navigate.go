package pagetools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/idna"

	"github.com/Oni-giri/browser-pier/internal/cdp"
	"github.com/Oni-giri/browser-pier/internal/fence"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// navigateTimeout is how long navigate waits for the page to load,
// following a chain of script redirects included.
const navigateTimeout = 30 * time.Second

// After a load, navigate watches for a script redirect (an SSO check, an
// identity provider, its callback page's own redirect): every chainPoll
// for chainQuiet, it looks whether the tab's document changed or went back
// to loading; if so it waits for that one to load and watches again, for
// at most chainCap (PROJ-121).
const (
	chainQuiet = 300 * time.Millisecond
	chainPoll  = 100 * time.Millisecond
	chainCap   = 15 * time.Second
)

// failedPageWait is how long navigate waits, after Page.navigate failed,
// for the browser's error page to say which address failed.
const failedPageWait = time.Second

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
			t.openingDirectly(ctx, s, target)
			if err := c.renderer(ctx, c.t.slow, "Page.navigate", map[string]any{"url": target}, &r); err != nil {
				return "", err
			}
			if r.ErrorText != "" {
				return "", c.openFailed(ctx, before, target, r.ErrorText)
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
		// Page.navigate gives no loader for a move within the document
		// (only the fragment changed): nothing to load or wait out.
		newDoc := target == "" || want != ""
		deadline := time.Now().Add(c.t.navWait)
		pi, loaded, err := c.waitLoad(ctx, before, beforeURL, want, newDoc, deadline)
		if err != nil {
			return "", err
		}
		end := chainSettled
		if loaded && newDoc {
			if pi, end, err = c.followChain(ctx, pi, deadline); err != nil {
				return "", err
			}
		}
		c.st.refs.dropStale(c.tgt.ID, c.loader)
		if c.unreachable != "" && c.loader != before {
			// The tab moved to the browser's error page. (Still on the
			// error page it was on, its address is no news of this one.)
			c.note(what + ", but the page couldn't be loaded")
			return "", failedAt(target, c.unreachable, "")
		}
		c.note(what)
		trailer := ""
		switch {
		case !loaded:
			trailer = "The page was still loading after " + seconds(c.t.navWait, true) + "."
		case end == chainCapped:
			trailer = fmt.Sprintf("The page was still redirecting after %d s.", int(c.t.chainCap.Seconds()))
		case end == chainTimedOut:
			trailer = "The page was still redirecting after " + seconds(c.t.navWait, true) + "."
		}
		if pi.URL == "" {
			pi.URL = c.url // a document still on its way
		}
		body := header(&page{url: pi.URL, title: pi.Title})
		c.noteTitle(pi.Title)
		c.noteChallenge(pi.CF)
		if loaded && target != "" && !sameAddress(target, pi.URL) {
			// Inside the fence: the address is the agent's own, but the
			// line sits among the page's.
			body += "\nRedirected from: " + capText(target, maxHeader)
		}
		return c.result("page", body, trailer), nil
	})
}

// openingDirectly lets the page an agent opens by its address load even
// when its host is on the ad and tracker list: the session's forwarder
// lets that host and its subdomains through from now on (PROJ-119, SPEC
// §3 Egress). What the page then loads from other listed hosts is still
// blocked. Failing to tell the browser isn't fatal: the page may be
// blocked, as it would be without this.
func (t *Tools) openingDirectly(ctx context.Context, s sessions.Session, raw string) {
	if !s.BlockAds {
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	host := u.Hostname()
	if a, err := idna.Lookup.ToASCII(host); err == nil {
		host = a
	}
	if err := t.m.ExemptHost(ctx, s.ID, host); err != nil {
		t.log.Debug("couldn't exempt a host from the ad and tracker list", "session", s.ID, "err", err)
	}
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
	if hostOf(a) != hostOf(b) && !samePunycode(a, b) {
		return false
	}
	return addressRest(a) == addressRest(b)
}

// samePunycode reports whether a host typed in Unicode may be the one
// that came back as punycode, which the standard library can't convert:
// the same port and labels, each label equal or a Unicode one against an
// xn-- one with the same ASCII letters in the same order (punycode keeps
// those, before its last "-"). Two Unicode names with the same ASCII
// letters (bücher, bächer) still pass; the URL line shows the real one.
func samePunycode(a, b *url.URL) bool {
	la, lb := strings.Split(strings.ToLower(a.Hostname()), "."), strings.Split(strings.ToLower(b.Hostname()), ".")
	if portOf(a) != portOf(b) || len(la) != len(lb) {
		return false
	}
	for i, x := range la {
		if x == lb[i] {
			continue
		}
		y, puny := strings.CutPrefix(lb[i], "xn--")
		basic := ""
		if j := strings.LastIndexByte(y, '-'); j >= 0 {
			basic = y[:j]
		}
		if !puny || !nonASCII(x) || strings.Map(asciiOnly, x) != basic {
			return false
		}
	}
	return true
}

func asciiOnly(r rune) rune {
	if r >= 0x80 {
		return -1
	}
	return r
}

// hostOf is a URL's host in lower case, with its port unless it is the
// scheme's default.
func hostOf(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if p := portOf(u); p != "" {
		return net.JoinHostPort(host, p)
	}
	return host
}

// portOf is a URL's port, "" if none or the scheme's default.
func portOf(u *url.URL) string {
	scheme, p := strings.ToLower(u.Scheme), u.Port()
	if (scheme == "http" && p == "80") || (scheme == "https" && p == "443") {
		return ""
	}
	return p
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
// else any but before, or a new URL, when newDoc) has loaded, or the
// deadline passes. It polls Page.getFrameTree and document.readyState: no
// events, no enable.
//
// A document after want counts too: want already redirected on by
// script, before it was looked at, and followChain follows the rest.
func (c *call) waitLoad(ctx context.Context, before, beforeURL, want string, newDoc bool, deadline time.Time) (pageInfo, bool, error) {
	for {
		err := c.refresh(ctx)
		if err != nil {
			return pageInfo{}, false, err
		}
		changed := !newDoc || c.loader != before || c.url != beforeURL
		if want != "" {
			changed = c.loader == want || c.loader != before
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

// chainEnd is how navigate's watch for script redirects ended.
type chainEnd int

const (
	chainSettled  chainEnd = iota // a document stayed put for chainQuiet
	chainCapped                   // still redirecting after chainCap
	chainTimedOut                 // still redirecting when navigate's time ran out
)

// followChain waits out a chain of script redirects after a load: while
// the tab's document changes (or goes back to loading) within chainQuiet
// of loading, it waits for the next one to load, until one stays put, for
// at most chainCap and never past the deadline. It answers with the last
// document's page info. Like waitLoad, it only polls; the caller's context
// (a handoff, a stop) ends it at once.
func (c *call) followChain(ctx context.Context, pi pageInfo, deadline time.Time) (pageInfo, chainEnd, error) {
	capAt, capped := time.Now().Add(c.t.chainCap), chainCapped
	if deadline.Before(capAt) {
		capAt, capped = deadline, chainTimedOut
	}
	for {
		cur, moved, err := c.staysPut(ctx, deadline)
		if err != nil {
			return pageInfo{}, 0, err
		}
		if !moved {
			return cur, chainSettled, nil
		}
		if cur.URL != "" {
			pi = cur
		}
		if !time.Now().Before(capAt) {
			return pi, capped, nil
		}
		next, loaded, err := c.waitLoad(ctx, "", "", "", false, capAt)
		if err != nil {
			return pageInfo{}, 0, err
		}
		if next.URL != "" {
			pi = next
		}
		if !loaded {
			return pi, capped, nil
		}
	}
}

// staysPut looks at the tab every chainPoll for chainQuiet (or until the
// deadline): moved is true as soon as its document changes or goes back
// to loading. Otherwise pi is the document's latest page info.
func (c *call) staysPut(ctx context.Context, deadline time.Time) (pi pageInfo, moved bool, err error) {
	doc := c.loader
	quietEnd := time.Now().Add(c.t.chainQuiet)
	if deadline.Before(quietEnd) {
		quietEnd = deadline
	}
	for {
		select {
		case <-ctx.Done():
			return pageInfo{}, false, ctx.Err()
		case <-time.After(c.t.chainPoll):
		}
		pi = pageInfo{}
		err := c.eval(ctx, jsPageInfo, &pi)
		switch {
		case errors.Is(err, cdp.ErrWorldGone):
			return pageInfo{}, true, nil // the document went as it was read
		case err != nil:
			return pageInfo{}, false, err
		case c.loader != doc || pi.Ready == "loading":
			return pi, true, nil
		case !time.Now().Before(quietEnd):
			return pi, false, nil
		}
	}
}

// openFailed is the error for a Page.navigate that failed. When the
// browser's error page names another address than the one asked for (a
// redirect led there), it says which. net::ERR_ABORTED (a download, a 204)
// leaves the tab where it was, with no error page to wait for.
func (c *call) openFailed(ctx context.Context, before, target, errorText string) error {
	if errorText == "net::ERR_ABORTED" {
		e := failedAt(target, "", errorText)
		e.Msg += " The address gave no page to show (a download or an empty response, most likely); the tab stayed on its page."
		return e
	}
	end := time.Now().Add(failedPageWait)
	for time.Now().Before(end) {
		f, err := c.topFrame(ctx, c.tgt)
		if err != nil {
			break
		}
		if f.LoaderID != before {
			return failedAt(target, f.UnreachableURL, errorText)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.t.chainPoll):
		}
	}
	return failedAt(target, "", errorText)
}

// failedAt says a navigation failed: with the browser's error, if known,
// and the address that failed, when it isn't the one asked for. Both are
// fenced: the address may be a redirect's, which the page chose.
func failedAt(target, failed, errorText string) *Error {
	var lines []string
	if errorText != "" {
		lines = append(lines, errorText)
	}
	msg := "couldn't open the page: "
	if failed != "" && (target == "" || !sameAddress(target, failed)) {
		lines = append(lines, "Failed at: "+capText(failed, maxHeader))
		if target != "" {
			msg = "couldn't open the page after a redirect: "
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "the browser couldn't load the page")
	}
	return &Error{Status: 502, Msg: msg + fence.Wrap("browser error", strings.Join(lines, "\n"))}
}
