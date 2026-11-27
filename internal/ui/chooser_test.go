package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

// fakeChooser is the browser's file chooser portal: open is what GET
// /chooser lists; answers what POST /chooser/{id} was sent.
type fakeChooser struct {
	mu      sync.Mutex
	open    string
	answers []string
	// inputs is what the live view sent the browser as mouse and keys.
	inputs []string
}

func (c *fakeChooser) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /chooser", func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, _ = io.WriteString(w, `{"choosers":`+c.open+`}`)
	})
	mux.HandleFunc("POST /input/events", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.inputs = append(c.inputs, string(b))
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /input/release", sessionstest.AnswerLift) // a hand-back's lift fences the takeover
	mux.HandleFunc("POST /chooser/restart", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /chooser/{id}", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !strings.Contains(c.open, `"`+r.PathValue("id")+`"`) {
			http.Error(w, "no open file chooser with that id", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		c.answers = append(c.answers, string(b))
		c.open = `[]`
		w.WriteHeader(http.StatusNoContent)
	})
}

func (c *fakeChooser) set(open string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.open, c.answers = open, nil
}

func (c *fakeChooser) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.answers...)
}

// filesJSON is the browser's answer naming files, as the orchestrator
// encodes it.
func filesJSON(names ...string) string {
	b, _ := json.Marshal(map[string][]string{"files": names})
	return string(b)
}

func (f *fixture) putUpload(profile, name, content string) {
	f.t.Helper()
	dir := filepath.Join(f.m.Layout().FilesDir(profile), "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) uploadFrom(path, id, name string, content []byte) reply {
	f.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("id", id)
	part, _ := mw.CreateFormFile("file", name)
	_, _ = part.Write(content)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header.Get("Location"), string(b), resp.Header}
}

