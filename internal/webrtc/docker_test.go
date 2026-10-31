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
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// TestRealBrowserReachesAViewer: frames from a real browser's display
// arrive at a WebRTC viewer as H.264.
func TestRealBrowserReachesAViewer(t *testing.T) {
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	ctx := context.Background()
	rt, err := docker.New(docker.Options{Image: image, UsernsMode: os.Getenv("BROWSERPIER_USERNS_MODE"), Instance: fmt.Sprintf("test%d", time.Now().UnixNano())})
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
	defer func() { _ = d.Close() }()
	log := slog.New(slog.DiscardHandler)
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("w%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	p := profiles.New(name)
	p.Width, p.Height = 1280, 800
	if _, err := m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	s, _, err := m.Start(ctx, name, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, _ = m.Stop(sctx, s.ID)
		cancel()
		m.Close()
	}()

	relay, err := New(Options{Port: 0, IPs: []string{"127.0.0.1"}}, m.Layout().ControlSocket, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()

	viewer, err := pion.NewPeerConnection(pion.Configuration{})
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
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, answer, err := relay.Connect(cctx, name, viewer.LocalDescription().SDP, func([]byte, func(string)) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := viewer.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-packets:
		// 30 fps for 3 s is far more than this, even for a static page.
		if n < 30 {
			t.Fatalf("only %d RTP packets in 3 s", n)
		}
		t.Logf("%d RTP packets in 3 s", n)
	case <-time.After(30 * time.Second):
		t.Fatal("no video from the browser")
	}
}
