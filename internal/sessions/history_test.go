package sessions

import (
	"context"
	"testing"
)

func TestLastSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if last, err := f.m.LastSessions(ctx); err != nil || len(last) != 0 {
		t.Fatalf("before any run: %+v, %v", last, err)
	}
	first := f.start(f.m)
	if _, err := f.m.Stop(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	last, err := f.m.LastSessions(ctx)
	if err != nil || last["google"].ID != first.ID || last["google"].State != Stopped {
		t.Fatalf("after a stop: %+v, %v", last, err)
	}
	second := f.start(f.m)
	last, _ = f.m.LastSessions(ctx)
	if last["google"].ID != second.ID || last["google"].State != Running {
		t.Fatalf("while running again: %+v", last["google"])
	}
}
