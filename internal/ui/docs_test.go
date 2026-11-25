package ui

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The docs are at /docs/ for anyone, signed in or not, and linked from the
// avatar menu.
func TestDocsAreOpen(t *testing.T) {
	f := newFixture(t)
	if r := f.get("/"); !strings.Contains(r.body, `<a href="/docs/">Docs</a>`) {
		t.Fatal("no Docs link in the avatar menu")
	}
	_, token, err := f.tokens.Create(context.Background(), "research-bot", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, header := range map[string][]string{
		"signed out":  nil,
		"agent token": {"Authorization", "Bearer " + token},
	} {
		req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/docs/", nil)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d, want the docs", name, resp.StatusCode)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
			t.Errorf("%s: CSP %q", name, csp)
		}
	}
	if r := f.get("/docs/"); r.code != http.StatusOK || !strings.Contains(r.body, "BrowserPier docs") {
		t.Fatalf("signed in: %d", r.code)
	}
}
