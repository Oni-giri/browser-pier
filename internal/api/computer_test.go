package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// screenPNG is the fake browser's full-size screen: each pixel's colour
// says where it is, so a crop can be checked.
var screenPNG = func() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 1440, 900))
	for y := range 900 {
		for x := range 1440 {
			img.Set(x, y, pixelAt(x, y))
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}()

func pixelAt(x, y int) color.NRGBA {
	return color.NRGBA{uint8(x % 256), uint8(y % 256), uint8(x/256<<4 | y/256), 255}
}

// computer posts one action and returns the status, headers and body.
func (f *fixture) computer(id, body string) (int, http.Header, []byte) {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/sessions/"+id+"/computer", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.all)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

func (f *fixture) lastInput() string {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	if len(f.b.inputs) == 0 {
		return ""
	}
	return f.b.inputs[len(f.b.inputs)-1]
}

func TestComputerForwardsInput(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	for body, want := range map[string]string{
		`{"action": "click", "x": 10, "y": 20}`:                                  `/input/click {"x":10,"y":20}`,
		`{"action": "click", "x": 0, "y": 0, "button": "right", "double": true}`: `/input/click {"x":0,"y":0,"button":"right","double":true}`,
		`{"action": "type", "text": "hello"}`:                                    `/input/type {"text":"hello"}`,
		`{"action": "scroll", "x": 5, "y": 6, "dy": 3}`:                          `/input/scroll {"x":5,"y":6,"dx":0,"dy":3}`,
		`{"action": "key", "keys": ["ctrl+l", "Return"]}`:                        `/input/key {"keys":["ctrl+l","Return"]}`,
		`{"action": "key", "keys": "ctrl+shift+t"}`:                              `/input/key {"keys":"ctrl+shift+t"}`,
		`{"action": "drag", "from": {"x": 1, "y": 2}, "to": {"x": 3, "y": 4}}`:   `/input/drag {"from":{"x":1,"y":2},"to":{"x":3,"y":4}}`,
		// The pace goes along when it's given.
		`{"action": "click", "x": 1, "y": 2, "pace": "instant"}`:                                  `/input/click {"x":1,"y":2,"pace":"instant"}`,
		`{"action": "type", "text": "a", "pace": "human"}`:                                        `/input/type {"text":"a","pace":"human"}`,
		`{"action": "scroll", "x": 5, "y": 6, "dy": 3, "pace": "instant"}`:                        `/input/scroll {"x":5,"y":6,"dx":0,"dy":3,"pace":"instant"}`,
		`{"action": "key", "keys": "Tab", "pace": "instant"}`:                                     `/input/key {"keys":"Tab","pace":"instant"}`,
		`{"action": "drag", "from": {"x": 1, "y": 2}, "to": {"x": 3, "y": 4}, "pace": "instant"}`: `/input/drag {"from":{"x":1,"y":2},"to":{"x":3,"y":4},"pace":"instant"}`,
	} {
		if code, _, raw := f.computer(s.ID, body); code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", body, code, raw)
		}
		if got := f.lastInput(); got != want {
			t.Errorf("%s: forwarded %s, want %s", body, got, want)
		}
	}

	f.b.mu.Lock()
	sent := len(f.b.inputs)
	f.b.mu.Unlock()
	for _, body := range []string{
		`{"action": "jump"}`, `{}`, `{"action": "click", "x": 1}`, `{"action": "scroll", "dy": 1}`,
		`{"action": "key"}`, `{"action": "drag", "from": {"x": 1, "y": 2}}`, `{"action": "click", "x": 1, "y": 1, "extra": 1}`,
		`{"action": "click", "x": 1, "y": 1, "screenshot": true, "width": 8}`, `not json`,
		`{"action": "click", "x": 1, "y": 1, "pace": "slow"}`, `{"action": "type", "text": "a", "pace": 1}`,
	} {
		if code, _, raw := f.computer(s.ID, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, code, raw)
		}
	}
	f.b.mu.Lock()
	if len(f.b.inputs) != sent {
		t.Errorf("refused actions reached the browser: %q", f.b.inputs[sent:])
	}
	f.b.mu.Unlock()
	// The browser's own refusal comes back as it is.
	if code, _, raw := f.computer(s.ID, `{"action": "scroll", "x": 99999, "y": 1, "dy": 1}`); code != http.StatusBadRequest || !strings.Contains(string(raw), "outside") {
		t.Fatalf("off screen: %d %s", code, raw)
	}
}

