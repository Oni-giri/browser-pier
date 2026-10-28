package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestLockout(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := NewLockout(5, 15*time.Minute, 5*time.Minute)
	l.now = func() time.Time { return now }

	for want := 4; want > 0; want-- {
		if ok, _ := l.Check("admin"); !ok {
			t.Fatal("locked too early")
		}
		if left := l.Fail("admin"); left != want {
			t.Fatalf("left = %d, want %d", left, want)
		}
	}
	if left := l.Fail("admin"); left != 0 {
		t.Fatalf("fifth failure left %d", left)
	}
	if ok, retry := l.Check("admin"); ok || retry != 5*time.Minute {
		t.Fatalf("Check = %t, %s; want locked for 5m", ok, retry)
	}
	if ok, _ := l.Check("other"); !ok {
		t.Fatal("another key is locked too")
	}

	now = now.Add(5*time.Minute + time.Second)
	if ok, _ := l.Check("admin"); !ok {
		t.Fatal("still locked after the lockout")
	}
	if left := l.Fail("admin"); left != 4 {
		t.Fatalf("after a lockout the count starts over; left = %d", left)
	}

	// Failures older than the window don't count.
	now = now.Add(16 * time.Minute)
	if left := l.Fail("admin"); left != 4 {
		t.Fatalf("old failure still counted; left = %d", left)
	}
	l.Succeed("admin")
	if left := l.Fail("admin"); left != 4 {
		t.Fatalf("Succeed didn't reset; left = %d", left)
	}
}

func TestLockoutForgetsIdleKeys(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := NewLockout(5, time.Minute, time.Minute)
	l.now = func() time.Time { return now }
	for i := range 2000 {
		l.Fail(fmt.Sprint("10.0.0.", i))
	}
	now = now.Add(2 * time.Minute)
	l.Fail("fresh")
	if n := len(l.keys); n > 10 {
		t.Fatalf("%d keys kept after their failures expired", n)
	}
}
