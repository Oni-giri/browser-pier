package webrtc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/recording"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

// gateRecorder is a recorder that only keeps the gate's last answer.
type gateRecorder struct {
	mu    sync.Mutex
	allow map[string]bool
}

func (r *gateRecorder) Set(id, _ string, allow bool, _ recording.Reason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allow[id] = allow
}

func (r *gateRecorder) End(string) {}

func (r *gateRecorder) recording(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.allow[id]
}

// TestPersonsInputNeverReachesARecordedPage: every input the live view
// forwards to the browser arrives while the recorder is stopped.
func TestPersonsInputNeverReachesARecordedPage(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "rv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, err := db.Open(ctx, filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.DiscardHandler)
	rt := sessionstest.New(dir)
	rec := &gateRecorder{allow: map[string]bool{}}
	var sessID string
	var mu sync.Mutex
	posts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	for _, path := range []string{"POST /input/events", "POST /input/type"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			posts++
			id := sessID
			mu.Unlock()
			if rec.recording(id) {
				t.Errorf("%s reached the browser while it was being recorded", r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	rt.Control = mux
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	m.SetRecorder(rec, nil)
	p := profiles.New("google")
	p.Record = true
	if _, err := m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	s, _, err := m.Start(ctx, "google", "")
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	sessID = s.ID
	mu.Unlock()
	h := NewHandler(nil, m, nil, log)
	v := &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: m.Layout().ControlSocket("google")}
	input := []byte(`{"type": "input", "events": [{"t": "keydown", "k": "a"}, {"t": "keyup", "k": "a"}]}`)
	text := []byte(`{"type": "text", "text": "hunter2"}`)
	reply := func(string) {}

	agentActs := func() {
		t.Helper()
		_, release, err := m.Attach(ctx, s.ID, "tok", "claude")
		if err != nil {
			t.Fatal(err)
		}
		release() // its tail keeps the recording going, and lets a person take control
		if !rec.recording(s.ID) {
			t.Fatal("not recording after the agent acted")
		}
	}
	agentActs()
	// Without control, nothing goes through.
	v.message(input, reply)
	v.message(text, reply)
	v.message([]byte(`{"type": "take"}`), reply)
	if !v.control {
		t.Fatal("take control failed")
	}
	v.message(input, reply)
	v.message(text, reply)
	v.message([]byte(`{"type": "release"}`), reply)
	agentActs()
	v.message(input, reply) // control was handed back: refused
	v.message([]byte(`{"type": "take"}`), reply)
	v.message(text, reply)
	v.handBack()
	mu.Lock()
	defer mu.Unlock()
	if posts != 3 {
		t.Fatalf("%d inputs reached the browser, want the 3 sent with control", posts)
	}
}

// TestLiveStatusSaysWhetherTheProfileRecords: the live view's status
// carries the Record sessions setting as the running browser applies
// it, so an open live view's hint follows a change made elsewhere.
func TestLiveStatusSaysWhetherTheProfileRecords(t *testing.T) {
	h, m, _ := offerFixture(t)
	ctx := context.Background()
	record := func() bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/live/google/status", nil)
		req.SetPathValue("profile", "google")
		w := httptest.NewRecorder()
		h.status(w, req)
		var out struct {
			Record *bool `json:"record"`
		}
		if err := json.NewDecoder(w.Body).Decode(&out); err != nil || out.Record == nil {
			t.Fatalf("status %s: %v", w.Body, err)
		}
		return *out.Record
	}
	if record() {
		t.Fatal("record before the setting is on")
	}
	p, _ := m.Profiles().Get(ctx, "google")
	p.Record = true
	if _, err := m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := m.RefreshRecording(ctx, "google"); err != nil {
		t.Fatal(err)
	}
	if !record() {
		t.Fatal("the status doesn't follow the setting")
	}
}
