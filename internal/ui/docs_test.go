package ui

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The docs are the admin's, at /docs/, linked from the avatar menu; agents
// read them through MCP, so a token doesn't open them.
func TestDocsAreTheAdmins(t *testing.T) {
	f := newFixture(t)
	r := f.get("/docs/")
	if r.code != http.StatusOK || !strings.Contains(r.body, "BrowserPier docs") {
		t.Fatalf("signed in: %d", r.code)
	}
	if csp := r.header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("CSP %q", csp)
	}
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
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
			t.Errorf("%s: %d to %q, want the sign-in page", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}
