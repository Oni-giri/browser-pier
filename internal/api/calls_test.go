package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/pagetools"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// coveredRoutes are the routes that take a call id, with their tool.
var coveredRoutes = map[string]string{
	"POST /api/sessions/{id}/click":    "click",
	"POST /api/sessions/{id}/type":     "type",
	"POST /api/sessions/{id}/computer": "computer",
	"POST /api/sessions/{id}/act":      "act",
	"POST /api/sessions/{id}/navigate": "navigate",
	"POST /api/sessions/{id}/tabs":     "tabs",
}

// reply is an answer to a request with a call id.
type reply struct {
	code     int
	replayed bool
	body     struct {
		Call  *callJSON `json:"call"`
		Text  string    `json:"text"`
		Error string    `json:"error"`
	}
	raw string
}

// send posts body to a session's tool with a call id ("" for none).
func (f *fixture) send(srv *httptest.Server, token, sessionID, tool, callID, body string) reply {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/sessions/"+sessionID+"/"+tool, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if callID != "" {
		req.Header.Set(IdempotencyHeader, callID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var r reply
	r.code, r.replayed, r.raw = resp.StatusCode, resp.Header.Get(ReplayedHeader) == "true", string(raw)
	_ = json.Unmarshal(raw, &r.body)
	return r
}

// receipt reads a call's receipt: its state, or "" for 404.
func (f *fixture) receipt(token, sessionID, callID string) string {
	f.t.Helper()
	var out struct{ Call callJSON }
	code := f.call(token, http.MethodGet, "/api/sessions/"+sessionID+"/calls/"+callID, "", &out)
	switch code {
	case http.StatusOK:
		if out.Call.CallID != callID {
			f.t.Fatalf("receipt for %s: %+v", callID, out)
		}
		return out.Call.State
	case http.StatusNotFound:
		return ""
	}
	f.t.Fatalf("receipt for %s: %d", callID, code)
	return ""
}

func (f *fixture) inputCount() int {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	return len(f.b.inputs)
}

// checkReplay checks the answer to a repeat of a call that ended state.
func checkReplay(t *testing.T, what string, r reply, state string) {
	t.Helper()
	want := http.StatusConflict
	if state == "ok" {
		want = http.StatusOK
	}
	if r.code != want || !r.replayed || r.body.Call == nil || r.body.Call.State != state {
		t.Fatalf("%s: %d replayed %t %s", what, r.code, r.replayed, r.raw)
	}
	if strings.Contains(r.raw, "PNG") || strings.Contains(r.raw, "Downloads") {
		t.Fatalf("%s: a repeat gave the first answer again: %s", what, r.raw)
	}
}

// TestEveryRouteChoosesOnCallIDs: every agent route either takes a call
// id, and is one of the routes that change the page, or refuses the
// header, so an agent never believes it is protected when it isn't.
func TestEveryRouteChoosesOnCallIDs(t *testing.T) {
	f := newFixture(t)
	routes := routeRecorder{}
	f.api.Register(routes)
	for pattern, tool := range coveredRoutes {
		if r, ok := routes[pattern].(agentRoute); !ok || r.tool != tool {
			t.Errorf("%s doesn't take a call id as %s", pattern, tool)
		}
	}
	for pattern, h := range routes {
		r, ok := h.(agentRoute)
		if !ok {
			continue // the CDP proxy: raw CDP is the agent's own client
		}
		if r.tool != "" {
			if coveredRoutes[pattern] != r.tool {
				t.Errorf("%s takes a call id as %q but isn't a covered route", pattern, r.tool)
			}
			continue
		}
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.NewReplacer("{id}", "s1", "{name}", "google", "{file}", "a.txt", "{call_id}", "x").Replace(path)
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+f.all)
		req.Header.Set(IdempotencyHeader, "buy-1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no call id is taken here") {
			t.Errorf("%s with a call id: %d %s", pattern, rec.Code, rec.Body)
		}
	}
}

func TestTheHeaderIsRefusedWhereNoCallIDIsTaken(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "api")
	for _, r := range [][3]string{
		{"POST", "/api/sessions/" + s.ID + "/snapshot", `{}`},
		{"POST", "/api/sessions/" + s.ID + "/files", ``},
		{"POST", "/api/sessions/" + s.ID + "/handoff", `{"reason": "captcha"}`},
		{"POST", "/api/sessions", `{"profile": "google"}`},
		{"GET", "/api/sessions/" + s.ID + "/tabs", ``},
		{"GET", "/api/sessions/" + s.ID, ``},
	} {
		req, _ := http.NewRequest(r[0], f.srv.URL+r[1], strings.NewReader(r[2]))
		req.Header.Set("Authorization", "Bearer "+f.all)
		req.Header.Set(IdempotencyHeader, "buy-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "no call id is taken here") {
			t.Errorf("%s %s: %d %s", r[0], r[1], resp.StatusCode, raw)
		}
	}
	// A bad id is refused before anything runs.
	v := f.start(f.all, "github", "visual")
	for _, id := range []string{strings.Repeat("x", 65), "a b", "é", "a/b"} {
		if r := f.send(f.srv, f.all, v.ID, "click", id, `{"x": 1, "y": 2}`); r.code != http.StatusBadRequest || !strings.Contains(r.body.Error, IdempotencyHeader) {
			t.Errorf("id %q: %d %s", id, r.code, r.raw)
		}
	}
	if n := f.inputCount(); n != 0 {
		t.Errorf("%d inputs reached the browser", n)
	}
	if code := f.call(f.all, http.MethodGet, "/api/sessions/"+v.ID+"/calls/a.b", "", nil); code != http.StatusBadRequest {
		t.Errorf("a bad id's receipt: %d", code)
	}
}

