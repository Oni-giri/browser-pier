package sessions

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/Oni-giri/browser-pier/internal/favicon"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// IconFile is a profile's cached icon, as the orchestrator sees it. It sits
// beside chrome/, not in it, so the browser can't write it.
func (l Layout) IconFile(profile string) string { return l.dir(profile, favicon.FileName) }

// iconTimeout bounds reading a profile's icon, so it can't hold up a stop.
const iconTimeout = 20 * time.Second

// RefreshIcon reads a stopped profile's icon again now, as a graceful stop
// does: after its main site changed, say. It fails with ErrProfileBusy
// while the browser runs and ErrProfileHeld during an export or import.
func (m *Manager) RefreshIcon(profile string) error {
	release, err := m.Hold(profile)
	if err != nil {
		return err
	}
	defer release()
	m.refreshIcon(profile)
	return nil
}

// refreshIcon caches the profile's icon from its browser's favicon cache,
// and records its most visited site if no main site is set. The browser
// must have exited: it holds its databases while it runs. Failures are
// logged, never returned: the icon then falls back to the glyph.
func (m *Manager) refreshIcon(profile string) {
	ctx, cancel := context.WithTimeout(context.Background(), iconTimeout)
	defer cancel()
	// The main site may change in Edit settings while the icon is read:
	// read again for the new one.
	for range 3 {
		p, err := m.profiles.Get(ctx, profile)
		if err != nil {
			return
		}
		m.readIcon(ctx, p)
		if now, err := m.profiles.Get(ctx, profile); err != nil || now.Site == p.Site {
			return
		}
	}
}

// readIcon is refreshIcon for the profile's settings as given.
func (m *Manager) readIcon(ctx context.Context, p profiles.Profile) {
	profile := p.Name
	dir := m.layout.dir(profile)
	if _, err := os.Stat(filepath.Join(dir, "chrome")); err != nil {
		return // never started: nothing to read
	}
	// Copies a crash left behind.
	if old, _ := filepath.Glob(filepath.Join(dir, ".icon-*")); len(old) > 0 {
		for _, o := range old {
			_ = os.RemoveAll(o)
		}
	}
	scratch, err := os.MkdirTemp(dir, ".icon-")
	if err != nil {
		m.log.Info("reading the profile's icon", "profile", profile, "err", err)
		return
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	read := func(site string) (favicon.Result, error) {
		sub, err := os.MkdirTemp(scratch, "")
		if err != nil {
			return favicon.Result{}, err
		}
		return favicon.Read(ctx, filepath.Join(dir, "chrome"), site, sub)
	}
	res, err := read(p.Site)
	if err != nil && p.Site == "" && p.SiteInferred != "" {
		// The history couldn't be read (too large, say): keep the site it
		// gave last time.
		m.log.Info("reading the profile's history", "profile", profile, "err", err)
		res, err = read(p.SiteInferred)
		res.Site = p.SiteInferred
	}
	if err != nil {
		// The icon cached before stays.
		m.log.Info("reading the profile's icon", "profile", profile, "err", err)
		return
	}
	if p.Site == "" && res.Site != p.SiteInferred {
		if err := m.profiles.SetSiteInferred(ctx, profile, res.Site); err != nil {
			m.log.Info("recording the profile's main site", "profile", profile, "err", err)
		}
	}
	if res.PNG == nil {
		err = favicon.RemoveFile(dir)
	} else {
		err = favicon.WriteFile(dir, res.PNG)
	}
	if err != nil {
		m.log.Info("saving the profile's icon", "profile", profile, "err", err)
		return
	}
	m.log.Debug("profile icon refreshed", "profile", profile, "found", res.PNG != nil)
}