// TestLiveViewChooser (PROJ-181): the live view's file chooser modal and
// its answers, only while the person has control; names escaped; the
// log says what was done without a name.
func TestLiveViewChooser(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	s, _, err := f.m.Start(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	f.choosers.set(`[{"id":"c1","multiple":false,"accept":["*.pdf"],"left_ms":181000}]`)
	marked := `<img src=x onerror=alert(1)>.pdf`
	f.putUpload("google", marked, "%PDF")
	f.putUpload("google", "cv.pdf", "%PDF cv")
	base := "/profiles/google/chooser"

	// The page has the modal's place and its route.
	want(t, f.get("/profiles/google"), http.StatusOK, `data-chooser="/profiles/google/chooser"`, `<dialog id="chooser"`)

	// Without control, nothing: not the modal, not an answer, not an upload.
	want(t, f.get(base), http.StatusConflict, "Take control")
	want(t, f.post(base, url.Values{"id": {"c1"}, "file": {"cv.pdf"}}), http.StatusConflict)
	want(t, f.uploadFrom(base+"/upload", "c1", "x.pdf", []byte("x")), http.StatusConflict)
	if got := f.choosers.got(); len(got) != 0 {
		t.Fatalf("answered without control: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.m.Layout().FilesDir("google"), "uploads", "x.pdf")); !os.IsNotExist(err) {
		t.Fatalf("uploaded without control: %v", err)
	}

	if err := f.m.TakeControl(s.ID, f.holder); err != nil {
		t.Fatal(err)
	}
	r := f.get(base)
	want(t, r, http.StatusOK, `data-id="c1"`, `type="radio" name="file" value="cv.pdf"`, "Choose a file for the page", "closes in 4 min", "Upload from this device", "Cancel")
	if strings.Contains(r.body, "<img") || !strings.Contains(r.body, "&lt;img src=x onerror=alert(1)&gt;.pdf") || strings.Contains(r.body, "*.pdf") {
		t.Fatalf("names aren't escaped, or the page's accept list shows:\n%s", r.body)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type %q", ct)
	}

	// Refused before reaching the browser: a path, a missing upload.
	want(t, f.post(base, url.Values{"id": {"c1"}, "file": {"../downloads/x.pdf"}}), http.StatusBadRequest)
	want(t, f.post(base, url.Values{"id": {"c1"}, "file": {"gone.pdf"}}), http.StatusUnprocessableEntity)
	want(t, f.post(base, url.Values{"id": {"c1"}}), http.StatusBadRequest)
	if got := f.choosers.got(); len(got) != 0 {
		t.Fatalf("the browser was asked: %q", got)
	}

	want(t, f.post(base, url.Values{"id": {"c1"}, "file": {marked}}), http.StatusNoContent)
	if got := f.choosers.got(); len(got) != 1 || got[0] != filesJSON(marked) {
		t.Fatalf("the browser got %q", got)
	}
	// It closed: the modal goes, a late answer says so.
	want(t, f.get(base), http.StatusNoContent)
	want(t, f.post(base, url.Values{"id": {"c1"}, "cancel": {"1"}}), http.StatusConflict, "closed")

	// Upload from this device: stored in the uploads, then the answer.
	f.choosers.set(`[{"id":"c2","multiple":true,"accept":[],"left_ms":300000}]`)
	want(t, f.get(base), http.StatusOK, `type="checkbox"`, "Choose files for the page", "closes in 5 min")
	want(t, f.uploadFrom(base+"/upload", "c2", "photo <b>.jpg", []byte("jpeg")), http.StatusNoContent)
	if got := f.choosers.got(); len(got) != 1 || got[0] != filesJSON("photo <b>.jpg") {
		t.Fatalf("the browser got %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(f.m.Layout().FilesDir("google"), "uploads", "photo <b>.jpg")); err != nil || string(b) != "jpeg" {
		t.Fatalf("the upload: %q %v", b, err)
	}
	// Over the files cap (1 MiB here), or with a name the files can't
	// have: refused, and the chooser stays open.
	f.choosers.set(`[{"id":"c3","multiple":false,"accept":[],"left_ms":300000}]`)
	want(t, f.uploadFrom(base+"/upload", "c3", "big.bin", bytes.Repeat([]byte("x"), 2<<20)), http.StatusRequestEntityTooLarge, "too large")
	want(t, f.uploadFrom(base+"/upload", "c3", ".hidden", []byte("x")), http.StatusBadRequest, "rename it")
	if got := f.choosers.got(); len(got) != 0 {
		t.Fatalf("a refused upload was answered: %q", got)
	}
	want(t, f.post(base, url.Values{"id": {"c3"}, "cancel": {"1"}}), http.StatusNoContent)
	if got := f.choosers.got(); len(got) != 1 || got[0] != `{"cancel":true}` {
		t.Fatalf("the cancel: %q", got)
	}

	// The activity log: what the person did, never what they chose.
	var log []string
	for _, a := range f.m.Activity(s.ID) {
		log = append(log, a.What)
	}
	all := strings.Join(log, "\n")
	if strings.Count(all, "A person chose 1 file") != 2 || !strings.Contains(all, "Cancelled a file chooser") {
		t.Fatalf("activity: %q", log)
	}
	for _, name := range []string{"img", "photo", ".pdf", ".jpg", "big.bin"} {
		if strings.Contains(all, name) {
			t.Fatalf("the activity log names a file (%s): %q", name, log)
		}
	}

	// Handed back: refused again.
	f.m.ReleaseControl(s.ID)
	f.choosers.set(`[{"id":"c4","multiple":false,"accept":[],"left_ms":300000}]`)
	want(t, f.get(base), http.StatusConflict)
	want(t, f.post(base, url.Values{"id": {"c4"}, "file": {"cv.pdf"}}), http.StatusConflict)
	if got := f.choosers.got(); len(got) != 0 {
		t.Fatalf("answered after hand-back: %q", got)
	}
}

// TestLiveViewChooserOnlyForTheHolder (PROJ-184): while a handoff link's
// holder has control, the admin's live view can't see, answer or upload
// to the chooser, and nothing it sends is stored or reaches the browser.
func TestLiveViewChooserOnlyForTheHolder(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	s, _, err := f.m.Start(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	f.choosers.set(`[{"id":"c1","multiple":false,"accept":[],"left_ms":300000}]`)
	f.putUpload("google", "cv.pdf", "%PDF cv")
	base := "/profiles/google/chooser"
	refused := func(why string) {
		t.Helper()
		want(t, f.get(base), http.StatusConflict, "Take control")
		want(t, f.post(base, url.Values{"id": {"c1"}, "file": {"cv.pdf"}}), http.StatusConflict)
		want(t, f.post(base, url.Values{"id": {"c1"}, "cancel": {"1"}}), http.StatusConflict)
		want(t, f.uploadFrom(base+"/upload", "c1", "x.pdf", []byte("x")), http.StatusConflict)
		if got := f.choosers.got(); len(got) != 0 {
			t.Fatalf("answered %s: %q", why, got)
		}
		if _, err := os.Stat(filepath.Join(f.m.Layout().FilesDir("google"), "uploads", "x.pdf")); !os.IsNotExist(err) {
			t.Fatalf("uploaded %s: %v", why, err)
		}
	}

	// The link's phone drives.
	if err := f.m.TakeControl(s.ID, sessions.HandoffHolder("h1")); err != nil {
		t.Fatal(err)
	}
	refused("while the handoff link has control")

	// The admin's live view takes control: it answers.
	f.m.ReleaseControl(s.ID)
	if err := f.m.TakeControl(s.ID, f.holder); err != nil {
		t.Fatal(err)
	}
	want(t, f.get(base), http.StatusOK, `data-id="c1"`)
	want(t, f.post(base, url.Values{"id": {"c1"}, "file": {"cv.pdf"}}), http.StatusNoContent)
	if got := f.choosers.got(); len(got) != 1 || got[0] != filesJSON("cv.pdf") {
		t.Fatalf("the browser got %q", got)
	}

	// Signed out and in again (in another tab) while the live view keeps
	// control: its choosers still answer (review N2).
	u, _ := url.Parse(f.srv.URL)
	var old string
	for _, c := range f.client.Jar.Cookies(u) {
		if c.Name == auth.CookieName {
			old = c.Value
		}
	}
	if err := f.admin.EndSession(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	cookie, _, err := f.admin.NewSession(context.Background(), false, "test")
	if err != nil {
		t.Fatal(err)
	}
	f.client.Jar, _ = cookiejar.New(nil)
	f.client.Jar.SetCookies(u, []*http.Cookie{{Name: auth.CookieName, Value: cookie, Path: "/"}})
	f.choosers.set(`[{"id":"c2","multiple":false,"accept":[],"left_ms":300000}]`)
	want(t, f.get(base), http.StatusOK, `data-id="c2"`)
	want(t, f.post(base, url.Values{"id": {"c2"}, "cancel": {"1"}}), http.StatusNoContent)
}

// An upload names a chooser that is open, or nothing is stored.
func TestChooserUploadNeedsAnOpenChooser(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	s, _, err := f.m.Start(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.TakeControl(s.ID, f.holder); err != nil {
		t.Fatal(err)
	}
	f.choosers.set(`[{"id":"c1","multiple":false,"accept":[],"left_ms":300000}]`)
	want(t, f.uploadFrom("/profiles/google/chooser/upload", "c9", "x.pdf", []byte("x")), http.StatusConflict, "closed")
	if _, err := os.Stat(filepath.Join(f.m.Layout().FilesDir("google"), "uploads", "x.pdf")); !os.IsNotExist(err) {
		t.Fatalf("stored with no chooser open: %v", err)
	}
	if got := f.choosers.got(); len(got) != 0 {
		t.Fatalf("answered: %q", got)
	}
}

// keys is the key events the live view sent the browser.
func (c *fakeChooser) keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, in := range c.inputs {
		if strings.Contains(in, `"key`) {
			out = append(out, in)
		}
	}
	return out
}
