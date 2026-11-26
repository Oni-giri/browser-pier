//go:build docker

package sessions

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestIdleStopSaysWhy (PROJ-158): auto-stop stops a real browser, and its
// kept run says it was idle. The clock is moved on rather than waited for.
func TestIdleStopSaysWhy(t *testing.T) {
	f := newRealFixture(t)
	m := f.manager(f.layout)
	var shift atomic.Int64
	m.now = func() time.Time { return time.Now().Add(time.Duration(shift.Load())).Truncate(time.Millisecond) }
	name := f.profile(m)
	p, err := m.Profiles().Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	p.AutoStop = 5 * time.Minute
	if _, err := m.Profiles().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	s := startRunning(t, m, name)
	m.Note(s.ID, "claude", "Clicked at 1, 2")
	shift.Store(int64(6 * time.Minute))
	m.stopIdle(context.Background())
	runs := m.Runs(name)
	if len(runs) != 1 || runs[0].SessionID != s.ID || runs[0].End != EndIdle || runs[0].EndText != "Stopped: idle for 5 min" ||
		runs[0].Entries[0].What != "Stopped: idle for 5 min" || runs[0].Entries[1].What != "Clicked at 1, 2" {
		t.Fatalf("kept %+v", runs)
	}
	if got, _ := m.Get(context.Background(), s.ID); got.State != Stopped {
		t.Fatalf("ended %s, want a clean stop", got.State)
	}
}