func TestComputerScreenshotAfterAction(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	code, h, raw := f.computer(s.ID, `{"action": "scroll", "x": 5, "y": 6, "dy": -2, "screenshot": true, "width": 640}`)
	if code != http.StatusOK || string(raw) != "PNG:640" || h.Get("Content-Type") != "image/png" || h.Get("X-Screen-Width") != "1440" || h.Get("X-Screen-Height") != "900" {
		t.Fatalf("scroll with a screenshot: %d %q %v", code, raw, h)
	}
	if !strings.HasPrefix(f.lastInput(), "/input/scroll ") {
		t.Fatalf("the scroll didn't run: %q", f.lastInput())
	}
	if code, _, raw := f.computer(s.ID, `{"action": "wait", "ms": 1, "screenshot": true}`); code != http.StatusOK || !bytes.Equal(raw, screenPNG) {
		t.Fatalf("wait with a screenshot: %d", code)
	}
}

func TestComputerWait(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	start := time.Now()
	if code, _, raw := f.computer(s.ID, `{"action": "wait", "ms": 50}`); code != http.StatusNoContent {
		t.Fatalf("wait: %d %s", code, raw)
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Fatalf("waited %v, want 50ms", d)
	}
	if code, _, _ := f.computer(s.ID, `{"action": "wait", "ms": -1}`); code != http.StatusBadRequest {
		t.Fatalf("negative wait: %d", code)
	}
	// A huge wait is capped, not wrapped round to nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.srv.URL+"/api/sessions/"+s.ID+"/computer", strings.NewReader(`{"action": "wait", "ms": 1000000000000000}`))
	req.Header.Set("Authorization", "Bearer "+f.all)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a huge wait answered at once: %d", resp.StatusCode)
	}
}

func TestComputerZoom(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	for body, want := range map[string]image.Rectangle{
		`{"action": "zoom", "x": 300, "y": 280, "w": 40, "h": 30}`:    image.Rect(300, 280, 340, 310),
		`{"action": "zoom", "x": 1430, "y": 890, "w": 100, "h": 100}`: image.Rect(1430, 890, 1440, 900), // cut at the edge
		`{"action": "zoom", "x": 0, "y": 0, "w": 99999999, "h": 1}`:   image.Rect(0, 0, 1440, 1),
	} {
		code, h, raw := f.computer(s.ID, body)
		if code != http.StatusOK || h.Get("Content-Type") != "image/png" || h.Get("X-Screen-Width") != "1440" || h.Get("X-Screen-Height") != "900" {
			t.Fatalf("%s: %d %v %.100s", body, code, h, raw)
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if img.Bounds().Size() != want.Size() {
			t.Fatalf("%s: %v, want %v", body, img.Bounds().Size(), want.Size())
		}
		for _, p := range []image.Point{{0, 0}, {want.Dx() - 1, want.Dy() - 1}} {
			got := color.NRGBAModel.Convert(img.At(img.Bounds().Min.X+p.X, img.Bounds().Min.Y+p.Y))
			if wantC := pixelAt(want.Min.X+p.X, want.Min.Y+p.Y); got != wantC {
				t.Fatalf("%s: pixel %v is %v, want %v", body, p, got, wantC)
			}
		}
	}
	for _, body := range []string{
		`{"action": "zoom", "x": 1440, "y": 0, "w": 10, "h": 10}`, `{"action": "zoom", "x": -1, "y": 0, "w": 10, "h": 10}`,
		`{"action": "zoom", "x": 0, "y": 0, "w": 0, "h": 10}`, `{"action": "zoom", "w": 10, "h": 10}`,
	} {
		if code, _, raw := f.computer(s.ID, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, code, raw)
		}
	}
}

func TestComputerDuringAHandoff(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	// Agents can look and wait, but not touch.
	if code, _, _ := f.computer(s.ID, `{"action": "scroll", "x": 1, "y": 1, "dy": 1}`); code != http.StatusConflict {
		t.Fatalf("scroll during a handoff: %d", code)
	}
	if code, _, _ := f.computer(s.ID, `{"action": "wait", "ms": 1}`); code != http.StatusNoContent {
		t.Fatalf("wait during a handoff: %d", code)
	}
	if code, _, _ := f.computer(s.ID, `{"action": "zoom", "x": 1, "y": 1, "w": 5, "h": 5}`); code != http.StatusOK {
		t.Fatalf("zoom during a handoff: %d", code)
	}
}

