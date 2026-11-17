// Package notify tells one admin-set webhook when a person is needed: a
// handoff asked for, expired or cancelled by wrong PINs, and optionally a
// browser that failed. Each event is a small signed JSON POST, built only
// from BrowserPier's own words and typed fields, sent from a bounded
// in-memory queue so a slow or dead webhook never holds anyone up.
package notify

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Kind is an event's name.
type Kind string

const (
	HandoffRequested Kind = "handoff.requested"
	HandoffExpired   Kind = "handoff.expired"
	HandoffCancelled Kind = "handoff.cancelled"
	BrowserFailed    Kind = "browser.failed"
	Test             Kind = "test"
)

// Toggles are the events the admin can turn on or off, in the order shown.
var Toggles = []Kind{HandoffRequested, HandoffExpired, HandoffCancelled, BrowserFailed}

// Toggle reports whether the admin can turn the event on or off.
func (k Kind) Toggle() bool { return slices.Contains(Toggles, k) }

// Label is the event in words, for Settings.
func (k Kind) Label() string {
	return map[Kind]string{
		HandoffRequested: "An agent asks for a handoff",
		HandoffExpired:   "Nobody took a handoff in 15 minutes",
		HandoffCancelled: "Five wrong PINs cancelled a handoff",
		BrowserFailed:    "A browser crashed or failed to start",
	}[k]
}

// Delivery limits.
const (
	// Workers is how many POSTs may be under way at once.
	Workers = 4
	// QueueSize is how many events may wait; past it they are dropped.
	QueueSize = 64
	// FloodLimit handoff.requested events per profile in FloodWindow at
	// most: an agent talked into opening and withdrawing handoffs in a
	// loop must not fill a channel.
	FloodLimit  = 6
	FloodWindow = 15 * time.Minute
	// Timeout is per POST, connecting included.
	Timeout = 10 * time.Second
)

// retries are the waits before the second and third tries.
var retries = []time.Duration{2 * time.Second, 10 * time.Second}

// SignatureHeader carries t=<unix seconds>,v1=<hex HMAC-SHA256>.
const SignatureHeader = "BrowserPier-Signature"

// ErrNoWebhook is Send test with no webhook saved.
var ErrNoWebhook = errors.New("no webhook is set")

// Options are what the notifier needs from the server's configuration.
type Options struct {
	// PublicURL is BROWSERPIER_PUBLIC_URL; when set, links are built on it
	// and the admin's address for links is ignored. Never the request's
	// Host: an agent chooses that.
	PublicURL string
	// Version goes in the User-Agent.
	Version string
	// Lookup resolves webhook hosts; nil is the system resolver (tests
	// stub it).
	Lookup Lookup
	// OnQueue, if set, is called with each event queued, in the caller's
	// goroutine (tests).
	OnQueue func(kind Kind, body []byte)
}

// Notifier queues and sends the events.
type Notifier struct {
	db      *sql.DB
	log     *slog.Logger
	o       Options
	lookup  Lookup
	https   *http.Client
	http    *http.Client // plain http://: private addresses only
	queue   chan delivery
	now     func() time.Time
	newID   func() string
	timeout time.Duration
	retries []time.Duration

	saveMu sync.Mutex // one change of the settings at a time
	mu     sync.Mutex
	cfg    config
	// gen changes whenever the webhook's URL or secret does, so a retry
	// never goes to a webhook replaced or removed meanwhile.
	gen   uint64
	flood map[string][]time.Time // handoff.requested queued, per profile
}

// New reads the webhook's settings.
func New(ctx context.Context, db *sql.DB, log *slog.Logger, o Options) (*Notifier, error) {
	cfg, err := load(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("reading the webhook's settings: %w", err)
	}
	if o.PublicURL != "" {
		if o.PublicURL, err = CheckLinkBase(o.PublicURL); err != nil {
			// Links are left out rather than built on an odd address.
			log.Warn("BROWSERPIER_PUBLIC_URL can't be used in webhook links", "err", err)
			o.PublicURL = ""
		}
	}
	n := &Notifier{db: db, log: log, o: o, lookup: o.Lookup, queue: make(chan delivery, QueueSize),
		now: time.Now, newID: newID, timeout: Timeout, retries: retries, cfg: cfg, flood: map[string][]time.Time{}}
	if n.lookup == nil {
		n.lookup = systemLookup
	}
	n.https, n.http = n.client(false), n.client(true)
	return n, nil
}

func (n *Notifier) client(plain bool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:             nil, // never HTTP_PROXY: the address check must see the real address
			DialContext:       dialer(n.lookup, plain),
			DisableKeepAlives: true,
		},
		// Redirects aren't followed: a 3xx is final.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewSecret makes a webhook secret: 32 random bytes as 64 hex characters.
func NewSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Sign is the signature header's value for a body sent at t: the key is
// the secret's 64 hex characters as ASCII bytes, the message "<t>.<body>".
func Sign(secret string, t int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	ts := strconv.FormatInt(t, 10)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Run sends the queued events until ctx ends. An event waiting when it
// ends is lost: nothing is written to disk, since it may hold a link.
func (n *Notifier) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range Workers {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case d := <-n.queue:
					n.deliver(ctx, d)
				}
			}
		})
	}
	wg.Wait()
}

