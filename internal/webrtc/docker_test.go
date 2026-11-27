//go:build docker

// The relay against a real browser container; see
// internal/sessions/docker_test.go for the environment it needs.
package webrtc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/recording"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// TestRealBrowserReachesAViewer: frames from a real browser's display
// arrive at a WebRTC viewer as H.264.
func TestRealBrowserReachesAViewer(t *testing.T) {
	m, name := startBrowser(t)
	relay, err := New(Options{Port: 0, IPs: []string{"127.0.0.1"}}, m.Layout().ControlSocket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()
	n := watch(t, relay, name, pion.Configuration{})
	// 30 fps for 3 s is far more than this, even for a static page.
	if n < 30 {
		t.Fatalf("only %d RTP packets in 3 s", n)
	}
	t.Logf("%d RTP packets in 3 s", n)
}

// TestWatchingWhileRecording: the live view gets its frames as before
// while the same browser is recorded, two pipelines at once (PROJ-23).
func TestWatchingWhileRecording(t *testing.T) {
	m, name := startBrowserWith(t, true)
	s, _ := m.Live(name)
	_, release, err := m.Attach(context.Background(), s.ID, "tok", "claude") // an agent at work: recording
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !m.Recording(s.ID) {
		t.Fatal("not recording while the agent is connected")
	}
	relay, err := New(Options{Port: 0, IPs: []string{"127.0.0.1"}}, m.Layout().ControlSocket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()
	if n := watch(t, relay, name, pion.Configuration{}); n < 30 {
		t.Fatalf("only %d RTP packets in 3 s while recording", n)
	} else {
		t.Logf("%d RTP packets in 3 s while recording", n)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if out, _ := exec.Command("docker", "exec", "browserpier-"+name, "sh", "-c", "cat /proc/[0-9]*/cmdline 2>/dev/null | tr '\\0' ' '").Output(); strings.Contains(string(out), "mp4mux") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no recording pipeline ran beside the live one")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// startBrowser starts a real browser for a new profile, stopped when the
// test ends.
func startBrowser(t *testing.T) (*sessions.Manager, string) {
	t.Helper()
	return startBrowserIn(t, "", nil)
}

// startBrowserWith is startBrowser, with the profile's Record sessions
// setting and a real recorder when record is set.
func startBrowserWith(t *testing.T, record bool) (*sessions.Manager, string) {
	t.Helper()
	return startBrowserIn(t, "", func(p *profiles.Profile) { p.Record = record })
}

// startBrowserIn is startBrowser in mode ("" for the profile's default),
// with the profile changed by change first, if set, and a real recorder
// if the change turns its Record sessions setting on (PROJ-23).
func startBrowserIn(t *testing.T, mode profiles.Mode, change func(*profiles.Profile)) (*sessions.Manager, string) {
	t.Helper()
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	ctx := context.Background()
	rt, err := docker.New(docker.Options{Image: image, UsernsMode: os.Getenv("BROWSERPIER_USERNS_MODE"), User: docker.User(os.Getuid(), os.Getgid()), Instance: fmt.Sprintf("test%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := rt.UseDataDir(ctx, dir, dir); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(ctx, filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("w%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	p := profiles.New(name)
	p.Width, p.Height = 1280, 800
	if change != nil {
		change(&p)
	}
	if p.Record {
		store := recording.NewStore(filepath.Join(dir, "recordings"), slog.New(slog.DiscardHandler))
		rec := recording.NewRecorder(store, m.RecordStream, slog.New(slog.DiscardHandler))
		t.Cleanup(rec.Close)
		m.SetRecorder(rec, store)
	}
	if _, err := m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	s, _, err := m.Start(ctx, name, mode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, _ = m.Stop(sctx, s.ID)
		cancel()
		m.Close()
	})
	return m, name
}

// watch connects a viewer configured so to the profile's live view, and
// counts the video's RTP packets for 3 s.
func watch(t *testing.T, relay *Relay, name string, cfg pion.Configuration) int {
	t.Helper()
	viewer, err := pion.NewPeerConnection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = viewer.Close() }()
	if _, err := viewer.AddTransceiverFromKind(pion.RTPCodecTypeVideo, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	packets := make(chan int, 1)
	viewer.OnTrack(func(tr *pion.TrackRemote, _ *pion.RTPReceiver) {
		n := 0
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, _, err := tr.ReadRTP(); err != nil {
				break
			}
			n++
		}
		packets <- n
	})
	offer, _ := viewer.CreateOffer(nil)
	gathered := pion.GatheringCompletePromise(viewer)
	if err := viewer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	if len(candidates(viewer.LocalDescription().SDP)) == 0 {
		t.Fatal("the viewer has no candidates")
	}
	cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	v, answer, err := relay.Connect(cctx, name, viewer.LocalDescription().SDP, Input{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := viewer.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-packets:
		return n
	case <-time.After(30 * time.Second):
		t.Fatal("no video from the browser")
		return 0
	}
}