// A handoff doesn't wait for a long human-paced action to finish: it
// breaks it off at once.
func TestHandoffInterruptsALongAction(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	type answer struct {
		code int
		raw  []byte
	}
	got := make(chan answer, 1)
	go func() {
		code, _, raw := f.computer(s.ID, `{"action": "type", "text": "typed slowly"}`)
		got <- answer{code, raw}
	}()
	for deadline := time.Now().Add(5 * time.Second); f.lastInput() == ""; {
		if time.Now().After(deadline) {
			t.Fatal("the type never reached the browser")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the handoff waited %v for the type", d)
	}
	a := <-got
	if a.code != http.StatusConflict || !strings.Contains(string(a.raw), "handoff") {
		t.Fatalf("the type cut short answered %d %s", a.code, a.raw)
	}
	for deadline := time.Now().Add(5 * time.Second); f.lastInput() != "cut short"; {
		if time.Now().After(deadline) {
			t.Fatalf("the browser never saw the type cut short: %q", f.lastInput())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The log says it was cut short.
	if got := strings.Join(f.whats(s.ID), "\n"); !strings.HasSuffix(got, "claude: Typing interrupted") {
		t.Fatalf("activity:\n%s", got)
	}
}

// One input at a time: a second while a long one is under way is refused.
func TestComputerOneInputAtATime(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	got := make(chan int, 1)
	go func() {
		code, _, _ := f.computer(s.ID, `{"action": "type", "text": "typed slowly"}`)
		got <- code
	}()
	for deadline := time.Now().Add(5 * time.Second); f.lastInput() == ""; {
		if time.Now().After(deadline) {
			t.Fatal("the type never reached the browser")
		}
		time.Sleep(10 * time.Millisecond)
	}
	code, _, raw := f.computer(s.ID, `{"action": "key", "keys": "Return"}`)
	if code != http.StatusConflict || !strings.Contains(string(raw), "another input is still running") {
		t.Fatalf("a key during the type: %d %s", code, raw)
	}
	if strings.Contains(f.lastInput(), "Return") {
		t.Fatalf("the key reached the browser: %q", f.lastInput())
	}
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	<-got
}

func TestComputerWhileAPersonDrives(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	if err := f.m.TakeControl(s.ID, "person"); err != nil {
		t.Fatal(err)
	}
	var e map[string]string
	code, _, raw := f.computer(s.ID, `{"action": "key", "keys": "Return"}`)
	if err := json.Unmarshal(raw, &e); code != http.StatusConflict || err != nil || !strings.Contains(e["error"], "live view") {
		t.Fatalf("key while a person drives: %d %s", code, raw)
	}
	if f.lastInput() != "" {
		t.Fatalf("the key reached the browser: %q", f.lastInput())
	}
}

func TestComputerActivityLog(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	for _, body := range []string{
		`{"action": "scroll", "x": 5, "y": 6, "dy": 3}`,
		`{"action": "key", "keys": ["ctrl+l", "Return"]}`,
		`{"action": "key", "keys": ["s", "e", "c", "r", "e", "t"]}`,
		`{"action": "drag", "from": {"x": 1, "y": 2}, "to": {"x": 3, "y": 4}}`,
		`{"action": "wait", "ms": 20}`,
		`{"action": "zoom", "x": 1, "y": 1, "w": 5, "h": 5}`,
		`{"action": "type", "text": "hunter2", "screenshot": true}`,
	} {
		if code, _, raw := f.computer(s.ID, body); code != http.StatusNoContent && code != http.StatusOK {
			t.Fatalf("%s: %d %s", body, code, raw)
		}
	}
	got := strings.Join(f.whats(s.ID), "\n")
	want := strings.Join([]string{
		"claude: Started the browser", "claude: Scrolled at 5, 6", "claude: Pressed ctrl+l, Return", "claude: Pressed 6 keys",
		"claude: Dragged from 1, 2 to 3, 4", "claude: Waited 0.02 s", "claude: Zoomed into a region", "claude: Typed 7 characters",
	}, "\n")
	if got != want {
		t.Fatalf("activity:\n%s\nwant:\n%s", got, want)
	}
}

func TestDescribeKeys(t *testing.T) {
	for keys, want := range map[string]string{
		`"ctrl+l"`:                  "Pressed ctrl+l",
		`"Ctrl + Shift + T"`:        "Pressed Ctrl+Shift+T",
		`["Tab", "shift+Tab"]`:      "Pressed Tab, shift+Tab",
		`["PageDown", "F5", "Esc"]`: "Pressed PageDown, F5, Esc",
		`"a"`:                       "Pressed 1 key",
		`"shift+a"`:                 "Pressed 1 key",
		`["ctrl+a", "x"]`:           "Pressed 2 keys",
		`["U0031", "Return"]`:       "Pressed 2 keys",
	} {
		if got := describeKeys(json.RawMessage(keys)); got != want {
			t.Errorf("%s: %q, want %q", keys, got, want)
		}
	}
}

// An action that ran isn't reported as failed because the screenshot
// after it did: the agent would do it again.
func TestComputerActionStandsWhenItsScreenshotFails(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "github", "visual")
	f.b.mu.Lock()
	f.b.noScreen = true
	f.b.mu.Unlock()
	r, _ := f.raw(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/computer", strings.NewReader(`{"action": "click", "x": 10, "y": 10, "screenshot": true}`), "application/json")
	if r.StatusCode != http.StatusNoContent || r.Header.Get("X-Screenshot-Error") == "" {
		t.Fatalf("click with a failing screenshot: %d %q", r.StatusCode, r.Header.Get("X-Screenshot-Error"))
	}
}

// A screenshot over the cap isn't cut off and passed off as the screen:
// the action stands, and X-Screenshot-Error says why there is none.
func TestComputerScreenshotOverTheCap(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "github", "visual")
	old := maxScreenshot
	maxScreenshot = 4
	t.Cleanup(func() { maxScreenshot = old })
	r, _ := f.raw(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/computer", strings.NewReader(`{"action": "click", "x": 10, "y": 10, "screenshot": true, "width": 640}`), "application/json")
	if r.StatusCode != http.StatusNoContent || !strings.Contains(r.Header.Get("X-Screenshot-Error"), "screenshot is over") {
		t.Fatalf("click with a screenshot over the cap: %d %q", r.StatusCode, r.Header.Get("X-Screenshot-Error"))
	}
	// At the cap, it is the screen.
	maxScreenshot = int64(len("PNG:640"))
	code, _, raw := f.computer(s.ID, `{"action": "click", "x": 10, "y": 10, "screenshot": true, "width": 640}`)
	if code != http.StatusOK || string(raw) != "PNG:640" {
		t.Fatalf("at the cap: %d %q", code, raw)
	}
}

// Revoking a token breaks off its long human-paced type at once, as it
// does its page calls, whether from the UI or the token CLI (the
// revocation watcher) (PROJ-166).
func TestRevokingATokenInterruptsItsType(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.start(f.all, "google", "visual")
	tok, secret, err := f.tokens.Create(ctx, "doomed", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		code int
		raw  []byte
	}
	got := make(chan answer, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/sessions/"+s.ID+"/type", strings.NewReader(`{"text": "typed slowly"}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- answer{0, []byte(err.Error())}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		got <- answer{resp.StatusCode, raw}
	}()
	waitFor(t, func() bool { return f.lastInput() != "" })
	// A check before the revocation leaves it be.
	f.api.checkRevocations(ctx)
	if err := f.tokens.Revoke(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	f.api.checkRevocations(ctx)
	var a answer
	select {
	case a = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the type went on after its token was revoked")
	}
	if a.code != http.StatusUnauthorized || !strings.Contains(string(a.raw), "input broke off: the agent token was revoked") {
		t.Fatalf("the type cut short answered %d %s", a.code, a.raw)
	}
	waitFor(t, func() bool { return f.lastInput() == "cut short" })
	if got := strings.Join(f.whats(s.ID), "\n"); !strings.HasSuffix(got, "doomed: Typing interrupted") {
		t.Fatalf("activity:\n%s", got)
	}
}

// gatedBody is a request body that says when the handler starts reading
// it, then waits to be let go.
type gatedBody struct {
	r       io.Reader
	reading chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedBody) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.reading) })
	<-g.release
	return g.r.Read(p)
}

