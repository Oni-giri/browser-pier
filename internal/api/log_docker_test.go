//go:build docker

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/webrtc"
)

// PROJ-158 on real browsers: a run's log is kept after its browser stops,
// and says why it stopped.

// keptRun waits for the profile's newest kept run to be session id's.
func (a agent) keptRun(t *testing.T, profile, id string) sessions.Run {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		if runs := a.m.Runs(profile); len(runs) > 0 && runs[0].SessionID == id {
			return runs[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s's run wasn't kept: %+v", id, a.m.Runs(profile))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// oldestFirst lists entries' words, with any title, oldest first.
func oldestFirst(entries []sessions.Activity) []string {
	var out []string
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		out = append(out, e.What+map[bool]string{true: ": " + e.Title}[e.Title != ""])
	}
	return out
}

func TestLogSurvivesAStop(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "log")
	const marker = "MARKER-typed-4242"
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/get-signin"})
	if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/click", `{"x": 100, "y": 60}`, nil); code != http.StatusNoContent {
		t.Fatalf("click: %d", code)
	}
	if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/type", `{"text": "`+marker+`"}`, nil); code != http.StatusNoContent {
		t.Fatalf("type: %d", code)
	}
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	if got := a.m.Activity(s.ID); got != nil {
		t.Fatalf("a stopped session has a live log: %+v", got)
	}
	r := a.keptRun(t, s.Profile, s.ID)
	got := strings.Join(oldestFirst(r.Entries), "\n")
	for _, want := range []string{"Started the browser", "Page title: Old sign-in", "Clicked at 100, 60", "Typed 17 characters", "Stopped by e2e"} {
		if !strings.Contains(got, want) {
			t.Errorf("the kept run lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, marker) || strings.Contains(got, "4242") || r.End != sessions.EndAgent || r.StartedBy != "e2e" {
		t.Fatalf("kept %+v:\n%s", r, got)
	}
	// Started again: the new run starts empty, the old one is still kept.
	var s2 sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", `{"profile": "`+s.Profile+`"}`, &s2); code != http.StatusCreated {
		t.Fatalf("start again: %d", code)
	}
	if got := oldestFirst(a.m.Activity(s2.ID)); strings.Join(got, "|") != "Started the browser" {
		t.Fatalf("the new run has %q", got)
	}
	if runs := a.m.Runs(s.Profile); len(runs) != 1 || runs[0].SessionID != s.ID {
		t.Fatalf("kept runs %+v", runs)
	}
}