// visualCalls are a call to each Visual-mode route that takes an id.
var visualCalls = [][2]string{
	{"click", `{"x": 10, "y": 20}`},
	{"type", `{"text": "hello world!"}`},
	{"computer", `{"action": "click", "x": 10, "y": 20}`},
	{"computer", `{"action": "type", "text": "hello world!"}`},
	{"computer", `{"action": "key", "keys": "Return"}`},
	{"computer", `{"action": "scroll", "x": 1, "y": 2, "dy": 3}`},
	{"computer", `{"action": "drag", "from": {"x": 1, "y": 2}, "to": {"x": 3, "y": 4}}`},
	{"computer", `{"action": "open", "url": "https://example.com/"}`},
	{"computer", `{"action": "click", "x": 10, "y": 20, "screenshot": true}`},
}

func TestARepeatedVisualCallRunsOnce(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	for i, c := range visualCalls {
		id := "call-" + string(rune('a'+i))
		before := f.inputCount()
		if r := f.send(f.srv, f.all, s.ID, c[0], id, c[1]); r.code != http.StatusNoContent && r.code != http.StatusOK || r.replayed {
			t.Fatalf("%s %s: %d %s", c[0], c[1], r.code, r.raw)
		}
		if n := f.inputCount(); n != before+1 {
			t.Fatalf("%s: %d inputs", c[0], n-before)
		}
		r := f.send(f.srv, f.all, s.ID, c[0], id, c[1])
		checkReplay(t, c[0]+" again", r, "ok")
		if !strings.Contains(r.body.Text, "wasn't run again") || r.body.Call.Tool != c[0] || r.body.Call.Ended == nil {
			t.Fatalf("%s again: %s", c[0], r.raw)
		}
		// Key order and spacing don't make it another call.
		var v map[string]any
		_ = json.Unmarshal([]byte(c[1]), &v)
		again, _ := json.MarshalIndent(v, "", "  ")
		checkReplay(t, c[0]+" reformatted", f.send(f.srv, f.all, s.ID, c[0], id, string(again)), "ok")
		// Other arguments under the same id are another call: refused.
		if r := f.send(f.srv, f.all, s.ID, c[0], id, strings.TrimSuffix(c[1], "}")+`, "pace": "instant"}`); r.code != http.StatusConflict || r.replayed || !strings.Contains(r.body.Error, "named another call") {
			t.Fatalf("%s with other arguments: %d %s", c[0], r.code, r.raw)
		}
		if n := f.inputCount(); n != before+1 {
			t.Fatalf("%s: %d inputs after the repeats", c[0], n-before)
		}
		if st := f.receipt(f.all, s.ID, id); st != "ok" {
			t.Fatalf("%s: receipt %q", c[0], st)
		}
	}
	// Without the header, the same call runs again, as it always did.
	before := f.inputCount()
	f.send(f.srv, f.all, s.ID, "click", "", `{"x": 10, "y": 20}`)
	f.send(f.srv, f.all, s.ID, "click", "", `{"x": 10, "y": 20}`)
	if n := f.inputCount(); n != before+2 {
		t.Fatalf("without an id: %d inputs", n-before)
	}
}

// fakePage serves act, navigate and tabs through the real pageTool and
// call-id wrapper over a fake page tool, which takes a page lease as the
// real ones do and counts the calls that got one. "fail" in a body fails
// it after the lease (as a step that fails at step 2), "slowly" holds the
// lease until it is cut.
type fakePage struct {
	mu    sync.Mutex
	calls int
}

func (p *fakePage) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (f *fixture) fakePageServer() (*httptest.Server, *fakePage) {
	p := &fakePage{}
	run := func(_ *pagetools.Tools, ctx context.Context, s sessions.Session, who pagetools.Caller, in map[string]any) (string, error) {
		if s.Mode != "api" {
			return "", &pagetools.Error{Status: http.StatusConflict, Msg: "Visual mode"}
		}
		ls, err := f.m.PageLease(ctx, s.ID, who.TokenID)
		if err != nil {
			return "", &pagetools.Error{Status: http.StatusConflict, Msg: err.Error()}
		}
		defer ls.Release()
		p.mu.Lock()
		p.calls++
		p.mu.Unlock()
		raw, _ := json.Marshal(in)
		switch {
		case strings.Contains(string(raw), "fail"):
			return "", &pagetools.Error{Status: http.StatusConflict, Msg: "step 2: the element is covered"}
		case strings.Contains(string(raw), "slowly"):
			<-ls.Ctx.Done()
			return "", &pagetools.Error{Status: http.StatusConflict, Msg: context.Cause(ls.Ctx).Error()}
		}
		f.m.NoteFor(ctx, s.ID, who.Agent, "Clicked a7e12")
		return "Clicked a7e12", nil
	}
	mux := http.NewServeMux()
	for _, tool := range []string{"act", "navigate", "tabs"} {
		mux.Handle("POST /api/sessions/{id}/"+tool, f.api.agentCall(tool, pageTool(f.api, run)))
	}
	mux.Handle("GET /api/sessions/{id}", f.api.agent(f.api.getSession))
	srv := httptest.NewServer(mux)
	f.t.Cleanup(srv.Close)
	return srv, p
}

