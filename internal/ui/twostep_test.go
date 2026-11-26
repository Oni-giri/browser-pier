package ui

import (
	"context"
	"encoding/base32"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
)

var (
	recoveryCodeRE = regexp.MustCompile(`\b[2-9A-HJ-NP-Z]{4}-[2-9A-HJ-NP-Z]{4}-[2-9A-HJ-NP-Z]{4}-[2-9A-HJ-NP-Z]{4}\b`)
	shownSecretRE  = regexp.MustCompile(`<code class="key">((?:[A-Z2-7]{4} ){7}[A-Z2-7]{4})</code>`)
	linkAttrRE     = regexp.MustCompile(`(?i)(?:href|src|action|hx-post|hx-get)="([^"]*)"|url\(([^)]*)\)`)
)

// shownTOTPSecret reads the secret off the scan dialog, as a person would type it.
func shownTOTPSecret(t *testing.T, body string) []byte {
	t.Helper()
	m := shownSecretRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no secret shown: %.600s", body)
	}
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(m[1], " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// turnOn goes through Settings and returns the secret and recovery codes.
func (f *fixture) turnOn() ([]byte, []string) {
	f.t.Helper()
	r := f.post("/settings/two-step/on", url.Values{"current": {"the-admin-password"}}, "HX-Request", "true")
	want(f.t, r, http.StatusOK, "<svg", "Scan this with your authenticator app")
	secret := shownTOTPSecret(f.t, r.body)
	r = f.post("/settings/two-step/confirm", url.Values{"code": {auth.TOTPCode(secret, time.Now())}}, "HX-Request", "true")
	want(f.t, r, http.StatusOK, "Save your recovery codes")
	codes := recoveryCodeRE.FindAllString(r.body, -1)
	return secret, codes
}

// nextCode is a code from the app that no sign-in or change has used yet.
func nextCode(secret []byte) string { return auth.TOTPCode(secret, time.Now().Add(30*time.Second)) }

func TestTwoStepInSettings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.web.Admin()
	want(t, f.get("/settings"), http.StatusOK, "Two-step sign-in", `<span class="pill">Off</span>`, "Turn on")

	if r := f.post("/settings/two-step/on", url.Values{"current": {"wrong"}}, "HX-Request", "true"); r.code != http.StatusUnprocessableEntity || !strings.Contains(r.body, "isn’t right") && !strings.Contains(r.body, "isn&#39;t right") {
		t.Fatalf("wrong password: %d %.300s", r.code, r.body)
	}
	r := f.post("/settings/two-step/on", url.Values{"current": {"the-admin-password"}}, "HX-Request", "true")
	want(t, r, http.StatusOK, "<svg", "BrowserPier: 127.0.0.1:")
	if r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the secret's dialog is cacheable: %q", r.header.Get("Cache-Control"))
	}
	secret := shownTOTPSecret(t, r.body)
	enc := auth.EncodeTOTPSecret(secret)
	for _, m := range linkAttrRE.FindAllString(r.body, -1) {
		if strings.Contains(strings.ReplaceAll(m, " ", ""), enc[:8]) || strings.Contains(m, "otpauth") {
			t.Fatalf("the secret is in a URL: %s", m)
		}
	}
	if strings.Contains(r.body, "otpauth") {
		t.Fatal("the otpauth address is in the page as text")
	}

	// A wrong code stores nothing.
	r = f.post("/settings/two-step/confirm", url.Values{"code": {"000000"}}, "HX-Request", "true")
	want(t, r, http.StatusUnprocessableEntity, "That code isn")
	if st, _ := admin.TwoStep(ctx); st.On {
		t.Fatal("on after a wrong code")
	}
	// A right one turns it on and shows the codes, once.
	r = f.post("/settings/two-step/confirm", url.Values{"code": {auth.TOTPCode(secret, time.Now())}}, "HX-Request", "true")
	want(t, r, http.StatusOK, "Save your recovery codes")
	codes := recoveryCodeRE.FindAllString(r.body, -1)
	if len(codes) < auth.RecoveryCodes {
		t.Fatalf("%d recovery codes shown", len(codes))
	}
	if !strings.Contains(r.header.Get("Set-Cookie"), auth.DeviceCookie+"=") {
		t.Fatal("this browser got no fresh device cookie after the device key rotated")
	}
	page := f.get("/settings")
	want(t, page, http.StatusOK, "On since", "Recovery codes left", "Manage")
	for _, c := range append(codes, enc) {
		if strings.Contains(page.body, c) {
			t.Fatalf("Settings shows %s again", c)
		}
	}
	if strings.Contains(page.body, "Make new ones under Manage") {
		t.Fatal("a low-codes warning with ten left")
	}

	// Two left: a warning.
	for _, c := range codes[:8] {
		if _, err := admin.ConfirmIdentity(ctx, "the-admin-password", c); err != nil {
			t.Fatal(err)
		}
	}
	want(t, f.get("/settings"), http.StatusOK, "Only 2 recovery codes left.", `num low">2<`)

	// New recovery codes need the password and a code; shown once.
	r = f.post("/settings/two-step/manage", url.Values{"action": {"codes"}, "current": {"the-admin-password"}, "code": {nextCode(secret)}}, "HX-Request", "true")
	want(t, r, http.StatusOK, "Save your recovery codes")
	fresh := recoveryCodeRE.FindAllString(r.body, -1)
	if page := f.get("/settings"); strings.Contains(page.body, fresh[0]) || !strings.Contains(page.body, `<span class="num">10</span>`) {
		t.Fatalf("after new codes: %.200s", page.body)
	}

	// Turning off: refused with the password alone and with a code alone.
	r = f.post("/settings/two-step/manage", url.Values{"action": {"off"}, "current": {"the-admin-password"}, "code": {""}}, "HX-Request", "true")
	want(t, r, http.StatusUnprocessableEntity, "That code isn")
	r = f.post("/settings/two-step/manage", url.Values{"action": {"off"}, "current": {""}, "code": {fresh[0]}}, "HX-Request", "true")
	want(t, r, http.StatusUnprocessableEntity, "current password isn")
	r = f.post("/settings/two-step/manage", url.Values{"action": {"off"}, "current": {"the-admin-password"}, "code": {fresh[0]}})
	if r.code != http.StatusSeeOther || r.location != "/settings" {
		t.Fatalf("turning off: %d to %q", r.code, r.location)
	}
	want(t, f.get("/settings"), http.StatusOK, "Two-step sign-in is off", `<span class="pill">Off</span>`)
}

