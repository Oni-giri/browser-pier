package handoff

// Handoffs that hand back by themselves (PROJ-118): an agent that knows
// where a task ends (the account page after a sign-in) gives that address
// as until_url, and the handoff ends as done once a tab gets there, so the
// agent isn't left waiting for a person who stopped at the signed-in page
// without tapping Done.
//
// While the handoff is open, a watch asks the browser for its tabs every
// watchEvery with Target.getTargets, on a short-lived browser-level
// connection through the control socket that can send nothing else
// (watchList). It attaches to no page, opens no page session, enables
// nothing and goes through neither the page tools nor their lease, so a
// person driving the browser (in either mode) shares it with no CDP client.
//
// Target.getTargets doesn't say which tab is in front (its order isn't the
// order tabs were last activated in; measured, Chromium 156), so a watch
// doesn't guess: any tab counts, but only once its origin and path have
// changed since the watch first looked, or it opened since. A page already
// at the address, or one that reloads or changes only its query, never
// ends a handoff; it takes a navigation to a matching origin and path.
//
// What the watch reads stays in it: the agent learns only that the
// handoff ended, with a fixed note (AutoNote), never the address.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/cdp"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

const (
	// MaxUntilURL caps an until_url pattern, in characters.
	MaxUntilURL = 200
	// watchEvery is how often a watch looks at the tabs.
	watchEvery = 500 * time.Millisecond
	// watchCallTimeout bounds one look: the dial and Target.getTargets,
	// which the browser answers even while a page has a dialog open.
	watchCallTimeout = 3 * time.Second
)

// watchList is all a watch may send. It is unexported, so no other caller
// can borrow it, and it can't attach to anything.
var watchList = cdp.NewAllowList("handoff address watch", "Target.getTargets")

// CheckUntilURL checks an until_url pattern and returns it without the
// spaces around it. An empty one is no pattern. It is matched against
// the origin and path only, so a pattern with ? or # could never match,
// and is refused, as are invisible characters and more than MaxUntilURL
// characters.
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

// tab is a page target, as Target.getTargets lists it.
type tab struct {
	ID  string `json:"targetId"`
	Typ string `json:"type"`
	URL string `json:"url"`
}

// tabSeen is what a watch remembers of a tab: where it was, and whether it
// has moved (to another origin or path, or opened) since the first look.
type tabSeen struct {
	where string
	moved bool
}

// look compares the tabs with what the last look saw, and reports whether
// a tab that has moved is at a matching address. The first look (seen is
// nil) only takes note of where the tabs are.
func look(seen map[string]tabSeen, tabs []tab, pattern string) (map[string]tabSeen, bool) {
	next := make(map[string]tabSeen, len(tabs))
	hit := false
	for _, t := range tabs {
		where := t.URL
		if shown, _, ok := address(t.URL); ok {
			where = shown
		}
		was, known := seen[t.ID]
		moved := seen != nil && (!known || was.moved || was.where != where)
		next[t.ID] = tabSeen{where: where, moved: moved}
		if moved && matches(pattern, t.URL) {
			hit = true
		}
	}
	return next, hit
}

// listTabs asks the browser for its page targets, on a connection of its
// own that it closes before returning.
func listTabs(ctx context.Context, sock string) ([]tab, error) {
	ctx, cancel := context.WithTimeout(ctx, watchCallTimeout)
	defer cancel()
	c, err := cdp.Dial(ctx, sock, watchList, cdp.Options{MaxMessage: 4 << 20})
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	var r struct {
		TargetInfos []tab `json:"targetInfos"`
	}
	if err := c.Call(ctx, "", "Target.getTargets", nil, &r); err != nil {
		return nil, err
	}
	tabs := r.TargetInfos[:0]
	for _, t := range r.TargetInfos {
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

// runWatch looks at the tabs now, then every watchPoll, until one that has
// moved is at a matching address, or the handoff is over.
func (s *Service) runWatch(ctx context.Context, h Handoff) {
	sock := s.m.Layout().ControlSocket(h.Profile)
	t := time.NewTicker(s.watchPoll)
	defer t.Stop()
	var seen map[string]tabSeen // nil until the first look that worked
	for {
		open, now := s.stillOpen(ctx, h)
		if !open {
			return
		}
		if now {
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

// reached ends a handoff whose page reached its until_url, as done with
// the fixed note, unless it is over already, its time is up or its browser
// stopped (Get ends it as that instead). A person's tap at the same moment
// ends it once: whichever gets to the database first.
func (s *Service) reached(ctx context.Context, h Handoff) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	cur, err := s.Get(ctx, h.ID)
	if err != nil || cur.Status.Final() {
		return
	}
	if _, err := s.end(ctx, cur, Done, AutoNote, endAuto); err != nil && !errors.Is(err, ErrInUse) {
		s.log.Warn("handing back at the address", "handoff", h.ID, "err", fmt.Sprint(err))
	}
}