func TestARepeatedPageCallRunsOnce(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "api")
	srv, page := f.fakePageServer()
	for _, tool := range []string{"act", "navigate", "tabs"} {
		before := page.count()
		body := `{"steps": [{"action": "click", "ref": "a7e12"}]}`
		r := f.send(srv, f.all, s.ID, tool, tool+"-1", body)
		if r.code != http.StatusOK || r.replayed || r.body.Text != "Clicked a7e12" {
			t.Fatalf("%s: %d %s", tool, r.code, r.raw)
		}
		r = f.send(srv, f.all, s.ID, tool, tool+"-1", body)
		checkReplay(t, tool+" again", r, "ok")
		if strings.Contains(r.body.Text, "Clicked") {
			t.Fatalf("%s again gave the first result: %s", tool, r.raw)
		}
		if r := f.send(srv, f.all, s.ID, tool, tool+"-1", `{"steps": []}`); r.code != http.StatusConflict || r.replayed {
			t.Fatalf("%s with another body: %d %s", tool, r.code, r.raw)
		}
		if n := page.count() - before; n != 1 {
			t.Fatalf("%s ran %d times", tool, n)
		}
	}
	// Another token's same id is its own call.
	if r := f.send(srv, f.scoped, s.ID, "act", "act-1", `{"steps": [{"action": "click", "ref": "a7e12"}]}`); r.code != http.StatusOK || r.replayed {
		t.Fatalf("another token's act-1: %d %s", r.code, r.raw)
	}
}

// Calls refused before they were admitted give their id back: the same
// id then runs.
func TestARefusedCallGivesItsIDBack(t *testing.T) {
	f := newFixture(t)
	v := f.start(f.all, "google", "visual")
	a := f.start(f.all, "github", "api")
	srv, page := f.fakePageServer()

	// Bad JSON or arguments.
	if r := f.send(f.srv, f.all, v.ID, "computer", "c1", `{"action": "click"`); r.code != http.StatusBadRequest {
		t.Fatalf("bad JSON: %d %s", r.code, r.raw)
	}
	if r := f.send(f.srv, f.all, v.ID, "computer", "c1", `{"action": "click"}`); r.code != http.StatusBadRequest {
		t.Fatalf("no x and y: %d %s", r.code, r.raw)
	}
	if r := f.send(f.srv, f.all, v.ID, "click", "c1", `{"x": 1, "y": 2, "download_wait_ms": "soon"}`); r.code != http.StatusBadRequest {
		t.Fatalf("a bad download_wait_ms: %d %s", r.code, r.raw)
	}
	if r := f.send(srv, f.all, a.ID, "act", "c1", `{"steps": [}`); r.code != http.StatusBadRequest {
		t.Fatalf("act's bad JSON: %d %s", r.code, r.raw)
	}
	// No such session, or one the token can't see.
	if r := f.send(f.srv, f.all, "nope", "click", "c1", `{"x": 1, "y": 2}`); r.code != http.StatusNotFound {
		t.Fatalf("no session: %d %s", r.code, r.raw)
	}
	if r := f.send(f.srv, f.scoped, a.ID, "act", "c1", `{"steps": []}`); r.code != http.StatusNotFound {
		t.Fatalf("another profile's session: %d %s", r.code, r.raw)
	}
	// The wrong mode: the real page tools refuse a Visual session.
	for _, c := range [][2]string{{"act", `{"steps": [{"action": "press", "keys": "Enter"}]}`}, {"navigate", `{"url": "https://example.com"}`}, {"tabs", `{}`}} {
		if r := f.send(f.srv, f.all, v.ID, c[0], "c1", c[1]); r.code != http.StatusConflict || r.replayed || !strings.Contains(r.raw, "Visual mode") {
			t.Fatalf("%s in Visual mode: %d %s", c[0], r.code, r.raw)
		}
	}
	// A person has the browser.
	for _, id := range []string{v.ID, a.ID} {
		if err := f.m.TakeControl(id, "person"); err != nil {
			t.Fatal(err)
		}
	}
	if r := f.send(f.srv, f.all, v.ID, "click", "c1", `{"x": 1, "y": 2}`); r.code != http.StatusConflict || r.replayed {
		t.Fatalf("click while a person drives: %d %s", r.code, r.raw)
	}
	if r := f.send(f.srv, f.all, a.ID, "act", "c1", `{"steps": [{"action": "press", "keys": "Enter"}]}`); r.code != http.StatusConflict || r.replayed {
		t.Fatalf("real act while a person drives: %d %s", r.code, r.raw)
	}
	if r := f.send(srv, f.all, a.ID, "act", "c2", `{}`); r.code != http.StatusConflict || r.replayed {
		t.Fatalf("act while a person drives: %d %s", r.code, r.raw)
	}
	f.m.ReleaseControl(v.ID)
	f.m.ReleaseControl(a.ID)
	// Another input is under way.
	slow := make(chan reply, 1)
	go func() { slow <- f.send(f.srv, f.all, v.ID, "type", "", `{"text": "typed slowly"}`) }()
	waitFor(t, func() bool { return f.inputCount() == 1 })
	if r := f.send(f.srv, f.all, v.ID, "click", "c1", `{"x": 1, "y": 2}`); r.code != http.StatusConflict || r.replayed || !strings.Contains(r.body.Error, "another input") {
		t.Fatalf("click while another input runs: %d %s", r.code, r.raw)
	}
	if _, err := f.m.SetNeedsHelp(v.ID, true); err != nil { // cuts the slow one
		t.Fatal(err)
	}
	<-slow
	if _, err := f.m.SetNeedsHelp(v.ID, false); err != nil {
		t.Fatal(err)
	}
	if n := page.count(); n != 0 {
		t.Fatalf("%d page calls ran", n)
	}
	for _, id := range []string{"c1", "c2"} {
		if st := f.receipt(f.all, v.ID, id); st != "" {
			t.Fatalf("Visual %s kept a receipt: %s", id, st)
		}
		if st := f.receipt(f.all, a.ID, id); st != "" {
			t.Fatalf("API %s kept a receipt: %s", id, st)
		}
	}
	// After all that, the same ids run.
	before := f.inputCount()
	if r := f.send(f.srv, f.all, v.ID, "click", "c1", `{"x": 1, "y": 2}`); r.code != http.StatusNoContent || r.replayed {
		t.Fatalf("click c1 at last: %d %s", r.code, r.raw)
	}
	if n := f.inputCount(); n != before+1 {
		t.Fatalf("click c1 at last: %d inputs", n-before)
	}
	if r := f.send(srv, f.all, a.ID, "act", "c1", `{"steps": [{"action": "click", "ref": "a7e12"}]}`); r.code != http.StatusOK || r.replayed {
		t.Fatalf("act c1 at last: %d %s", r.code, r.raw)
	}
	if r := f.send(srv, f.all, a.ID, "act", "c2", `{"steps": [{"action": "click", "ref": "a7e12"}]}`); r.code != http.StatusOK || r.replayed {
		t.Fatalf("act c2 at last: %d %s", r.code, r.raw)
	}
}

