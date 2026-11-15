package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// filledSecret is a value BrowserPier would have typed: a space, a "+", a
// quote, an "&", a non-ASCII letter and an emoji (no "/": it is also a
// file name below).
const filledSecret = `Vault Value+"&é🙂42`

// routeRecorder records what the API registers.
type routeRecorder map[string]http.Handler

func (r routeRecorder) Handle(pattern string, h http.Handler) { r[pattern] = h }

// unscrubbedExempt are the routes allowed past the scrub, and why.
var unscrubbedExempt = map[string]string{
	"GET /cdp/{profile}":           "raw CDP is full control of the browser: it can read any field (SPEC §11)",
	"GET /cdp/{profile}/{path...}": "the same CDP proxy's /json endpoints",
}

// unscrubbed lists the routes that aren't agent routes and aren't exempt,
// and the exemptions that aren't routes any more.
func unscrubbed(routes routeRecorder) []string {
	var bad []string
	for pattern, h := range routes {
		if _, ok := h.(agentRoute); !ok && unscrubbedExempt[pattern] == "" {
			bad = append(bad, pattern+": not an agent route")
		}
	}
	for pattern := range unscrubbedExempt {
		if routes[pattern] == nil {
			bad = append(bad, pattern+": exempt but not registered")
		}
	}
	slices.Sort(bad)
	return bad
}

// TestEveryRouteIsScrubbed (PROJ-125): every agent API route, the
// handoff routes included (the fixture has handoffs), is built by agent,
// so its answer is scrubbed; MCP tools call these routes, and scrub again
// (internal/mcp).
func TestEveryRouteIsScrubbed(t *testing.T) {
	f := newFixture(t)
	routes := routeRecorder{}
	f.api.Register(routes)
	if len(routes) < 25 { // 28 on 2026-11-15
		t.Fatalf("only %d routes registered: %v", len(routes), slices.Sorted(func(yield func(string) bool) {
			for p := range routes {
				if !yield(p) {
					return
				}
			}
		}))
	}
	for _, want := range []string{"POST /api/sessions/{id}/snapshot", "POST /api/sessions/{id}/handoff", "GET /api/profiles/{name}/files", "POST /api/sessions/{id}/computer"} {
		if routes[want] == nil {
			t.Errorf("%s isn't registered", want)
		}
	}
	if bad := unscrubbed(routes); len(bad) > 0 {
		t.Errorf("routes that bypass the scrub:\n%s", strings.Join(bad, "\n"))
	}
}

// TestUnscrubbedRoutesAreCaught is the test of the test: a route that
// skips agent, even behind the token check, is reported.
func TestUnscrubbedRoutesAreCaught(t *testing.T) {
	f := newFixture(t)
	routes := routeRecorder{}
	f.api.Register(routes)
	plain := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	routes["GET /api/sessions/{id}/leaky"] = f.web.RequireToken(plain)
	routes["GET /api/sessions/{id}/naked"] = plain
	got := unscrubbed(routes)
	want := []string{"GET /api/sessions/{id}/leaky: not an agent route", "GET /api/sessions/{id}/naked: not an agent route"}
	if !slices.Equal(got, want) {
		t.Errorf("unscrubbed = %q, want %q", got, want)
	}
	delete(routes, "GET /cdp/{profile}")
	if got := unscrubbed(routes); !slices.Contains(got, "GET /cdp/{profile}: exempt but not registered") {
		t.Errorf("a stale exemption isn't reported: %q", got)
	}
}

