//go:build docker

package sessions

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/recording"
)

// recRig is a manager with a real recorder, and a profile.
type recRig struct {
	f     *realFixture
	m     *Manager
	rec   *recording.Recorder
	store *recording.Store
	name  string
}

func newRecRig(t *testing.T, record bool) *recRig {
	t.Helper()
	f := newRealFixture(t)
	store := recording.NewStore(filepath.Join(f.dir, "recordings"), slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	m := f.manager(f.layout)
	rec := recording.NewRecorder(store, m.RecordStream, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	t.Cleanup(rec.Close)
	m.SetRecorder(rec, store)
	name := f.profile(m)
	if record {
		p, _ := m.Profiles().Get(context.Background(), name)
		p.Record = true
		if _, err := m.Profiles().Update(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	return &recRig{f: f, m: m, rec: rec, store: store, name: name}
}

// agent opens a CDP connection the way an agent's goes through BrowserPier:
// registered with the manager (Attach), then to the page.
func (r *recRig) agent(t *testing.T, s Session) (*browsertest.CDP, func()) {
	t.Helper()
	_, release, err := r.m.Attach(context.Background(), s.ID, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	p := browsertest.Page(t, browsertest.ControlClient(r.f.layout.ControlSocket(r.name)))
	return p, func() { p.Close(); release() }
}

// waitIndex waits for the session's recording to satisfy cond.
func (r *recRig) waitIndex(t *testing.T, id, what string, cond func(recording.Index) bool) recording.Index {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ix, err := r.store.Get(r.name, id)
		if err == nil && cond(ix) {
			return ix
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; recording %+v, %v", what, ix, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (r *recRig) segment(t *testing.T, id string, seg recording.Segment) []byte {
	t.Helper()
	f, _, err := r.store.Open(r.name, id, seg.File)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// checkMP4 checks a segment's boxes: ftyp, moov, then moof and mdat
// pairs, the first sample a keyframe (an IDR slice before any other
// slice). It looks at box and NAL headers only; nothing is decoded.
func checkMP4(t *testing.T, b []byte) {
	t.Helper()
	var types []string
	firstMdat := []byte(nil)
	for off := 0; off < len(b); {
		if len(b)-off < 8 {
			t.Fatalf("%d stray bytes at the end", len(b)-off)
		}
		size := int(binary.BigEndian.Uint32(b[off:]))
		typ := string(b[off+4 : off+8])
		if size < 8 || off+size > len(b) {
			t.Fatalf("box %q of size %d at %d runs past the file's %d bytes", typ, size, off, len(b))
		}
		if typ == "mdat" && firstMdat == nil {
			firstMdat = b[off+8 : off+size]
		}
		types = append(types, typ)
		off += size
	}
	if len(types) < 4 || types[0] != "ftyp" || types[1] != "moov" {
		t.Fatalf("boxes %v, want ftyp, moov, then fragments", types)
	}
	for i := 2; i < len(types); i += 2 {
		if types[i] != "moof" || i+1 >= len(types) || types[i+1] != "mdat" {
			t.Fatalf("boxes %v: want moof and mdat pairs after the moov", types)
		}
	}
	for n := firstMdat; len(n) > 5; {
		l := int(binary.BigEndian.Uint32(n))
		switch n[4] & 0x1f {
		case 5:
			return // an IDR slice: a keyframe
		case 1:
			t.Fatal("the first slice isn't a keyframe")
		}
		if 4+l > len(n) {
			break
		}
		n = n[4+l:]
	}
	t.Fatal("no slice in the first fragment")
}

// recordingPipelines counts the recording pipelines running in the profile's container.
func recordingPipelines(t *testing.T, name string) int {
	t.Helper()
	out, err := exec.Command("docker", "exec", containerName(name), "sh", "-c", "cat /proc/[0-9]*/cmdline | tr '\\0' ' '").CombinedOutput()
	if err != nil {
		t.Fatalf("listing the container's processes: %v: %s", err, out)
	}
	return strings.Count(string(out), "mp4mux")
}

// TestRecordingShowsWhatTheAgentDid: an agent signs in to a test site and
// clicks; the recording covers it in whole MP4 segments, which play in
// Chromium after the browser has been stopped and started again, with
// the login kept.
func TestRecordingShowsWhatTheAgentDid(t *testing.T) {
	r := newRecRig(t, true)
	site := browsertest.NewSite(t, browsertest.SiteHost(t))
	s := startRunning(t, r.m, r.name)

	began := time.Now()
	p, done := r.agent(t, s)
	token := site.Login(t, p)
	p.Navigate(site.URL + "/login")
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		p.Call("Input.dispatchMouseEvent", map[string]any{"type": typ, "x": 300, "y": 100, "button": "left", "clickCount": 1})
	}
	// Something moving on screen for a while, as an agent at work makes it.
	p.Eval(`document.body.insertAdjacentHTML('beforeend', '<div id=clock style="font:40px sans-serif"></div>');
		setInterval(() => document.getElementById('clock').textContent = Date.now(), 100)`)
	// Past one segment's length, to see what a rotation loses.
	time.Sleep(recording.SegmentLength + 5*time.Second)
	// Then still, and a change: how soon after it is it recorded again?
	p.Eval(`clearInterval(1); for (let i = 1; i < 100; i++) clearInterval(i)`)
	time.Sleep(6 * time.Second)
	changed := time.Now()
	p.Eval(`document.body.style.background = '#2f6bff'; document.getElementById('clock').textContent = 'changed'`)
	time.Sleep(3 * time.Second)
	acted := time.Now()
	done()
	stopCleanly(t, r.m, s.ID)
	ix := r.waitIndex(t, s.ID, "the stop's gap", func(ix recording.Index) bool {
		return len(ix.Gaps) > 0 && ix.Gaps[len(ix.Gaps)-1].Reason == recording.Stopped
	})
	t.Logf("recording: %d segments, %d bytes, %s recorded, from %s after the agent connected; gaps %+v",
		len(ix.Segments), ix.Bytes(), ix.Recorded(), ix.Start().Sub(began), ix.Gaps)
	if ix.Recorded() < 5*time.Second {
		t.Errorf("only %s recorded of the agent's %s", ix.Recorded(), acted.Sub(began))
	}
	if first := ix.Segments[0]; first.Length() > recording.SegmentLength+time.Second {
		t.Errorf("the first segment lasts %s, over %s", first.Length(), recording.SegmentLength)
	}
	if start := ix.Start(); start.Sub(began) > 5*time.Second {
		t.Errorf("the recording starts %s after the agent connected", start.Sub(began))
	}
	if end := ix.End(); end.Before(acted.Add(-time.Second)) {
		t.Errorf("the recording ends %s before the agent's last action", acted.Sub(end))
	}
	for _, seg := range ix.Segments {
		checkMP4(t, r.segment(t, s.ID, seg))
	}
	// A rotation's gap: from the end of the first segment to the start of
	// the second, marked as a restart.
	if len(ix.Segments) < 3 {
		t.Fatalf("%d segments, want a rotation and a segment after the still stretch", len(ix.Segments))
	}
	rotation := ix.Segments[1].Start.Sub(ix.Segments[0].End)
	var after time.Duration = -1
	for _, seg := range ix.Segments {
		if seg.Start.After(changed.Add(-500 * time.Millisecond)) {
			after = seg.Start.Sub(changed)
			break
		}
	}
	t.Logf("MEASURE a rotation loses %s; a change after a still stretch is recorded from %s after it", rotation, after)
	if rotation > 1500*time.Millisecond {
		t.Errorf("a rotation loses %s", rotation)
	}
	if after < 0 || after > 1500*time.Millisecond {
		t.Errorf("the change after a still stretch was recorded from %s after it", after)
	}
	restart := false
	for _, g := range ix.Gaps {
		restart = restart || (g.Reason == recording.Restart && !g.Start.Before(ix.Segments[0].End))
	}
	if !restart {
		t.Errorf("no restart gap for the rotation: %+v", ix.Gaps)
	}

	// After a stop and a start: still listed, it plays, and the login is kept.
	next := startRunning(t, r.m, r.name)
	again, err := r.store.Get(r.name, s.ID)
	if err != nil || len(again.Segments) != len(ix.Segments) {
		t.Fatalf("the first run's recording after a restart: %+v, %v", again, err)
	}
	player := servePlayer(t, r.segment(t, s.ID, ix.Segments[0]))
	p, done = r.agent(t, next)
	defer done()
	if got := site.Account(t, p); got != token {
		t.Fatalf("after stop and start the site saw session %q, want %q", got, token)
	}
	p.Navigate(player)
	got := p.Eval(`new Promise((ok) => {
		const v = document.querySelector('video');
		const fail = (why) => ok('error: ' + why);
		v.onerror = () => fail(v.error && v.error.message);
		setTimeout(() => fail('timeout, readyState ' + v.readyState), 15000);
		v.play().then(() => setTimeout(() => {
			const played = v.currentTime;
			v.onseeked = () => ok(JSON.stringify({w: v.videoWidth, h: v.videoHeight, played, seeked: v.currentTime}));
			v.currentTime = 3;
		}, 1500), (e) => fail(String(e)));
	})`)
	t.Logf("Chromium played the segment: %v", got)
	if str, _ := got.(string); !strings.Contains(str, `"w":960,"h":600`) || !strings.Contains(str, `"seeked":3`) {
		t.Fatalf("playing the segment in Chromium: %v", got)
	}
}

// servePlayer serves a page that plays seg, where the browser reaches it.
func servePlayer(t *testing.T, seg []byte) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /play", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<video src="/seg.mp4" muted playsinline></video>`)
	})
	mux.HandleFunc("GET /seg.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "seg.mp4", time.Time{}, bytes.NewReader(seg))
	})
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // the browser container reaches it over the bridge
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return fmt.Sprintf("http://%s:%d/play", browsertest.SiteHost(t), ln.Addr().(*net.TCPAddr).Port)
}

// TestRecordingStopsForAPerson: an agent types into a plain field, a
// person takes control and types, hands back, the agent goes on. Nothing
// recorded overlaps the person's control, and the index says why; the
// same through a handoff.
func TestRecordingStopsForAPerson(t *testing.T) {
	r := newRecRig(t, true)
	site := browsertest.NewSite(t, browsertest.SiteHost(t))
	s := startRunning(t, r.m, r.name)
	control := browsertest.ControlClient(r.f.layout.ControlSocket(r.name))
	personTypes := func(text string) {
		t.Helper()
		// What the live view sends for a person's paste (viewer.message).
		done, err := r.m.HumanInput(s.ID, "person")
		if err != nil {
			t.Fatalf("the person has no control: %v", err)
		}
		defer done() // once the input has reached the browser
		resp, err := control.Post("http://control/input/type", "application/json",
			strings.NewReader(fmt.Sprintf(`{"text": %q, "pace": "instant"}`, text)))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			t.Fatalf("typing: %s", resp.Status)
		}
	}
	agentTypes := func(text string) {
		t.Helper()
		p, done := r.agent(t, s)
		defer done()
		p.Navigate(site.URL + "/login")
		p.Eval(`document.querySelector('[name=user]').focus()`)
		p.Call("Input.insertText", map[string]any{"text": text})
		time.Sleep(2 * time.Second)
	}
	var spans [][2]time.Time
	for _, viaHandoff := range []bool{false, true} {
		agentTypes("agent")
		time.Sleep(agentQuiet + time.Second) // the agent must be quiet before a person can take control
		took := time.Now()
		if viaHandoff {
			if _, err := r.m.SetNeedsHelp(s.ID, true); err != nil {
				t.Fatal(err)
			}
			// The handoff's person drives as Take control's does.
			if err := r.m.TakeControl(s.ID, "person"); err != nil {
				t.Fatal(err)
			}
		} else if err := r.m.TakeControl(s.ID, "person"); err != nil {
			t.Fatal(err)
		}
		personTypes("hunter2-person")
		time.Sleep(3 * time.Second)
		r.m.ReleaseControl(s.ID)
		if viaHandoff {
			if _, err := r.m.SetNeedsHelp(s.ID, false); err != nil {
				t.Fatal(err)
			}
		}
		spans = append(spans, [2]time.Time{took, time.Now()})
		agentTypes("again")
	}
	stopCleanly(t, r.m, s.ID)
	ix := r.waitIndex(t, s.ID, "the stop's gap", func(ix recording.Index) bool {
		return len(ix.Gaps) > 0 && ix.Gaps[len(ix.Gaps)-1].Reason == recording.Stopped
	})
	t.Logf("segments %+v gaps %+v", ix.Segments, ix.Gaps)
	for i, span := range spans {
		for _, seg := range ix.Segments {
			if seg.Start.Before(span[1]) && seg.End.After(span[0]) {
				t.Errorf("segment %s (%s to %s) overlaps the person's control (%s to %s)", seg.File,
					seg.Start.Format(time.StampMilli), seg.End.Format(time.StampMilli), span[0].Format(time.StampMilli), span[1].Format(time.StampMilli))
			}
		}
		want := recording.Person
		if i == 1 {
			want = recording.Handoff
		}
		found := false
		for _, g := range ix.Gaps {
			// From the take (TakeControl or the handoff, just after the
			// test noted the time) to the hand-back.
			if g.Reason == want && !g.Start.After(span[0].Add(500*time.Millisecond)) && !g.End.Before(span[1].Add(-time.Second)) {
				found = true
			}
		}
		if !found {
			t.Errorf("no %s gap covering %s to %s: %+v", want, span[0].Format(time.StampMilli), span[1].Format(time.StampMilli), ix.Gaps)
		}
	}
	for _, seg := range ix.Segments {
		checkMP4(t, r.segment(t, s.ID, seg))
	}
}

// TestStillScreenCostsLittle: an agent connected to a still page for two
// minutes adds little.
func TestStillScreenCostsLittle(t *testing.T) {
	r := newRecRig(t, true)
	site := browsertest.NewSite(t, browsertest.SiteHost(t))
	s := startRunning(t, r.m, r.name)
	p, done := r.agent(t, s)
	p.Navigate(site.URL + "/account")
	time.Sleep(10 * time.Second) // the page settles; the first segment ends
	before, _ := r.store.Usage()
	time.Sleep(2 * time.Minute)
	after, _ := r.store.Usage()
	done()
	stopCleanly(t, r.m, s.ID)
	ix, _ := r.store.Get(r.name, s.ID)
	t.Logf("2 minutes of a still page added %d bytes (%d before); %d segments, gaps %+v", after-before, before, len(ix.Segments), ix.Gaps)
	if after-before >= 60_000 {
		t.Fatalf("2 minutes of a still page added %d bytes, want under 60 kB", after-before)
	}
}

// TestNoAgentNoRecording: with recording on and no agent, nothing is
// written and no recording pipeline runs.
func TestNoAgentNoRecording(t *testing.T) {
	r := newRecRig(t, true)
	s := startRunning(t, r.m, r.name)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if n := recordingPipelines(t, r.name); n != 0 {
			t.Fatalf("%d recording pipelines with no agent", n)
		}
		time.Sleep(5 * time.Second)
	}
	if total, _ := r.store.Usage(); total != 0 {
		t.Fatalf("%d bytes recorded with no agent", total)
	}
	if _, err := os.Stat(filepath.Join(r.f.dir, "recordings")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a recordings folder with nothing recorded: %v", err)
	}
	stopCleanly(t, r.m, s.ID)
}

// TestRecordingOffByDefault: a default profile with an agent at work
// writes nothing and never runs the recording pipeline.
func TestRecordingOffByDefault(t *testing.T) {
	r := newRecRig(t, false)
	site := browsertest.NewSite(t, browsertest.SiteHost(t))
	s := startRunning(t, r.m, r.name)
	p, done := r.agent(t, s)
	site.Login(t, p)
	for range 3 {
		if n := recordingPipelines(t, r.name); n != 0 {
			t.Fatalf("%d recording pipelines with recording off", n)
		}
		time.Sleep(2 * time.Second)
	}
	done()
	stopCleanly(t, r.m, s.ID)
	if _, err := os.Stat(filepath.Join(r.f.dir, "recordings")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a recordings folder with recording off: %v", err)
	}
}