// delivery is one event to send. body may hold a handoff link: it lives
// in memory only, until the delivery ends.
type delivery struct {
	kind    Kind
	id      string
	profile string
	body    []byte
	gen     uint64
	// live, if set, says whether the handoff is still open; a retry of
	// one that ended would deliver a dead link.
	live func() bool
}

// current is the webhook's settings and their generation.
func (n *Notifier) current() (config, uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cfg, n.gen
}

// enqueue builds an event and queues it, without waiting: it reports
// whether it was queued. Nothing is queued with no webhook set or the
// event turned off.
func (n *Notifier) enqueue(kind Kind, profile string, p payload, live func() bool) bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	cfg, gen := n.cfg, n.gen
	if cfg.URL == "" || !cfg.On[kind] {
		n.mu.Unlock()
		return false
	}
	p.ID, p.Event, p.Time = n.newID(), kind, n.now().UTC().Format(time.RFC3339)
	if kind == HandoffRequested && !n.takeFloodSlot(profile) {
		n.mu.Unlock()
		n.log.Warn("webhook event dropped: too many handoffs asked for this profile", "event", kind, "id", p.ID, "profile", profile,
			"limit", FloodLimit, "window", FloodWindow.String())
		return false
	}
	n.mu.Unlock()
	body := p.marshal()
	select {
	case n.queue <- delivery{kind: kind, id: p.ID, profile: profile, body: body, gen: gen, live: live}:
	default:
		if kind == HandoffRequested {
			n.mu.Lock()
			n.giveFloodSlot(profile)
			n.mu.Unlock()
		}
		n.log.Warn("webhook event dropped: the queue is full", "event", kind, "id", p.ID, "profile", profile)
		return false
	}
	if n.o.OnQueue != nil {
		n.o.OnQueue(kind, body)
	}
	return true
}

// takeFloodSlot counts a handoff.requested for the profile, if it is
// under the limit. Call it with n.mu held.
func (n *Notifier) takeFloodSlot(profile string) bool {
	now := n.now()
	recent := n.flood[profile][:0]
	for _, t := range n.flood[profile] {
		if now.Sub(t) < FloodWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= FloodLimit {
		n.flood[profile] = recent
		return false
	}
	n.flood[profile] = append(recent, now)
	return true
}

// giveFloodSlot undoes the last takeFloodSlot. Call it with n.mu held.
func (n *Notifier) giveFloodSlot(profile string) {
	if l := n.flood[profile]; len(l) > 0 {
		n.flood[profile] = l[:len(l)-1]
	}
}

// deliver sends one event: a network error, a timeout or a 5xx is tried
// again after 2 s and after 10 s; anything else is final.
func (n *Notifier) deliver(ctx context.Context, d delivery) {
	for try := 1; try <= 1+len(n.retries); try++ {
		if try > 1 {
			t := time.NewTimer(n.retries[try-2])
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		cfg, gen := n.current()
		if gen != d.gen || cfg.URL == "" {
			n.log.Info("webhook event not sent: the webhook changed or was removed", "event", d.kind, "id", d.id, "profile", d.profile, "try", try)
			return
		}
		if try > 1 && d.live != nil && !d.live() {
			n.log.Info("webhook event not sent again: the handoff has ended", "event", d.kind, "id", d.id, "profile", d.profile, "try", try)
			return
		}
		status, err := n.post(ctx, cfg, d.body)
		host := hostOf(cfg.URL)
		switch {
		case err == nil && status >= 200 && status < 300:
			n.log.Info("webhook event delivered", "event", d.kind, "id", d.id, "profile", d.profile, "host", host, "status", status, "try", try)
			return
		case err == nil && (status < 500 || status > 599):
			n.log.Warn("webhook refused the event", "event", d.kind, "id", d.id, "profile", d.profile, "host", host, "status", status, "try", try)
			return
		case err == nil:
			n.log.Warn("webhook failed", "event", d.kind, "id", d.id, "profile", d.profile, "host", host, "status", status, "try", try)
		default:
			n.log.Warn("webhook unreachable", "event", d.kind, "id", d.id, "profile", d.profile, "host", host, "err", err.Error(), "try", try)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// post sends one signed POST and returns the answer's status; its body
// isn't read. The error is a short class, never net/http's own, which
// holds the whole URL.
func (n *Notifier) post(ctx context.Context, cfg config, body []byte) (int, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return 0, errors.New("bad URL")
	}
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytesReader(body))
	if err != nil {
		return 0, errors.New("bad URL")
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "BrowserPier/"+n.o.Version)
	req.Header.Set(SignatureHeader, Sign(cfg.Secret, n.now().Unix(), body)) // t per try
	c := n.https
	if u.Scheme == "http" {
		c = n.http
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, classify(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// SendTest posts a test event once, with no retry, and returns the
// receiver's status or a short error.
func (n *Notifier) SendTest(ctx context.Context) (int, error) {
	cfg, _ := n.current()
	if cfg.URL == "" {
		return 0, ErrNoWebhook
	}
	p := testPayload()
	p.ID, p.Event, p.Time = n.newID(), Test, n.now().UTC().Format(time.RFC3339)
	status, err := n.post(ctx, cfg, p.marshal())
	host := hostOf(cfg.URL)
	if err != nil {
		n.log.Warn("webhook test failed", "id", p.ID, "host", host, "err", err.Error())
	} else {
		n.log.Info("webhook test sent", "id", p.ID, "host", host, "status", status)
	}
	return status, err
}

// hostOf is a URL's host, the only part of it ever logged.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
