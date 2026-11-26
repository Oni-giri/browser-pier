package auth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	ps := profiles.NewStore(d)
	for _, n := range []string{"google", "github"} {
		if _, err := ps.Create(ctx, profiles.New(n)); err != nil {
			t.Fatal(err)
		}
	}
	s := NewTokens(d)

	all, secret, err := s.Create(ctx, "claude", true, []string{"ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) || len(secret) != len(TokenPrefix)+52 || all.Hint != secret[len(secret)-4:] {
		t.Fatalf("token %q, hint %q", secret, all.Hint)
	}
	var stored string
	if err := d.QueryRow(`SELECT hash FROM agent_tokens WHERE id = ?`, all.ID).Scan(&stored); err != nil || strings.Contains(stored, secret[len(TokenPrefix):]) {
		t.Fatalf("stored %q, %v; the token must not be stored", stored, err)
	}

	got, err := s.Authenticate(ctx, secret)
	if err != nil || got.ID != all.ID || !got.Allows("google") || !got.Allows("anything") || got.LastUsedAt.IsZero() {
		t.Fatalf("Authenticate = %+v, %v", got, err)
	}
	for _, bad := range []string{"", secret + "x", secret[:len(secret)-1], strings.TrimPrefix(secret, TokenPrefix), "bp_live_" + strings.Repeat("a", 52)} {
		if _, err := s.Authenticate(ctx, bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Authenticate(%q) = %v", bad, err)
		}
	}

	scoped, scopedSecret, err := s.Create(ctx, "browser-use", false, []string{"google"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.Authenticate(ctx, scopedSecret)
	if err != nil || !got.Allows("google") || got.Allows("github") {
		t.Fatalf("scoped token %+v, %v", got, err)
	}
	if _, _, err := s.Create(ctx, "x", false, []string{"nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("scope with an unknown profile: %v", err)
	}
	if _, _, err := s.Create(ctx, "x", false, nil); err == nil {
		t.Fatal("empty scope accepted")
	}
	if _, _, err := s.Create(ctx, "  ", true, nil); err == nil {
		t.Fatal("blank name accepted")
	}

	// Deleting a profile drops it from scopes, so a new profile with the
	// same name isn't reachable by the old token.
	if err := ps.Delete(ctx, "google"); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Create(ctx, profiles.New("google")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, scoped.ID); got.Allows("google") {
		t.Fatal("token still scoped to a deleted profile's name")
	}

	list, err := s.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != all.ID {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if err := s.Revoke(ctx, all.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, secret); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token: %v", err)
	}
	if err := s.Revoke(ctx, all.ID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("second Revoke = %v", err)
	}
}

func TestLastUsedIsThrottled(t *testing.T) {
	ctx := context.Background()
	s := NewTokens(openDB(t))
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	_, secret, err := s.Create(ctx, "a", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := s.Authenticate(ctx, secret)
	now = now.Add(30 * time.Second)
	if again, _ := s.Authenticate(ctx, secret); !again.LastUsedAt.Equal(first.LastUsedAt) {
		t.Fatal("last used written again within a minute")
	}
	now = now.Add(time.Minute)
	if later, _ := s.Authenticate(ctx, secret); !later.LastUsedAt.Equal(now) {
		t.Fatalf("last used %v, want %v", later.LastUsedAt, now)
	}
}

func TestRandomStringLength(t *testing.T) {
	for n, want := range map[int]int{32: 52, 10: 16, 5: 8} {
		if got := len(randomString(n)); got != want {
			t.Errorf("randomString(%d) has %d characters, want %d", n, got, want)
		}
	}
}

// Tokens made in the same millisecond list in the order they were made,
// not in the order of their random ids (PROJ-171).
func TestTokensMadeInTheSameMillisecondListInOrder(t *testing.T) {
	ctx := context.Background()
	s := NewTokens(openDB(t))
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return at }
	var want []string
	for i := range 20 {
		tok, _, err := s.Create(ctx, "agent-"+string(rune('a'+i)), true, nil)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, tok.ID)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tok := range list {
		got = append(got, tok.ID)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("List order\n got %v\nwant %v", got, want)
	}
}
