package ui

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/notify"
)

// hook is a webhook that answers with status and keeps what it got.
type hook struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	sigs   []string
	bodies [][]byte
}

func newHook(t *testing.T, status int) *hook {
	h := &hook{status: status}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.sigs = append(h.sigs, r.Header.Get(notify.SignatureHeader))
		h.bodies = append(h.bodies, b)
		st := h.status
		h.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(h.Close)
	return h
}

// last is the last POST's signature and body.
func (h *hook) last() (string, []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sigs) == 0 {
		return "", nil
	}
	return h.sigs[len(h.sigs)-1], h.bodies[len(h.bodies)-1]
}

// signedWith reports whether the signature header was made with secret.
func signedWith(secret, header string, body []byte) bool {
	ts, _, _ := strings.Cut(strings.TrimPrefix(header, "t="), ",")
	var t int64
	for _, c := range ts {
		t = t*10 + int64(c-'0')
	}
	return header != "" && notify.Sign(secret, t, body) == header
}

var secretRE = regexp.MustCompile(`<code>([0-9a-f]{64})</code>`)

func shownSecret(body string) string {
	if m := secretRE.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

func TestNotificationsAreAdminOnly(t *testing.T) {
	f := newFixture(t)
	form := url.Values{"url": {"https://hooks.example.com/T0/secret"}, "event": {"handoff.requested"}}
	paths := []string{"/settings/notify", "/settings/notify/remove", "/settings/notify/secret", "/settings/notify/test"}
	// Cross-site, with the admin's cookie.
	for _, p := range paths {
		if r := f.post(p, form, "Sec-Fetch-Site", "cross-site"); r.code != http.StatusForbidden {
			t.Errorf("cross-site %s: %d", p, r.code)
		}
	}
	// An agent's token, with no cookie.
	_, tok, err := f.tokens.Create(context.Background(), "agent", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.client.Jar, _ = cookiejar.New(nil)
	for _, p := range paths {
		if r := f.post(p, form, "Authorization", "Bearer "+tok); r.code != http.StatusUnauthorized {
			t.Errorf("agent token %s: %d %s", p, r.code, r.location)
		}
		if r := f.post(p, form); r.code != http.StatusUnauthorized {
			t.Errorf("signed out %s: %d %s", p, r.code, r.location)
		}
	}
	if f.notify.View().Set {
		t.Error("a webhook was saved")
	}
}

func TestSavingAWebhookShowsItsSecretOnce(t *testing.T) {
	f := newFixture(t)
	h := newHook(t, http.StatusOK)
	r := f.post("/settings/notify", url.Values{"url": {h.URL + "/T0/B0/secretpath"}, "link_base": {"https://pier.lan"}, "event": {"handoff.requested", "browser.failed"}})
	want(t, r, http.StatusOK, "Signing secret", "see it again")
	first := shownSecret(r.body)
	if first == "" {
		t.Fatalf("no secret shown: %.2000s", r.body)
	}
	page := f.get("/settings")
	want(t, page, http.StatusOK, "Notifications", h.URL+"/…", `value="https://pier.lan"`)
	if strings.Contains(page.body, first) || strings.Contains(page.body, "secretpath") || shownSecret(page.body) != "" {
		t.Errorf("Settings shows the secret or the URL's path again")
	}
	v := f.notify.View()
	if !v.On[notify.HandoffRequested] || !v.On[notify.BrowserFailed] || v.On[notify.HandoffExpired] {
		t.Errorf("events: %v", v.On)
	}

	// Saving the masked URL again keeps it, and shows no secret.
	r = f.post("/settings/notify", url.Values{"url": {h.URL + "/…"}, "event": {"handoff.requested"}})
	if r.code != http.StatusSeeOther || shownSecret(r.body) != "" {
		t.Fatalf("saving again: %d", r.code)
	}
	if r := f.post("/settings/notify/test", nil); r.code != http.StatusSeeOther {
		t.Fatalf("test: %d", r.code)
	}
	want(t, f.get("/settings"), http.StatusOK, "Test sent: the webhook answered 200")
	if sig, body := h.last(); !signedWith(first, sig, body) {
		t.Error("the masked URL lost the webhook or its secret")
	}

	// A new URL keeps the secret.
	h2 := newHook(t, http.StatusOK)
	r = f.post("/settings/notify", url.Values{"url": {h2.URL + "/other"}, "event": {"handoff.requested"}})
	if r.code != http.StatusSeeOther || shownSecret(r.body) != "" {
		t.Fatalf("changing the URL: %d %s", r.code, shownSecret(r.body))
	}
	f.post("/settings/notify/test", nil)
	if sig, body := h2.last(); !signedWith(first, sig, body) {
		t.Error("changing the URL changed the secret")
	}

	// Regenerate shows a new one, once; the old one no longer verifies.
	r = f.post("/settings/notify/secret", nil)
	want(t, r, http.StatusOK, "Signing secret")
	next := shownSecret(r.body)
	if next == "" || next == first {
		t.Fatalf("regenerate showed %q", next)
	}
	f.post("/settings/notify/test", nil)
	if sig, body := h2.last(); signedWith(first, sig, body) || !signedWith(next, sig, body) {
		t.Error("after Regenerate, the signature isn't the new secret's")
	}
	if strings.Contains(f.get("/settings").body, next) {
		t.Error("the new secret is shown twice")
	}

	// Remove.
	if r := f.post("/settings/notify/remove", nil); r.code != http.StatusSeeOther {
		t.Fatalf("remove: %d", r.code)
	}
	if f.notify.View().Set {
		t.Error("still set after Remove")
	}
	want(t, f.get("/settings"), http.StatusOK, "Webhook removed", "Not set")
}

func TestARefusedWebhookSavesNothing(t *testing.T) {
	f := newFixture(t)
	for _, u := range []string{"http://hooks.example.com/secret-path", "https://user:pw@hooks.example.com/secret-path", "https://169.254.169.254/secret-path", "ftp://x/secret-path"} {
		r := f.post("/settings/notify", url.Values{"url": {u}})
		if r.code != http.StatusUnprocessableEntity || !strings.Contains(r.body, `class="errors"`) {
			t.Errorf("%s: %d", u, r.code)
		}
		if strings.Contains(r.body, "secret-path") || strings.Contains(r.body, "pw@") {
			t.Errorf("%s: the error echoes the URL", u)
		}
		if shownSecret(r.body) != "" || f.notify.View().Set {
			t.Errorf("%s was saved", u)
		}
	}
	r := f.post("/settings/notify", url.Values{"url": {"https://hooks.example.com/x"}, "link_base": {"https://pier.lan/?q=<b>"}})
	if r.code != http.StatusUnprocessableEntity || f.notify.View().Set {
		t.Errorf("a bad address for links: %d", r.code)
	}
}

func TestSendTestSaysWhatHappened(t *testing.T) {
	f := newFixture(t)
	f.post("/settings/notify/test", nil)
	want(t, f.get("/settings"), http.StatusOK, "No webhook is set")

	h := newHook(t, http.StatusTeapot)
	f.post("/settings/notify", url.Values{"url": {h.URL + "/x"}})
	f.post("/settings/notify/test", nil)
	page := f.get("/settings")
	want(t, page, http.StatusOK, "The webhook answered 418")
	_, body := h.last()
	if !strings.Contains(string(body), `"event":"test"`) {
		t.Errorf("sent %s", body)
	}

	h.Close()
	f.post("/settings/notify/test", nil)
	page = f.get("/settings")
	want(t, page, http.StatusOK, "The test didn")
	if strings.Contains(page.body, "/x\"") || strings.Contains(page.body, "Post \"") {
		t.Error("the error shows net/http's message")
	}
}
