package sessions

import (
	"context"
	"crypto/sha256"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/scrub"
)

// Bounds of one run's activity log, live or kept (PROJ-158).
const (
	// maxActivity is how many entries each run's log keeps.
	maxActivity = 500
	// maxActivityBytes caps a run's log, counted by entrySize; the
	// oldest entries go first. It counts the entries' text and a fixed
	// overhead, not the Go values' whole size, which is about twice that.
	maxActivityBytes = 64 << 10
	// maxEntryRunes is how much of an entry's words are shown, to the
	// admin and to an agent (RecentCalls), and kept when its run ends: cut
	// after the scrub, so a secret registered later still matches whole.
	// maxEntryBytes caps what a live entry stores: a longer one (a long
	// list of file names) is kept as tooLongEntry instead, since cutting
	// it could leave a secret registered later cut across the end, where
	// no scrub would match it.
	maxEntryRunes = 400
	maxEntryBytes = 2 << 10
	// entryOverhead is what an entry counts for beyond its text.
	entryOverhead = 64
	// maxTitleBytes: a longer title is not kept at all.
	maxTitleBytes = 512
	// shownTitleRunes cuts a title when it is shown and when its run is
	// kept, after it has been scrubbed.
	shownTitleRunes = 80
	// unwatchGrace: a live view that reconnects within it takes back the
	// "Nobody is watching" entry instead of adding a pair.
	unwatchGrace = 30 * time.Second
)

// Kind is what an activity entry is about.
type Kind string

const (
	// KindAction is something an agent's call did: every entry the log
	// had before PROJ-158. Only these reach an agent (RecentCalls).
	KindAction Kind = "action"
	// KindSession: the browser started from the app, stopped and why, or
	// was picked up after a restart.
	KindSession Kind = "session"
	// KindControl: a person took control, or handed it back.
	KindControl Kind = "control"
	// KindHandoff: a handoff's link was opened, or the handoff ended.
	KindHandoff Kind = "handoff"
	// KindWatch: the first person started watching, or the last one left.
	KindWatch Kind = "watch"
	// KindPage: the active tab's title, as a page tool read it (Title).
	KindPage Kind = "page"
)

// BrowserPier's own words for the entries it writes itself.
const (
	tookControl          = "A person took control"
	tookControlByLink    = "The person took control through the handoff link"
	controlHandedBack    = "Control handed back"
	startedWatching      = "Someone started watching"
	nobodyWatching       = "Nobody is watching"
	pageTitled           = "Page title"
	pageChanged          = "The page changed"
	restartedNote        = "BrowserPier restarted: earlier entries of this run are gone"
	startedFromApp       = "Started from the app"
	helpCancelledStopped = "Help request cancelled: the browser stopped"
	tooLongEntry         = "An entry too long to keep (over 2 KiB)"
)

// Activity is one entry of a browser run's activity log, for the web UI:
// what an agent did, and what happened to the browser. It is kept in
// memory only: while the run is live, then for a while in its profile's
// kept runs (Runs).
type Activity struct {
	At    time.Time
	Kind  Kind
	Agent string // the token's name; "" for BrowserPier's own entries
	What  string // in words, e.g. "Clicked at 412, 220"; never what was typed
	// Title is a KindPage entry's tab title: page text, for the admin
	// only, cleaned, scrubbed and cut to 80 characters. What never holds
	// it.
	Title string
	// Attention marks an entry waiting for a person, such as a handoff.
	Attention bool
	// FromPage: What holds words a page chose, a downloaded file's name;
	// an agent reads the entry fenced (PROJ-156).
	FromPage bool
	// TokenID, Tool and CallID are the agent call that wrote the entry
	// (CallFrom), when known: the token's ID, the route's tool and the
	// call id, if it had one. The web UI doesn't show them; an agent
	// reads its own token's entries (RecentCalls). Kept runs drop them.
	TokenID string
	Tool    string
	CallID  string
}

// entrySize is what an entry counts for against maxActivityBytes and the
// kept runs' cap.
func entrySize(a Activity) int {
	return entryOverhead + len(a.Agent) + len(a.What) + len(a.Title) + len(a.TokenID) + len(a.Tool) + len(a.CallID)
}

// Note adds to a live session's activity log; it does nothing once the
// session has ended. what must never hold typed text or other page
// content: it is shown as is.
func (m *Manager) Note(sessionID, agent, what string) {
	m.note(sessionID, Activity{Agent: agent, What: what})
}

// NoteFor is Note for an entry an agent call wrote: the call ctx carries
// (CallFrom), if any, is recorded with it.
func (m *Manager) NoteFor(ctx context.Context, sessionID, agent, what string) {
	m.note(sessionID, byCall(ctx, Activity{Agent: agent, What: what}))
}

// NoteDownloadFor is NoteFor for an entry naming a download, whose file
// name a page chose.
func (m *Manager) NoteDownloadFor(ctx context.Context, sessionID, agent, what string) {
	m.note(sessionID, byCall(ctx, Activity{Agent: agent, What: what, FromPage: true}))
}

// NoteAttention is NoteFor for an entry waiting for a person.
func (m *Manager) NoteAttention(ctx context.Context, sessionID, agent, what string) {
	m.note(sessionID, byCall(ctx, Activity{Agent: agent, What: what, Attention: true}))
}

// NoteHandoff adds a handoff's entry (KindHandoff) in BrowserPier's own
// fixed words: never the agent's detail, the person's note, an address
// or the PIN.
func (m *Manager) NoteHandoff(sessionID, what string) {
	m.note(sessionID, Activity{Kind: KindHandoff, What: what})
}

