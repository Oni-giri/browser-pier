package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// proxyTestDir is the proxy test's own folder in the data dir, the only
// one its container mounts. One test runs at a time, so one folder does.
const proxyTestDir = "proxytest"

// ProxyTest is a proxy to test, from the admin's UI.
type ProxyTest struct {
	// Proxy is the URL or {sticky} template, as stored or typed.
	Proxy string
	// Profile fills {sticky} with its own token, so the test sees the
	// exit that profile will use. Empty: a throwaway token, never stored.
	Profile string
	// For names what the test is for in the log: a profile, "settings" or
	// "new profile".
	For string
}

// ErrProxyTestBusy is a second test while one runs.
var ErrProxyTestBusy = docker.ErrProxyTestBusy

// TestProxy tests a proxy the way a browser's start would use it, and
// stores nothing: neither the result nor the location (the one exception
// is a profile's first sticky token, which it then keeps). The proxy goes
// to the test's container as a file in a folder of its own, never in its
// environment, and the folder is gone when it returns. An error means the
// test didn't run; it never holds the proxy.
func (m *Manager) TestProxy(ctx context.Context, t ProxyTest) (docker.ProxyResult, error) {
	res, err := m.testProxy(ctx, t)
	outcome := res.Reason
	switch {
	case err != nil:
		outcome = docker.ProxyFailed
	case res.OK:
		outcome = "ok"
	}
	if !errors.Is(err, ErrProxyTestBusy) {
		// Never the URL, the token or the exit IP.
		m.log.Info("tested a proxy", "for", t.For, "result", outcome)
	}
	if err != nil && !errors.Is(err, ErrProxyTestBusy) && !errors.Is(err, docker.ErrNoProxyCheck) {
		m.log.Warn("the proxy test didn't run", "for", t.For, "err", err)
	}
	return res, err
}

func (m *Manager) testProxy(ctx context.Context, t ProxyTest) (docker.ProxyResult, error) {
	if !m.proxyTest.TryLock() {
		return docker.ProxyResult{}, ErrProxyTestBusy
	}
	defer m.proxyTest.Unlock()
	if err := profiles.ValidProxy(t.Proxy); err != nil || t.Proxy == "" {
		return docker.ProxyResult{}, errors.New("not a proxy URL")
	}
	if m.geoURL == "" {
		// Nothing to test with, and nothing contacted.
		return docker.ProxyResult{Reason: docker.ProxyLookupOff}, nil
	}
	proxy := t.Proxy
	if profiles.HasSticky(proxy) {
		tok := profiles.NewStickyToken()
		if t.Profile != "" {
			var err error
			if tok, err = m.profiles.StickyToken(ctx, t.Profile); err != nil {
				return docker.ProxyResult{}, err
			}
		}
		proxy = profiles.FillSticky(proxy, tok)
	}
	if err := m.writeProxyTestFile(proxy); err != nil {
		m.removeProxyTestDir()
		return docker.ProxyResult{}, err
	}

	// Shutting down cuts the test short, and waits for its container to go.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	var (
		res  docker.ProxyResult
		err  error
		done = make(chan struct{})
	)
	if !m.spawn(func() {
		defer close(done)
		// In here, so Close waits for the folder to go too.
		defer m.removeProxyTestDir()
		res, err = m.rt.TestProxy(ctx, docker.ProxyTestSpec{
			Dir:    filepath.Join(m.layout.HostDataDir, proxyTestDir),
			GeoURL: m.geoURL,
		})
	}) {
		m.removeProxyTestDir()
		return docker.ProxyResult{}, ErrClosed
	}
	<-done
	return res, err
}

// writeProxyTestFile makes the test's folder afresh and leaves the proxy
// in it as a browser's start does (writeProxyFile): 0600, owned by the
// browsers' user, which reads and deletes it.
func (m *Manager) writeProxyTestFile(proxy string) error {
	dir := filepath.Join(m.layout.DataDir, proxyTestDir)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if os.Getuid() == 0 {
		if err := root.Lchown(".", m.layout.UID, m.layout.GID); err != nil {
			return err
		}
	}
	b, err := json.Marshal(map[string]string{"url": proxy})
	if err != nil {
		return err
	}
	f, err := root.OpenFile(proxyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if os.Getuid() == 0 {
		if err := f.Chown(m.layout.UID, m.layout.GID); err != nil {
			_ = f.Close()
			return err
		}
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// removeProxyTestDir removes the test's folder, the proxy file in it if
// the check never read it: after each test, and at start-up after a run
// that was killed.
func (m *Manager) removeProxyTestDir() {
	if err := os.RemoveAll(filepath.Join(m.layout.DataDir, proxyTestDir)); err != nil {
		m.log.Warn("removing the proxy test's folder", "err", err)
	}
}
