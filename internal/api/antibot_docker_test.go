//go:build docker

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// TestVisualModeLooksLikeAPerson is PROJ-15's check: a page opened and
// driven in Visual mode, through the screen and xdotool only, sees no
// automation, and nothing is attached to it over CDP. API mode shows what
// Visual mode avoids: an attached client, and a page left unfocused.
func TestVisualModeLooksLikeAPerson(t *testing.T) {
	a := newRealAPI(t)
	det := browsertest.NewDetector(t, browsertest.SiteHost(t))
	// Exiting in Germany, so the language checks below aren't en-US's.
	a.m.SetGeoURL(browsertest.NewSite(t, browsertest.SiteHost(t)).URL + "/geo")
	name := fmt.Sprintf("b%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "visual"}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	a.allowLocalNetwork(t, name)

	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || s.Mode != "visual" {
		t.Fatalf("start: %d %+v", code, s)
	}
	// Open the page as a person would: the address bar, the URL, Enter.
	for _, body := range []string{
		`{"action": "key", "keys": "ctrl+l"}`,
		fmt.Sprintf(`{"action": "type", "text": %q}`, det.URL),
		`{"action": "key", "keys": "Return"}`,
	} {
		if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/computer", body, nil); code != http.StatusNoContent && code != http.StatusOK {
			t.Fatalf("%s: %d", body, code)
		}
	}
	vis := det.Report(t)
	t.Logf("Visual mode: %+v", vis)
	if vis.Webdriver || vis.CDPRuntime || len(vis.AutomationGlobals) > 0 {
		t.Errorf("Visual mode looks automated: webdriver %t, CDP Runtime %t, globals %v", vis.Webdriver, vis.CDPRuntime, vis.AutomationGlobals)
	}
	// Enter in the address bar hands the focus to the page, as for a person.
	if !vis.HasFocus {
		t.Error("in Visual mode the page isn't focused")
	}
	// Known and not asserted: often, in both modes, the page's first script
	// sees a 0x0 window. A window manager didn't change that (PROJ-29); its
	// cause is being checked in PROJ-65. See images/chromium/ANTI-BOT.md.
	t.Logf("Visual mode: window %dx%d as the page's first script ran", vis.OuterWidth, vis.OuterHeight)
	looksLikeChrome(t, vis)
	// And from the browser's side: no client is attached to the page.
	for _, tg := range targets(t, a.m.Layout().ControlSocket(name)) {
		if tg.Type == "page" && tg.Attached {
			t.Errorf("in Visual mode, a CDP client is attached to %s", tg.URL)
		}
	}
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}

	// API mode, driven the way CDP libraries do.
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q, "mode": "api"}`, name), &s); code != http.StatusCreated {
		t.Fatalf("API start: %d", code)
	}
	p := browsertest.PageVia(t, strings.Replace(s.CDPURL, "ws://", "http://", 1)+"/json/list", a.header())
	defer p.Close()
	p.Call("Runtime.enable", nil)
	p.Call("Page.navigate", map[string]any{"url": det.URL})
	api := det.Report(t)
	t.Logf("API mode: %+v", api)
	if api.Webdriver || len(api.AutomationGlobals) > 0 {
		t.Errorf("API mode shows webdriver %t, globals %v", api.Webdriver, api.AutomationGlobals)
	}
	// Page.navigate leaves the keyboard focus in the address bar, so
	// the page isn't focused until something clicks it.
	t.Logf("API mode: page focused %t", api.HasFocus)
	looksLikeChrome(t, api)
	// The controls: what Visual mode avoids shows here, so the checks
	// above mean something.
	attached := false
	for _, tg := range targets(t, a.m.Layout().ControlSocket(name)) {
		attached = attached || (tg.Type == "page" && tg.Attached)
	}
	if !attached {
		t.Error("an attached CDP client doesn't show in the targets; the Visual-mode check proves nothing")
	}
}

// looksLikeChrome checks the signals that give headless or a stripped
// browser away, which hold in both modes.
func looksLikeChrome(t *testing.T, s browsertest.Signals) {
	t.Helper()
	if strings.Contains(s.UserAgent, "Headless") || strings.Contains(strings.Join(s.Brands, ","), "Headless") {
		t.Errorf("headless user agent: %q %v", s.UserAgent, s.Brands)
	}
	if s.Plugins == 0 || len(s.Languages) == 0 || !s.Chrome {
		t.Errorf("plugins %d, languages %v, window.chrome %t", s.Plugins, s.Languages, s.Chrome)
	}
	if s.OuterWidthAfterLoad == 0 || s.OuterHeightAfterLoad == 0 {
		t.Errorf("window %dx%d once loaded, as headless has", s.OuterWidthAfterLoad, s.OuterHeightAfterLoad)
	}
	// A taskbar takes its strip of the screen, as on a person's desktop,
	// and the window fills the rest.
	if s.AvailHeight >= s.ScreenHeight {
		t.Errorf("screen %dx%d, available %dx%d: no taskbar, as headless has", s.ScreenWidth, s.ScreenHeight, s.AvailWidth, s.AvailHeight)
	}
	if s.OuterWidthAfterLoad != s.AvailWidth || s.OuterHeightAfterLoad != s.AvailHeight {
		t.Errorf("window %dx%d once loaded, want the available %dx%d", s.OuterWidthAfterLoad, s.OuterHeightAfterLoad, s.AvailWidth, s.AvailHeight)
	}
	if s.NotificationPermission == "denied" && s.PermissionQuery == "prompt" {
		t.Error("notification permissions disagree, as in headless Chrome")
	}
	if s.WebGLRenderer == "" {
		t.Errorf("no WebGL: %s", s.WebGLVendor)
	}
	// Intl takes Chromium's interface language, which is just "de".
	if len(s.Languages) == 0 || s.Languages[0] != "de-DE" || !strings.HasPrefix(s.IntlLocale, "de") || s.TimeZone != "Europe/Berlin" {
		t.Errorf("exiting in Germany: languages %v, Intl %s, time zone %s", s.Languages, s.IntlLocale, s.TimeZone)
	}
}

type target struct {
	Type, URL string
	Attached  bool
}

// attachedPages is a browsertest.Watch's Attached for a session's
// browser: the pages something is attached to.
func attachedPages(t *testing.T, sock string) func() []string {
	return func() []string {
		var out []string
		for _, tg := range targets(t, sock) {
			if tg.Type == "page" && tg.Attached {
				out = append(out, tg.URL)
			}
		}
		return out
	}
}

// targets lists the browser's targets over a browser-level CDP connection
// on the control socket, which attaches to no page.
func targets(t *testing.T, sock string) []target {
	t.Helper()
	control := browsertest.ControlClient(sock)
	resp, err := control.Get("http://control/cdp/json/version")
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ WebSocketDebuggerURL string }
	err = json.NewDecoder(resp.Body).Decode(&v)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := v.WebSocketDebuggerURL[strings.Index(v.WebSocketDebuggerURL, "/devtools/"):]
	//nolint:bodyclose // after an upgrade the connection owns the body
	conn, _, err := websocket.Dial(ctx, "ws://control/cdp"+path, &websocket.DialOptions{HTTPClient: control})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Target.getTargets"}`)); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			ID     int
			Result struct {
				TargetInfos []struct {
					Type, URL string
					Attached  bool
				}
			}
		}
		if json.Unmarshal(raw, &r) == nil && r.ID == 1 {
			var out []target
			for _, ti := range r.Result.TargetInfos {
				out = append(out, target(ti))
			}
			return out
		}
	}
}

