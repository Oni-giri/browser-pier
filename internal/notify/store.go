package notify

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// config is the webhook as the admin set it, kept in the settings table.
// URL and Secret are credentials: stored as they are (the secret is an
// HMAC key, so it can't be hashed), never logged, never shown again.
type config struct {
	URL    string
	Secret string // 64 hex characters, the HMAC key as ASCII
	// On says which events are sent.
	On map[Kind]bool
	// LinkBase is the admin's "Address for links", used when
	// BROWSERPIER_PUBLIC_URL isn't set.
	LinkBase string
}

// Keys in the settings table.
const (
	keyURL      = "notify.url"
	keySecret   = "notify.secret"
	keyLinkBase = "notify.link_base"
	keyEvent    = "notify.event." // + the event's name: "on" or "off"
)

// defaultOn is whether each event is sent before the admin chooses.
var defaultOn = map[Kind]bool{HandoffRequested: true, HandoffExpired: true, HandoffCancelled: true, BrowserFailed: false}

func load(ctx context.Context, db *sql.DB) (config, error) {
	c := config{On: map[Kind]bool{}}
	for k, v := range defaultOn {
		c.On[k] = v
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key LIKE 'notify.%'`)
	if err != nil {
		return c, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return c, err
		}
		switch {
		case k == keyURL:
			c.URL = v
		case k == keySecret:
			c.Secret = v
		case k == keyLinkBase:
			c.LinkBase = v
		case strings.HasPrefix(k, keyEvent):
			if kind := Kind(strings.TrimPrefix(k, keyEvent)); kind.Toggle() {
				c.On[kind] = v == "on"
			}
		}
	}
	return c, rows.Err()
}

func save(ctx context.Context, db *sql.DB, c config) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	kv := [][2]string{{keyURL, c.URL}, {keySecret, c.Secret}, {keyLinkBase, c.LinkBase}}
	for _, k := range Toggles {
		v := "off"
		if c.On[k] {
			v = "on"
		}
		kv = append(kv, [2]string{keyEvent + string(k), v})
	}
	for _, p := range kv {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, p[0], p[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// linkPathRE is what a link address's path may hold: no spaces, angle
// brackets, quotes or ampersands, which Slack's text would read as markup.
var linkPathRE = regexp.MustCompile(`^[A-Za-z0-9._~/-]*$`)

// CheckLinkBase checks an address for links and returns it without a
// trailing slash: http(s), a host, no user name or password, query or
// fragment.
func CheckLinkBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	bad := errors.New("the address for links must look like https://pier.example.com")
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || len(raw) > 512 {
		return "", bad
	}
	if !linkPathRE.MatchString(u.Path) || strings.ContainsAny(u.Host, `<>"'&| `) {
		return "", bad
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/"), nil
}
