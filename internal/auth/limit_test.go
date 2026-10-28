package auth

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func fail(t *testing.T, l *Lockout, key string) int {
	t.Helper()
	a, wait := l.Begin(key)
	if a == nil {
		t.Fatalf("Begin(%q) refused for %s", key, wait)
	}
	return a.Fail()
}

func TestLockout(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := NewLockout(5, 15*time.Minute, 5*time.Minute)
	l.now = func() time.Time { return now }

	for want := 4; want > 0; want-- {
		if left := fail(t, l, "admin"); left != want {
			t.Fatalf("left = %d, want %d", left, want)
		}
	}
	if left := fail(t, l, "admin"); left != 0 {
		t.Fatalf("fifth failure left %d", left)
	}
	if a, wait := l.Begin("admin"); a != nil || wait != 5*time.Minute {
		t.Fatalf("Begin while locked = %v, %s; want refused for 5m", a, wait)
	}
	if a, _ := l.Begin("other"); a == nil {
		t.Fatal("another key is locked too")
	} else {
		a.Cancel()
	}

	now = now.Add(5*time.Minute + time.Second)
	if left := fail(t, l, "admin"); left != 4 {
		t.Fatalf("after a lockout the count starts over; left = %d", left)
	}

	// Failures older than the window don't count.
	now = now.Add(16 * time.Minute)
	if left := fail(t, l, "admin"); left != 4 {
		t.Fatalf("old failure still counted; left = %d", left)
	}
	a, _ := l.Begin("admin")
	a.Succeed()
	if left := fail(t, l, "admin"); left != 4 {
		t.Fatalf("Succeed didn't reset; left = %d", left)
	}
}

func TestLockoutCountsAttemptsInFlight(t *testing.T) {
	l := NewLockout(5, time.Minute, time.Minute)
	// 50 guesses sent at once must not all get to check the password.
	var mu sync.Mutex
	var begun []*Attempt
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a, _ := l.Begin("admin"); a != nil {
				mu.Lock()
				begun = append(begun, a)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(begun) != 5 {
		t.Fatalf("%d attempts got through at once, want 5", len(begun))
	}
	for _, a := range begun {
		a.Fail()
	}
	if a, _ := l.Begin("admin"); a != nil {
		t.Fatal("not locked after 5 failures")
	}

	// A cancelled attempt doesn't count.
	l2 := NewLockout(2, time.Minute, time.Minute)
	for range 10 {
		a, _ := l2.Begin("k")
		a.Cancel()
	}
	if left := fail(t, l2, "k"); left != 1 {
		t.Fatalf("left = %d after cancelled attempts", left)
	}
}

func TestLockoutForgetsIdleKeys(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := NewLockout(5, time.Minute, time.Minute)
	l.now = func() time.Time { return now }
	for i := range 2000 {
		fail(t, l, fmt.Sprint("10.0.0.", i))
	}
	now = now.Add(2 * time.Minute)
	fail(t, l, "fresh")
	if n := len(l.keys); n > 10 {
		t.Fatalf("%d keys kept after their failures expired", n)
	}
}