// A call that reached the browser keeps its receipt, however it ended:
// the same id is never run again.
func TestACallThatRanKeepsItsReceipt(t *testing.T) {
	f := newFixture(t)
	v := f.start(f.all, "google", "visual")
	a := f.start(f.all, "github", "api")
	srv, page := f.fakePageServer()

	// The control socket answers 500, or refuses the point.
	for _, c := range [][3]string{{"type", "k1", `{"text": "boom"}`}, {"computer", "k2", `{"action": "type", "text": "boom"}`}, {"computer", "k3", `{"action": "click", "x": 99999, "y": 1}`}} {
		before := f.inputCount()
		if r := f.send(f.srv, f.all, v.ID, c[0], c[1], c[2]); r.code < 400 || r.replayed {
			t.Fatalf("%s: %d %s", c[2], r.code, r.raw)
		}
		checkReplay(t, c[2]+" again", f.send(f.srv, f.all, v.ID, c[0], c[1], c[2]), "error")
		if n := f.inputCount(); n != before+1 {
			t.Fatalf("%s: %d inputs", c[2], n-before)
		}
	}

	// Input broke off: a handoff cut it.
	slow := make(chan reply, 1)
	go func() { slow <- f.send(f.srv, f.all, v.ID, "type", "k4", `{"text": "typed slowly"}`) }()
	waitFor(t, func() bool { return strings.HasPrefix(f.lastInput(), "/input/type") })
	if _, err := f.m.SetNeedsHelp(v.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-slow; r.code != http.StatusConflict || !strings.Contains(r.body.Error, "input broke off") {
		t.Fatalf("the cut type: %d %s", r.code, r.raw)
	}
	if _, err := f.m.SetNeedsHelp(v.ID, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.lastInput() == "cut short" })
	before := f.inputCount()
	r := f.send(f.srv, f.all, v.ID, "type", "k4", `{"text": "typed slowly"}`)
	checkReplay(t, "the cut type again", r, "interrupted")
	if !strings.Contains(r.body.Error, "new call id") {
		t.Fatalf("the cut type again: %s", r.raw)
	}
	if n := f.inputCount(); n != before {
		t.Fatalf("the cut type ran again: %d inputs", n-before)
	}

	// An act whose second step fails, and the real page tools failing
	// once they reached the browser.
	if r := f.send(srv, f.all, a.ID, "act", "k5", `{"steps": [{"action": "click", "ref": "a1"}, {"action": "click", "ref": "fail"}]}`); r.code != http.StatusConflict || r.replayed {
		t.Fatalf("a failing act: %d %s", r.code, r.raw)
	}
	checkReplay(t, "the failing act again", f.send(srv, f.all, a.ID, "act", "k5", `{"steps": [{"action": "click", "ref": "a1"}, {"action": "click", "ref": "fail"}]}`), "error")
	if n := page.count(); n != 1 {
		t.Fatalf("the failing act ran %d times", n)
	}
	for _, c := range [][2]string{{"act", `{"steps": [{"action": "press", "keys": "Enter"}]}`}, {"navigate", `{"url": "https://example.com"}`}, {"tabs", `{"action": "new"}`}} {
		if r := f.send(f.srv, f.all, a.ID, c[0], "k6-"+c[0], c[1]); r.code < 400 || r.replayed {
			t.Fatalf("real %s: %d %s", c[0], r.code, r.raw)
		}
		f.b.mu.Lock()
		sent := len(f.b.methods)
		f.b.mu.Unlock()
		checkReplay(t, "real "+c[0]+" again", f.send(f.srv, f.all, a.ID, c[0], "k6-"+c[0], c[1]), "error")
		f.b.mu.Lock()
		again := len(f.b.methods)
		f.b.mu.Unlock()
		if again != sent {
			t.Fatalf("real %s again reached the browser: %d more methods", c[0], again-sent)
		}
	}
}

