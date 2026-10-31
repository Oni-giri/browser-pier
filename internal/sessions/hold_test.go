package sessions

import (
	"context"
	"errors"
	"testing"
)

func TestHoldKeepsTheBrowserFromStarting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	release, err := f.m.Hold("google")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Hold("google"); !errors.Is(err, ErrProfileHeld) {
		t.Fatalf("second hold: %v", err)
	}
	if _, _, err := f.m.Start(ctx, "google", ""); !errors.Is(err, ErrProfileHeld) {
		t.Fatalf("start while held: %v", err)
	}
	if err := f.m.DeleteProfile(ctx, "google"); !errors.Is(err, ErrProfileHeld) {
		t.Fatalf("delete while held: %v", err)
	}
	release()
	release() // twice is fine
	s := f.start(f.m)
	if _, err := f.m.Hold("google"); !errors.Is(err, ErrProfileBusy) {
		t.Fatalf("hold while running: %v", err)
	}
	if _, err := f.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
}
