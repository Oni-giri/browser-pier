package notify

import (
	"context"
	"errors"
	"maps"
	"net/url"
)

// View is the webhook as Settings shows it: never its secret, nor its URL
// beyond the scheme and host (a Slack or Discord URL is a credential).
type View struct {
	Set    bool
	Masked string // scheme://host/…
	On     map[Kind]bool
	// LinkBase is the admin's address for links; PublicURL, when set,
	// is used instead and the field can't be changed.
	LinkBase  string
	PublicURL string
}

// Mask shows a webhook URL's scheme and host, the rest as "…".
func Mask(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(hidden)"
	}
	m := u.Scheme + "://" + u.Host
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		m += "/…"
	}
	return m
}

// View returns the webhook's settings to show.
func (n *Notifier) View() View {
	cfg, _ := n.current()
	return View{Set: cfg.URL != "", Masked: Mask(cfg.URL), On: maps.Clone(cfg.On), LinkBase: cfg.LinkBase, PublicURL: n.o.PublicURL}
}

// Change is what the Settings form saves.
type Change struct {
	// URL is the webhook's; its masked form keeps the saved one, and ""
	// removes it.
	URL      string
	On       map[Kind]bool
	LinkBase string
}

// Save checks and stores a change. A first URL gets a new secret, which
// is returned to show once; changing the URL keeps the secret.
func (n *Notifier) Save(ctx context.Context, c Change) (secret string, err error) {
	base, err := CheckLinkBase(c.LinkBase)
	if err != nil {
		return "", err
	}
	return n.change(ctx, func(cfg *config) (string, error) {
		raw := c.URL
		if raw != "" && raw == Mask(cfg.URL) {
			raw = cfg.URL
		}
		if raw != cfg.URL && raw != "" {
			if err := checkURL(ctx, n.lookup, raw); err != nil {
				return "", err
			}
		}
		fresh := ""
		switch {
		case raw == "":
			cfg.Secret = ""
		case cfg.URL == "" || cfg.Secret == "":
			fresh = NewSecret()
			cfg.Secret = fresh
		}
		cfg.URL = raw
		for _, k := range Toggles {
			cfg.On[k] = c.On[k]
		}
		if n.o.PublicURL == "" {
			cfg.LinkBase = base
		}
		return fresh, nil
	})
}

// Regenerate replaces the secret and returns the new one, to show once.
func (n *Notifier) Regenerate(ctx context.Context) (string, error) {
	return n.change(ctx, func(cfg *config) (string, error) {
		if cfg.URL == "" {
			return "", ErrNoWebhook
		}
		cfg.Secret = NewSecret()
		return cfg.Secret, nil
	})
}

// Remove forgets the webhook and its secret; the event choices stay.
func (n *Notifier) Remove(ctx context.Context) error {
	_, err := n.change(ctx, func(cfg *config) (string, error) {
		cfg.URL, cfg.Secret = "", ""
		return "", nil
	})
	return err
}

// change applies f to a copy of the settings, stores it, then uses it.
// A new URL or secret starts a new generation: events queued for the old
// webhook aren't sent, or sent again, to the new one.
func (n *Notifier) change(ctx context.Context, f func(*config) (string, error)) (string, error) {
	if n == nil {
		return "", errors.New("webhooks aren't available")
	}
	n.saveMu.Lock()
	defer n.saveMu.Unlock()
	old, _ := n.current()
	next := old
	next.On = maps.Clone(old.On)
	out, err := f(&next)
	if err != nil {
		return "", err
	}
	if err := save(ctx, n.db, next); err != nil {
		return "", err
	}
	n.mu.Lock()
	if next.URL != old.URL || next.Secret != old.Secret {
		n.gen++
	}
	n.cfg = next
	n.mu.Unlock()
	return out, nil
}
