//go:build docker

// The handoff page on a phone, rendered by a real browser container; see
// internal/sessions/docker_test.go for the environment it needs.
package ui

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/webrtc"
)

// TestHandoffFitsAPhone: on a phone, the handoff page's live view is as
// wide as the screen, in the browser's shape, and the page doesn't
// scroll sideways.
func TestHandoffFitsAPhone(t *testing.T) {
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)

	// BrowserPier, with a handoff open on a link-only profile, so the
	// phone needs no PIN. The profile's browser is a stand-in.
	f := newFixture(t)
	p := profiles.New("google")
	p.HandoffPolicy = profiles.PolicyLink
	if _, err := f.m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	s, _, err := f.m.Start(ctx, "google", "")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := f.handoffs.Create(ctx, s.ID, "claude", handoff.SensitiveInput, "Enter the 2FA code", "")
	if err != nil {
		t.Fatal(err)
	}
	relay, err := webrtc.New(webrtc.Options{Port: 0, IPs: []string{"127.0.0.1"}}, f.m.Layout().ControlSocket, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	mux := http.NewServeMux()
	handoff.NewWeb(f.handoffs, f.web, webrtc.NewHandler(relay, f.m, f.web, log), f.m, log).Register(mux)
	mux.Handle("/", f.srv.Config.Handler)
	link := serveToBrowser(t, mux) + "/h/" + token

	// The phone: a real browser container, shown at a phone's size.
	phone := newPhone(t, image)
	phone.Call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 390, "height": 844, "deviceScaleFactor": 2, "mobile": true})
	phone.Navigate(link)
	if got := phone.Eval(`document.body.innerText`); !strings.Contains(fmt.Sprint(got), "Done, hand back") {
		t.Fatalf("the phone isn't on the control step: %v", got)
	}
	m, _ := phone.Eval(`(() => { const r = document.getElementById("stage").getBoundingClientRect();
		return [document.documentElement.scrollWidth, innerWidth, r.width, r.height] })()`).([]any)
	if len(m) != 4 {
		t.Fatalf("measuring the page: %v", m)
	}
	scroll, width, stageW, stageH := m[0].(float64), m[1].(float64), m[2].(float64), m[3].(float64)
	// A phone widens the page to fit what overflows it.
	if width != 390 || scroll > width {
		t.Fatalf("on a 390 px phone the page is %v px wide", scroll)
	}
	if stageW != width || math.Abs(stageH-stageW*900/1440) > 1 {
		t.Fatalf("the live view is %v×%v, want %v wide at 1440:900", stageW, stageH, width)
	}
}

// newPhone starts a real browser to load the page in, and connects to it.
func newPhone(t *testing.T, image string) *browsertest.CDP {
	t.Helper()
	phone, _ := newPhoneContainer(t, image)
	return phone
}

// newPhoneContainer is newPhone, and names the browser's container.
func newPhoneContainer(t *testing.T, image string) (*browsertest.CDP, string) {
	t.Helper()
	ctx := context.Background()
	rt, err := docker.New(docker.Options{Image: image, UsernsMode: os.Getenv("BROWSERPIER_USERNS_MODE"), User: docker.User(os.Getuid(), os.Getgid()), Instance: fmt.Sprintf("test%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	dir, err := os.MkdirTemp("", "bp") // short, for socket paths
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
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	p := profiles.New(fmt.Sprintf("phone%d", time.Now().UnixNano()%1e9))
	p.LocalNetwork = true // BrowserPier is on the Docker host
	if _, err := m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+p.Name).Run() })
	s, _, err := m.Start(ctx, p.Name, profiles.ModeAPI)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, _ = m.Stop(sctx, s.ID)
		cancel()
		m.Close()
		_ = d.Close()
	})
	return browsertest.Page(t, browsertest.ControlClient(m.Layout().ControlSocket(p.Name))), "browserpier-" + p.Name
}

// serveToBrowser serves h where a browser container can reach it, and
// returns its base URL.
func serveToBrowser(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // reached from the browser container
	if err != nil {
		t.Fatal(err)
	}
	// The browser is on the LAN. On Docker's bridge it comes from a
	// private address, but rootless Podman can show it as the host's
	// public one, where pages would ask for HTTPS.
	lan := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "192.168.1.20:50000"
		h.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: lan, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + net.JoinHostPort(browsertest.SiteHost(t), fmt.Sprint(ln.Addr().(*net.TCPAddr).Port))
}

