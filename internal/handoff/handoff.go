// Package handoff lets an agent ask a person to take over its browser: a
// one-time link, checked against the profile's policy, that gives one
// browser control of that one profile until the person hands back.
package handoff

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/webrtc"
)

// Reason says what kind of help is needed, so the page can word it.
type Reason string

const (
	SensitiveInput Reason = "sensitive_input" // login, 2FA, payment
	Captcha        Reason = "captcha"
	FileUpload     Reason = "file_upload"
	Review         Reason = "review" // check before the agent continues
	ManualStep     Reason = "manual_step"
)

var reasons = map[Reason]string{
	SensitiveInput: "Sign-in or payment", Captcha: "CAPTCHA", FileUpload: "File upload", Review: "Review", ManualStep: "Manual step",
}

// Label is the reason in words.
func (r Reason) Label() string { return reasons[r] }

// Status is where a handoff stands.
type Status string

const (
	Pending   Status = "pending"   // waiting for the person
	Done      Status = "done"      // handed back
	Cant      Status = "cant"      // the person couldn't do it
	Cancelled Status = "cancelled" // five wrong PINs, or the browser stopped
	Expired   Status = "expired"   // nobody came within 15 minutes
)

// Final reports whether the handoff is over.
func (s Status) Final() bool { return s != Pending }

// Lifetime is how long a handoff link works.
const Lifetime = 15 * time.Minute

// MaxPINFailures cancels a link.
const MaxPINFailures = 5

var (
	ErrNotFound     = errors.New("no such handoff")
	ErrBadReason    = errors.New("reason: want sensitive_input, captcha, file_upload, review or manual_step")
	ErrBadRequire   = errors.New("require: want link, pin or signin")
	ErrNotRunning   = errors.New("the browser isn't running")
	ErrOpen         = errors.New("this browser already has a handoff open")
	ErrOtherBrowser = errors.New("this link was opened in another browser")
	ErrInUse        = errors.New("a person has opened this link; wait for them to hand back")
	// ErrBadUntilURL wraps why an until_url pattern was refused.
	ErrBadUntilURL = errors.New("until_url")
)

// Handoff is one request for help.
type Handoff struct {
	ID, SessionID, Profile string
	Agent                  string // the token's name
	Reason                 Reason
	Detail                 string
	// Origin is the page's scheme and host, never its path or query,
	// which the page controls and could word a phishing message with.
	Origin string
	Policy profiles.HandoffPolicy
	Status Status
	Note   string // the person's, for the agent
	// UntilURL, if set, ends the handoff as done once a tab reaches an
	// address it is part of (see watch.go); Auto says it ended that way.
	UntilURL    string
	Auto        bool
	PINFailures int
	CreatedAt   time.Time
	ExpiresAt   time.Time
	unlocked    bool // a browser has passed the policy check
	// helpEpoch is which time the browser went to Needs help for this
	// handoff (sessions.Manager.OpenHelp): its end ends only that help.
	helpEpoch uint64
}

// Service creates and tracks handoffs.
type Service struct {
	db  *sql.DB
	m   *sessions.Manager
	log *slog.Logger
	now func() time.Time
	// beforeListen, if set, runs in Wait between its first read and
	// registering to be woken (tests).
	beforeListen func()

	// base ends every address watch when the service closes; watchPoll is
	// how often a watch looks at the tabs (watchEvery; tests shorten it).
	base      context.Context
	closeBase context.CancelFunc
	watchPoll time.Duration
	watchWG   sync.WaitGroup

	mu      sync.Mutex
	waiters map[string][]chan struct{}
	// ending are the handoffs whose end is under way (runEnd): one at a
	// time per handoff, which any other end of it joins.
	ending   map[string]*ending
	viewers  map[string][]*webrtc.Viewer // live views opened through a handoff
	checking map[string]int              // PIN checks under way, per handoff
	watching map[string]*watch           // address watches, per handoff (watch.go)
}

