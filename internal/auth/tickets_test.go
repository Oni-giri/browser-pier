package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTickets(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	k := newTickets()
	k.now = func() time.Time { return now }

	a, exp, err := k.mint("tok1", "google")
	if err != nil || !strings.HasPrefix(a, TicketPrefix) || len(a) != len(TicketPrefix)+26 || !exp.Equal(now.Add(TicketTTL)) {
		t.Fatalf("mint = %q, %v, %v", a, exp, err)
	}
	for h := range k.m {
		if strings.Contains(h, strings.TrimPrefix(a, TicketPrefix)) {
			t.Fatal("ticket stored as is")
		}
	}
	if got, ok := k.redeem(a); !ok || got.tokenID != "tok1" || got.profile != "google" {
		t.Fatalf("redeem = %+v, %t", got, ok)
	}
	if _, ok := k.redeem(a); ok {
		t.Fatal("a ticket worked twice")
	}
	if _, ok := k.redeem("bp_cdp_nope"); ok {
		t.Fatal("an unknown ticket worked")
	}

	// Expired: refused, and used up all the same.
	b, _, _ := k.mint("tok1", "google")
	now = now.Add(TicketTTL)
	if _, ok := k.redeem(b); ok {
		t.Fatal("an expired ticket worked")
	}
	if len(k.m) != 0 {
		t.Fatalf("%d tickets left", len(k.m))
	}
}

func TestTicketsPerTokenAreCapped(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	k := newTickets()
	k.now = func() time.Time { return now }
	for range maxTicketsPerToken {
		if _, _, err := k.mint("tok1", "google"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := k.mint("tok1", "google"); !errors.Is(err, ErrTooManyTickets) {
		t.Fatalf("past the cap: %v", err)
	}
	if _, _, err := k.mint("tok2", "google"); err != nil {
		t.Fatalf("another token: %v", err)
	}
	// Once they expire, they're swept and the token can mint again.
	now = now.Add(TicketTTL)
	if _, _, err := k.mint("tok1", "google"); err != nil || len(k.m) != 1 {
		t.Fatalf("after expiry: %v, %d kept", err, len(k.m))
	}
}