// scrubServer serves handler as an agent route at GET /x.
func (f *fixture) scrubServer(h http.HandlerFunc) *httptest.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /x", f.api.agent(h))
	srv := httptest.NewServer(mux)
	f.t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestAgentRoutesScrubWhatTheyWrite(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "api")
	if err := f.m.RegisterSecret(s.ID, filledSecret); err != nil {
		t.Fatal(err)
	}
	query := url.QueryEscape(filledSecret)
	cases := []struct {
		name   string
		handle http.HandlerFunc
		hidden bool // the answer must say <hidden>
	}{
		{"JSON", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"text": "Title: " + filledSecret + "\nURL: https://x.example/?p=" + query})
		}, true},
		{"an error", func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusConflict, "the page says "+filledSecret)
		}, true},
		{"text", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "plain "+filledSecret, http.StatusBadGateway)
		}, true},
		{"no content type", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<p>"+filledSecret+"</p>")
		}, true},
		{"a header", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(DownloadsHeader, asciiJSON(`{"files":[{"name":"`+strings.ReplaceAll(filledSecret, `"`, `\"`)+`.csv"}]}`))
			w.WriteHeader(http.StatusNoContent)
		}, false},
	}
	srv := map[string]*httptest.Server{}
	for _, c := range cases {
		srv[c.name] = f.scrubServer(c.handle)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := get(t, srv[c.name], f.all)
			all := body + " " + resp.Header.Get(DownloadsHeader)
			for _, leak := range []string{filledSecret, query, "Vault Value", `Vault Value+\"`, `é\ud83d`} {
				if strings.Contains(all, leak) {
					t.Errorf("%q leaked: %d %s", leak, resp.StatusCode, all)
				}
			}
			if !strings.Contains(all, "hidden") {
				t.Errorf("nothing hidden: %s", all)
			}
			if c.name == "JSON" && resp.Header.Get("Content-Type") != "application/json" {
				t.Errorf("content type %q", resp.Header.Get("Content-Type"))
			}
		})
	}

	// A screenshot, a PDF and a downloaded file go through as they are.
	for _, ct := range []string{"image/png", "application/pdf", "application/octet-stream"} {
		srv := f.scrubServer(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = io.WriteString(w, "bytes "+filledSecret)
		})
		if _, body := get(t, srv, f.all); body != "bytes "+filledSecret {
			t.Errorf("%s was changed: %q", ct, body)
		}
	}

	// A token that can't see the profile isn't told its secret is one.
	s2 := f.start(f.all, "github", "api")
	if err := f.m.RegisterSecret(s2.ID, "github-only-secret"); err != nil {
		t.Fatal(err)
	}
	other := f.scrubServer(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"text": "github-only-secret"})
	})
	if _, body := get(t, other, f.scoped); !strings.Contains(body, "github-only-secret") {
		t.Errorf("a token scoped to google had github's secret hidden: %s", body)
	}
	if _, body := get(t, other, f.all); strings.Contains(body, "github-only-secret") {
		t.Errorf("github's secret leaked: %s", body)
	}

	// A secret filled during the call counts.
	during := f.scrubServer(func(w http.ResponseWriter, _ *http.Request) {
		_ = f.m.RegisterSecret(s.ID, "filled-just-now")
		writeJSON(w, http.StatusOK, map[string]string{"text": "filled-just-now"})
	})
	if _, body := get(t, during, f.all); strings.Contains(body, "filled-just-now") {
		t.Errorf("a secret filled during the call leaked: %s", body)
	}
}

// A token scoped to the profile has its secrets hidden too.
func TestScopedTokenSeesItsOwnProfileScrubbed(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "api")
	_ = f.m.RegisterSecret(s.ID, filledSecret)
	srv := f.scrubServer(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"text": filledSecret})
	})
	if _, body := get(t, srv, f.scoped); strings.Contains(body, "Vault Value") {
		t.Errorf("google's scoped token read google's secret: %s", body)
	}
}

func TestRealRoutesScrubFileNamesAndDownloadNotices(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	if err := f.m.RegisterSecret(s.ID, filledSecret); err != nil {
		t.Fatal(err)
	}
	name := filledSecret + ".csv"
	f.downloadOn("google", "/input/click", name, "a,b\n")
	code, h, raw := f.computer(s.ID, `{"action": "click", "x": 10, "y": 20}`)
	if code != http.StatusNoContent {
		t.Fatalf("click: %d %s", code, raw)
	}
	n := notice(t, h)
	if len(n.Files) != 1 || n.Files[0].Name != "<hidden>.csv" {
		t.Errorf("X-Downloads: %q", h.Get(DownloadsHeader))
	}
	var list struct {
		Files []struct{ Name string } `json:"files"`
	}
	if code := f.call(f.all, http.MethodGet, "/api/profiles/google/files", "", &list); code != http.StatusOK || len(list.Files) != 1 || list.Files[0].Name != "<hidden>.csv" {
		t.Errorf("files: %d %+v", code, list)
	}
	for _, a := range f.m.Activity(s.ID) {
		if strings.Contains(a.What, "Vault") {
			t.Errorf("activity: %q", a.What)
		}
	}
	// The session goes, and its secrets with it.
	if code := f.call(f.all, http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	if f.m.SecretCount(s.ID) != 0 {
		t.Error("the secrets outlived the session")
	}
}