func New(db *sql.DB, m *sessions.Manager, log *slog.Logger) *Service {
	base, closeBase := context.WithCancel(context.Background())
	return &Service{db: db, m: m, log: log, now: time.Now, base: base, closeBase: closeBase, watchPoll: watchEvery,
		waiters: map[string][]chan struct{}{}, ending: map[string]*ending{}, viewers: map[string][]*webrtc.Viewer{}, checking: map[string]int{}, watching: map[string]*watch{}}
}

// Close stops every address watch and waits for them. Open handoffs stay
// open; the next start's sweep watches them again.
func (s *Service) Close() {
	s.mu.Lock() // so watch, which checks base under it, adds no watch after
	s.closeBase()
	s.mu.Unlock()
	s.watchWG.Wait()
}

var rank = map[profiles.HandoffPolicy]int{profiles.PolicyLink: 0, profiles.PolicyPIN: 1, profiles.PolicySignIn: 2}

// Stricter returns the stricter of two policies. An agent can raise its
// profile's policy for one handoff, never lower it: it browses untrusted
// pages and could be talked into asking for less.
func Stricter(a, b profiles.HandoffPolicy) profiles.HandoffPolicy {
	if _, ok := rank[b]; ok && rank[b] > rank[a] {
		return b
	}
	return a
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Request is what an agent asks for when it opens a handoff.
type Request struct {
	SessionID string
	Agent     string // the token's name
	Reason    Reason
	Detail    string
	Require   profiles.HandoffPolicy // stricter than the profile's, or ignored
	// UntilURL, if not empty, ends the handoff by itself once a tab
	// reaches an address with it in its origin and path (CheckUntilURL).
	UntilURL string
}

// Create opens a handoff with no until_url; see Open.
func (s *Service) Create(ctx context.Context, sessionID, agent string, reason Reason, detail string, require profiles.HandoffPolicy) (Handoff, string, error) {
	return s.Open(ctx, Request{SessionID: sessionID, Agent: agent, Reason: reason, Detail: detail, Require: require})
}

// Open opens a handoff for a running session and pauses the agent
// (Needs help closes its connections). It returns the handoff and the
// link's token, which is not stored.
func (s *Service) Open(ctx context.Context, r Request) (Handoff, string, error) {
	sessionID, agent, reason, detail, require := r.SessionID, r.Agent, r.Reason, r.Detail, r.Require
	if _, ok := reasons[reason]; !ok {
		return Handoff{}, "", ErrBadReason
	}
	if require != "" {
		if _, ok := rank[require]; !ok {
			return Handoff{}, "", ErrBadRequire
		}
	}
	until, err := CheckUntilURL(r.UntilURL)
	if err != nil {
		return Handoff{}, "", err
	}
	detail = strings.TrimSpace(detail)
	if len(detail) > 500 {
		detail = detail[:500]
	}
	sess, err := s.m.Get(ctx, sessionID)
	if err != nil {
		return Handoff{}, "", err
	}
	if sess.State == sessions.NeedsHelp {
		return Handoff{}, "", ErrOpen
	}
	if sess.State != sessions.Running {
		return Handoff{}, "", ErrNotRunning
	}
	p, err := s.m.Profiles().Get(ctx, sess.Profile)
	if err != nil {
		return Handoff{}, "", err
	}
	now := s.now().Truncate(time.Millisecond)
	h := Handoff{
		ID: "h_" + randomToken()[:16], SessionID: sessionID, Profile: sess.Profile, Agent: agent,
		Reason: reason, Detail: detail, Origin: s.pageOrigin(ctx, sess.Profile),
		Policy: Stricter(p.HandoffPolicy, require), Status: Pending, UntilURL: until, CreatedAt: now, ExpiresAt: now.Add(Lifetime),
	}
	token := randomToken()
	// The browser goes to Needs help for this handoff alone first: it
	// refuses if another handoff has it there already (one that got in
	// after the Running check), and the epoch it answers is stored with
	// the handoff's row in the same insert, so no open handoff is ever
	// stored without it: its end ends this help and no other.
	epoch, err := s.m.OpenHelp(sessionID)
	if err != nil {
		if errors.Is(err, sessions.ErrHelpOpen) {
			return Handoff{}, "", ErrOpen
		}
		return Handoff{}, "", err
	}
	h.helpEpoch = epoch
	if _, err := s.db.ExecContext(ctx, `INSERT INTO handoffs (id, session_id, profile, agent, reason, detail, origin, policy, status, token_hash, created_at, expires_at, until_url, help_epoch)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.SessionID, h.Profile, h.Agent, h.Reason, h.Detail, h.Origin, h.Policy, h.Status, hash(token),
		h.CreatedAt.UnixMilli(), h.ExpiresAt.UnixMilli(), h.UntilURL, epoch); err != nil {
		// No handoff, so no help: the browser is the agent's again.
		_, _ = s.m.EndHandoff(sessionID, epoch)
		if strings.Contains(err.Error(), "UNIQUE constraint failed: handoffs.session_id") {
			return Handoff{}, "", ErrOpen // another Create got in first
		}
		return Handoff{}, "", err
	}
	s.m.NoteAttention(sessionID, agent, "Paused, waiting for you: "+reason.Label())
	s.log.Info("handoff opened", "handoff", h.ID, "profile", h.Profile, "agent", agent, "reason", reason, "policy", h.Policy, "until_url", h.UntilURL != "")
	s.watch(h)
	return h, token, nil
}

// pageOrigin is the origin of the browser's current page, or "" if it
// can't be read. It reads Chrome's target list over HTTP: no CDP client
// attaches to the page.
func (s *Service) pageOrigin(ctx context.Context, profile string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c := controlClient(s.m.Layout().ControlSocket(profile))
	defer c.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://control/cdp/json/list", nil)
	resp, err := c.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var targets []struct{ Type, URL string }
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&targets) != nil {
		return ""
	}
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
		return Origin(t.URL)
	}
	return ""
}

// Origin keeps only a URL's scheme and host (with a non-default port).
func Origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

const columns = `id, session_id, profile, agent, reason, detail, origin, policy, status, note, pin_failures, created_at, expires_at, browser_hash, until_url, auto_done, help_epoch`

func scan(row interface{ Scan(...any) error }) (Handoff, error) {
	var h Handoff
	var created, expires int64
	var browser string
	err := row.Scan(&h.ID, &h.SessionID, &h.Profile, &h.Agent, &h.Reason, &h.Detail, &h.Origin, &h.Policy, &h.Status, &h.Note,
		&h.PINFailures, &created, &expires, &browser, &h.UntilURL, &h.Auto, &h.helpEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Handoff{}, ErrNotFound
	}
	h.CreatedAt, h.ExpiresAt = time.UnixMilli(created), time.UnixMilli(expires)
	h.unlocked = browser != ""
	return h, err
}

// Get returns a handoff, expiring it first if its time is up.
func (s *Service) Get(ctx context.Context, id string) (Handoff, error) {
	h, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM handoffs WHERE id = ?`, id))
	if err != nil {
		return h, err
	}
	return s.checkLive(ctx, h)
}