// TestHandoffPageWaitsOutAStuckHandBack (PROJ-196): a handoff page whose
// time runs out while the browser doesn't answer its hand-back says
// "Handing back…" and waits, asking the status less and less often: it
// never reloads in a loop, neither as the time runs out on the page nor
// when it is opened again meanwhile.
func TestHandoffPageWaitsOutAStuckHandBack(t *testing.T) {
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	f := newFixture(t)
	p := profiles.New("google")
	p.HandoffPolicy = profiles.PolicyLink
	if _, err := f.m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	s, _, err := f.m.Start(ctx, "google", "")
	if err != nil {
		t.Fatal(err)
	}
	h, token, err := f.handoffs.Create(ctx, s.ID, "claude", handoff.SensitiveInput, "Enter the 2FA code", "")
	if err != nil {
		t.Fatal(err)
	}
	f.choosers.wedged.Store(true) // the stand-in browser never answers a hand-back
	// A few seconds left, so the page's own clock runs out.
	if _, err := f.db.Exec(`UPDATE handoffs SET expires_at = ? WHERE id = ?`, time.Now().Add(8*time.Second).UnixMilli(), h.ID); err != nil {
		t.Fatal(err)
	}
	relay, err := webrtc.New(webrtc.Options{Port: 0, IPs: []string{"127.0.0.1"}}, f.m.Layout().ControlSocket, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	mux := http.NewServeMux()
	handoff.NewWeb(f.handoffs, f.web, webrtc.NewHandler(relay, f.m, f.web, log), f.m, log).Register(mux)
	mux.Handle("/", f.srv.Config.Handler)
	// loads counts each link's page loads.
	var loadsMu sync.Mutex
	loaded := map[string]int{}
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			loadsMu.Lock()
			loaded[r.URL.Path]++
			loadsMu.Unlock()
		}
		mux.ServeHTTP(w, r)
	})
	loads := func(token string) int {
		loadsMu.Lock()
		defer loadsMu.Unlock()
		return loaded["/h/"+token]
	}
	link := serveToBrowser(t, counted) + "/h/" + token

	// The link's holder has control (the stand-in browser streams no
	// video for the phone's own live view to take it with), so the end
	// has a hand-back to wait for.
	epoch, _ := f.m.HelpEpoch(s.ID)
	if _, err := f.m.TakeControlTurnByLink(s.ID, sessions.HandoffHolder(h.ID), epoch); err != nil {
		t.Fatal(err)
	}
	phone := newPhone(t, image)
	phone.Navigate(link)
	if got := fmt.Sprint(phone.Eval(`document.body.innerText`)); !strings.Contains(got, "Done, hand back") {
		t.Fatalf("the phone isn't on the control step: %q", got)
	}
	base := loads(token) // the link's first open redirects to the page: 2
	// The time runs out (8 s), the end starts and sticks; give the page
	// as long again to misbehave.
	time.Sleep(16 * time.Second)
	if n := loads(token) - base; n != 0 {
		t.Fatalf("the page reloaded %d times as its time ran out during a stuck hand-back", n)
	}
	if got := fmt.Sprint(phone.Eval(`document.body.innerText`)); !strings.Contains(got, "Handing back") {
		t.Fatalf("the page as the time ran out: %q", got)
	}
	// Opened again meanwhile: the server's "Handing back…" page, which
	// doesn't reload either.
	phone.Navigate(link)
	base = loads(token)
	time.Sleep(8 * time.Second)
	if n := loads(token) - base; n != 0 {
		t.Fatalf("the reopened page reloaded %d times during a stuck hand-back", n)
	}
	if got := fmt.Sprint(phone.Eval(`document.body.innerText`)); !strings.Contains(got, "Handing back") {
		t.Fatalf("the reopened page: %q", got)
	}

	// A PIN link nobody opened, past its time while someone in the live
	// view holds the browser: "Handing back…", not a PIN form that
	// reloads as soon as it shows.
	if err := f.admin.SetPIN(ctx, "4821"); err != nil {
		t.Fatal(err)
	}
	pp := profiles.New("github")
	pp.HandoffPolicy = profiles.PolicyPIN
	if _, err := f.m.Profiles().Create(ctx, pp); err != nil {
		t.Fatal(err)
	}
	s2, _, err := f.m.Start(ctx, "github", "")
	if err != nil {
		t.Fatal(err)
	}
	h2, token2, err := f.handoffs.Create(ctx, s2.ID, "claude", handoff.SensitiveInput, "Enter the 2FA code", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.TakeControl(s2.ID, sessions.AdminHolder); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE handoffs SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Second).UnixMilli(), h2.ID); err != nil {
		t.Fatal(err)
	}
	phone.Navigate(strings.TrimSuffix(link, token) + token2)
	time.Sleep(8 * time.Second)
	if n := loads(token2); n != 1 {
		t.Fatalf("the expired PIN link's page loaded %d times in 8 s, want once", n)
	}
	if got := fmt.Sprint(phone.Eval(`document.body.innerText`)); !strings.Contains(got, "Handing back") {
		t.Fatalf("the expired PIN link's page: %q", got)
	}
}
