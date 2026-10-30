package browsertest

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// Signals are what a bot-detection script reads from a page: the checks
// public test pages (sannysoft, CreepJS, BotD) make, reported back by the
// page itself so a test needn't touch it over CDP.
type Signals struct {
	// Webdriver is navigator.webdriver.
	Webdriver bool `json:"webdriver"`
	// CDPRuntime: a CDP client has the Runtime domain enabled on the page,
	// seen by console.debug reading an Error's stack for the inspector.
	// Chromium has since closed that leak (it stays false in 156 even with
	// Runtime enabled); kept for older browsers.
	CDPRuntime bool `json:"cdpRuntime"`
	// AutomationGlobals are window properties drivers leave (cdc_…,
	// __playwright…, __puppeteer…).
	AutomationGlobals []string `json:"automationGlobals"`
	UserAgent         string   `json:"userAgent"`
	Brands            []string `json:"brands"`
	Plugins           int      `json:"plugins"`
	Languages         []string `json:"languages"`
	Chrome            bool     `json:"chrome"` // window.chrome exists
	// OuterWidth and OuterHeight are read as the page's first script runs,
	// when headless (and pages opened over CDP) report 0; AfterLoad once
	// it has loaded.
	OuterWidth           int    `json:"outerWidth"`
	OuterHeight          int    `json:"outerHeight"`
	OuterWidthAfterLoad  int    `json:"outerWidthAfterLoad"`
	OuterHeightAfterLoad int    `json:"outerHeightAfterLoad"`
	HasFocus             bool   `json:"hasFocus"`
	ScreenWidth          int    `json:"screenWidth"`
	ScreenHeight         int    `json:"screenHeight"`
	WebGLVendor          string `json:"webglVendor"`
	WebGLRenderer        string `json:"webglRenderer"`
	// NotificationPermission and PermissionQuery disagree (denied and
	// prompt) in headless Chrome.
	NotificationPermission string `json:"notificationPermission"`
	PermissionQuery        string `json:"permissionQuery"`
	HardwareConcurrency    int    `json:"hardwareConcurrency"`
	DeviceMemory           int    `json:"deviceMemory"`
	TimeZone               string `json:"timeZone"`
	Voices                 int    `json:"voices"`
}

// Detector serves a page that collects Signals and posts them back.
type Detector struct {
	URL     string
	reports chan Signals
}

// detectPage runs its checks on load, waits for the console check to be
// read, then reports; it also shows the results, for screenshots.
const detectPage = `<!doctype html><title>Detector</title><pre id="out">checking…</pre><script>
(async () => {
  const s = {};
  s.webdriver = navigator.webdriver === true;
  // Only a CDP client with Runtime enabled serializes console arguments,
  // which reads the error's stack.
  let cdp = false;
  const e = new Error();
  Object.defineProperty(e, 'stack', { get() { cdp = true; return ''; } });
  console.debug(e);
  s.automationGlobals = Object.getOwnPropertyNames(window).filter(k => /^(cdc_|\$cdc_|__playwright|__pw|__puppeteer|__selenium|__webdriver|domAutomation)/.test(k));
  s.userAgent = navigator.userAgent;
  s.brands = (navigator.userAgentData?.brands || []).map(b => b.brand);
  s.plugins = navigator.plugins.length;
  s.languages = [...navigator.languages];
  s.chrome = typeof window.chrome === 'object' && window.chrome !== null;
  s.outerWidth = outerWidth; s.outerHeight = outerHeight;
  s.screenWidth = screen.width; s.screenHeight = screen.height;
  try {
    const gl = document.createElement('canvas').getContext('webgl');
    const ext = gl.getExtension('WEBGL_debug_renderer_info');
    s.webglVendor = gl.getParameter(ext.UNMASKED_VENDOR_WEBGL);
    s.webglRenderer = gl.getParameter(ext.UNMASKED_RENDERER_WEBGL);
  } catch (err) { s.webglVendor = 'none: ' + err; }
  s.notificationPermission = Notification.permission;
  s.permissionQuery = (await navigator.permissions.query({ name: 'notifications' })).state;
  s.hardwareConcurrency = navigator.hardwareConcurrency || 0;
  s.deviceMemory = navigator.deviceMemory || 0;
  s.timeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  await new Promise(r => { speechSynthesis.onvoiceschanged = r; setTimeout(r, 1000); });
  if (document.readyState !== 'complete') await new Promise(r => addEventListener('load', r));
  s.outerWidthAfterLoad = outerWidth; s.outerHeightAfterLoad = outerHeight;
  s.hasFocus = document.hasFocus();
  s.voices = speechSynthesis.getVoices().length;
  s.cdpRuntime = cdp;
  document.getElementById('out').textContent = JSON.stringify(s, null, 2);
  await fetch('/report', { method: 'POST', body: JSON.stringify(s) });
})();
</script>`

// NewDetector serves the detection page on every interface; host is how
// the browser container reaches this machine.
func NewDetector(t *testing.T, host string) *Detector {
	t.Helper()
	d := &Detector{reports: make(chan Signals, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, detectPage)
	})
	mux.HandleFunc("POST /report", func(w http.ResponseWriter, r *http.Request) {
		var s Signals
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&s); err != nil {
			t.Errorf("detector report: %v", err)
			return
		}
		select {
		case d.reports <- s:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // the browser container reaches it over the bridge
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	d.URL = fmt.Sprintf("http://%s:%d/", host, ln.Addr().(*net.TCPAddr).Port)
	return d
}

// Report waits for the page's next report.
func (d *Detector) Report(t *testing.T) Signals {
	t.Helper()
	select {
	case s := <-d.reports:
		return s
	case <-time.After(45 * time.Second):
		t.Fatal("the detection page never reported")
		return Signals{}
	}
}
