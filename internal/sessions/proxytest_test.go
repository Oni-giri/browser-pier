package sessions

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

const proxySecret = "s3cret-pw-91"

func proxyTestFixture(t *testing.T) (*fixture, *lockedBuffer) {
	t.Helper()
	f := newFixture(t)
	var logs lockedBuffer
	f.m.log = slog.New(slog.NewTextHandler(&logs, nil))
	f.m.SetGeoURL("https://geo.example/json")
	return f, &logs
}

func TestProxyTestHandsOverAFileAndLeavesNothing(t *testing.T) {
	f, logs := proxyTestFixture(t)
	proxy := "http://pier:" + proxySecret + "@proxy.example:8080"
	var dirDuring []os.DirEntry
	f.rt.ProxyTest = func(context.Context, string) (docker.ProxyResult, error) {
		dirDuring, _ = os.ReadDir(filepath.Join(f.dir, proxyTestDir))
		return docker.ProxyResult{Reason: docker.ProxyAuth}, nil
	}
	res, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: proxy, Profile: "google", For: "google"})
	if err != nil || res.Reason != docker.ProxyAuth {
		t.Fatalf("TestProxy = %+v, %v", res, err)
	}
	if len(f.rt.ProxyTests) != 1 || f.rt.ProxyTests[0] != proxy {
		t.Fatalf("the check got %v", f.rt.ProxyTests)
	}
	if len(dirDuring) != 0 {
		t.Fatalf("the check left %d files in its folder after reading the proxy", len(dirDuring))
	}
	if _, err := os.Stat(filepath.Join(f.dir, proxyTestDir)); !os.IsNotExist(err) {
		t.Fatal("the test's folder is still there")
	}

	// A passing test: its exit IP isn't logged either.
	f.rt.ProxyTest = nil
	if res, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: proxy, Profile: "google", For: "google"}); err != nil || !res.OK || res.IP.String() != "198.51.100.23" {
		t.Fatalf("TestProxy = %+v, %v", res, err)
	}

	// A test that didn't run leaves nothing either.
	f.rt.ProxyTest = func(context.Context, string) (docker.ProxyResult, error) {
		return docker.ProxyResult{}, errors.New("the daemon is unwell")
	}
	if _, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: proxy, For: "settings"}); err == nil {
		t.Fatal("a failed test reported no error")
	}
	if _, err := os.Stat(filepath.Join(f.dir, proxyTestDir)); !os.IsNotExist(err) {
		t.Fatal("the test's folder is still there after a failure")
	}

	// Logs: what was tested for, and how it went; never the proxy.
	out := logs.String()
	for _, want := range []string{"for=google result=auth", "for=google result=ok", "for=settings result=failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{proxy, proxySecret, "proxy.example", "198.51.100.23"} {
		if strings.Contains(out, secret) {
			t.Fatalf("the log holds %q:\n%s", secret, out)
		}
	}
}

func TestProxyTestFillsSticky(t *testing.T) {
	f, logs := proxyTestFixture(t)
	if _, err := f.m.Profiles().Create(context.Background(), profiles.New("work")); err != nil {
		t.Fatal(err)
	}
	tmpl := "http://pier-{sticky}:" + proxySecret + "@proxy.example:8080"
	// The profile has no token yet: it gets one now, and keeps it.
	if _, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: tmpl, Profile: "google", For: "google"}); err != nil {
		t.Fatal(err)
	}
	tok, err := f.m.Profiles().StickyToken(context.Background(), "google")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.rt.ProxyTests[0]; got != profiles.FillSticky(tmpl, tok) {
		t.Fatalf("Edit tested %q; want the profile's own token %s", got, tok)
	}
	if _, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: tmpl, Profile: "google", For: "google"}); err != nil {
		t.Fatal(err)
	}
	if f.rt.ProxyTests[1] != f.rt.ProxyTests[0] {
		t.Fatal("the profile's token changed between tests")
	}

	// Settings and New profile: a throwaway token, no profile's.
	work, _ := f.m.Profiles().StickyToken(context.Background(), "work")
	if _, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: tmpl, For: "settings"}); err != nil {
		t.Fatal(err)
	}
	got := f.rt.ProxyTests[2]
	if strings.Contains(got, "{sticky}") || got == profiles.FillSticky(tmpl, tok) || got == profiles.FillSticky(tmpl, work) {
		t.Fatalf("Settings tested %q", got)
	}
	throwaway := strings.TrimSuffix(strings.TrimPrefix(got, "http://pier-"), ":"+proxySecret+"@proxy.example:8080")
	for _, name := range []string{"google", "work"} {
		if tk, _ := f.m.Profiles().StickyToken(context.Background(), name); tk == throwaway {
			t.Fatal("the throwaway token is a profile's")
		}
	}
	for _, secret := range []string{tok, throwaway, proxySecret} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the log holds %q", secret)
		}
	}
}

