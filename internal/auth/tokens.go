package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TokenPrefix starts every agent token, so a leaked one is recognisable
// (and secret scanners can be taught to find it).
const TokenPrefix = "bp_live_"

var (
	ErrInvalidToken  = errors.New("invalid or revoked token")
	ErrTokenNotFound = errors.New("token not found")
)

// Token is an agent token's record. The token itself is shown once, when
// it is created, and never stored.
type Token struct {
	ID   string
	Name string
	// Hint is the token's last four characters, to tell tokens apart in the UI.
	Hint string
	// AllProfiles, or else only Profiles.
	AllProfiles bool
	Profiles    []string
	CreatedAt   time.Time
	LastUsedAt  time.Time // zero if never used
}

// Allows reports whether the token may use the profile.
func (t Token) Allows(profile string) bool {
	if t.AllProfiles {
		return true
	}
	for _, p := range t.Profiles {
		if p == profile {
			return true
		}
	}
	return false
}

// Tokens stores agent tokens.
type Tokens struct {
	db  *sql.DB
	now func() time.Time
}

func NewTokens(db *sql.DB) *Tokens { return &Tokens{db: db, now: time.Now} }

// base32 without padding, lowercase: 32 random bytes make 52 characters.
const tokenAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	var sb strings.Builder
	var acc, bits uint
	for _, c := range b {
		acc = acc<<8 | uint(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			sb.WriteByte(tokenAlphabet[(acc>>bits)&31])
		}
	}
	if bits > 0 {
		sb.WriteByte(tokenAlphabet[(acc<<(5-bits))&31])
	}
	return sb.String()
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Create makes a token. It returns the token itself, which can't be
// recovered later. profiles is ignored when all is true.
func (s *Tokens) Create(ctx context.Context, name string, all bool, profiles []string) (Token, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return Token{}, "", errors.New("name: give 1 to 64 characters")
	}
	if !all && len(profiles) == 0 {
		return Token{}, "", errors.New("scope: choose all profiles or at least one")
	}
	if all {
		profiles = nil
	}
	secret := TokenPrefix + randomString(32)
	t := Token{
		ID:          "t_" + randomString(10),
		Name:        name,
		Hint:        secret[len(secret)-4:],
		AllProfiles: all,
		Profiles:    profiles,
		CreatedAt:   s.now().Truncate(time.Millisecond),
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Token{}, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_tokens (id, name, hash, hint, all_profiles, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, hashToken(secret), t.Hint, t.AllProfiles, t.CreatedAt.UnixMilli()); err != nil {
		return Token{}, "", err
	}
	for _, p := range profiles {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_token_profiles (token_id, profile) VALUES (?, ?)`, t.ID, p); err != nil {
			if strings.Contains(err.Error(), "FOREIGN KEY") {
				return Token{}, "", fmt.Errorf("scope: no profile named %q", p)
			}
			return Token{}, "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return Token{}, "", err
	}
	return t, secret, nil
}

// lastUsedEvery limits how often a token's last-used time is written.
const lastUsedEvery = time.Minute

// Authenticate returns the token's record, or ErrInvalidToken.
func (s *Tokens) Authenticate(ctx context.Context, secret string) (Token, error) {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return Token{}, ErrInvalidToken
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM agent_tokens WHERE hash = ?`, hashToken(secret)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrInvalidToken
	}
	if err != nil {
		return Token{}, err
	}
	t, err := s.Get(ctx, id)
	if errors.Is(err, ErrTokenNotFound) {
		return Token{}, ErrInvalidToken // revoked a moment ago
	}
	if err != nil {
		return Token{}, err
	}
	if now := s.now(); now.Sub(t.LastUsedAt) >= lastUsedEvery {
		t.LastUsedAt = now.Truncate(time.Millisecond)
		if _, err := s.db.ExecContext(ctx, `UPDATE agent_tokens SET last_used_at = ? WHERE id = ?`, t.LastUsedAt.UnixMilli(), id); err != nil {
			return Token{}, err
		}
	}
	return t, nil
}

// Get returns one token's record.
func (s *Tokens) Get(ctx context.Context, id string) (Token, error) {
	t, err := s.scanOne(ctx, s.db.QueryRowContext(ctx, `SELECT id, name, hint, all_profiles, created_at, last_used_at FROM agent_tokens WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrTokenNotFound
	}
	return t, err
}

// List returns every token, oldest first. Tokens made in the same
// millisecond keep the order they were made in: ids are random, so the
// tie goes to SQLite's rowid, which grows with each insert.
func (s *Tokens) List(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM agent_tokens ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Token, 0, len(ids))
	for _, id := range ids {
		t, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// Revoke deletes a token; it stops working at once.
func (s *Tokens) Revoke(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agent_tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTokenNotFound
	}
	return nil
}

func (s *Tokens) scanOne(ctx context.Context, row *sql.Row) (Token, error) {
	var t Token
	var created, lastUsed int64
	if err := row.Scan(&t.ID, &t.Name, &t.Hint, &t.AllProfiles, &created, &lastUsed); err != nil {
		return Token{}, err
	}
	t.CreatedAt = time.UnixMilli(created)
	if lastUsed != 0 {
		t.LastUsedAt = time.UnixMilli(lastUsed)
	}
	if t.AllProfiles {
		return t, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT profile FROM agent_token_profiles WHERE token_id = ? ORDER BY profile`, t.ID)
	if err != nil {
		return Token{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return Token{}, err
		}
		t.Profiles = append(t.Profiles, p)
	}
	return t, rows.Err()
}
