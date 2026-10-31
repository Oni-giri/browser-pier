// Package profiles stores profiles: saved browser identities, each with its
// own persistent folder and settings.
package profiles

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // the orchestrator image has no zoneinfo to check time zones against
)

// Mode is how agents drive a session by default.
type Mode string

const (
	ModeAPI    Mode = "api"    // CDP
	ModeVisual Mode = "visual" // screenshots plus OS-level input
)

// HandoffPolicy is what a human must pass to open a handoff link.
type HandoffPolicy string

const (
	PolicyLink   HandoffPolicy = "link"   // the link alone
	PolicyPIN    HandoffPolicy = "pin"    // the link plus the handoff PIN
	PolicySignIn HandoffPolicy = "signin" // the admin password
)

// Screen size limits, matching what the browser image accepts.
const (
	MinWidth, MinHeight = 640, 480
	MaxWidth, MaxHeight = 7680, 4320
)

// AutoStopChoices are the idle timeouts a profile can have, in minutes; 0 is never.
var AutoStopChoices = []int{5, 10, 15, 30, 0}

var (
	ErrNotFound = errors.New("profile not found")
	ErrExists   = errors.New("a profile with that name already exists")
)

var nameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

// Profile is a saved browser identity and its settings. Settings apply the
// next time its browser starts.
type Profile struct {
	Name        string
	DefaultMode Mode
	// AutoStop is how long the browser may sit idle before it is stopped; 0 is never.
	AutoStop      time.Duration
	Width, Height int
	// Proxy is the upstream proxy URL, which may carry credentials: never log it.
	Proxy         string
	HandoffPolicy HandoffPolicy
	// TimeZone and Locale, when set, are used instead of the ones matching
	// the exit location: an IANA zone such as Europe/Berlin and a BCP 47
	// locale such as de-DE.
	TimeZone string
	Locale   string
	// LocalNetwork lets pages reach private addresses: the LAN, the Docker
	// host and its other containers. Loopback and link-local addresses (the
	// cloud metadata endpoint) stay out of reach. Only the admin sets it.
	LocalNetwork bool
	// Location is where the browser appeared to be the last time it ran.
	// BrowserPier records it; it can't be set.
	Location  Location
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Location is a browser's exit location as last seen.
type Location struct {
	Country   string // ISO 3166-1 alpha-2
	TimeZone  string // IANA
	IP        string
	CheckedAt time.Time // zero if never
}

var localeRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2})?$`)

// New returns a profile with the default settings.
func New(name string) Profile {
	return Profile{
		Name:          name,
		DefaultMode:   ModeVisual,
		AutoStop:      15 * time.Minute,
		Width:         1440,
		Height:        900,
		HandoffPolicy: PolicyPIN,
	}
}

// Screen is the size in the WIDTHxHEIGHT form the browser image takes.
func (p Profile) Screen() string { return fmt.Sprintf("%dx%d", p.Width, p.Height) }

// Validate reports the first setting that isn't allowed.
func (p Profile) Validate() error {
	if !nameRE.MatchString(p.Name) {
		return fmt.Errorf("name %q: use 1 to 32 lowercase letters, numbers and dashes, not starting or ending with a dash", p.Name)
	}
	if p.DefaultMode != ModeAPI && p.DefaultMode != ModeVisual {
		return fmt.Errorf("default mode %q: want %q or %q", p.DefaultMode, ModeAPI, ModeVisual)
	}
	if !validAutoStop(p.AutoStop) {
		return fmt.Errorf("auto-stop %s: want 5, 10, 15 or 30 minutes, or 0 for never", p.AutoStop)
	}
	if p.Width < MinWidth || p.Height < MinHeight || p.Width > MaxWidth || p.Height > MaxHeight {
		return fmt.Errorf("screen %s: want between %dx%d and %dx%d", p.Screen(), MinWidth, MinHeight, MaxWidth, MaxHeight)
	}
	switch p.HandoffPolicy {
	case PolicyLink, PolicyPIN, PolicySignIn:
	default:
		return fmt.Errorf("handoff policy %q: want %q, %q or %q", p.HandoffPolicy, PolicyLink, PolicyPIN, PolicySignIn)
	}
	if p.TimeZone != "" {
		if _, err := time.LoadLocation(p.TimeZone); err != nil || p.TimeZone == "Local" {
			return fmt.Errorf("time zone %q: want an IANA name such as Europe/Berlin", p.TimeZone)
		}
	}
	if p.Locale != "" && !localeRE.MatchString(p.Locale) {
		return fmt.Errorf("locale %q: want a language and country such as de-DE", p.Locale)
	}
	return validProxy(p.Proxy)
}

func validAutoStop(d time.Duration) bool {
	for _, m := range AutoStopChoices {
		if d == time.Duration(m)*time.Minute {
			return true
		}
	}
	return false
}

// validProxy checks the proxy URL without ever echoing it, since it may
// hold a password.
func validProxy(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("proxy: not a valid URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	default:
		return errors.New("proxy: the scheme must be http, https or socks5")
	}
	if u.Hostname() == "" || u.Port() == "" {
		return errors.New("proxy: give a host and a port, e.g. http://user:pass@proxy.example:8080")
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("proxy: give only scheme, credentials, host and port")
	}
	return nil
}

// Store keeps profiles in the database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func NewStore(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

const columns = `name, default_mode, auto_stop_minutes, screen_width, screen_height, proxy, handoff_policy, created_at, updated_at,
	tz_override, locale_override, geo_country, geo_time_zone, geo_ip, geo_checked_at, local_network`

// Create adds a new profile.
func (s *Store) Create(ctx context.Context, p Profile) (Profile, error) {
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	now := s.now().Truncate(time.Millisecond)
	p.CreatedAt, p.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx, `INSERT INTO profiles (name, default_mode, auto_stop_minutes, screen_width, screen_height,
		proxy, handoff_policy, created_at, updated_at, tz_override, locale_override, local_network) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO NOTHING`,
		p.Name, p.DefaultMode, int(p.AutoStop/time.Minute), p.Width, p.Height, p.Proxy, p.HandoffPolicy,
		now.UnixMilli(), now.UnixMilli(), p.TimeZone, p.Locale, p.LocalNetwork)
	if err != nil {
		return Profile{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Profile{}, ErrExists
	}
	return p, nil
}

// Update replaces a profile's settings. The name can't change.
func (s *Store) Update(ctx context.Context, p Profile) (Profile, error) {
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	now := s.now().Truncate(time.Millisecond)
	res, err := s.db.ExecContext(ctx, `UPDATE profiles SET default_mode = ?, auto_stop_minutes = ?, screen_width = ?,
		screen_height = ?, proxy = ?, handoff_policy = ?, tz_override = ?, locale_override = ?, local_network = ?, updated_at = ? WHERE name = ?`,
		p.DefaultMode, int(p.AutoStop/time.Minute), p.Width, p.Height, p.Proxy, p.HandoffPolicy, p.TimeZone, p.Locale, p.LocalNetwork,
		now.UnixMilli(), p.Name)
	if err != nil {
		return Profile{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Profile{}, ErrNotFound
	}
	return s.Get(ctx, p.Name)
}

// SetLocation records where the profile's browser appeared to be.
func (s *Store) SetLocation(ctx context.Context, name string, l Location) error {
	_, err := s.db.ExecContext(ctx, `UPDATE profiles SET geo_country = ?, geo_time_zone = ?, geo_ip = ?, geo_checked_at = ? WHERE name = ?`,
		l.Country, l.TimeZone, l.IP, l.CheckedAt.UnixMilli(), name)
	return err
}

// Get returns one profile.
func (s *Store) Get(ctx context.Context, name string) (Profile, error) {
	p, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM profiles WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	return p, err
}

// List returns every profile, by name.
func (s *Store) List(ctx context.Context) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Profile
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete removes a profile's record and its session history. Its folder is
// the caller's to remove (see sessions.Manager.DeleteProfile).
func (s *Store) Delete(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM profiles WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scan(row interface{ Scan(...any) error }) (Profile, error) {
	var p Profile
	var autoStop int
	var created, updated, checked int64
	err := row.Scan(&p.Name, &p.DefaultMode, &autoStop, &p.Width, &p.Height, &p.Proxy, &p.HandoffPolicy, &created, &updated,
		&p.TimeZone, &p.Locale, &p.Location.Country, &p.Location.TimeZone, &p.Location.IP, &checked, &p.LocalNetwork)
	p.AutoStop = time.Duration(autoStop) * time.Minute
	p.CreatedAt, p.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	if checked != 0 {
		p.Location.CheckedAt = time.UnixMilli(checked)
	}
	return p, err
}