// NoteControl adds what the person with control did through the live
// view (KindControl), in BrowserPier's own fixed words: never what they
// typed or sent.
func (m *Manager) NoteControl(sessionID, what string) {
	m.note(sessionID, Activity{Kind: KindControl, What: what})
}

// byCall records the call ctx carries in a.
func byCall(ctx context.Context, a Activity) Activity {
	if c := CallFrom(ctx); c != nil {
		a.TokenID, a.Tool, a.CallID = c.tokenID, c.tool, c.id
	}
	return a
}

func (m *Manager) note(sessionID string, a Activity) {
	// A file name or an address in an entry may hold a secret the page
	// echoed (PROJ-125).
	sc := m.lockScrubber(sessionID)
	defer m.mu.Unlock()
	l := m.findLocked(sessionID)
	if l == nil {
		return
	}
	// Scrubbed whole; cut to maxEntryRunes only when shown, after the
	// scrubs then, so a secret registered later still matches whole.
	a.What = sc.String(a.What)
	if len(a.What) > maxEntryBytes {
		a.What, a.FromPage = tooLongEntry, false
	}
	if a.Kind == "" {
		a.Kind = KindAction
	}
	a.At = m.now()
	l.appendLocked(a)
}

// noteLocked adds an entry in BrowserPier's own fixed words, which need
// no scrubbing. m.mu must be held, and l must still be live.
func (l *live) noteLocked(at time.Time, kind Kind, what string) {
	l.appendLocked(Activity{At: at, Kind: kind, What: what})
}

// appendLocked adds a to the log, dropping the oldest entries past its
// bounds.
func (l *live) appendLocked(a Activity) {
	l.activity = append(l.activity, a)
	l.activityBytes += entrySize(a)
	drop := 0
	for len(l.activity)-drop > maxActivity || l.activityBytes > maxActivityBytes {
		l.activityBytes -= entrySize(l.activity[drop])
		drop++
	}
	if drop > 0 {
		n := copy(l.activity, l.activity[drop:])
		clear(l.activity[n:])
		l.activity = l.activity[:n]
	}
}

// removeLocked takes the entry at i out of the log.
func (l *live) removeLocked(i int) {
	l.activityBytes -= entrySize(l.activity[i])
	l.activity = append(l.activity[:i], l.activity[i+1:]...)
}

// NoteTitle records the active tab's title, which a page tool read for
// its answer (snapshot, navigate, tabs, page_content): one KindPage entry
// each time it differs from the last one recorded for the session. A
// title entry right before it is replaced, so titles can't push actions
// out of the log. Nothing is recorded while a person has control or a
// handoff is open (page tools are refused then anyway), and nothing reads
// the page for it. Control, C1 and bidi characters are removed and
// whitespace collapsed; a title over 512 bytes is not kept (the entry
// says only that the page changed). The title is scrubbed as it is
// written, and again, then cut to 80 characters, when shown and when the
// run is kept. It is page text, for the admin only: no agent reads it,
// and it is never logged.
func (m *Manager) NoteTitle(sessionID, title string) {
	clean := cleanTitle(title)
	if clean == "" {
		return
	}
	sum := sha256.Sum256([]byte(clean))
	sc := m.lockScrubber(sessionID)
	defer m.mu.Unlock()
	l := m.findLocked(sessionID)
	if l == nil || l.human || l.sess.State != Running {
		return
	}
	if l.lastTitle == sum {
		return
	}
	l.lastTitle = sum
	a := Activity{At: m.now(), Kind: KindPage, What: pageTitled}
	if len(clean) > maxTitleBytes {
		a.What = pageChanged
	} else {
		a.Title = sc.String(clean)
	}
	if n := len(l.activity); n > 0 && l.activity[n-1].Kind == KindPage {
		l.removeLocked(n - 1)
	}
	l.appendLocked(a)
}

// cleanTitle turns C0 and C1 control characters into spaces, removes bidi
// controls (U+202A to U+202E, U+2066 to U+2069) and collapses whitespace.
func cleanTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r >= 0x7f && r <= 0x9f:
			return ' '
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// cutRunes cuts s to at most n characters, the last of them "…" when it
// was cut.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// shown is an entry as it is read: scrubbed with the secrets as they are
// now, its title cut after.
func shown(sc *scrub.Scrubber, a Activity) Activity {
	a.What = cutRunes(sc.String(a.What), maxEntryRunes)
	a.Title = cutRunes(sc.String(a.Title), shownTitleRunes)
	return a
}

// Activity returns a live session's activity entries, newest first; none
// once it has ended (its run is then in Runs). They are scrubbed again
// with the session's secrets as they are now: one registered after an
// entry was written is hidden in it too.
func (m *Manager) Activity(sessionID string) []Activity {
	out, _ := m.RecentActivity(sessionID, maxActivity)
	return out
}

// RecentActivity is Activity's newest n entries, and whether there are
// more: the live view's side column shows a few, polled, and scrubs only
// those.
func (m *Manager) RecentActivity(sessionID string, n int) ([]Activity, bool) {
	sc := m.lockScrubber(sessionID)
	defer m.mu.Unlock()
	l := m.findLocked(sessionID)
	if l == nil {
		return nil, false
	}
	n = min(n, len(l.activity))
	out := make([]Activity, n)
	for i := range n {
		out[i] = shown(sc, l.activity[len(l.activity)-1-i])
	}
	return out, len(l.activity) > n
}
