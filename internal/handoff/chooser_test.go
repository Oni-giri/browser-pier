package handoff

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// fakeChoosers is every browser's file chooser portal: one single-file
// chooser, c1, open in each. It records each answer by the control
// socket it came to, so a test knows which browser got it.
type fakeChoosers struct {
	mu      sync.Mutex
	answers map[string][]string // control socket path: bodies
}

func (f *fakeChoosers) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /chooser", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choosers":[{"id":"c1","multiple":false,"accept":[],"left_ms":240000}]}`)
	})
	mux.HandleFunc("POST /chooser/restart", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /chooser/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "c1" {
			http.Error(w, "no open file chooser with that id", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		sock := r.Context().Value(http.LocalAddrContextKey).(net.Addr).String()
		f.mu.Lock()
		if f.answers == nil {
			f.answers = map[string][]string{}
		}
		f.answers[sock] = append(f.answers[sock], string(b))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
}

func (f *fixture) answered(profile string) []string {
	f.choosers.mu.Lock()
	defer f.choosers.mu.Unlock()
	return f.choosers.answers[f.m.Layout().ControlSocket(profile)]
}

// putUpload puts a file in the profile's uploads, as an agent's upload would.
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

// upload sends a file from the phone, as the modal's Upload from this
// device does.
func (f *fixture) upload(c *http.Client, path, id, name string, content []byte, header http.Header) reply {
	f.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("id", id)
	part, _ := mw.CreateFormFile("file", name)
	_, _ = part.Write(content)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header.Get("Location"), string(b)}
}

// openLink opens a link-only handoff on the phone, which then holds it.
func (f *fixture) openLink(phone *http.Client, token string) {
	f.t.Helper()
	if p := f.do(phone, "GET", "/h/"+token, nil, nil); p.code != http.StatusSeeOther {
		f.t.Fatalf("opening the link: %d", p.code)
	}
}

// TestHandoffChooserIsTheLinksOwn (PROJ-181): the link's holder answers
// its own browser's file chooser, from its own profile's uploads, while
// it has control; never another profile's, and nothing once the link
// has ended or expired.
func TestHandoffChooserIsTheLinksOwn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	other := profiles.New("github")
	other.HandoffPolicy = profiles.PolicyLink
	if _, err := f.m.Profiles().Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	h, token := f.open("")
	gs, _, err := f.m.Start(ctx, "github", "")
	if err != nil {
		t.Fatal(err)
	}
	oh, otherToken, err := f.svc.Create(ctx, gs.ID, "research-bot", FileUpload, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f.putUpload("google", "cv.pdf", "%PDF google")
	f.putUpload("github", "secret.pdf", "%PDF github")

	phone := browser()
	f.openLink(phone, token)
	base := "/h/" + token + "/chooser"
	// Unlocked, but not driving yet (the live view takes control).
	if p := f.do(phone, "GET", base, nil, nil); p.code != http.StatusConflict {
		t.Fatalf("without control: %d %s", p.code, p.body)
	}
	if p := f.do(phone, "POST", base, url.Values{"id": {"c1"}, "file": {"cv.pdf"}}, nil); p.code != http.StatusConflict {
		t.Fatalf("an answer without control: %d %s", p.code, p.body)
	}
	if err := f.m.TakeControl(h.SessionID, sessions.HandoffHolder(h.ID)); err != nil {
		t.Fatal(err)
	}
	if err := f.m.TakeControl(gs.ID, sessions.HandoffHolder(oh.ID)); err != nil {
		t.Fatal(err)
	}
	p := f.do(phone, "GET", base, nil, nil)
	if p.code != http.StatusOK || !strings.Contains(p.body, "cv.pdf") || strings.Contains(p.body, "secret.pdf") {
		t.Fatalf("the modal: %d %s", p.code, p.body)
	}

	// The phone's cookie, sent to the other link's routes as a script
	// would (a browser keeps it to its own link's path), opens nothing.
	var cookie *http.Cookie
	u, _ := url.Parse(f.srv.URL + "/h/" + token)
	for _, c := range phone.Jar.Cookies(u) {
		if c.Name == browserCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no handoff cookie")
	}
	stolen := http.Header{"Cookie": {browserCookie + "=" + cookie.Value}}
	ob := "/h/" + otherToken + "/chooser"
	if p := f.do(&http.Client{}, "GET", ob, nil, stolen); p.code != http.StatusForbidden || strings.Contains(p.body, "secret.pdf") {
		t.Fatalf("another link's chooser: %d %s", p.code, p.body)
	}
	if p := f.do(&http.Client{}, "POST", ob, url.Values{"id": {"c1"}, "file": {"secret.pdf"}}, stolen); p.code != http.StatusForbidden {
		t.Fatalf("answering another link's chooser: %d", p.code)
	}
	if p := f.upload(&http.Client{}, ob+"/upload", "c1", "planted.pdf", []byte("x"), stolen); p.code != http.StatusForbidden {
		t.Fatalf("uploading into another profile: %d", p.code)
	}
	// Its own link's route can't reach the other profile's uploads.
	if p := f.do(phone, "POST", base, url.Values{"id": {"c1"}, "file": {"secret.pdf"}}, nil); p.code != http.StatusUnprocessableEntity {
		t.Fatalf("another profile's upload by name: %d", p.code)
	}
	if got := f.answered("github"); len(got) != 0 {
		t.Fatalf("the other browser was answered: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.m.Layout().FilesDir("github"), "uploads", "planted.pdf")); !os.IsNotExist(err) {
		t.Fatalf("a file reached the other profile's uploads: %v", err)
	}

	// Its own: an upload from the phone answers its own browser.
	if p := f.upload(phone, base+"/upload", "c1", "scan.jpg", []byte("jpeg"), nil); p.code != http.StatusNoContent {
		t.Fatalf("upload from this device: %d %s", p.code, p.body)
	}
	if got := f.answered("google"); len(got) != 1 || got[0] != `{"files":["scan.jpg"]}` {
		t.Fatalf("the browser got %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(f.m.Layout().FilesDir("google"), "uploads", "scan.jpg")); err != nil || string(b) != "jpeg" {
		t.Fatalf("the upload: %q %v", b, err)
	}
	// Over the files cap (1 MiB here): refused, nothing answered.
	if p := f.upload(phone, base+"/upload", "c1", "big.bin", bytes.Repeat([]byte("x"), 2<<20), nil); p.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the cap: %d %s", p.code, p.body)
	}
	if got := f.answered("google"); len(got) != 1 {
		t.Fatalf("an upload over the cap was answered: %q", got)
	}

	// Handed back: the link opens nothing more.
	hh, err := f.svc.Get(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Finish(ctx, hh, Done, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.m.TakeControl(h.SessionID, sessions.AdminHolder); err != nil { // someone in the live view now
		t.Fatal(err)
	}
	for _, p := range []reply{
		f.do(phone, "GET", base, nil, nil),
		f.do(phone, "POST", base, url.Values{"id": {"c1"}, "cancel": {"1"}}, nil),
		f.upload(phone, base+"/upload", "c1", "late.pdf", []byte("x"), nil),
	} {
		if p.code != http.StatusForbidden {
			t.Fatalf("after hand-back: %d %s", p.code, p.body)
		}
	}

	// Expired: the same.
	later := time.Now().Add(Lifetime + time.Second)
	f.svc.now = func() time.Time { return later }
	if p := f.do(&http.Client{}, "GET", ob, nil, http.Header{"Cookie": {browserCookie + "=x"}}); p.code != http.StatusForbidden {
		t.Fatalf("an expired link: %d", p.code)
	}
	if got := f.answered("google"); len(got) != 1 {
		t.Fatalf("answers after the link ended: %q", got)
	}
}

// TestHandoffChooserExpired: the holder's own routes stop at expiry.
func TestHandoffChooserExpired(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	h, token := f.open("")
	phone := browser()
	f.openLink(phone, token)
	if err := f.m.TakeControl(h.SessionID, sessions.HandoffHolder(h.ID)); err != nil {
		t.Fatal(err)
	}
	if p := f.do(phone, "GET", "/h/"+token+"/chooser", nil, nil); p.code != http.StatusOK {
		t.Fatalf("while open: %d %s", p.code, p.body)
	}
	later := time.Now().Add(Lifetime + time.Second)
	f.svc.now = func() time.Time { return later }
	if p := f.do(phone, "POST", "/h/"+token+"/chooser", url.Values{"id": {"c1"}, "cancel": {"1"}}, nil); p.code != http.StatusForbidden {
		t.Fatalf("after expiry: %d %s", p.code, p.body)
	}
	if got := f.answered("google"); len(got) != 0 {
		t.Fatalf("answered after expiry: %q", got)
	}
}

// TestHandoffChooserOnlyWhileTheLinkHasControl (PROJ-184): while the
// admin has control from the live view, the link's holder can't see,
// answer or upload to the chooser, though the link is open and unlocked;
// once the link has control again, it can.
func TestHandoffChooserOnlyWhileTheLinkHasControl(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	h, token := f.open("")
	f.putUpload("google", "cv.pdf", "%PDF google")
	phone := browser()
	f.openLink(phone, token)
	base := "/h/" + token + "/chooser"
	if err := f.m.TakeControl(h.SessionID, sessions.AdminHolder); err != nil {
		t.Fatal(err)
	}
	for what, p := range map[string]reply{
		"show":   f.do(phone, "GET", base, nil, nil),
		"answer": f.do(phone, "POST", base, url.Values{"id": {"c1"}, "file": {"cv.pdf"}}, nil),
		"cancel": f.do(phone, "POST", base, url.Values{"id": {"c1"}, "cancel": {"1"}}, nil),
		"upload": f.upload(phone, base+"/upload", "c1", "scan.jpg", []byte("jpeg"), nil),
	} {
		if p.code != http.StatusConflict {
			t.Errorf("%s while the admin has control: %d %s", what, p.code, p.body)
		}
	}
	if got := f.answered("google"); len(got) != 0 {
		t.Fatalf("the link answered while the admin had control: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.m.Layout().FilesDir("google"), "uploads", "scan.jpg")); !os.IsNotExist(err) {
		t.Fatalf("the link uploaded while the admin had control: %v", err)
	}

	f.m.ReleaseControl(h.SessionID)
	if err := f.m.TakeControl(h.SessionID, sessions.HandoffHolder(h.ID)); err != nil {
		t.Fatal(err)
	}
	if p := f.do(phone, "GET", base, nil, nil); p.code != http.StatusOK || !strings.Contains(p.body, "cv.pdf") {
		t.Fatalf("the link's own modal: %d %s", p.code, p.body)
	}
	if p := f.do(phone, "POST", base, url.Values{"id": {"c1"}, "file": {"cv.pdf"}}, nil); p.code != http.StatusNoContent {
		t.Fatalf("the link's answer: %d %s", p.code, p.body)
	}
	if got := f.answered("google"); len(got) != 1 || got[0] != `{"files":["cv.pdf"]}` {
		t.Fatalf("the browser got %q", got)
	}
}