// ByToken finds the handoff a link points to.
func (s *Service) ByToken(ctx context.Context, token string) (Handoff, error) {
	h, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM handoffs WHERE token_hash = ?`, hash(token)))
	if err != nil {
		return h, err
	}
	return s.checkLive(ctx, h)
}

// Pending returns the open handoffs, oldest first, ending any whose time
// is up or whose browser stopped.
func (s *Service) Pending(ctx context.Context) ([]Handoff, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM handoffs WHERE status = 'pending' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	var open []Handoff
	for rows.Next() {
		h, err := scan(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		open = append(open, h)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []Handoff
	for _, h := range open {
		h, err := s.checkLive(ctx, h)
		if err != nil {
			return nil, err
		}
		if h.Status == Pending {
			out = append(out, h)
		}
	}
	return out, nil
}

// Stats counts the handoffs asked for since a time, and the median time
// people took to answer the ones they did (done or can't).
func (s *Service) Stats(ctx context.Context, since time.Time) (count int, median time.Duration, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, finished_at - created_at FROM handoffs WHERE created_at >= ?`, since.UnixMilli())
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	var took []int64
	for rows.Next() {
		var status Status
		var ms int64
		if err := rows.Scan(&status, &ms); err != nil {
			return 0, 0, err
		}
		count++
		if status == Done || status == Cant { // a person answered
			took = append(took, ms)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if n := len(took); n > 0 {
		slices.Sort(took)
		mid := took[n/2]
		if n%2 == 0 {
			mid = took[n/2-1] + (took[n/2]-took[n/2-1])/2
		}
		median = time.Duration(mid) * time.Millisecond
	}
	return count, median, nil
}

// checkLive ends a pending handoff whose time is up or whose browser
// stopped, or whose browser no longer waits for its help with no end of
// it under way (an end cut short by a restart, or whose final status
// couldn't be saved): it waits for that end as end does.
func (s *Service) checkLive(ctx context.Context, h Handoff) (Handoff, error) {
	if h.Status.Final() {
		return h, nil
	}
	if !s.now().Before(h.ExpiresAt) {
		return s.finish(ctx, h, Expired, "")
	}
	if sess, err := s.m.Get(ctx, h.SessionID); err != nil || !sess.State.Live() {
		return s.finish(ctx, h, Cancelled, "the browser stopped")
	}
	if !s.isEnding(h.ID) && !s.m.WaitsForHelp(h.SessionID, h.helpEpoch) {
		return s.finish(ctx, h, Cancelled, "the browser stopped waiting for help")
	}
	return h, nil
}

// Unlock binds the handoff to one browser, identified by the value of a
// cookie only that browser holds. The first browser to pass the policy
// check gets it; any other is refused, so the link works once.
func (s *Service) Unlock(ctx context.Context, h Handoff, browser string) error {
	// Not one that is ending: under s.mu, as startEnd's checks are, so a
	// withdrawal that found it unopened ends it unopened.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ending[h.ID] != nil {
		return ErrOtherBrowser
	}
	res, err := s.db.ExecContext(ctx, `UPDATE handoffs SET browser_hash = ? WHERE id = ? AND status = 'pending' AND (browser_hash = '' OR browser_hash = ?)`,
		hash(browser), h.ID, hash(browser))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrOtherBrowser
	}
	return nil
}