func TestCrashSaysWhy(t *testing.T) {
	a := newRealAPI(t)
	s := a.startAPIBrowser(t, "crash")
	if out, err := exec.Command("docker", "exec", "browserpier-"+s.Profile, "pkill", "-KILL", "-o", "-x", "chromium").CombinedOutput(); err != nil {
		t.Fatalf("killing Chromium: %v %s", err, out)
	}
	r := a.keptRun(t, s.Profile, s.ID)
	if r.End != sessions.EndCrashed || r.Entries[0].What != "The browser exited unexpectedly" {
		t.Fatalf("kept %+v", r)
	}
	stored, err := a.m.Get(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the container's last line: %q, error %q", stored.LastLog, stored.Error)
	for _, e := range r.Entries {
		for _, words := range []string{stored.LastLog, stored.Error, sessions.CrashReason(stored.LastLog)} {
			if words != "" && strings.Contains(e.What+e.Title, words) {
				t.Fatalf("an entry quotes the container: %+v", e)
			}
		}
	}
}

// viewer is a live view over WebRTC, as the web UI's page opens one: its
// data channel takes control, types and hands back.
type viewer struct {
	pc      *pion.PeerConnection
	dc      *pion.DataChannel
	replies chan string
}

func openViewer(t *testing.T, a agent, profile string) *viewer {
	t.Helper()
	relay, err := webrtc.New(webrtc.Options{Port: 0, IPs: []string{"127.0.0.1"}}, a.m.Layout().ControlSocket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	h := webrtc.NewHandler(relay, a.m, nil, slog.New(slog.DiscardHandler))
	pc, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(pion.RTPCodecTypeVideo, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	v := &viewer{pc: pc, replies: make(chan string, 16)}
	open := make(chan struct{})
	v.dc, err = pc.CreateDataChannel("input", nil)
	if err != nil {
		t.Fatal(err)
	}
	v.dc.OnOpen(func() { close(open) })
	v.dc.OnMessage(func(m pion.DataChannelMessage) { v.replies <- string(m.Data) })
	offer, _ := pc.CreateOffer(nil)
	gathered := pion.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	body, _ := json.Marshal(map[string]string{"sdp": pc.LocalDescription().SDP})
	rec := httptest.NewRecorder()
	h.Offer(rec, httptest.NewRequest(http.MethodPost, "/live/"+profile+"/offer", strings.NewReader(string(body))), profile, webrtc.OfferOptions{Holder: sessions.AdminHolder})
	var ans struct{ SDP string }
	raw, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || json.Unmarshal(raw, &ans) != nil {
		t.Fatalf("offer: %d %s", rec.Code, raw)
	}
	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: ans.SDP}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-open:
	case <-time.After(30 * time.Second):
		t.Fatal("the data channel didn't open")
	}
	return v
}

func (v *viewer) send(t *testing.T, msg map[string]any) {
	t.Helper()
	b, _ := json.Marshal(msg)
	if err := v.dc.SendText(string(b)); err != nil {
		t.Fatal(err)
	}
}

// control asks for control on or off until the browser says so.
func (v *viewer) control(t *testing.T, on bool) {
	t.Helper()
	want := `{"on":` + map[bool]string{true: "true", false: "false"}[on] + `,"type":"control"}`
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		v.send(t, map[string]any{"type": map[bool]string{true: "take", false: "release"}[on]})
		select {
		case r := <-v.replies:
			if r == want {
				return
			}
		case <-time.After(2 * time.Second):
		}
		time.Sleep(time.Second) // the agent counts as driving for a few seconds
	}
	t.Fatalf("control never went %t", on)
}

// tabTitle reads the front tab's title the way the handoff does: over
// HTTP from the target list, with no CDP client on the page.
func tabTitle(t *testing.T, a agent, profile string) string {
	t.Helper()
	resp, err := browsertest.ControlClient(a.m.Layout().ControlSocket(profile)).Get("http://control/cdp/json/list")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var targets []struct{ Type, Title string }
	_ = json.NewDecoder(resp.Body).Decode(&targets)
	for _, tg := range targets {
		if tg.Type == "page" {
			return tg.Title
		}
	}
	return ""
}

func TestNoTitleWhilePersonHasControl(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "person")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/title-echo"})
	snap := a.mustTool(s.ID, "snapshot", nil)
	a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `textbox "Title"`)}))

	v := openViewer(t, a, s.Profile)
	v.control(t, true)
	const typed = "PERSON-TYPED-TITLE"
	v.send(t, map[string]any{"type": "text", "text": typed})
	deadline := time.Now().Add(15 * time.Second)
	for tabTitle(t, a, s.Profile) != typed {
		if time.Now().After(deadline) {
			t.Fatalf("the person's typing didn't reach the title: %q", tabTitle(t, a, s.Profile))
		}
		time.Sleep(200 * time.Millisecond)
	}
	// The agent can't read the page meanwhile, and nothing notes the title.
	if code, _ := a.tool(s.ID, "snapshot", nil); code != http.StatusConflict {
		t.Fatalf("snapshot while a person has control: %d", code)
	}
	titles := func() []string {
		var out []string
		for _, e := range a.m.Activity(s.ID) {
			if e.Kind == sessions.KindPage {
				out = append(out, e.Title)
			}
		}
		return out
	}
	for _, title := range titles() {
		if strings.Contains(title, typed) {
			t.Fatalf("a title was noted while a person had control: %q", titles())
		}
	}
	v.control(t, false)
	log := strings.Join(oldestFirst(a.m.Activity(s.ID)), "\n")
	for _, want := range []string{"A person took control", "Someone started watching", "Control handed back"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	// After hand-back the agent's first read notes what the page now says.
	a.mustTool(s.ID, "snapshot", nil)
	if got := titles(); len(got) == 0 || got[0] != typed {
		t.Fatalf("after hand-back: %q", got)
	}
}
