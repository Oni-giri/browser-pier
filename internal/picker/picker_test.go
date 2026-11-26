package picker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

func TestSizesAndTimes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 bytes", 999: "999 bytes", 1500: "2 KB", 640_000: "640 KB", 1_234_567: "1.2 MB", 23_400_000: "23 MB"} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{0: "less than a minute", 59 * time.Second: "less than a minute", time.Minute: "1 min", 181 * time.Second: "4 min", 5 * time.Minute: "5 min"} {
		if got := left(d); got != want {
			t.Errorf("left(%v) = %q, want %q", d, got, want)
		}
	}
	p := &Picker{now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
	for d, want := range map[time.Duration]string{10 * time.Second: "just now", 3 * time.Minute: "3 min ago", 5 * time.Hour: "5 h ago", 30 * time.Hour: "yesterday", 72 * time.Hour: "3 days ago"} {
		if got := p.ago(p.now().Add(-d)); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}

// holder is who holds control in the rig.
var holder = sessions.HandoffHolder("h1")

// rig is a running session whose stand-in browser serves ctl, with a
// person (holder) in control.
func rig(t *testing.T, ctl *http.ServeMux) (*sessions.Manager, sessions.Session) {
	t.Helper()
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "pk") // short, for the socket's path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, err := db.Open(ctx, filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	rt := sessionstest.New(dir)
	ctl.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	rt.Control = ctl
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if _, err := m.Profiles().Create(ctx, profiles.New("google")); err != nil {
		t.Fatal(err)
	}
	s, _, err := m.Start(ctx, "google", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.TakeControl(s.ID, holder); err != nil {
		t.Fatal(err)
	}
	return m, s
}

// TestUploadOver100MiB: a person's upload is capped at 100 MiB, as an
// agent's is (lowered here, so the test sends kilobytes, not 100 MiB).
func TestUploadOver100MiB(t *testing.T) {
	if MaxUpload != 100<<20 {
		t.Fatalf("MaxUpload is %d, want 100 MiB", MaxUpload)
	}
	ctl := http.NewServeMux()
	var answered atomic.Int32
	ctl.HandleFunc("POST /chooser/restart", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	ctl.HandleFunc("POST /chooser/{id}", func(w http.ResponseWriter, _ *http.Request) { answered.Add(1); w.WriteHeader(http.StatusNoContent) })
	ctl.HandleFunc("GET /chooser", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choosers":[{"id":"c1","multiple":false,"left_ms":1000}]}`)
	})
	m, s := rig(t, ctl)
	log := slog.New(slog.DiscardHandler)
	p := New(m, files.New(m.Layout().FilesDir, 0, os.Getuid(), os.Getgid()), log)
	p.max = 4096
	send := func(n int) int {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("id", "c1")
		part, _ := mw.CreateFormFile("file", "big.bin")
		_, _ = part.Write(bytes.Repeat([]byte("x"), n))
		_ = mw.Close()
		r := httptest.NewRequest(http.MethodPost, "/chooser/upload", &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		p.Upload(w, r, s, holder)
		return w.Code
	}
	if code := send(4097); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("just over: %d", code)
	}
	if code := send(2 << 20); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("far over: %d", code)
	}
	if answered.Load() != 0 {
		t.Fatal("a file over the cap answered the chooser")
	}
	if code := send(4096); code != http.StatusNoContent || answered.Load() != 1 {
		t.Fatalf("at the cap: %d, %d answers", code, answered.Load())
	}
	// The log says the person answered, as one of their control entries,
	// not an agent's action (PROJ-158).
	var noted bool
	for _, a := range m.Activity(s.ID) {
		if a.What == "A person chose 1 file" {
			noted = a.Kind == sessions.KindControl && a.Agent == ""
		}
	}
	if !noted {
		t.Errorf("no control entry for the answer: %+v", m.Activity(s.ID))
	}
}