// TestVisualInputLooksHuman is PROJ-113's check in Visual mode: what a
// page's own script records of a click and of typing looks like a hand,
// not like xdotool's jumps and 12 ms keystrokes.
func TestVisualInputLooksHuman(t *testing.T) {
	a := newRealAPI(t)
	det := browsertest.NewDetector(t, browsertest.SiteHost(t))
	name := fmt.Sprintf("h%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "visual"}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	a.allowLocalNetwork(t, name)
	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || s.Mode != "visual" {
		t.Fatalf("start: %d %+v", code, s)
	}
	computer := func(body string) {
		t.Helper()
		if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/computer", body, nil); code != http.StatusNoContent {
			t.Fatalf("%s: %d", body, code)
		}
	}
	sw, sh := a.screenSize(t, s.ID)
	for _, body := range []string{
		`{"action": "key", "keys": "ctrl+l", "pace": "instant"}`,
		fmt.Sprintf(`{"action": "type", "text": %q, "pace": "instant"}`, det.InputURL()),
		`{"action": "key", "keys": "Return", "pace": "instant"}`,
	} {
		computer(body)
	}
	// Where the page is on the screen: an instant click lands exactly
	// where it is sent. Once the page has loaded, its recorder sees it.
	x0, y0 := sw/2, sh*3/4
	var offX, offY float64
	for deadline := time.Now().Add(30 * time.Second); ; {
		computer(fmt.Sprintf(`{"action": "click", "x": %d, "y": %d, "pace": "instant"}`, x0, y0))
		time.Sleep(500 * time.Millisecond)
		if downs := ofType(det.Events(), "mousedown"); len(downs) > 0 {
			offX, offY = float64(x0)-downs[0].X, float64(y0)-downs[0].Y
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the input recorder never loaded")
		}
	}
	var all []browsertest.InputEvent
	take := func() []browsertest.InputEvent {
		ev := det.Events()
		det.ResetEvents()
		all = append(all, ev...)
		return ev
	}
	ups := func(n int) func([]browsertest.InputEvent) bool {
		return func(ev []browsertest.InputEvent) bool { return len(ofType(ev, "mouseup")) >= n }
	}
	time.Sleep(300 * time.Millisecond)
	take()

	// A 400 px click: a glide, not a jump, and a held button.
	ax, bx := sw/2-200, sw/2+200
	computer(fmt.Sprintf(`{"action": "click", "x": %d, "y": %d}`, ax, y0))
	det.WaitEvents(t, "the first click", ups(1))
	time.Sleep(300 * time.Millisecond)
	take()
	computer(fmt.Sprintf(`{"action": "click", "x": %d, "y": %d}`, bx, y0))
	det.WaitEvents(t, "the 400 px click", ups(1))
	time.Sleep(300 * time.Millisecond)
	looksLikeAHandsClick(t, take())

	// Ten clicks at one point land on more than one pixel, within 40% of
	// 44 px of it.
	for range 10 {
		computer(fmt.Sprintf(`{"action": "click", "x": %d, "y": %d}`, bx, y0))
	}
	det.WaitEvents(t, "ten clicks", ups(10))
	time.Sleep(300 * time.Millisecond)
	ev := take()
	seen := map[[2]float64]bool{}
	for _, d := range ofType(ev, "mousedown") {
		seen[[2]float64{d.X, d.Y}] = true
		if dx, dy := d.X+offX-float64(bx), d.Y+offY-float64(y0); math.Abs(dx) > 18 || math.Abs(dy) > 18 {
			t.Errorf("a click at (%d, %d) landed %v, %v px away", bx, y0, dx, dy)
		}
	}
	if len(seen) < 2 {
		t.Errorf("ten clicks at one point all landed on %v", seen)
	}
	holdsLikeAHand(t, ev)

	// Typing 30 characters into the textarea: a typist's uneven rhythm.
	computer(fmt.Sprintf(`{"action": "click", "x": %d, "y": %d}`, sw/2, int(offY)+40))
	det.WaitEvents(t, "the click into the textarea", ups(1))
	time.Sleep(300 * time.Millisecond)
	take()
	const text = "the quick brown fox jumps over"
	computer(fmt.Sprintf(`{"action": "type", "text": %q}`, text))
	ev = det.WaitEvents(t, "30 keystrokes", func(ev []browsertest.InputEvent) bool { return len(ofType(ev, "keyup")) >= len(text) })
	typesLikeATypist(t, ev, text)
	take()

	for _, e := range all {
		if !e.Trusted {
			t.Fatalf("an untrusted event: %+v", e)
		}
	}
	// And nothing attached to the page over CDP all along.
	for _, tg := range targets(t, a.m.Layout().ControlSocket(name)) {
		if tg.Type == "page" && tg.Attached {
			t.Errorf("in Visual mode, a CDP client is attached to %s", tg.URL)
		}
	}
}

