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
	"net"
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

	mu       sync.Mutex
	waiters  map[string][]chan struct{}
	viewers  map[string][]*webrtc.Viewer // live views opened through a handoff
	checking map[string]int              // PIN checks under way, per handoff
	watching map[string]*watch           // address watches, per handoff (watch.go)
}

func New(db *sql.DB, m *sessions.Manager, log *slog.Logger) *Service {
	base, closeBase := context.WithCancel(context.Background())
	return &Service{db: db, m: m, log: log, now: time.Now, base: base, closeBase: closeBase, watchPoll: watchEvery,
		waiters: map[string][]chan struct{}{}, viewers: map[string][]*webrtc.Viewer{}, checking: map[string]int{}, watching: map[string]*watch{}}
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
	if _, err := s.db.ExecContext(ctx, `INSERT INTO handoffs (id, session_id, profile, agent, reason, detail, origin, policy, status, token_hash, created_at, expires_at, until_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.SessionID, h.Profile, h.Agent, h.Reason, h.Detail, h.Origin, h.Policy, h.Status, hash(token),
		h.CreatedAt.UnixMilli(), h.ExpiresAt.UnixMilli(), h.UntilURL); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: handoffs.session_id") {
			return Handoff{}, "", ErrOpen // another Create got in first
		}
		return Handoff{}, "", err
	}
	if _, err := s.m.SetNeedsHelp(sessionID, true); err != nil {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM handoffs WHERE id = ?`, h.ID)
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
	sock := s.m.Layout().ControlSocket(profile)
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
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

const columns = `id, session_id, profile, agent, reason, detail, origin, policy, status, note, pin_failures, created_at, expires_at, browser_hash, until_url, auto_done`

func scan(row interface{ Scan(...any) error }) (Handoff, error) {
	var h Handoff
	var created, expires int64
	var browser string
	err := row.Scan(&h.ID, &h.SessionID, &h.Profile, &h.Agent, &h.Reason, &h.Detail, &h.Origin, &h.Policy, &h.Status, &h.Note,
		&h.PINFailures, &created, &expires, &browser, &h.UntilURL, &h.Auto)
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

// checkLive ends a pending handoff whose time is up or whose browser stopped.
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
	return h, nil
}

// Unlock binds the handoff to one browser, identified by the value of a
// cookie only that browser holds. The first browser to pass the policy
// check gets it; any other is refused, so the link works once.
func (s *Service) Unlock(ctx context.Context, h Handoff, browser string) error {
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

// end is finish, as how says.
func (s *Service) end(ctx context.Context, h Handoff, status Status, note string, how endHow) (Handoff, error) {
	now := s.now()
	query, args := `UPDATE handoffs SET status = ?, note = ?, finished_at = ? WHERE id = ? AND status = 'pending'`, []any{status, note, now.UnixMilli(), h.ID}
	switch how {
	case endUnopened:
		query += ` AND browser_hash = ''`
	case endAuto:
		query = `UPDATE handoffs SET status = ?, note = ?, finished_at = ?, auto_done = 1 WHERE id = ? AND status = 'pending' AND expires_at > ?`
		args = append(args, now.UnixMilli())
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return h, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Someone else finished it first, or the condition didn't hold.
		cur, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM handoffs WHERE id = ?`, h.ID))
		if err == nil && cur.Status == Pending {
			return cur, ErrInUse
		}
		return cur, err
	}
	h.Status, h.Note, h.Auto = status, note, how == endAuto
	s.m.ReleaseControl(h.SessionID)
	if _, err := s.m.SetNeedsHelp(h.SessionID, false); err != nil && !errors.Is(err, sessions.ErrNotRunning) {
		s.log.Warn("handing the browser back", "handoff", h.ID, "err", err)
	}
	logNote := finishedNotes[status]
	if h.Auto {
		logNote = autoDoneLog
	}
	s.m.Note(h.SessionID, "", logNote)
	s.log.Info("handoff finished", "handoff", h.ID, "profile", h.Profile, "status", status, "until_url_reached", h.Auto)
	s.wake(h.ID)
	return h, nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.waiters[id] {
		close(ch)
	}
	delete(s.waiters, id)
	// The link is dead: so are the live views it opened.
	for _, v := range s.viewers[id] {
		v.Close()
	}
	delete(s.viewers, id)
	delete(s.checking, id)
	// And so is its address watch, if it has one.
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
		<-v.Done()
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
	if len(old) == 0 {
		return
	}
	for _, v := range old {
		v.Close()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, v := range old {
		select {
		case <-v.Done():
		case <-ctx.Done():
			return
		}
	}
	for {
		if sess, ok := s.m.Live(h.Profile); !ok || !sess.HumanControl {
			return
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
}

// Wait returns the handoff once it's over, or as it stands after wait.
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
	for _, id := range ids {
		h, err := s.Get(ctx, id) // Get ends it if it should
		if err != nil {
			s.log.Warn("checking a handoff", "handoff", id, "err", fmt.Sprint(err))
			continue
		}
		// An open handoff with an until_url and no watch (the orchestrator
		// restarted) is watched again.
		s.watch(h)
	}
}
