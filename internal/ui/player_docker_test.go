//go:build docker

package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/recording"
)

// testSegment is a real segment of n frames at 5 fps, made by the browser
// image's GStreamer with the recording pipeline's encoder and muxer.
func testSegment(t *testing.T, image string, frames int) []byte {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", "--entrypoint", "gst-launch-1.0", image, "-q",
		"videotestsrc", fmt.Sprintf("num-buffers=%d", frames), "pattern=ball", "!", "video/x-raw,width=320,height=200,framerate=5/1", "!",
		"videoconvert", "!", "video/x-raw,format=I420", "!",
		"x264enc", "pass=qual", "quantizer=28", "speed-preset=veryfast", "tune=zerolatency", "sliced-threads=false", "threads=1", "key-int-max=1000", "bframes=0", "!",
		"video/x-h264,stream-format=avc,alignment=au,profile=high", "!",
		"mp4mux", "fragment-duration=200", "streamable=true", "!", "fdsink", "fd=1").Output()
	if err != nil || len(out) < 1000 {
		t.Fatalf("making a test segment: %v (%d bytes)", err, len(out))
	}
	return out
}

// TestPlayerJS plays a two-segment recording in a real browser: in
// order, skipping the gap and saying why, at the speed chosen, seeking
// where the timeline is clicked, and waiting through the gap when Skip
// gaps is off.
func TestPlayerJS(t *testing.T) {
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	f := newFixture(t)
	f.profile("mail")
	seg := testSegment(t, image, 25)
	dir := filepath.Join(f.m.Layout().DataDir, "recordings", "mail", recSession)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	ix := recording.Index{Profile: "mail", Session: recSession,
		Segments: []recording.Segment{
			{File: "000001.mp4", Start: at, End: at.Add(5 * time.Second)},
			{File: "000002.mp4", Start: at.Add(25 * time.Second), End: at.Add(30 * time.Second)},
		},
		Gaps: []recording.Gap{{Start: at.Add(5 * time.Second), End: at.Add(25 * time.Second), Reason: recording.Person}}}
	b, _ := json.Marshal(ix)
	for name, body := range map[string][]byte{"index.json": b, "000001.mp4": seg, "000002.mp4": seg} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.recordings.Reconcile([]string{"mail"}); err != nil {
		t.Fatal(err)
	}
	base := serveToBrowser(t, f.srv.Config.Handler)
	p := newPhone(t, image)
	p.Call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1280, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	p.Navigate(base + "/login")
	p.Eval(`document.querySelector("#password").value = "the-admin-password"; document.querySelector("#password").form.requestSubmit()`)
	until(t, p, "signing in didn't leave the login page", `location.pathname === "/"`)
	p.Navigate(base + "/profiles/mail/recordings/" + recSession)
	until(t, p, "the first segment didn't load", `document.querySelector("#rec-video").videoWidth === 320`)

	p.Eval(`document.querySelector("input[name=speed][value='4']").click()`)
	p.Eval(`document.querySelector("#rec-play").click()`)
	is(t, p, `document.querySelector("#rec-video").playbackRate`, float64(4))
	untilFor(t, p, 15*time.Second, "playback never reached the second segment",
		`document.querySelector("#rec-video").src.endsWith("000002.mp4") && !document.querySelector("#rec-video").paused`)
	note := `document.querySelector("#rec-note").textContent`
	if got, _ := p.Eval(note).(string); got != "Skipped 0:20 · A person had control" {
		t.Errorf("note at the skipped gap: %q", got)
	}

	// A click on the timeline seeks: 10% in is 3 s into the first segment.
	p.Eval(`(() => { const b = document.querySelector("#rec-bar").getBoundingClientRect();
		document.querySelector("#rec-bar").dispatchEvent(new MouseEvent("click", {clientX: b.left + b.width * 0.1, clientY: b.top + 5, bubbles: true})) })()`)
	// The new source loads, then the player seeks into it.
	until(t, p, "the click didn't seek back to the first segment", `document.querySelector("#rec-video").src.endsWith("000001.mp4") && document.querySelector("#rec-video").currentTime > 1`)
	if got, _ := p.Eval(`document.querySelector("#rec-video").currentTime`).(float64); got < 2 || got > 5 {
		t.Errorf("seeked to %.2f s, want about 3", got)
	}

	// Skip gaps off: the gap is waited through, at 4×, saying why.
	p.Eval(`document.querySelector("#rec-skip").click()`)
	untilFor(t, p, 10*time.Second, "the gap's reason wasn't shown", note+`.startsWith("A person had control. Nothing was recorded from")`)
	if got, _ := p.Eval(`document.querySelector("#rec-video").src.endsWith("000001.mp4")`).(bool); !got {
		t.Error("moved on to the next segment without waiting through the gap")
	}
	untilFor(t, p, 10*time.Second, "playback didn't go on after the gap", `document.querySelector("#rec-video").src.endsWith("000002.mp4")`)
}
