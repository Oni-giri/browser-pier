package handoff

import (
	"context"
	"net/http"
	"net/url"
	"strings"
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

// TestTheHandoffPageSaysRecordingIsPaused: for a profile that records,
// the line is in the hint under the stream, inside the viewer, so full
// screen keeps it (PROJ-193), and live.js keeps it as it rewrites the
// hint; for one that doesn't, there is no line.
func TestTheHandoffPageSaysRecordingIsPaused(t *testing.T) {
	const line = "Recording is paused while you have control."
	for _, record := range []bool{true, false} {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		ctx := context.Background()
		p, _ := f.m.Profiles().Get(ctx, "google")
		p.Record = record
		if _, err := f.m.Profiles().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
		_, token := f.open("")
		phone := browser()
		if r := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); r.code != http.StatusSeeOther {
			t.Fatalf("right PIN: %d %s", r.code, r.body)
		}
		r := f.do(phone, "GET", "/h/"+token, nil, nil)
		if r.code != http.StatusOK {
			t.Fatalf("control page: %d", r.code)
		}
		viewer := between(r.body, "data-viewer", `class="ho"`)
		hint := `<p id="hint" class="caption ho-hint" data-rec="` + line + `">` + line + `</p>`
		if record && (!strings.Contains(viewer, hint) || strings.Count(r.body, line) != 2) {
			t.Errorf("a recording profile's handoff page doesn't say so in the viewer's hint, once: %s", viewer)
		}
		if !record && strings.Contains(r.body, line) {
			t.Error("a profile that doesn't record says recording is paused")
		}
	}
}