// TestShowOnABrowserWithoutThePortal: a browser kept running through an
// update, on an image without the portal, shows no modal, quietly: the
// page asks every second.
func TestShowOnABrowserWithoutThePortal(t *testing.T) {
	m, s := rig(t, http.NewServeMux())
	var logs bytes.Buffer
	p := New(m, files.New(m.Layout().FilesDir, 0, os.Getuid(), os.Getgid()), slog.New(slog.NewTextHandler(&logs, nil)))
	for range 3 {
		w := httptest.NewRecorder()
		p.Show(w, httptest.NewRequest(http.MethodGet, "/chooser", nil), s, holder)
		if w.Code != http.StatusNoContent {
			t.Fatalf("Show: %d %s", w.Code, w.Body)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("logged: %s", logs.String())
	}
}

// TestOnlyTheHolderAnswers (PROJ-184): while one viewer holds control
// (the handoff link's phone here), another (the admin's live view) can't
// see, answer, cancel or upload to the chooser, and nothing it sends is
// stored or reaches the browser. Once control changes hands, it is the
// reverse.
func TestOnlyTheHolderAnswers(t *testing.T) {
	ctl := http.NewServeMux()
	var answered atomic.Int32
	ctl.HandleFunc("POST /chooser/restart", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	ctl.HandleFunc("POST /chooser/{id}", func(w http.ResponseWriter, _ *http.Request) { answered.Add(1); w.WriteHeader(http.StatusNoContent) })
	ctl.HandleFunc("GET /chooser", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choosers":[{"id":"c1","multiple":false,"left_ms":60000}]}`)
	})
	m, s := rig(t, ctl)
	fs := files.New(m.Layout().FilesDir, 0, os.Getuid(), os.Getgid())
	if _, _, err := fs.Upload(s.Profile, "cv.pdf", bytes.NewReader([]byte("%PDF")), 1<<20); err != nil {
		t.Fatal(err)
	}
	p := New(m, fs, slog.New(slog.DiscardHandler))
	phone, admin := holder, sessions.AdminHolder

	show := func(who string) int {
		w := httptest.NewRecorder()
		p.Show(w, httptest.NewRequest(http.MethodGet, "/chooser", nil), s, who)
		return w.Code
	}
	answer := func(who, form string) int {
		r := httptest.NewRequest(http.MethodPost, "/chooser", bytes.NewReader([]byte(form)))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		p.Answer(w, r, s, who)
		return w.Code
	}
	upload := func(who, name string) int {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("id", "c1")
		part, _ := mw.CreateFormFile("file", name)
		_, _ = part.Write([]byte("hello"))
		_ = mw.Close()
		r := httptest.NewRequest(http.MethodPost, "/chooser/upload", &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		p.Upload(w, r, s, who)
		return w.Code
	}
	refused := func(who, from string) {
		t.Helper()
		for what, code := range map[string]int{
			"show":   show(who),
			"answer": answer(who, "id=c1&file=cv.pdf"),
			"cancel": answer(who, "id=c1&cancel=1"),
			"upload": upload(who, from+".txt"),
		} {
			if code != http.StatusConflict {
				t.Errorf("%s from %q, not in control: %d, want 409", what, who, code)
			}
		}
		if answered.Load() != 0 {
			t.Fatalf("%q, not in control, answered the chooser", who)
		}
		if _, _, err := fs.Uploaded(s.Profile, from+".txt"); err == nil {
			t.Fatalf("%q, not in control, stored an upload", who)
		}
	}

	// The phone holds control: the admin's live view is refused.
	refused(admin, "admin")
	refused("", "nobody")
	if code := show(phone); code != http.StatusOK {
		t.Fatalf("the holder's show: %d", code)
	}

	// Control changes hands: now the phone is refused, the admin answers.
	m.ReleaseControl(s.ID)
	if err := m.TakeControl(s.ID, admin); err != nil {
		t.Fatal(err)
	}
	refused(phone, "phone")
	if code := show(admin); code != http.StatusOK {
		t.Fatalf("the admin's show: %d", code)
	}
	if code := answer(admin, "id=c1&file=cv.pdf"); code != http.StatusNoContent || answered.Load() != 1 {
		t.Fatalf("the admin's answer: %d, %d answers", code, answered.Load())
	}
	if code := upload(admin, "admin.txt"); code != http.StatusNoContent || answered.Load() != 2 {
		t.Fatalf("the admin's upload: %d, %d answers", code, answered.Load())
	}
}