// A type the browser says failed before any key was pressed gives its id
// back, as a call never admitted does (PROJ-195): the same call runs when
// sent again. Any other failure keeps the receipt.
func TestATypeThatTypedNothingGivesItsIDBack(t *testing.T) {
	f := newFixture(t)
	v := f.start(f.all, "google", "visual")
	for _, c := range [][3]string{{"type", "n1", `{"text": "unbound"}`}, {"computer", "n2", `{"action": "type", "text": "unbound"}`}} {
		before := f.inputCount()
		r := f.send(f.srv, f.all, v.ID, c[0], c[1], c[2])
		if r.code != http.StatusInternalServerError || r.replayed || !strings.Contains(r.body.Error, "with the same call id") {
			t.Fatalf("%s: %d %s", c[2], r.code, r.raw)
		}
		if st := f.receipt(f.all, v.ID, c[1]); st != "" {
			t.Fatalf("%s kept a receipt: %s", c[2], st)
		}
		if r := f.send(f.srv, f.all, v.ID, c[0], c[1], c[2]); r.replayed || f.inputCount() != before+2 {
			t.Fatalf("%s again: replayed %t, %d inputs", c[2], r.replayed, f.inputCount()-before)
		}
	}
}

// A repeat sent while the first is still running doesn't run, and says so.
func TestARepeatWhileTheFirstRuns(t *testing.T) {
	f := newFixture(t)
	v := f.start(f.all, "google", "visual")
	a := f.start(f.all, "github", "api")
	srv, page := f.fakePageServer()

	first := make(chan reply, 1)
	go func() {
		first <- f.send(f.srv, f.all, v.ID, "computer", "r1", `{"action": "type", "text": "typed slowly"}`)
	}()
	waitFor(t, func() bool { return f.inputCount() == 1 })
	r := f.send(f.srv, f.all, v.ID, "computer", "r1", `{"action": "type", "text": "typed slowly"}`)
	checkReplay(t, "the type again", r, "running")
	if !strings.Contains(r.body.Error, "/calls/r1") || !strings.Contains(r.body.Error, "A 404 there doesn't prove it never ran") {
		t.Fatalf("the type again doesn't say where to ask, or what a 404 means: %s", r.raw)
	}
	var st sessionStatusJSON
	f.call(f.all, http.MethodGet, "/api/sessions/"+v.ID, "", &st)
	if len(st.RunningCalls) != 1 || st.RunningCalls[0].CallID != "r1" || st.RunningCalls[0].Tool != "computer" || st.RunningCalls[0].State != "running" {
		t.Fatalf("running_calls: %+v", st.RunningCalls)
	}
	f.call(f.scoped, http.MethodGet, "/api/sessions/"+v.ID, "", &st)
	if len(st.RunningCalls) != 0 {
		t.Fatalf("another token sees running_calls %+v", st.RunningCalls)
	}
	if _, err := f.m.SetNeedsHelp(v.ID, true); err != nil {
		t.Fatal(err)
	}
	<-first
	waitFor(t, func() bool { return f.lastInput() == "cut short" })
	if n := f.inputCount(); n != 2 { // the type, and the fake's note that it was cut
		t.Fatalf("inputs: %d", n)
	}

	// Two copies sent together make one call: the second, taken while
	// the first waits for its turn behind another call, doesn't run.
	someone, _, err := f.tokens.Create(context.Background(), "someone", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := f.m.PageLease(context.Background(), a.ID, someone.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"steps": [{"action": "click", "ref": "a7e12"}]}`
	results := make(chan reply, 2)
	for range 2 {
		go func() { results <- f.send(srv, f.all, a.ID, "act", "r2", body) }()
	}
	var got []reply
	select {
	case r := <-results: // the copy that came second answers at once
		got = append(got, r)
	case <-time.After(5 * time.Second):
		hold.Release()
		t.Fatalf("both copies waited for their turn; act ran %d times", page.count())
	}
	if !got[0].replayed || got[0].body.Call.State != "waiting" || !strings.Contains(got[0].body.Error, "waiting for its turn") || !strings.Contains(got[0].body.Error, "Read the page before sending it again") {
		t.Fatalf("the second copy: %d %s", got[0].code, got[0].raw)
	}
	hold.Release()
	got = append(got, <-results)
	if got[1].code != http.StatusOK || got[1].replayed {
		t.Fatalf("the first copy: %d %s", got[1].code, got[1].raw)
	}
	if n := page.count(); n != 1 {
		t.Fatalf("act ran %d times", n)
	}
}

func TestCallStatus(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	other := f.start(f.all, "github", "visual")
	f.send(f.srv, f.all, s.ID, "click", "mine", `{"x": 1, "y": 2}`)
	f.send(f.srv, f.scoped, s.ID, "click", "theirs", `{"x": 1, "y": 2}`)
	f.send(f.srv, f.all, other.ID, "click", "elsewhere", `{"x": 1, "y": 2}`)
	var out struct{ Call callJSON }
	if code := f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID+"/calls/mine", "", &out); code != http.StatusOK ||
		out.Call.State != "ok" || out.Call.Tool != "click" || out.Call.Started.IsZero() || out.Call.Ended == nil {
		t.Fatalf("mine: %d %+v", code, out)
	}
	for _, c := range [][3]string{
		{f.all, s.ID, "nope"},             // never taken
		{f.all, s.ID, "theirs"},           // another token's
		{f.scoped, s.ID, "mine"},          // the same, the other way
		{f.scoped, other.ID, "elsewhere"}, // a session the token can't see
		{f.all, "nope", "mine"},           // no session
	} {
		if code := f.call(c[0], http.MethodGet, "/api/sessions/"+c[1]+"/calls/"+c[2], "", nil); code != http.StatusNotFound {
			t.Errorf("%s on %s: %d", c[2], c[1], code)
		}
	}
	// A stopped browser answers nothing.
	if code := f.call(f.all, http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatal(code)
	}
	if st := f.receipt(f.all, s.ID, "mine"); st != "" {
		t.Fatalf("a stopped session's receipt: %s", st)
	}
}

func TestRecentCalls(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	f.downloadOn("google", "/input/click", "report.csv", "a,b\n")
	f.send(f.srv, f.all, s.ID, "click", "dl", `{"x": 1, "y": 2}`)
	f.b.mu.Lock()
	f.b.onInput = nil
	f.b.mu.Unlock()
	const typed = "hello world!"
	f.send(f.srv, f.all, s.ID, "type", "t1", `{"text": "`+typed+`"}`)
	f.send(f.srv, f.scoped, s.ID, "click", "other", `{"x": 5, "y": 6}`)
	f.send(f.srv, f.all, s.ID, "click", "", `{"x": 3, "y": 4}`)
	var st sessionStatusJSON
	if code := f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID, "", &st); code != http.StatusOK || st.ID != s.ID || st.State != "running" {
		t.Fatalf("session: %d %+v", code, st)
	}
	var whats []string
	for _, c := range st.RecentCalls {
		whats = append(whats, c.What+" | "+c.CallID+" | "+c.State)
	}
	got := strings.Join(whats, "\n")
	if len(st.RecentCalls) < 4 || st.RecentCalls[0].What != "Clicked at 3, 4" || st.RecentCalls[0].CallID != "" ||
		st.RecentCalls[1].What != "Typed 12 characters" || st.RecentCalls[1].CallID != "t1" || st.RecentCalls[1].State != "ok" {
		t.Fatalf("recent_calls:\n%s", got)
	}
	if strings.Contains(got, typed) || strings.Contains(got, "5, 6") || strings.Contains(got, "other") {
		t.Fatalf("recent_calls hold typed text or another token's calls:\n%s", got)
	}
	// The download's name is the page's: fenced.
	var dl *recentCallJSON
	for i, c := range st.RecentCalls {
		if strings.Contains(c.What, "report") {
			dl = &st.RecentCalls[i]
		}
	}
	if dl == nil || !strings.Contains(dl.What, "<untrusted-content-") || dl.CallID != "dl" {
		t.Fatalf("the download line: %+v\n%s", dl, got)
	}
	if !slices.Contains(f.whats(s.ID), "claude: Downloaded report.csv (4 B)") {
		t.Fatalf("the admin's log changed: %q", f.whats(s.ID))
	}
	// At most 20.
	for range 25 {
		f.send(f.srv, f.all, s.ID, "click", "", `{"x": 3, "y": 4}`)
	}
	f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID, "", &st)
	if len(st.RecentCalls) != maxRecentCalls {
		t.Fatalf("%d recent calls", len(st.RecentCalls))
	}
	// The other token sees its own only.
	f.call(f.scoped, http.MethodGet, "/api/sessions/"+s.ID, "", &st)
	if len(st.RecentCalls) != 1 || st.RecentCalls[0].What != "Clicked at 5, 6" || st.RecentCalls[0].CallID != "other" {
		t.Fatalf("the other token's recent_calls: %+v", st.RecentCalls)
	}
}

// The call ids, the typed text and the fingerprint never reach the log.
func TestCallsLogNothingOfTheirs(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := newFixtureLogging(t, log)
	v := f.start(f.all, "google", "visual")
	a := f.start(f.all, "github", "api")
	const typed = "Tr0ub4dor-typed"
	for i, c := range [][3]string{
		{v.ID, "type", `{"text": "` + typed + `"}`},
		{v.ID, "computer", `{"action": "type", "text": "` + typed + `"}`},
		{v.ID, "computer", `{"action": "type", "text": "` + typed + ` boom"}`},
		{a.ID, "act", `{"steps": [{"action": "fill", "ref": "a1", "text": "` + typed + `"}]}`},
		{a.ID, "navigate", `{"url": "https://example.com/?q=` + typed + `"}`},
		{a.ID, "tabs", `{"action": "new", "url": "https://example.com/?q=` + typed + `"}`},
	} {
		id := "secret-call-id-" + string(rune('a'+i))
		f.send(f.srv, f.all, c[0], c[1], id, c[2])
		f.send(f.srv, f.all, c[0], c[1], id, c[2])
		f.send(f.srv, f.all, c[0], c[1], id, c[2]+" ")
		f.send(f.srv, f.all, c[0], c[1], id, strings.Replace(c[2], typed, "other", 1))
		f.receipt(f.all, c[0], id)
	}
	mu.Lock()
	logged := buf.String()
	mu.Unlock()
	if !strings.Contains(logged, "session started") {
		t.Fatalf("the log isn't captured:\n%s", logged)
	}
	for _, bad := range []string{typed, "secret-call-id", "fingerprint"} {
		if strings.Contains(logged, bad) {
			t.Errorf("the log holds %q:\n%s", bad, logged)
		}
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// With 1,000 calls under way on a session, a new id answers 429 and
// nothing runs: no receipt is dropped, and the call doesn't run without one.
func TestTooManyCallsUnderWay(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	tok, err := f.tokens.Authenticate(context.Background(), f.all)
	if err != nil {
		t.Fatal(err)
	}
	fp := sessions.FingerprintOf("click", nil)
	for i := range 1000 {
		if _, _, err := f.m.TakeCall(s.ID, tok.ID, tok.Name, "click", "w"+strconv.Itoa(i), fp); err != nil {
			t.Fatal(err)
		}
	}
	r := f.send(f.srv, f.all, s.ID, "click", "one-more", `{"x": 1, "y": 2}`)
	if r.code != http.StatusTooManyRequests || r.replayed || !strings.Contains(r.body.Error, "1000 calls") {
		t.Fatalf("one more: %d %s", r.code, r.raw)
	}
	if n := f.inputCount(); n != 0 {
		t.Fatalf("%d inputs reached the browser", n)
	}
	// Without an id, it runs as ever.
	if r := f.send(f.srv, f.all, s.ID, "click", "", `{"x": 1, "y": 2}`); r.code != http.StatusNoContent {
		t.Fatalf("without an id: %d %s", r.code, r.raw)
	}
}

// A Visual click looking for its download shows in running_calls, as
// call_status says it runs, until it answers. That it stays listed once
// its lease has ended is sessions' TestACallRunningAfterItsLeaseIsListed,
// which can order the two without a sleep.
func TestAClickWaitingForItsDownloadIsRunning(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	done := make(chan reply, 1)
	go func() {
		done <- f.send(f.srv, f.all, s.ID, "click", "dl-1", `{"x": 1, "y": 2, "download_wait_ms": 1500}`)
	}()
	waitFor(t, func() bool { return f.inputCount() == 1 })
	var st sessionStatusJSON
	f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID, "", &st)
	if len(st.RunningCalls) != 1 || st.RunningCalls[0].CallID != "dl-1" || st.RunningCalls[0].State != "running" || st.RunningCalls[0].Tool != "click" {
		t.Fatalf("running_calls: %+v", st.RunningCalls)
	}
	if got := f.receipt(f.all, s.ID, "dl-1"); got != "running" {
		t.Fatalf("receipt: %s", got)
	}
	if r := <-done; r.code != http.StatusNoContent {
		t.Fatalf("click: %d %s", r.code, r.raw)
	}
	f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID, "", &st)
	if len(st.RunningCalls) != 0 {
		t.Fatalf("running_calls after: %+v", st.RunningCalls)
	}
}

// Each release case, on every covered route it applies to: the id comes
// back, and the same id then runs (is admitted: a Visual input reaches the
// browser, a real page tool gets its lease and fails at the fake CDP,
// keeping its receipt as error).
func TestEveryRouteGivesARefusedIDBack(t *testing.T) {
	// bad: bodies of the right JSON syntax the route refuses for their
	// shape (a field of the wrong type, a pace, button or text the
	// browser would refuse).
	type route struct {
		tool, body string
		bad        []string
	}
	visual := []route{
		{"click", `{"x": 1, "y": 2}`, []string{`{"x": "1", "y": 2}`, `{"x": 1, "y": 2, "button": "side"}`, `{"x": 1, "y": 2, "pace": "slow"}`, `{"x": 1, "y": 2, "nope": 1}`}},
		{"type", `{"text": "hi"}`, []string{`{"text": 42}`, `{"text": ""}`, `{}`, `{"text": "hi", "pace": "slow"}`,
			`{"text": "hi\u001b"}`, `{"text": "hi\ud800"}`, `{"text": "hi\ufffd"}`, `{"text": "hi\u007f"}`}},
		{"computer", `{"action": "click", "x": 1, "y": 2}`, []string{`{"action": "click", "x": "1", "y": 2}`, `{"action": "type", "text": ""}`, `{"action": "click", "x": 1, "y": 2, "button": "side"}`,
			`{"action": "type", "text": "hi\u001b"}`, `{"action": "type", "text": "hi\ud800"}`, `{"action": "open", "url": "ftp://example.com/"}`}},
	}
	page := []route{
		{"act", `{"steps": [{"action": "press", "keys": "Enter"}]}`, []string{`{"steps": "press"}`}},
		{"navigate", `{"url": "https://example.com"}`, []string{`{"url": 42}`}},
		{"tabs", `{"action": "new"}`, []string{`{"action": 1}`}},
	}
	f := newFixture(t)
	v := f.start(f.all, "google", "visual")
	a := f.start(f.all, "github", "api")
	refused := func(sessionID string, rt route, id, body string, want int) {
		t.Helper()
		before := f.inputCount()
		if r := f.send(f.srv, f.all, sessionID, rt.tool, id, body); r.code != want || r.replayed {
			t.Fatalf("%s %s %s: %d %s", rt.tool, id, body, r.code, r.raw)
		}
		if st := f.receipt(f.all, sessionID, id); st != "" {
			t.Fatalf("%s %s %s kept a receipt: %s", rt.tool, id, body, st)
		}
		if n := f.inputCount() - before; n != 0 {
			t.Fatalf("%s %s %s: %d inputs reached the browser", rt.tool, id, body, n)
		}
	}
	runs := func(sessionID string, rt route, id string) {
		t.Helper()
		before := f.inputCount()
		r := f.send(f.srv, f.all, sessionID, rt.tool, id, rt.body)
		if r.replayed {
			t.Fatalf("%s %s at last: replayed %s", rt.tool, id, r.raw)
		}
		switch st := f.receipt(f.all, sessionID, id); {
		case sessionID == v.ID && (st != "ok" || f.inputCount() != before+1):
			t.Fatalf("%s %s at last: %s, %d inputs", rt.tool, id, st, f.inputCount()-before)
		case sessionID == a.ID && st != "error":
			t.Fatalf("%s %s at last: %s", rt.tool, id, st)
		}
	}
	all := append(append([]route{}, visual...), page...)
	for i, rt := range all {
		sid := v.ID
		if i >= len(visual) {
			sid = a.ID
		}
		id := "rel-" + rt.tool
		refused(sid, rt, id, `{"oops"`, http.StatusBadRequest)   // bad JSON
		refused(sid, rt, id, rt.body+"}", http.StatusBadRequest) // more after the JSON
		for _, bad := range rt.bad {
			refused(sid, rt, id, bad, http.StatusBadRequest)
		}
		if r := f.send(f.srv, f.all, "nope", rt.tool, id, rt.body); r.code != http.StatusNotFound { // unknown session
			t.Fatalf("%s on no session: %d", rt.tool, r.code)
		}
		if sid == a.ID { // the wrong mode
			refused(v.ID, rt, id, rt.body, http.StatusConflict)
		}
		// A person has the browser (a handoff: Take control would wait
		// out the agent's last few seconds of input).
		if _, err := f.m.SetNeedsHelp(sid, true); err != nil {
			t.Fatal(err)
		}
		refused(sid, rt, id, rt.body, http.StatusConflict)
		if _, err := f.m.SetNeedsHelp(sid, false); err != nil {
			t.Fatal(err)
		}
		if sid == v.ID { // another input is under way
			slow := make(chan reply, 1)
			before := f.inputCount()
			go func() { slow <- f.send(f.srv, f.all, v.ID, "type", "", `{"text": "typed slowly"}`) }()
			waitFor(t, func() bool { return f.inputCount() == before+1 })
			refused(sid, rt, id, rt.body, http.StatusConflict)
			if _, err := f.m.SetNeedsHelp(v.ID, true); err != nil {
				t.Fatal(err)
			}
			<-slow
			if _, err := f.m.SetNeedsHelp(v.ID, false); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return f.lastInput() == "cut short" })
		}
		runs(sid, rt, id)
	}
}

// recent_calls gives an entry as the web UI shows it: scrubbed with the
// secrets as they are now, then cut to 400 characters. One too long to
// store whole is a fixed summary, so a secret registered later can't be
// left cut across its end, where no scrub would match it.
func TestRecentCallsAreScrubbedThenCut(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	tok, err := f.tokens.Authenticate(context.Background(), f.all)
	if err != nil {
		t.Fatal(err)
	}
	ctx := sessions.WithCall(context.Background(), f.m.NewCall(tok.ID, tok.Name, "click"))
	// Under 2 KiB, the secret within the first 400 characters.
	f.m.NoteFor(ctx, s.ID, tok.Name, "Uploaded "+filledSecret+" "+strings.Repeat("x", 1500))
	// Over 2 KiB, the secret across byte 2048.
	f.m.NoteFor(ctx, s.ID, tok.Name, "Uploaded "+strings.Repeat("b", 2025)+filledSecret)
	if err := f.m.RegisterSecret(s.ID, filledSecret); err != nil {
		t.Fatal(err)
	}
	var st sessionStatusJSON
	f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID, "", &st)
	if len(st.RecentCalls) != 3 || st.RecentCalls[2].What != "Started the browser" {
		t.Fatalf("recent_calls: %+v", st.RecentCalls)
	}
	for _, c := range st.RecentCalls {
		if strings.Contains(c.What, filledSecret[:7]) {
			t.Errorf("a secret's start in %q", c.What)
		}
	}
	if long := st.RecentCalls[0].What; !strings.HasPrefix(long, "An entry too long to keep") {
		t.Errorf("over 2 KiB: %q", long)
	}
	short := st.RecentCalls[1].What
	if n := utf8.RuneCountInString(short); n != 400 || !strings.HasPrefix(short, "Uploaded <hidden> xxx") || !strings.HasSuffix(short, "…") {
		t.Errorf("under 2 KiB: %d characters, %q", n, short)
	}
}

// A slow /type with a call id, cut by its token's revocation, answers
// 401 and its receipt ends interrupted (PROJ-166, with PROJ-156).
func TestRevokedTypeWithACallIDEndsInterrupted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v := f.start(f.all, "google", "visual")
	tok, secret, err := f.tokens.Create(ctx, "doomed", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	slow := make(chan reply, 1)
	go func() { slow <- f.send(f.srv, secret, v.ID, "type", "r1", `{"text": "typed slowly"}`) }()
	waitFor(t, func() bool { return strings.HasPrefix(f.lastInput(), "/input/type") })
	if err := f.tokens.Revoke(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	f.api.checkRevocations(ctx)
	if r := <-slow; r.code != http.StatusUnauthorized || !strings.Contains(r.body.Error, "input broke off: the agent token was revoked") {
		t.Fatalf("the cut type: %d %s", r.code, r.raw)
	}
	rc, ok := f.m.CallReceipt(v.ID, tok.ID, "r1")
	if !ok || rc.State != sessions.CallInterrupted {
		t.Fatalf("receipt = %+v, %v", rc, ok)
	}
}
