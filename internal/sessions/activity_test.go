package sessions

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestActivityLog(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	if got := f.m.Activity(s.ID); len(got) != 0 {
		t.Fatalf("a new session has activity %+v", got)
	}
	for i := range maxActivity + 5 {
		f.m.Note(s.ID, "claude", fmt.Sprintf("step %d", i))
		advance(time.Second)
	}
	f.m.NoteAttention(context.Background(), s.ID, "claude", "Paused, waiting for you: CAPTCHA")
	got := f.m.Activity(s.ID)
	if len(got) != maxActivity {
		t.Fatalf("%d entries, want the last %d", len(got), maxActivity)
	}
	if !got[0].Attention || got[0].What != "Paused, waiting for you: CAPTCHA" || got[1].Attention || got[1].What != fmt.Sprintf("step %d", maxActivity+4) {
		t.Fatalf("newest first: %+v, %+v", got[0], got[1])
	}
	if last := got[len(got)-1]; last.What != "step 6" || last.Agent != "claude" || !last.At.Before(got[0].At) {
		t.Fatalf("oldest kept: %+v", last)
	}
	got[0].What = "changed"
	if f.m.Activity(s.ID)[0].What == "changed" {
		t.Fatal("Activity returned the log itself, not a copy")
	}

	// The log goes with the session.
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.m.Activity(s.ID); got != nil {
		t.Fatalf("a stopped session still has activity %+v", got)
	}
	f.m.Note(s.ID, "claude", "too late")
	s2 := f.start(f.m)
	if got := f.m.Activity(s2.ID); len(got) != 0 {
		t.Fatalf("the next session inherited activity %+v", got)
	}
}
