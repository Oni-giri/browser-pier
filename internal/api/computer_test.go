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
	"strings"
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

func TestComputerWhileAPersonDrives(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	if err := f.m.TakeControl(s.ID); err != nil {
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
