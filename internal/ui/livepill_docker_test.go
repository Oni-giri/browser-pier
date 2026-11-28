//go:build docker

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePeer stands in for WebRTC, which this test doesn't need: the live
// view's offer is answered, and its data channel is window.dc, which the
// test sends control messages through as BrowserPier would. It is served
// inside live.js.
const fakePeer = `window.RTCPeerConnection = class {
  constructor() { this.iceGatheringState = "complete"; this.connectionState = "new"; this.localDescription = { sdp: "fake" }; }
  addTransceiver() {}
  createDataChannel() {
    const dc = { readyState: "open", send() {} };
    window.dc = dc;
    setTimeout(() => dc.onopen && dc.onopen(), 0);
    return dc;
  }
  async createOffer() { return { type: "offer", sdp: "fake" }; }
  async setLocalDescription() {}
  async setRemoteDescription() {}
};`

// TestLiveViewPillIsOrangeOnlyWhileHelpIsNeeded runs the live view's own
// script on the profile page, with its status served by the test, and
// reads the pill's class in each state: orange (help), saying "Needs
// you", while the browser needs help and nobody has control, and never
// after, whether it goes idle, an agent drives, or the person takes
// control. orange_test.go only checks the script's text.
func TestLiveViewPillIsOrangeOnlyWhileHelpIsNeeded(t *testing.T) {
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	src, err := os.ReadFile("../webrtc/live.js")
	if err != nil {
		t.Fatal(err)
	}
	// The page's live.js is the real one with the fake peer just after its
	// "use strict" (the page's CSP allows no eval).
	const strict = "\"use strict\";\n"
	if !strings.Contains(string(src), strict) {
		t.Fatal("live.js has no \"use strict\" line to put the fake peer after")
	}
	liveJS := strings.Replace(string(src), strict, strict+fakePeer+"\n", 1)
	f := newFixture(t)
	f.profile("mail")
	if _, _, err := f.m.Start(context.Background(), "mail", ""); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	status, polls := map[string]any{"state": "running"}, 0
	set := func(s map[string]any) int {
		mu.Lock()
		defer mu.Unlock()
		status = s
		return polls
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/live.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = fmt.Fprint(w, liveJS)
	})
	mux.HandleFunc("GET /live/mail/status", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		polls++
		_ = json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("GET /live/mail/ice", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"iceServers": []}`)
	})
	mux.HandleFunc("POST /live/mail/offer", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"sdp": "fake"}`)
	})
	mux.Handle("/", f.srv.Config.Handler)
	base := serveToBrowser(t, mux)

	b := newPhone(t, image)
	b.Call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1440, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	b.Navigate(base + "/login")
	b.Eval(`document.querySelector("#password").value = "the-admin-password"; document.querySelector("#password").form.requestSubmit()`)
	until(t, b, "signing in didn't leave the login page", `location.pathname === "/"`)
	b.Navigate(base + "/profiles/mail")
	until(t, b, "the live view didn't answer its offer", `!!window.dc && document.querySelector("#who").className.startsWith("pill")`)

	orange := `document.querySelector("#who").classList.contains("help")`
	// step serves s and waits until the page has rendered it: two polls
	// after the change, the first of which brought it.
	step := func(what string, s map[string]any, control *bool, want bool) {
		t.Helper()
		from := set(s)
		if control != nil {
			b.Eval(fmt.Sprintf(`window.dc.onmessage({ data: JSON.stringify({ type: "control", on: %t }) })`, *control))
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			n := polls
			mu.Unlock()
			if n >= from+2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the live view stopped polling its status", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if got := b.Eval(orange); got != want {
			t.Errorf("%s: the pill is %q (%q); orange %v, want %v", what, b.Eval(`document.querySelector("#who").className`), b.Eval(`document.querySelector("#who").textContent`), got, want)
		}
		// While help is needed it says so in the design's words.
		if want {
			if got := b.Eval(`document.querySelector("#who").textContent`); got != stHelp.Label {
				t.Errorf("%s: the pill says %q, want %q", what, got, stHelp.Label)
			}
		}
	}
	needsHelp := map[string]any{"state": "needs_help"}
	on, off := true, false
	step("idle", map[string]any{"state": "running"}, nil, false)
	step("needs help", needsHelp, nil, true)
	step("idle after needing help", map[string]any{"state": "running"}, nil, false)
	step("needs help again", needsHelp, nil, true)
	step("an agent driving after needing help", map[string]any{"state": "running", "in_use_by": []string{"claude"}}, nil, false)
	step("needs help a third time", needsHelp, nil, true)
	step("the person took control", map[string]any{"state": "needs_help", "human_control": true}, &on, false)
	step("the person handed back, idle", map[string]any{"state": "running"}, &off, false)
	step("needs help a fourth time", needsHelp, nil, true)
	step("someone else took control", map[string]any{"state": "running", "human_control": true}, nil, false)
	step("needs help a fifth time", needsHelp, nil, true)
	step("stopped after needing help", map[string]any{"state": "stopped"}, nil, false)
	step("starting", map[string]any{"state": "starting"}, nil, false)
}
