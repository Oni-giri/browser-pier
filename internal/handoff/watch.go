package handoff

// Handoffs that hand back by themselves (PROJ-118): an agent that knows
// where a task ends (the account page after a sign-in) gives that address
// as until_url, and the handoff ends as done once the tab in front gets
// there, so the agent isn't left waiting for a person who stopped at the
// signed-in page without tapping Done.
//
// While the handoff is open, a watch asks the browser for its tabs every
// watchEvery with a plain HTTP GET of its /json/list, through the control
// socket's relay: no CDP connection at all, so nothing attaches to a page,
// opens a session or enables anything, and the watch goes through neither
// the page tools nor their lease. A person driving the browser (in either
// mode) shares it with no CDP client.
//
// /json/list lists tabs most recently in front first (Chromium sorts it by
// each tab's last activation; measured on the image's Chromium after tabs
// switch, tabs new, ctrl+Tab, ctrl+1 and closing a tab through the live
// view's keys), so its first page is the tab in front. Only that tab
// counts, and only once its origin and path have changed since the watch
// first looked, or it opened since: a page already at the address, or one
// that reloads or changes only its query, never ends a handoff, nor does a
// background tab or popup that navigates by itself.
//
// The first look is taken once the person has opened the link (the
// handoff is unlocked), not when the agent asks: nothing the page or the
// agent does before then can end a handoff the person hasn't seen.
//
// What the watch reads stays in it: the agent learns only that the
// handoff ended, with a fixed note (AutoNote), never the address.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/sessions"
)

const (
	// MaxUntilURL caps an until_url pattern, in characters.
	MaxUntilURL = 200
	// watchEvery is how often a watch looks at the tabs.
	watchEvery = 500 * time.Millisecond
	// watchCallTimeout bounds one look: the GET of /json/list, which the
	// browser answers even while a page has a dialog open.
	watchCallTimeout = 3 * time.Second
	// minUntilURL is the shortest until_url pattern, in characters.
	minUntilURL = 4
)

// CheckUntilURL checks an until_url pattern and returns it without the
// spaces around it. An empty one is no pattern. It is matched against
// the origin and path only, so a pattern with ? or # could never match,
// and is refused, as are invisible characters and more than MaxUntilURL
// characters. So is one that would match nearly any address: under
// minUntilURL characters, made only of "/", "." and ":", or a bare scheme
// ("https://").
func CheckUntilURL(raw string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", fmt.Errorf("%w: not valid UTF-8", ErrBadUntilURL)
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return "", fmt.Errorf("%w: no control or invisible characters", ErrBadUntilURL)
		}
	}
	p := strings.TrimSpace(raw)
	if n := utf8.RuneCountInString(p); n > MaxUntilURL {
		return "", fmt.Errorf("%w: at most %d characters, not %d", ErrBadUntilURL, MaxUntilURL, n)
	}
	if strings.ContainsAny(p, "?#") {
		return "", fmt.Errorf("%w: it is matched against the address's origin and path only, so leave out ? and #", ErrBadUntilURL)
	}
	if p == "" {
		return "", nil
	}
	bare := strings.ToLower(p)
	if b, ok := strings.CutPrefix(bare, "https"); ok {
		bare = b
	} else {
		bare = strings.TrimPrefix(bare, "http")
	}
	if utf8.RuneCountInString(p) < minUntilURL || strings.Trim(bare, "/.:") == "" {
		return "", fmt.Errorf("%w: too general, it would match nearly any address; give at least %d characters naming the page, like /account or example.com/home", ErrBadUntilURL, minUntilURL)
	}
	return p, nil
}

// address is what a watch compares and matches of a tab's URL: its scheme,
// host (with a non-default port) and path, as the browser shows it, and
// the path decoded too. Never the query, the fragment or a user name. A
// page that isn't http or https (about:blank, the new tab page, an error
// page) has no address.
func address(raw string) (shown, decoded string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", false
	}
	origin := u.Scheme + "://" + u.Host
	return origin + u.EscapedPath(), origin + u.Path, true
}

// matches reports whether a tab's URL has pattern in its origin and path.
func matches(pattern, raw string) bool {
	shown, decoded, ok := address(raw)
	return ok && pattern != "" && (strings.Contains(shown, pattern) || strings.Contains(decoded, pattern))
}

// tab is a target, as /json/list lists it.
type tab struct {
	ID  string `json:"id"`
	Typ string `json:"type"`
	URL string `json:"url"`
}

// tabSeen is what a watch remembers of a tab: where it was, and whether it
// has moved (to another origin or path, or opened) since the first look.
type tabSeen struct {
	where string
	moved bool
}

// look compares the tabs (page targets, the one in front first) with what
// the last look saw, and reports whether the tab in front has moved and is
// at a matching address. The first look (seen is nil) only takes note of
// where the tabs are. Every tab is remembered, so one that moved while in
// the background (an opener a sign-in popup sent on) counts once it is in
// front.
func look(seen map[string]tabSeen, tabs []tab, pattern string) (map[string]tabSeen, bool) {
	next := make(map[string]tabSeen, len(tabs))
	hit := false
	for i, t := range tabs {
		where := t.URL
		if shown, _, ok := address(t.URL); ok {
			where = shown
		}
		was, known := seen[t.ID]
		moved := seen != nil && (!known || was.moved || was.where != where)
		next[t.ID] = tabSeen{where: where, moved: moved}
		if i == 0 && moved && matches(pattern, t.URL) {
			hit = true
		}
	}
	return next, hit
}

