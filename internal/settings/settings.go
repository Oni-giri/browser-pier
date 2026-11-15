// Package settings stores the admin's defaults from the web UI's
// Settings screen: what new profiles start with, and the memory cap.
package settings

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// Settings are the admin's defaults.
type Settings struct {
	// DefaultPolicy is the handoff policy new profiles get.
	DefaultPolicy profiles.HandoffPolicy
	// DefaultAutoStop is new profiles' idle timeout, one of
	// profiles.AutoStopChoices; 0 is never.
	DefaultAutoStop time.Duration
	// DefaultProxy is new profiles' proxy URL, "" for none. It may hold
	// credentials: never log it.
	DefaultProxy string
	// DefaultBlockAds is whether new profiles block ad and tracker
	// domains (PROJ-119); on unless the admin turns it off.
	DefaultBlockAds bool
	// RAMLimitPercent caps the memory all browsers may use together, as a
	// share of the host's: 1 to 100, or 0 for not set here (the
	// BROWSERPIER_RAM_LIMIT_PERCENT default then applies).
	RAMLimitPercent int
}

// defaults are the settings before the admin changes any.
func defaults() Settings {
	p := profiles.New("")
	return Settings{DefaultPolicy: p.HandoffPolicy, DefaultAutoStop: p.AutoStop, DefaultBlockAds: p.BlockAds}
}

// NewProfile is profiles.New with these defaults applied.
func (s Settings) NewProfile(name string) profiles.Profile {
	p := profiles.New(name)
	p.HandoffPolicy, p.AutoStop, p.Proxy, p.BlockAds = s.DefaultPolicy, s.DefaultAutoStop, s.DefaultProxy, s.DefaultBlockAds
	return p
}

// Validate reports the first setting that isn't allowed. Errors never
// echo the proxy URL.
func (s Settings) Validate() error {
	// A profile made from these defaults checks them the way any profile
	// is checked, proxy included.
	if err := s.NewProfile("default").Validate(); err != nil {
		return err
	}
	if s.RAMLimitPercent < 0 || s.RAMLimitPercent > 100 {
		return fmt.Errorf("RAM limit %d%%: want 1 to 100, or 0 for the default", s.RAMLimitPercent)
	}
	return nil
}

// Keys in the settings table; the ui. prefix keeps them apart from auth's.
const (
	keyPolicy   = "ui.default_handoff_policy"
	keyAutoStop = "ui.default_auto_stop_minutes"
	keyProxy    = "ui.default_proxy"
	keyRAM      = "ui.ram_limit_percent"
	keyBlockAds = "ui.default_block_ads"
)

// Store keeps the settings in the database's settings table.
type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

// Get returns the settings, with the defaults for any never saved.
func (s *Store) Get(ctx context.Context) (Settings, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN (?, ?, ?, ?, ?)`,
		keyPolicy, keyAutoStop, keyProxy, keyRAM, keyBlockAds)
	if err != nil {
		return Settings{}, err
	}
	defer func() { _ = rows.Close() }()
	out := defaults()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return Settings{}, err
		}
		switch k {
		case keyPolicy:
			out.DefaultPolicy = profiles.HandoffPolicy(v)
		case keyAutoStop:
			n, err := strconv.Atoi(v)
			if err != nil {
				return Settings{}, fmt.Errorf("stored setting %s: %w", k, err)
			}
			out.DefaultAutoStop = time.Duration(n) * time.Minute
		case keyProxy:
			out.DefaultProxy = v
		case keyRAM:
			if out.RAMLimitPercent, err = strconv.Atoi(v); err != nil {
				return Settings{}, fmt.Errorf("stored setting %s: %w", k, err)
			}
		case keyBlockAds:
			if out.DefaultBlockAds, err = strconv.ParseBool(v); err != nil {
				return Settings{}, fmt.Errorf("stored setting %s: %w", k, err)
			}
		}
	}
	return out, rows.Err()
}

// Save checks the settings and stores them all, in one transaction.
func (s *Store) Save(ctx context.Context, set Settings) error {
	if err := set.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, kv := range [][2]string{
		{keyPolicy, string(set.DefaultPolicy)},
		{keyAutoStop, strconv.Itoa(int(set.DefaultAutoStop / time.Minute))},
		{keyProxy, set.DefaultProxy},
		{keyRAM, strconv.Itoa(set.RAMLimitPercent)},
		{keyBlockAds, strconv.FormatBool(set.DefaultBlockAds)},
	} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}
