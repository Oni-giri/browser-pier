package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/files"
)

// pageLeaseRoutes are the agent routes that take a page lease
// (sessions.Manager.PageLease or ReadLease) before they reach the
// browser, and content's screenshot, which takes none but checks the
// token again too. "REF" in a body is a Visual-mode ref to the login
// form's button. call: the route takes an Idempotency-Key.
var pageLeaseRoutes = []struct {
	name, method, route, body, mode string
	call                            bool
}{
	{"snapshot", http.MethodPost, "snapshot", `{}`, "api", false},
	{"find", http.MethodPost, "find", `{"text": "x"}`, "api", false},
	{"navigate", http.MethodPost, "navigate", `{"url": "https://example.com"}`, "api", true},
	{"act", http.MethodPost, "act", `{"steps": [{"action": "press", "keys": "Enter"}]}`, "api", true},
	{"wait", http.MethodPost, "wait", `{"text": "x"}`, "api", false},
	{"tabs list", http.MethodGet, "tabs", ``, "api", false},
	{"tabs", http.MethodPost, "tabs", `{"action": "list"}`, "api", true},
	{"markdown", http.MethodPost, "content", `{"format": "markdown"}`, "api", false},
	{"readability", http.MethodPost, "content", `{"format": "readability"}`, "api", false},
	{"pdf", http.MethodPost, "content", `{"format": "pdf"}`, "api", false},
	{"screenshot", http.MethodPost, "content", `{"format": "screenshot"}`, "api", false},
	{"visual screenshot", http.MethodPost, "content", `{"format": "screenshot"}`, "visual", false},
	{"visual-snapshot", http.MethodPost, "visual-snapshot", `{}`, "visual", false},
	{"click by ref", http.MethodPost, "click", `{"ref": "REF"}`, "visual", true},
	{"computer click by ref", http.MethodPost, "computer", `{"action": "click", "ref": "REF"}`, "visual", true},
}

// browserCalls counts what reached the browser: CDP methods, element tree
// reads (whole and by node), input actions, screenshots, file chooser
// reads and ad-list exemptions.
func (f *fixture) browserCalls(nodes *int, mu *sync.Mutex) int {
	f.b.mu.Lock()
	n := len(f.b.methods) + f.b.treeGets + len(f.b.inputs) + f.b.screenshots + f.b.chooserGets + len(f.b.exempts)
	f.b.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	return n + *nodes
}

// A page call whose token is revoked after its request was let in, while
// its body is still being read (before its lease, where the revocation
// has nothing to cancel), reaches nothing in the browser: it answers 401
// (PROJ-206). It tells of no file chooser or download either, and logs
// nothing. A token lookup that fails refuses it too, and gives its call
// id back; a live token still gets through.
func TestATokenRevokedBeforeItsPageCallBeginsReadsNothing(t *testing.T) {
	for _, rt := range pageLeaseRoutes {
		for _, how := range []string{"revoked", "lookup fails", "live"} {
			t.Run(rt.name+"/"+how, func(t *testing.T) {
				f := newFixture(t)
				ctx := context.Background()
				s := f.start(f.all, "google", rt.mode)
				var mu sync.Mutex
				nodes := 0
				node := nodeAt(615, 167, onScreen, "")
				f.setTree(loginTree(onScreen), func(q url.Values) string {
					mu.Lock()
					nodes++
					mu.Unlock()
					return node(q)
				})
				body := rt.body
				if strings.Contains(body, "REF") {
					_, snap := f.vsnapshot(f.all, s.ID, "")
					body = strings.ReplaceAll(body, "REF", refNamed(t, snap, `button "Sign in"`))
				}
				// What a call's notices would tell: an open file chooser,
				// and a download since the last look (act's notice).
				f.openChooser(`[{"id":"c1","multiple":false,"accept":[]}]`)
				f.api.files.Mark(s.Profile, s.ID).Downloads(ctx, 0)
				dir := filepath.Join(f.m.Layout().FilesDir(s.Profile), files.Downloads)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "unannounced.csv"), []byte("a,b\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				tok, secret, err := f.tokens.Create(ctx, "doomed", true, nil)
				if err != nil {
					t.Fatal(err)
				}
				before := f.browserCalls(&nodes, &mu)
				g := &gatedBody{r: strings.NewReader(body), reading: make(chan struct{}), release: make(chan struct{})}
				req := httptest.NewRequest(rt.method, "/api/sessions/"+s.ID+"/"+rt.route, g)
				req.Header.Set("Authorization", "Bearer "+secret)
				if rt.call {
					req.Header.Set(IdempotencyHeader, "k1")
				}
				rec := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					defer close(done)
					f.srv.Config.Handler.ServeHTTP(rec, req)
				}()
				<-g.reading
				switch how {
				case "revoked":
					// As the UI revokes: the token goes, then its
					// connections and calls.
					if err := f.tokens.Revoke(ctx, tok.ID); err != nil {
						t.Fatal(err)
					}
					f.m.DisconnectToken(tok.ID)
				case "lookup fails":
					if _, err := f.db.ExecContext(ctx, `ALTER TABLE agent_tokens RENAME TO agent_tokens_gone`); err != nil {
						t.Fatal(err)
					}
				}
				close(g.release)
				<-done
				reached := f.browserCalls(&nodes, &mu) - before
				switch how {
				case "revoked":
					if rec.Code != http.StatusUnauthorized || rec.Body.String() != `{"error":"the agent token was revoked"}`+"\n" {
						t.Fatalf("answered %d %s", rec.Code, rec.Body)
					}
				case "lookup fails":
					if rec.Code != http.StatusInternalServerError {
						t.Fatalf("answered %d %s", rec.Code, rec.Body)
					}
				case "live":
					if rec.Code == http.StatusUnauthorized || reached == 0 {
						t.Fatalf("a live token's call: %d %s, %d calls reached the browser", rec.Code, rec.Body, reached)
					}
					return
				}
				if reached != 0 {
					t.Fatalf("%d calls reached the browser", reached)
				}
				if got := f.m.ConnectedTokens(); len(got) != 0 {
					t.Fatalf("still held by %v", got)
				}
				for _, what := range f.whats(s.ID) {
					if strings.HasPrefix(what, "doomed:") {
						t.Fatalf("logged %q", what)
					}
				}
				if how == "lookup fails" && rt.call {
					if _, err := f.db.ExecContext(ctx, `ALTER TABLE agent_tokens_gone RENAME TO agent_tokens`); err != nil {
						t.Fatal(err)
					}
					if state := f.receipt(secret, s.ID, "k1"); state != "" {
						t.Fatalf("the refused call kept its receipt: %s", state)
					}
				}
			})
		}
	}
}