// A token revoked after its request was let in, while the body is still
// being read (before the input is registered, where the revocation has
// nothing to break off), sends no input: it answers 401 (PROJ-166).
func TestATokenRevokedBeforeItsInputBeginsSendsNone(t *testing.T) {
	for _, route := range []string{"type", "click", "computer"} {
		t.Run(route, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			s := f.start(f.all, "google", "visual")
			tok, secret, err := f.tokens.Create(ctx, "doomed", true, nil)
			if err != nil {
				t.Fatal(err)
			}
			body := map[string]string{"type": `{"text": "hi"}`, "click": `{"x": 1, "y": 2}`, "computer": `{"action": "click", "x": 1, "y": 2}`}[route]
			g := &gatedBody{r: strings.NewReader(body), reading: make(chan struct{}), release: make(chan struct{})}
			req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+s.ID+"/"+route, g)
			req.Header.Set("Authorization", "Bearer "+secret)
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				f.srv.Config.Handler.ServeHTTP(rec, req)
			}()
			<-g.reading
			// As the UI revokes: the token goes, then its connections.
			if err := f.tokens.Revoke(ctx, tok.ID); err != nil {
				t.Fatal(err)
			}
			f.m.DisconnectToken(tok.ID)
			close(g.release)
			<-done
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "revoked") {
				t.Fatalf("answered %d %s", rec.Code, rec.Body)
			}
			if in := f.lastInput(); in != "" {
				t.Fatalf("the browser got %q", in)
			}
		})
	}
}
