package handoff

import (
	"context"
	"sync"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/recording"
)

type gateRecorder struct {
	mu    sync.Mutex
	allow map[string]bool
	why   map[string]recording.Reason
}

func (r *gateRecorder) Set(id, _ string, allow bool, why recording.Reason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allow[id], r.why[id] = allow, why
}

func (r *gateRecorder) End(string) {}

func (r *gateRecorder) state(id string) (bool, recording.Reason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.allow[id], r.why[id]
}

// TestAHandoffPausesRecordingFromItsOpening: recording stops as the
// handoff opens, before anyone follows the link, and after it ends waits
// for the agent's next action.
func TestAHandoffPausesRecordingFromItsOpening(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	ctx := context.Background()
	rec := &gateRecorder{allow: map[string]bool{}, why: map[string]recording.Reason{}}
	f.m.SetRecorder(rec, nil)
	p, _ := f.m.Profiles().Get(ctx, "google")
	p.Record = true
	if _, err := f.m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	s := f.running()
	_, release, err := f.m.Attach(ctx, s.ID, "tok", "research-bot")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := rec.state(s.ID); !ok {
		t.Fatal("not recording while the agent is connected")
	}
	h, _, err := f.svc.Create(ctx, s.ID, "research-bot", SensitiveInput, "Enter the 2FA code", "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if ok, why := rec.state(s.ID); ok || why != recording.Handoff {
		t.Fatalf("recording %v (%s) with the handoff open and nobody at the link yet", ok, why)
	}
	if _, err := f.svc.Finish(ctx, h, Done, ""); err != nil {
		t.Fatal(err)
	}
	if ok, why := rec.state(s.ID); ok || why != recording.Idle {
		t.Fatalf("recording %v (%s) after hand-back, before the agent acted", ok, why)
	}
	lease, err := f.m.PageLease(ctx, s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if ok, _ := rec.state(s.ID); !ok {
		t.Fatal("not recording once the agent acted again")
	}
}