// screenSize is a session's screen, from a screenshot's headers.
func (a agent) screenSize(t *testing.T, id string) (int, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, a.base+"/api/sessions/"+id+"/screenshot", nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	w, _ := strconv.Atoi(resp.Header.Get("X-Screen-Width"))
	h, _ := strconv.Atoi(resp.Header.Get("X-Screen-Height"))
	if resp.StatusCode != http.StatusOK || w == 0 || h == 0 {
		t.Fatalf("screenshot: %d, %dx%d", resp.StatusCode, w, h)
	}
	return w, h
}

func ofType(ev []browsertest.InputEvent, typ string) []browsertest.InputEvent {
	var out []browsertest.InputEvent
	for _, e := range ev {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// looksLikeAHandsClick checks a long click's events: at least 10 moves
// before the press, not on one line, and a button held 60-200 ms.
func looksLikeAHandsClick(t *testing.T, ev []browsertest.InputEvent) {
	t.Helper()
	var moves []browsertest.InputEvent
	for _, e := range ev {
		if e.Type == "mousedown" {
			break
		}
		if e.Type == "mousemove" {
			moves = append(moves, e)
		}
	}
	if len(moves) < 10 {
		t.Fatalf("%d mousemoves before the press of a 400 px click, want 10 or more", len(moves))
	}
	a, b := moves[0], moves[len(moves)-1]
	dx, dy := b.X-a.X, b.Y-a.Y
	worst := 0.0
	for _, m := range moves {
		worst = math.Max(worst, math.Abs(dy*(m.X-a.X)-dx*(m.Y-a.Y))/math.Hypot(dx, dy))
	}
	if worst <= 2 {
		t.Errorf("the moves stray at most %.1f px from a straight line", worst)
	}
	t.Logf("%d moves, %.0f px, at most %.1f px off the line", len(moves), math.Hypot(dx, dy), worst)
	holdsLikeAHand(t, ev)
}

// holdsLikeAHand checks every press is released 60-200 ms later.
func holdsLikeAHand(t *testing.T, ev []browsertest.InputEvent) {
	t.Helper()
	var down *browsertest.InputEvent
	for i, e := range ev {
		switch e.Type {
		case "mousedown":
			down = &ev[i]
		case "mouseup":
			if down == nil {
				continue
			}
			if held := e.T - down.T; held < 60 || held > 200 {
				t.Errorf("the button was held %.0f ms", held)
			}
			down = nil
		}
	}
}

// typesLikeATypist checks the keystrokes typed text, with gaps between
// key-downs that vary (coefficient of variation over 0.25) around a
// typist's mean (0.12-0.45 s).
func typesLikeATypist(t *testing.T, ev []browsertest.InputEvent, text string) {
	t.Helper()
	downs := ofType(ev, "keydown")
	keys := ""
	for _, d := range downs {
		keys += d.Key
	}
	if keys != text {
		t.Fatalf("the page saw keys %q, want %q", keys, text)
	}
	var gaps []float64
	for i := 1; i < len(downs); i++ {
		gaps = append(gaps, (downs[i].T-downs[i-1].T)/1000)
	}
	mean, sd := 0.0, 0.0
	for _, g := range gaps {
		mean += g
	}
	mean /= float64(len(gaps))
	for _, g := range gaps {
		sd += (g - mean) * (g - mean)
	}
	sd = math.Sqrt(sd / float64(len(gaps)))
	t.Logf("key-down gaps: mean %.3f s, SD %.3f s, CV %.2f", mean, sd, sd/mean)
	if sd/mean <= 0.25 || mean < 0.12 || mean > 0.45 {
		t.Errorf("key-down gaps: mean %.3f s, CV %.2f; want a typist's (0.12-0.45 s, CV over 0.25)", mean, sd/mean)
	}
}
