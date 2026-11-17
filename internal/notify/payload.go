package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"syscall"
	"time"
)

// payload is an event's body. Every string in it is BrowserPier's own
// word, the profile's name, a typed reason, policy or category, a time,
// an id, the token's name or the page's origin. Never the agent's detail,
// the person's note, the agent's withdrawal note, until_url, a session's
// error or log, or any page text, title, path or query.
type payload struct {
	ID      string `json:"id"` // the same on every try, so a receiver can drop repeats
	Event   Kind   `json:"event"`
	Time    string `json:"time"`
	Profile string `json:"profile,omitempty"`

	HandoffID string `json:"handoff_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Policy    string `json:"policy,omitempty"`
	Agent     string `json:"agent,omitempty"`
	// Origin is the page's scheme and host. The page controls it, so it
	// is a field only, never part of text, where a chat would link it.
	Origin    string `json:"origin,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	URL       string `json:"url,omitempty"`
	Cause     string `json:"cause,omitempty"`

	Category string `json:"category,omitempty"`

	// Text and Content are the same sentence, which Slack's and
	// Discord's incoming webhooks show as they are.
	Text    string `json:"text"`
	Content string `json:"content"`
}

func (p payload) marshal() []byte {
	b, _ := json.Marshal(p) // strings only: it can't fail
	return b
}

// Handoff is what an event says about a handoff.
type Handoff struct {
	ID      string
	Profile string
	// Reason is the typed reason, ReasonLabel its fixed words.
	Reason, ReasonLabel string
	Policy              string // after Stricter
	Agent               string // the token's name
	Origin              string // scheme and host only
	ExpiresAt           time.Time
}

// Categories of a failed browser: fixed words, never the session's error
// or its container's log, which a page can write to.
var categories = map[string]string{
	"start_failed":  "the browser failed to start",
	"crashed":       "the browser stopped unexpectedly",
	"unclean_close": "the browser didn't close cleanly, so recent logins may be lost",
	"lost":          "BrowserPier lost track of the browser",
}

// profileRE is a profile's name (profiles.nameRE): only such a name goes
// into the sentence.
var profileRE = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

func safeProfile(name string) string {
	if profileRE.MatchString(name) {
		return name
	}
	return ""
}

// sentence is the text for a chat: the fixed words, the profile and the
// link, or a pointer to BrowserPier when there is no address for links.
func sentence(profile, what, link string) string {
	s := "BrowserPier"
	if profile != "" {
		s += " · " + profile
	}
	s += ": " + what + "."
	if link != "" {
		return s + " " + link
	}
	return s + " Open BrowserPier to see it."
}

// base is the address links are built on: BROWSERPIER_PUBLIC_URL, else
// the admin's address for links, else none.
func (n *Notifier) base(cfg config) string {
	if n.o.PublicURL != "" {
		return n.o.PublicURL
	}
	return cfg.LinkBase
}

func (n *Notifier) handoffPayload(cfg config, h Handoff, token string) payload {
	p := payload{Profile: safeProfile(h.Profile), HandoffID: h.ID, Reason: h.Reason, Policy: h.Policy, Agent: h.Agent,
		Origin: h.Origin, ExpiresAt: h.ExpiresAt.UTC().Format(time.RFC3339)}
	if b := n.base(cfg); b != "" {
		if token != "" {
			p.URL = b + "/h/" + token
		} else if p.Profile != "" {
			p.URL = b + "/profiles/" + p.Profile
		}
	}
	return p
}

// HandoffRequested queues handoff.requested and reports whether it was
// queued. token is the link's, given only for a PIN or sign-in handoff,
// whose link asks for the PIN or the password before it unlocks anything;
// for a link-only one it is "" and the event links to the profile's page.
// live says whether the handoff is still open, checked before a retry.
func (n *Notifier) HandoffRequested(h Handoff, token string, live func() bool) bool {
	if n == nil {
		return false
	}
	cfg, _ := n.current()
	p := n.handoffPayload(cfg, h, token)
	what := "an agent needs you"
	if h.ReasonLabel != "" {
		what += " (" + h.ReasonLabel + ")"
	}
	p.Text = sentence(p.Profile, what, p.URL)
	p.Content = p.Text
	return n.enqueue(HandoffRequested, h.Profile, p, live)
}

// HandoffExpired queues handoff.expired: nobody handed back in time.
func (n *Notifier) HandoffExpired(h Handoff) bool {
	if n == nil {
		return false
	}
	cfg, _ := n.current()
	p := n.handoffPayload(cfg, h, "")
	p.Text = sentence(p.Profile, "nobody took the handoff within 15 minutes, so it expired", p.URL)
	p.Content = p.Text
	return n.enqueue(HandoffExpired, h.Profile, p, nil)
}

// CauseWrongPINs is the cause of a handoff five wrong PINs cancelled.
const CauseWrongPINs = "wrong_pins"

// HandoffCancelled queues handoff.cancelled by five wrong PINs.
func (n *Notifier) HandoffCancelled(h Handoff) bool {
	if n == nil {
		return false
	}
	cfg, _ := n.current()
	p := n.handoffPayload(cfg, h, "")
	p.Cause = CauseWrongPINs
	p.Text = sentence(p.Profile, "five wrong PINs cancelled a handoff", p.URL)
	p.Content = p.Text
	return n.enqueue(HandoffCancelled, h.Profile, p, nil)
}

// BrowserFailed queues browser.failed with a fixed category:
// start_failed, crashed, unclean_close or lost. Any other is dropped.
func (n *Notifier) BrowserFailed(profile, category string) bool {
	what, ok := categories[category]
	if n == nil || !ok {
		return false
	}
	cfg, _ := n.current()
	p := payload{Profile: safeProfile(profile), Category: category}
	if b := n.base(cfg); b != "" && p.Profile != "" {
		p.URL = b + "/profiles/" + p.Profile
	}
	p.Text = sentence(p.Profile, what, p.URL)
	p.Content = p.Text
	return n.enqueue(BrowserFailed, profile, p, nil)
}

func testPayload() payload {
	p := payload{Text: "BrowserPier: a test from Settings. The webhook works."}
	p.Content = p.Text
	return p
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// sendError is why a POST got no answer, in a few words: never the
// error net/http returns, which holds the whole URL.
type sendError string

func (e sendError) Error() string { return string(e) }

func classify(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var rec tls.RecordHeaderError
	var ne net.Error
	switch {
	case errors.Is(err, errAddress):
		return sendError("address not allowed")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return sendError("timed out")
	case errors.Is(err, context.Canceled):
		return sendError("cancelled")
	case errors.As(err, &dns):
		return sendError("host not found")
	case errors.Is(err, syscall.ECONNREFUSED):
		return sendError("connection refused")
	case errors.As(err, &cert), errors.As(err, &unknown), errors.As(err, &host), errors.As(err, &rec):
		return sendError("TLS error")
	}
	return sendError("network error")
}