func TestProxyTestWithTheLookupOff(t *testing.T) {
	f, _ := proxyTestFixture(t)
	f.m.SetGeoURL("")
	res, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: "http://u:p@proxy.example:8080", For: "settings"})
	if err != nil || res.Reason != docker.ProxyLookupOff || len(f.rt.ProxyTests) != 0 {
		t.Fatalf("TestProxy = %+v, %v; %d tests run", res, err, len(f.rt.ProxyTests))
	}
}

func TestProxyTestRefusesWhatCantBeSaved(t *testing.T) {
	f, _ := proxyTestFixture(t)
	for _, bad := range []string{"", "ftp://u:p@proxy.example:21", "http://proxy.example"} {
		if _, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: bad, For: "settings"}); err == nil {
			t.Errorf("%q tested", bad)
		}
	}
	if len(f.rt.ProxyTests) != 0 {
		t.Fatal("an invalid proxy reached the check")
	}
}

func TestProxyTestOneAtATime(t *testing.T) {
	f, _ := proxyTestFixture(t)
	running, release := make(chan struct{}), make(chan struct{})
	f.rt.ProxyTest = func(context.Context, string) (docker.ProxyResult, error) {
		close(running)
		<-release
		return docker.ProxyResult{Reason: docker.ProxyRefused}, nil
	}
	done := make(chan error)
	go func() {
		_, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: "http://u:p@proxy.example:8080", For: "settings"})
		done <- err
	}()
	<-running
	if _, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: "http://u:p@proxy.example:8080", For: "settings"}); !errors.Is(err, ErrProxyTestBusy) {
		t.Fatalf("second test: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestCloseCutsAProxyTestShort: shutting down cancels a test under way
// and waits for it.
func TestCloseCutsAProxyTestShort(t *testing.T) {
	f, _ := proxyTestFixture(t)
	running := make(chan struct{})
	f.rt.ProxyTest = func(ctx context.Context, _ string) (docker.ProxyResult, error) {
		close(running)
		<-ctx.Done()
		return docker.ProxyResult{}, ctx.Err()
	}
	done := make(chan error)
	go func() {
		_, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: "http://u:p@proxy.example:8080", For: "settings"})
		done <- err
	}()
	<-running
	f.m.Close()
	// Gone once Close returns, before the request's own goroutine ends.
	if _, err := os.Stat(filepath.Join(f.dir, proxyTestDir)); !os.IsNotExist(err) {
		t.Fatal("the test's folder is still there after Close")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// TestProxyTestAfterClose: once the manager is closed, a test doesn't
// run and writes nothing: the file is only ever written inside work that
// Close waits for.
func TestProxyTestAfterClose(t *testing.T) {
	f, _ := proxyTestFixture(t)
	marker := filepath.Join(f.dir, proxyTestDir, "marker")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.m.Close()
	if _, err := f.m.TestProxy(context.Background(), ProxyTest{Proxy: "http://u:p@proxy.example:8080", For: "settings"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if len(f.rt.ProxyTests) != 0 {
		t.Fatalf("a test ran after Close: %v", f.rt.ProxyTests)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("a test after Close touched the test's folder")
	}
}

func TestReconcileRemovesALeftoverProxyTestFolder(t *testing.T) {
	f, _ := proxyTestFixture(t)
	dir := filepath.Join(f.dir, proxyTestDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "proxy.json"), []byte(`{"url":"http://u:p@x:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("start-up left a killed test's folder, credentials included")
	}
}
