package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// pageLeaseRoutes are the agent routes that take a page lease
// (sessions.Manager.PageLease or ReadLease) before they reach the
// browser. "REF" in a body is a Visual-mode ref to the login form's
// button.
var pageLeaseRoutes = []struct {
	name, method, route, body, mode string
}{
	{"snapshot", http.MethodPost, "snapshot", `{}`, "api"},
	{"find", http.MethodPost, "find", `{"text": "x"}`, "api"},
	{"navigate", http.MethodPost, "navigate", `{"url": "https://example.com"}`, "api"},
	{"act", http.MethodPost, "act", `{"steps": [{"action": "press", "keys": "Enter"}]}`, "api"},
	{"wait", http.MethodPost, "wait", `{"text": "x"}`, "api"},
	{"tabs list", http.MethodGet, "tabs", ``, "api"},
	{"tabs", http.MethodPost, "tabs", `{"action": "list"}`, "api"},
	{"markdown", http.MethodPost, "content", `{"format": "markdown"}`, "api"},
	{"readability", http.MethodPost, "content", `{"format": "readability"}`, "api"},
	{"pdf", http.MethodPost, "content", `{"format": "pdf"}`, "api"},
	{"visual-snapshot", http.MethodPost, "visual-snapshot", `{}`, "visual"},
	{"click by ref", http.MethodPost, "click", `{"ref": "REF"}`, "visual"},
	{"computer click by ref", http.MethodPost, "computer", `{"action": "click", "ref": "REF"}`, "visual"},
}

// browserCalls counts what reached the browser: CDP methods, element tree
// reads (whole and by node) and input actions.
func (f *fixture) browserCalls(nodes *int, mu *sync.Mutex) int {
	f.b.mu.Lock()
	n := len(f.b.methods) + f.b.treeGets + len(f.b.inputs)
	f.b.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	return n + *nodes
}

// A page call whose token is revoked after its request was let in, while
// its body is still being read (before its lease, where the revocation
// has nothing to cancel), reaches nothing in the browser: it answers 401
// (PROJ-206). A token lookup that fails refuses it too, and a live token
// still gets through.
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
				tok, secret, err := f.tokens.Create(ctx, "doomed", true, nil)
				if err != nil {
					t.Fatal(err)
				}
				before := f.browserCalls(&nodes, &mu)
				g := &gatedBody{r: strings.NewReader(body), reading: make(chan struct{}), release: make(chan struct{})}
				req := httptest.NewRequest(rt.method, "/api/sessions/"+s.ID+"/"+rt.route, g)
				req.Header.Set("Authorization", "Bearer "+secret)
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
					if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "the agent token was revoked") {
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
			})
		}
	}
}
