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
	"strings"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
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
)

// Handoff is one request for help.
type Handoff struct {
	ID, SessionID, Profile string
	Agent                  string // the token's name
	Reason                 Reason
	Detail                 string
	// Origin is the page's scheme and host, never its path or query,
	// which the page controls and could word a phishing message with.
	Origin      string
	Policy      profiles.HandoffPolicy
	Status      Status
	Note        string // the person's, for the agent
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

	mu      sync.Mutex
	waiters map[string][]chan struct{}
}

func New(db *sql.DB, m *sessions.Manager, log *slog.Logger) *Service {
	return &Service{db: db, m: m, log: log, now: time.Now, waiters: map[string][]chan struct{}{}}
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

// Create opens a handoff for a running session and pauses the agent
// (Needs help closes its connections). It returns the handoff and the
// link's token, which is not stored.
func (s *Service) Create(ctx context.Context, sessionID, agent string, reason Reason, detail string, require profiles.HandoffPolicy) (Handoff, string, error) {
	if _, ok := reasons[reason]; !ok {
		return Handoff{}, "", ErrBadReason
	}
	if require != "" {
		if _, ok := rank[require]; !ok {
			return Handoff{}, "", ErrBadRequire
		}
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
		Policy: Stricter(p.HandoffPolicy, require), Status: Pending, CreatedAt: now, ExpiresAt: now.Add(Lifetime),
	}
	token := randomToken()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO handoffs (id, session_id, profile, agent, reason, detail, origin, policy, status, token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.SessionID, h.Profile, h.Agent, h.Reason, h.Detail, h.Origin, h.Policy, h.Status, hash(token),
		h.CreatedAt.UnixMilli(), h.ExpiresAt.UnixMilli()); err != nil {
		return Handoff{}, "", err
	}
	if _, err := s.m.SetNeedsHelp(sessionID, true); err != nil {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM handoffs WHERE id = ?`, h.ID)
		return Handoff{}, "", err
	}
	s.log.Info("handoff opened", "handoff", h.ID, "profile", h.Profile, "agent", agent, "reason", reason, "policy", h.Policy)
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

const columns = `id, session_id, profile, agent, reason, detail, origin, policy, status, note, pin_failures, created_at, expires_at, browser_hash`

func scan(row interface{ Scan(...any) error }) (Handoff, error) {
	var h Handoff
	var created, expires int64
	var browser string
	err := row.Scan(&h.ID, &h.SessionID, &h.Profile, &h.Agent, &h.Reason, &h.Detail, &h.Origin, &h.Policy, &h.Status, &h.Note,
		&h.PINFailures, &created, &expires, &browser)
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

// PINFailed counts a wrong PIN and cancels the link at the limit. It
// returns how many tries are left.
func (s *Service) PINFailed(ctx context.Context, h Handoff) (int, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE handoffs SET pin_failures = pin_failures + 1 WHERE id = ?`, h.ID); err != nil {
		return 0, err
	}
	h, err := s.Get(ctx, h.ID)
	if err != nil {
		return 0, err
	}
	left := MaxPINFailures - h.PINFailures
	if left <= 0 {
		_, err := s.finish(ctx, h, Cancelled, "five wrong PINs")
		return 0, err
	}
	return left, nil
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

// finish ends a pending handoff, gives the browser back to the agent and
// wakes anyone waiting.
func (s *Service) finish(ctx context.Context, h Handoff, status Status, note string) (Handoff, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE handoffs SET status = ?, note = ?, finished_at = ? WHERE id = ? AND status = 'pending'`,
		status, note, s.now().UnixMilli(), h.ID)
	if err != nil {
		return h, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Someone else finished it first.
		return scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM handoffs WHERE id = ?`, h.ID))
	}
	h.Status, h.Note = status, note
	s.m.ReleaseControl(h.SessionID)
	if _, err := s.m.SetNeedsHelp(h.SessionID, false); err != nil && !errors.Is(err, sessions.ErrNotRunning) {
		s.log.Warn("handing the browser back", "handoff", h.ID, "err", err)
	}
	s.log.Info("handoff finished", "handoff", h.ID, "profile", h.Profile, "status", status)
	s.wake(h.ID)
	return h, nil
}

func (s *Service) wake(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.waiters[id] {
		close(ch)
	}
	delete(s.waiters, id)
}

// Wait returns the handoff once it's over, or as it stands after wait.
func (s *Service) Wait(ctx context.Context, id string, wait time.Duration) (Handoff, error) {
	h, err := s.Get(ctx, id)
	if err != nil || h.Status.Final() || wait <= 0 {
		return h, err
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

// Sweep expires handoffs left pending past their time, every interval,
// so their browsers go back to the agents.
func (s *Service) Sweep(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rows, err := s.db.QueryContext(ctx, `SELECT id FROM handoffs WHERE status = 'pending' AND expires_at <= ?`, s.now().UnixMilli())
			if err != nil {
				continue
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
				if _, err := s.Get(ctx, id); err != nil {
					s.log.Warn("expiring a handoff", "handoff", id, "err", fmt.Sprint(err))
				}
			}
		}
	}
}
