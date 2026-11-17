package ui

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/archive"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

const stickyProxy = "http://pier-{sticky}:s3cret@proxy.example:8080"

// stickyProfile makes a profile whose proxy is a {sticky} template, and
// returns its token.
func (f *fixture) stickyProfile(name string) string {
	f.t.Helper()
	p := profiles.New(name)
	p.Proxy = stickyProxy
	created, err := f.m.Profiles().Create(context.Background(), p)
	if err != nil {
		f.t.Fatal(err)
	}
	return created.StickyToken
}

func (f *fixture) token(name string) string {
	f.t.Helper()
	p, err := f.m.Profiles().Get(context.Background(), name)
	if err != nil {
		f.t.Fatal(err)
	}
	return p.StickyToken
}

// TestStickyProxyForm: the form shows the template masked, never the
// token; saving it unchanged keeps the stored URL and the token.
func TestStickyProxyForm(t *testing.T) {
	f := newFixture(t)
	tok := f.stickyProfile("bank")
	edit := f.get("/profiles/bank/edit")
	want(t, edit, http.StatusOK, `value="http://pier-{sticky}:••••@proxy.example:8080"`, "<code>{sticky}</code> in the user name or password")
	if strings.Contains(edit.body, tok) || strings.Contains(edit.body, "s3cret") {
		t.Fatal("the edit page shows the token or the password")
	}
	r := f.post("/profiles/bank/edit", url.Values{"policy": {"pin"}, "proxy": {"http://pier-{sticky}:••••@proxy.example:8080"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", r.code, r.body)
	}
	p, _ := f.m.Profiles().Get(context.Background(), "bank")
	if p.Proxy != stickyProxy || p.StickyToken != tok {
		t.Fatalf("after saving unchanged: proxy %q, token %q (was %q)", p.Proxy, p.StickyToken, tok)
	}
	// Another template, then none, then one again: the token stays.
	for _, proxy := range []string{"socks5://u-{sticky}:pw@other.example:1080", "", stickyProxy} {
		if r := f.post("/profiles/bank/edit", url.Values{"policy": {"pin"}, "proxy": {proxy}}); r.code != http.StatusSeeOther {
			t.Fatalf("save %q: %d %s", proxy, r.code, r.body)
		}
		if got := f.token("bank"); got != tok {
			t.Fatalf("saving %q changed the token to %q", proxy, got)
		}
	}
	// A placeholder outside the user info is refused, without echoing it.
	r = f.post("/profiles/bank/edit", url.Values{"policy": {"pin"}, "proxy": {"http://u:s3cret@{sticky}.example:8080"}})
	want(t, r, http.StatusUnprocessableEntity, "only in the user name or password")

	// Settings take a template too, and show it masked.
	if r := f.post("/settings", url.Values{"proxy": {stickyProxy}}); r.code != http.StatusSeeOther {
		t.Fatalf("settings: %d %s", r.code, r.body)
	}
	set := f.get("/settings")
	want(t, set, http.StatusOK, `value="http://pier-{sticky}:••••@proxy.example:8080"`, "<code>{sticky}</code>")
	if strings.Contains(set.body, "s3cret") {
		t.Fatal("settings show the password")
	}
}

// TestNewExitIP: only the admin, only while stopped, only for a {sticky}
// proxy, and the menu offers it only then.
func TestNewExitIP(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tok := f.stickyProfile("bank")
	f.profile("plain")

	list := f.get("/profiles")
	want(t, list, http.StatusOK, `data-dialog="exit-dialog" data-name="bank"`, `id="exit-dialog"`, "/profiles/{name}/new-exit")
	if strings.Contains(list.body, `data-dialog="exit-dialog" data-name="plain"`) {
		t.Fatal("the menu offers a new exit for a proxy without {sticky}")
	}
	want(t, f.get("/profiles/bank"), http.StatusOK, `data-dialog="exit-dialog" data-name="bank"`)
	if strings.Contains(f.get("/profiles/plain").body, `data-dialog="exit-dialog"`) {
		t.Fatal("the profile page offers a new exit for a proxy without {sticky}")
	}

	r := f.post("/profiles/bank/new-exit", url.Values{"back": {"/profiles/bank"}})
	if r.code != http.StatusSeeOther || r.location != "/profiles/bank" {
		t.Fatalf("new exit: %d %s", r.code, r.location)
	}
	newTok := f.token("bank")
	if newTok == tok || newTok == "" {
		t.Fatalf("the token is still %q", newTok)
	}
	if p, _ := f.m.Profiles().Get(ctx, "bank"); p.Proxy != stickyProxy {
		t.Fatalf("the proxy changed to %q", p.Proxy)
	}
	if body := f.get("/profiles/bank").body; strings.Contains(body, newTok) || strings.Contains(body, tok) {
		t.Fatal("the page shows a token")
	}

	// Not while the browser runs: refused, and the menu says why.
	if _, _, err := f.m.Start(ctx, "bank", ""); err != nil {
		t.Fatal(err)
	}
	want(t, f.get("/profiles"), http.StatusOK, `disabled title="Stop the browser first">New exit IP…`)
	f.post("/profiles/bank/new-exit", nil)
	if got := f.token("bank"); got != newTok {
		t.Fatal("a running profile got a new exit")
	}
	want(t, f.get("/profiles"), http.StatusOK, "Stop bank&#39;s browser before giving it a new exit IP.")
	s, _ := f.m.Live("bank")
	if _, err := f.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}

	// A proxy without the placeholder: nothing to change.
	plainTok := f.token("plain")
	f.post("/profiles/plain/new-exit", nil)
	if got := f.token("plain"); got != plainTok {
		t.Fatal("a profile without {sticky} got a new token")
	}
	if r := f.post("/profiles/missing/new-exit", nil); r.code != http.StatusNotFound {
		t.Fatalf("a missing profile: %d", r.code)
	}

	// Signed out, or cross-site: refused.
	jar := f.client.Jar
	f.client.Jar, _ = cookiejar.New(nil)
	if r := f.post("/profiles/bank/new-exit", nil); r.code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d %s", r.code, r.location)
	}
	f.client.Jar = jar
	if r := f.post("/profiles/bank/new-exit", nil, "Sec-Fetch-Site", "cross-site"); r.code != http.StatusForbidden {
		t.Fatalf("cross-site: %d %s", r.code, r.location)
	}
	if got := f.token("bank"); got != newTok {
		t.Fatal("a signed-out or cross-site request changed the token")
	}
}

// TestImportWithoutAStickyToken: an archive from before tokens imports,
// and the profile gets a token of its own when it needs one.
func TestImportWithoutAStickyToken(t *testing.T) {
	f := newFixture(t)
	const pass = "correct horse battery"
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "chrome"), 0o700); err != nil {
		t.Fatal(err)
	}
	m := archive.Manifest{Version: "1.0.0", ExportedAt: time.Now(), Profile: archive.Profile{
		Name: "old", DefaultMode: "visual", AutoStopMinutes: 15, Width: 1440, Height: 900, Proxy: stickyProxy, HandoffPolicy: "pin"}}
	var exp bytes.Buffer
	if err := archive.Export(&exp, src, m, pass); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("passphrase", pass)
	fw, _ := mw.CreateFormFile("archive", "old.browserpier.age")
	_, _ = fw.Write(exp.Bytes())
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/profiles/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/profiles/old" {
		t.Fatalf("import: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	p, err := f.m.Profiles().Get(context.Background(), "old")
	if err != nil || p.Proxy != stickyProxy || p.StickyToken == "" {
		t.Fatalf("imported %+v, %v", p, err)
	}
}