// controlClient is an HTTP client for a browser's control socket.
func controlClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
}

// listTabs asks the browser for its page targets, the one in front first,
// with a GET of /json/list (no CDP connection).
func listTabs(ctx context.Context, sock string) ([]tab, error) {
	ctx, cancel := context.WithTimeout(ctx, watchCallTimeout)
	defer cancel()
	c := controlClient(sock)
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://control/cdp/json/list", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req) //nolint:gosec // our own control socket
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/json/list: %s", resp.Status)
	}
	var all []tab
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&all); err != nil {
		return nil, err
	}
	tabs := all[:0]
	for _, t := range all {
		if t.Typ == "page" {
			tabs = append(tabs, t)
		}
	}
	return tabs, nil
}

// watch is one handoff's address watch.
type watch struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// watch starts watching an open handoff's tabs, if it has an until_url and
// isn't watched already. The watch ends when the handoff does (wake), when
// it expires, when its browser stops or leaves Needs help, and on Close.
func (s *Service) watch(h Handoff) {
	if h.UntilURL == "" || h.Status.Final() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watching[h.ID] != nil || s.base.Err() != nil {
		return
	}
	// It may have ended since it was read; end marks it so before wake
	// takes s.mu, so either this sees it or wake cancels the watch.
	var status Status
	if err := s.db.QueryRow(`SELECT status FROM handoffs WHERE id = ?`, h.ID).Scan(&status); err != nil || status.Final() {
		return
	}
	ctx, cancel := context.WithCancel(s.base)
	w := &watch{cancel: cancel, done: make(chan struct{})}
	s.watching[h.ID] = w
	s.watchWG.Add(1)
	go func() {
		defer s.watchWG.Done()
		defer close(w.done)
		defer func() {
			cancel()
			s.mu.Lock()
			if s.watching[h.ID] == w {
				delete(s.watching, h.ID)
			}
			s.mu.Unlock()
		}()
		s.runWatch(ctx, h)
	}()
}

// runWatch looks at the tabs every watchPoll, from when the person has
// opened the link, until the tab in front has moved to a matching address,
// or the handoff is over.
func (s *Service) runWatch(ctx context.Context, h Handoff) {
	sock := s.m.Layout().ControlSocket(h.Profile)
	t := time.NewTicker(s.watchPoll)
	defer t.Stop()
	var seen map[string]tabSeen // nil until the first look that worked
	unlocked := h.unlocked
	for {
		open, now := s.stillOpen(ctx, h)
		if !open {
			return
		}
		if now && !unlocked {
			unlocked = s.isUnlocked(ctx, h.ID)
		}
		if now && unlocked {
			tabs, err := listTabs(ctx, sock)
			if err == nil {
				var hit bool
				if seen, hit = look(seen, tabs, h.UntilURL); hit {
					s.reached(ctx, h)
					return
				}
			}
			// A look that failed (the browser busy starting a tab, say) is
			// tried again at the next tick.
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// isUnlocked reports whether a person has opened the handoff's link.
func (s *Service) isUnlocked(ctx context.Context, id string) bool {
	var unlocked bool
	err := s.db.QueryRowContext(ctx, `SELECT browser_hash != '' FROM handoffs WHERE id = ?`, id).Scan(&unlocked)
	return err == nil && unlocked
}

// stillOpen reports whether a watch should go on: the handoff hasn't been
// ended (which cancels ctx), its time isn't up, and its browser is live;
// otherwise Get ends the handoff as it should (expired, or cancelled
// because the browser stopped). It looks at the tabs only while the
// browser is in Needs help, not while it is stopping, say.
func (s *Service) stillOpen(ctx context.Context, h Handoff) (open, look bool) {
	if ctx.Err() != nil {
		return false, false
	}
	sess, ok := s.m.Live(h.Profile)
	if ok && sess.ID == h.SessionID && s.now().Before(h.ExpiresAt) {
		return true, sess.State == sessions.NeedsHelp
	}
	if _, err := s.Get(context.WithoutCancel(ctx), h.ID); err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Warn("checking a handoff", "handoff", h.ID, "err", fmt.Sprint(err))
	}
	return false, false
}

// reached starts the end of a handoff whose page reached its until_url,
// as done with the fixed note, unless it is over already, its time is up
// or its browser stopped (Get starts its end as that instead). A person's
// tap at the same moment ends it once: whichever starts first, the other
// joining it. It doesn't wait for the browser's hand-back: the end runs
// on its own, so the watch, which Close waits for, never waits on a
// browser.
func (s *Service) reached(ctx context.Context, h Handoff) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	cur, err := s.Get(ctx, h.ID)
	if err != nil || cur.Status.Final() {
		return
	}
	if _, _, err := s.startEnd(cur, endAs{status: Done, note: AutoNote, how: endAuto}); err != nil && !errors.Is(err, ErrInUse) {
		s.log.Warn("handing back at the address", "handoff", h.ID, "err", fmt.Sprint(err))
	}
}
