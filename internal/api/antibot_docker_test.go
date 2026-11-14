//go:build docker

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
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