// Unlocked reports whether this browser holds the handoff.
func (s *Service) Unlocked(ctx context.Context, h Handoff, browser string) bool {
	if browser == "" {
		return false
	}
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM handoffs WHERE id = ? AND browser_hash = ? AND status = 'pending'`, h.ID, hash(browser)).Scan(&n)
	return n == 1
}

// ReservePINTry starts a PIN check. It refuses while the link's wrong
// PINs plus the checks already under way reach MaxPINFailures, so parallel
// guesses can't all be tried before the count catches up. End each
// reserved check with PINRight or PINWrong; only a wrong PIN counts.
func (s *Service) ReservePINTry(ctx context.Context, h Handoff) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var failures int
	err := s.db.QueryRowContext(ctx, `SELECT pin_failures FROM handoffs WHERE id = ? AND status = 'pending'`, h.ID).Scan(&failures)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if failures+s.checking[h.ID] >= MaxPINFailures {
		return false, nil
	}
	s.checking[h.ID]++
	return true, nil
}

// doneChecking ends a reserved check. Call it with s.mu held.
func (s *Service) doneChecking(id string) {
	if s.checking[id]--; s.checking[id] <= 0 {
		delete(s.checking, id)
	}
}

// PINRight ends a check that found the right PIN (or never got to check
// it): it isn't counted.
func (s *Service) PINRight(h Handoff) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doneChecking(h.ID)
}

// PINWrong ends a check that found a wrong PIN, counts it, and returns how
// many tries are left, cancelling the link when none are. It fails
// closed: the guess is counted even if the client has gone, and if it
// can't be recorded the check stays reserved, still using up a try.
func (s *Service) PINWrong(ctx context.Context, h Handoff) (int, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	s.mu.Lock()
	// Count it in the same step as ending the check, so a reservation never
	// sees it as neither.
	_, err := s.db.ExecContext(ctx, `UPDATE handoffs SET pin_failures = pin_failures + 1 WHERE id = ? AND status = 'pending'`, h.ID)
	if err == nil {
		s.doneChecking(h.ID)
	}
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	h, err = s.Get(ctx, h.ID)
	if err != nil {
		return 0, err
	}
	left := MaxPINFailures - h.PINFailures
	if left <= 0 && !h.Status.Final() {
		_, err := s.finish(ctx, h, Cancelled, "five wrong PINs")
		return 0, err
	}
	return max(left, 0), nil
}

// Finish records the person's answer: done or can't, with a note.
func (s *Service) Finish(ctx context.Context, h Handoff, status Status, note string) (Handoff, error) {
	if status != Done && status != Cant {
		return h, errors.New("status: want done or cant")
	}
	note = strings.TrimSpace(note)
	if len(note) > 1000 {
		note = note[:1000]
	}
	return s.finish(ctx, h, status, note)
}

// Withdraw cancels a pending handoff at the agent's request; the agent
// gets its browser back. A handoff already over is returned as it is.
//
// Once a person has opened the link it is theirs until they hand back: an
// agent taking the browser back then could read what they were typing.
func (s *Service) Withdraw(ctx context.Context, h Handoff, note string) (Handoff, error) {
	note = strings.TrimSpace(note)
	if len(note) > 300 {
		note = note[:300]
	}
	return s.end(ctx, h, Cancelled, note, endUnopened)
}

// finish ends a pending handoff, gives the browser back to the agent and
// wakes anyone waiting.
func (s *Service) finish(ctx context.Context, h Handoff, status Status, note string) (Handoff, error) {
	return s.end(ctx, h, status, note, endAny)
}

// How end may end a handoff.
type endHow int

const (
	endAny      endHow = iota // whatever stage it is at
	endUnopened               // only if no person has opened the link
	endAuto                   // as done, by its until_url, only before it expires
)

// end is finish, as how says. The end runs on its own (runEnd), once per
// handoff: an end asked for while one is under way joins it, and gets
// its outcome, never undoing it. The handoff's status turns final only
// once its browser is handed back (fenced, its clipboard emptied, agents
// let in again), so whoever reads it final can act on the browser at
// once; until then it reads as pending. end waits for that, no longer
// than ctx: if ctx ends first it returns ErrHandingBack, and the end goes
// on without it.
func (s *Service) end(ctx context.Context, h Handoff, status Status, note string, how endHow) (Handoff, error) {
	e, cur, err := s.startEnd(h, status, note, how)
	if e == nil || err != nil {
		return cur, err
	}
	select {
	case <-e.done:
		return e.h, e.err
	case <-ctx.Done():
		return cur, ErrHandingBack
	}
}

// ErrHandingBack: the handoff is ending, but its browser isn't handed
// back yet; ask again shortly.
var ErrHandingBack = sessions.ErrHandingBack

// ending is a handoff's end under way: done closes once it is over, with
// the handoff as it ended (h) or why it couldn't (err).
type ending struct {
	done chan struct{}
	h    Handoff
	err  error
}

// startEnd starts ending handoff h, as end describes, unless an end of
// it is under way, which it returns to join. It returns no ending, but
// the handoff as it stands, if the handoff has ended already, or with
// ErrInUse if how's condition doesn't hold (opened, for endUnopened;
// expired, for endAuto). Its checks, and Unlock's, run under s.mu.
func (s *Service) startEnd(h Handoff, status Status, note string, how endHow) (*ending, Handoff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ending[h.ID]; e != nil {
		return e, h, nil
	}
	cur, err := scan(s.db.QueryRow(`SELECT `+columns+` FROM handoffs WHERE id = ?`, h.ID))
	switch {
	case err != nil:
		return nil, h, err
	case cur.Status.Final():
		return nil, cur, nil // someone else ended it first
	case how == endUnopened && cur.unlocked:
		return nil, cur, ErrInUse
	case how == endAuto && !s.now().Before(cur.ExpiresAt):
		return nil, cur, ErrInUse
	}
	e := &ending{done: make(chan struct{})}
	s.ending[h.ID] = e
	go s.runEnd(e, cur, status, note, how == endAuto)
	return e, cur, nil
}

// ending reports whether an end of handoff id is under way.
func (s *Service) isEnding(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ending[id] != nil
}

// runEnd is the end startEnd began: the browser stops waiting for this
// handoff's help and is handed back, then the status turns final.
func (s *Service) runEnd(e *ending, h Handoff, status Status, note string, auto bool) {
	// The link is dead: so are the live views it opened.
	s.closeViewers(h.ID)
	// One step: the browser stops waiting for this handoff's help, so no
	// live view of it can take control any more (TakeControlInHandoff),
	// and the control of the person helping, if any, begins to end. A
	// take after this is someone else's, left alone, as is help another
	// handoff asked for.
	done, err := s.m.EndHandoff(h.SessionID, h.helpEpoch)
	if errors.Is(err, sessions.ErrHandoffEnded) {
		done, err = s.repairHelp(h)
	}
	if err != nil && !errors.Is(err, sessions.ErrNotRunning) {
		s.log.Warn("handing the browser back", "handoff", h.ID, "err", err)
	}
	if done != nil {
		// However long the browser takes: a browser that never answers is
		// still stopped (idle, for room, or by the admin), which ends it.
		<-done
	}
	now := s.now()
	_, err = s.db.Exec(`UPDATE handoffs SET status = ?, note = ?, finished_at = ?, auto_done = ? WHERE id = ? AND status = 'pending'`,
		status, note, now.UnixMilli(), auto, h.ID)
	if err == nil {
		h.Status, h.Note, h.Auto = status, note, auto
		logNote := finishedNotes[status]
		if h.Auto {
			logNote = autoDoneLog
		}
		s.m.Note(h.SessionID, "", logNote)
		s.log.Info("handoff finished", "handoff", h.ID, "profile", h.Profile, "status", status, "until_url_reached", h.Auto)
	} else {
		// The next look at it ends it (checkLive): its browser no longer
		// waits for its help.
		s.log.Error("saving a handoff's end", "handoff", h.ID, "err", err)
	}
	e.h, e.err = h, err
	s.mu.Lock()
	delete(s.ending, h.ID)
	s.mu.Unlock()
	s.wake(h.ID)
	close(e.done)
}

// repairHelp ends the help a handoff's browser waits for when its epoch
// isn't the handoff's (one stored by an older BrowserPier): only if that
// help is no newer than the handoff's, so it can't be a later handoff's.
// Logged.
func (s *Service) repairHelp(h Handoff) (<-chan struct{}, error) {
	cur, waiting := s.m.HelpEpoch(h.SessionID)
	if !waiting || cur > h.helpEpoch {
		return nil, nil // no help, or a later handoff's: left alone
	}
	s.log.Warn("a handoff ended without its browser's help epoch; ending the help it waits for all the same", "handoff", h.ID, "session", h.SessionID, "epoch", h.helpEpoch, "browser_epoch", cur)
	return s.m.EndHandoff(h.SessionID, cur)
}

// AutoNote is the note of a handoff its until_url ended. It is fixed, so
// it never quotes the address the person was on.
const AutoNote = "the page reached a matching address"

// autoDoneLog is what the agent log says then, without the address too.
const autoDoneLog = "Handed back when the page reached the expected address"

// finishedNotes are what the agent log says when a handoff ends.
var finishedNotes = map[Status]string{
	Done:      "Handed back: done",
	Cant:      "Handed back: couldn't do it",
	Cancelled: "Help request cancelled",
	Expired:   "Help request expired",
}

func (s *Service) wake(id string) {
	s.closeViewers(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.waiters[id] {
		close(ch)
	}
	delete(s.waiters, id)
}

// closeViewers ends what a handoff that has ended still has going: the
// link is dead, so are the live views it opened, and its address watch.
func (s *Service) closeViewers(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.viewers[id] {
		v.Close()
	}
	delete(s.viewers, id)
	delete(s.checking, id)
	if w := s.watching[id]; w != nil {
		w.cancel()
	}
}

// track remembers a live view opened through a handoff, to close it when
// the handoff ends. If the handoff ended while the view was connecting,
// the view is closed at once.
func (s *Service) track(id string, v *webrtc.Viewer) {
	s.mu.Lock()
	var status Status
	err := s.db.QueryRow(`SELECT status FROM handoffs WHERE id = ?`, id).Scan(&status)
	if err != nil || status.Final() {
		s.mu.Unlock()
		v.Close()
		return
	}
	s.viewers[id] = append(s.viewers[id], v)
	s.mu.Unlock()
	go func() {
		// Listed until it has gone and handed its control back, so a
		// reload of the page waits for that (replaceViewers).
		<-v.Gone()
		s.mu.Lock()
		defer s.mu.Unlock()
		list := s.viewers[id]
		for i, x := range list {
			if x == v {
				s.viewers[id] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(s.viewers[id]) == 0 {
			delete(s.viewers, id)
		}
	}()
}

// replaceViewers closes the live views already open through a handoff,
// for when its browser opens it again (a reload), and waits until they
// have let go of the controls, so their hand-back can't take control from
// the new view.
func (s *Service) replaceViewers(ctx context.Context, h Handoff) {
	s.mu.Lock()
	old := slices.Clone(s.viewers[h.ID])
	s.mu.Unlock()
	for _, v := range old {
		v.Close()
	}
	// A view stays listed until it has handed its control back (track),
	// even one already gone (a page closed, then reloaded): waiting for
	// each to be gone waits for its hand-back, so the new view finds the
	// browser free; a few seconds at most.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, v := range old {
		select {
		case <-v.Gone():
		case <-ctx.Done():
			return
		}
	}
}

// Wait returns the handoff once it's over, or as it stands after wait.
// Over means its browser is handed back too: its status turns final only
// then (end).
func (s *Service) Wait(ctx context.Context, id string, wait time.Duration) (Handoff, error) {
	h, err := s.Get(ctx, id)
	if err != nil || h.Status.Final() || wait <= 0 {
		return h, err
	}
	if s.beforeListen != nil {
		s.beforeListen()
	}
	ch := make(chan struct{})
	s.mu.Lock()
	s.waiters[id] = append(s.waiters[id], ch)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		list := s.waiters[id]
		for i, c := range list {
			if c == ch {
				s.waiters[id] = append(list[:i], list[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()
	// It may have ended before we listened, waking nobody: read it again.
	if h, err = s.Get(ctx, id); err != nil || h.Status.Final() {
		return h, err
	}
	// Wake by the expiry too, so a waiter learns of it without polling.
	until := time.Until(h.ExpiresAt)
	t := time.NewTimer(min(wait, max(until, 0)+time.Second))
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
		return h, ctx.Err()
	}
	return s.Get(ctx, id)
}

// Sweep checks every pending handoff, every interval: one whose time is
// up expires and one whose browser stopped is cancelled, so the agents
// waiting on them hear at once and their browsers go back to them.
func (s *Service) Sweep(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

func (s *Service) sweep(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM handoffs WHERE status = 'pending'`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	// Each handoff on its own: one whose browser doesn't answer its
	// hand-back holds up neither the others nor the sweep. One whose end
	// is under way is left to it.
	for _, id := range ids {
		if s.isEnding(id) {
			continue
		}
		go func() {
			ctx, cancel := context.WithTimeout(ctx, sweepWait)
			defer cancel()
			h, err := s.Get(ctx, id) // Get ends it if it should
			if errors.Is(err, ErrHandingBack) {
				return // its end goes on by itself
			}
			if err != nil {
				s.log.Warn("checking a handoff", "handoff", id, "err", fmt.Sprint(err))
				return
			}
			// An open handoff with an until_url and no watch (the
			// orchestrator restarted) is watched again.
			s.watch(h)
		}()
	}
}

// sweepWait bounds how long the sweep waits for one handoff's end.
const sweepWait = 10 * time.Second