func TestReplacingTheAppInSettings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	secret, codes := f.turnOn()
	r := f.post("/settings/two-step/manage", url.Values{"action": {"replace"}, "current": {"the-admin-password"}, "code": {codes[0]}}, "HX-Request", "true")
	want(t, r, http.StatusOK, "<svg", "Use this app", "The old app stops working")
	next := shownTOTPSecret(t, r.body)
	if string(next) == string(secret) {
		t.Fatal("the same secret again")
	}
	r = f.post("/settings/two-step/confirm", url.Values{"code": {nextCode(secret)}}, "HX-Request", "true")
	want(t, r, http.StatusUnprocessableEntity, "That code isn")
	r = f.post("/settings/two-step/confirm", url.Values{"code": {auth.TOTPCode(next, time.Now())}}, "HX-Request", "true")
	if r.code != http.StatusNoContent || r.header.Get("HX-Redirect") != "/settings" {
		t.Fatalf("replacing: %d %q", r.code, r.header.Get("HX-Redirect"))
	}
	if st, _ := f.web.Admin().TwoStep(ctx); !st.On || st.CodesLeft != auth.RecoveryCodes-1 {
		t.Fatalf("after replacing: %+v", st)
	}
	// The old app's codes are refused now.
	if _, err := f.web.Admin().ConfirmIdentity(ctx, "the-admin-password", nextCode(secret)); err == nil {
		t.Fatal("the old app still works")
	}
}

func TestTwoStepDraftBelongsToItsSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var mu sync.Mutex
	now := time.Now()
	f.app.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r := f.post("/settings/two-step/on", url.Values{"current": {"the-admin-password"}}, "HX-Request", "true")
	want(t, r, http.StatusOK, "<svg")
	secret := shownTOTPSecret(t, r.body)

	// Another signed-in browser can't confirm this session's secret.
	other, _, err := f.web.Admin().NewSession(ctx, false, "other")
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(f.srv.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: auth.CookieName, Value: other, Path: "/"}})
	mine := f.client
	f.client = &http.Client{Jar: jar, CheckRedirect: mine.CheckRedirect}
	want(t, f.post("/settings/two-step/confirm", url.Values{"code": {auth.TOTPCode(secret, time.Now())}}, "HX-Request", "true"), http.StatusUnprocessableEntity, "Start again")
	f.client = mine
	if st, _ := f.web.Admin().TwoStep(ctx); st.On {
		t.Fatal("another session confirmed this one's secret")
	}

	// After 10 minutes, not even this session can.
	mu.Lock()
	now = now.Add(10 * time.Minute)
	mu.Unlock()
	want(t, f.post("/settings/two-step/confirm", url.Values{"code": {auth.TOTPCode(secret, time.Now())}}, "HX-Request", "true"), http.StatusUnprocessableEntity, "Start again")
	if st, _ := f.web.Admin().TwoStep(ctx); st.On {
		t.Fatal("a secret older than 10 minutes was confirmed")
	}
}

func TestTwoStepRoutesNeedTheAdminCookie(t *testing.T) {
	f := newFixture(t)
	_, token, err := f.tokens.Create(context.Background(), "agent", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/settings/two-step/on", "/settings/two-step/confirm", "/settings/two-step/manage"} {
		form := url.Values{"current": {"the-admin-password"}, "code": {"123456"}, "action": {"off"}}
		for _, auth := range []string{"", "Bearer " + token} {
			req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s with %q and no admin cookie: %d", path, auth, resp.StatusCode)
			}
		}
	}
}

// Through the Manage form itself: a wrong password costs the account-wide
// code budget nothing, and wrong app codes spend the same budget that
// sign-in's code step sees.
func TestManageCodesShareTheSignInBudget(t *testing.T) {
	f := newFixture(t)
	secret, _ := f.turnOn()
	f.app.pinTries = auth.NewLockout(1000, time.Minute, time.Minute) // only the account-wide cap counts here
	manage := func(password, code string) reply {
		return f.post("/settings/two-step/manage", url.Values{"action": {"codes"}, "current": {password}, "code": {code}}, "HX-Request", "true")
	}
	for range 5 {
		if r := manage("wrong-password", "000000"); !strings.Contains(r.body, "current password isn") {
			t.Fatalf("wrong password: %d %.300s", r.code, r.body)
		}
	}
	for i := range 10 {
		if r := manage("the-admin-password", "000000"); !strings.Contains(r.body, "That code isn") {
			t.Fatalf("wrong code %d with the right password: %d %.300s", i+1, r.code, r.body)
		}
	}
	if r := manage("the-admin-password", nextCode(secret)); !strings.Contains(r.body, "for this account today") {
		t.Fatalf("an app code in Manage past the cap: %d %.300s", r.code, r.body)
	}
	// Sign-in from a new browser meets the same cap at its code step.
	jar, _ := cookiejar.New(nil)
	b := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(path string, form url.Values) reply {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := b.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return reply{resp.StatusCode, resp.Header.Get("Location"), string(body), resp.Header}
	}
	if r := send("/login", url.Values{"password": {"the-admin-password"}}); r.code != http.StatusSeeOther || r.location != "/login/code" {
		t.Fatalf("password step: %d to %q", r.code, r.location)
	}
	if r := send("/login/code", url.Values{"code": {nextCode(secret)}}); r.code != http.StatusTooManyRequests || !strings.Contains(r.body, "for this account today") {
		t.Fatalf("sign-in's code step after 10 wrong codes in Manage: %d %.300s", r.code, r.body)
	}
}
