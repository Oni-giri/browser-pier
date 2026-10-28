package sessions

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestScreenshotPreviewDoesNotCountAsUse(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /screenshot", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("width") == "4000" {
			_, _ = w.Write(make([]byte, maxPreview+1))
			return
		}
		_, _ = io.WriteString(w, "PNG:"+r.URL.Query().Get("width"))
	})
	f.rt.Control = mux

	if _, err := f.m.Screenshot(ctx, "google", 320); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("with no browser: %v, want ErrNotRunning", err)
	}
	advance := f.clock()
	s := f.start(f.m)
	idleSince := func() time.Time {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		return f.m.live["google"].lastActivity
	}
	before := idleSince()
	advance(10 * time.Minute)
	png, err := f.m.Screenshot(ctx, "google", 320)
	if err != nil || string(png) != "PNG:320" {
		t.Fatalf("Screenshot = %q, %v", png, err)
	}
	if png, err := f.m.Screenshot(ctx, "google", 0); err != nil || string(png) != "PNG:" {
		t.Fatalf("full size = %q, %v", png, err)
	}
	if got := idleSince(); !got.Equal(before) {
		t.Fatalf("a preview reset the idle clock from %v to %v", before, got)
	}
	if _, err := f.m.Screenshot(ctx, "google", 8); err == nil {
		t.Fatal("a width under 16 was accepted")
	}
	if _, err := f.m.Screenshot(ctx, "google", 4000); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("an oversized screenshot = %v", err)
	}

	// A person has it during a handoff: still previewed.
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Screenshot(ctx, "google", 320); err != nil {
		t.Fatalf("during a handoff: %v", err)
	}
}

func TestMemoryReport(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const gib = 1 << 30
	if r := f.m.Memory(ctx); r.Limit != 0 || r.Room() != -1 {
		t.Fatalf("with no limit: %+v, room %d", r, r.Room())
	}
	f.rt.Memory = gib / 2
	f.m.SetMemoryLimit(4*gib, gib)
	if f.m.MemoryLimit() != 4*gib {
		t.Fatalf("MemoryLimit = %d", f.m.MemoryLimit())
	}
	f.start(f.m)
	r := f.m.Memory(ctx)
	if r.Limit != 4*gib || r.Each != gib || r.Used != gib/2 || len(r.Browsers) != 1 || r.Browsers[0].Profile != "google" {
		t.Fatalf("report %+v", r)
	}
	if r.Room() != 3 { // 3.5 GB left, 1 GB each
		t.Fatalf("room %d, want 3", r.Room())
	}
	// Lowered while running: nothing is stopped, but there's no more room.
	f.m.SetMemoryLimit(gib/4, gib)
	if r := f.m.Memory(ctx); r.Room() != 0 || len(r.Browsers) != 1 {
		t.Fatalf("over the limit: %+v, room %d", r, r.Room())
	}
}

func TestSetMemoryLimitWhileStarting(t *testing.T) {
	f := newFixture(t)
	const gib = 1 << 30
	f.rt.Memory = gib
	f.profile("other", 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			f.m.SetMemoryLimit(int64(8+i%2)*gib, gib)
			_ = f.m.Memory(context.Background())
		}
	}()
	f.start(f.m)
	f.startProfile("other")
	<-done
}
