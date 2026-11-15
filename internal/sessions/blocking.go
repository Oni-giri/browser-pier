package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// blockFile is where the ad and tracker list waits for the entrypoint,
// which reads it before Chromium starts (images/chromium/entrypoint,
// block.go). Unlike the proxy's file it holds no secret, so it stays.
const blockFile = "blocklist.txt"

// SetBlocklist gives the ad and tracker list each browser that blocks
// gets: f returns its file (internal/domainset's format), or nil when
// there is none yet, and the browser then starts without one. Call it
// before Start.
func (m *Manager) SetBlocklist(f func() []byte) { m.blocklist = f }

// writeBlockFile leaves the list in the profile's control folder when the
// session blocks, read-only, and removes the last one otherwise. It
// reports whether the browser has a list to block with.
func (m *Manager) writeBlockFile(profile string, on bool) (bool, error) {
	root, err := os.OpenRoot(m.layout.dir(profile, "run"))
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(blockFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	var list []byte
	if on && m.blocklist != nil {
		list = m.blocklist()
	}
	if len(list) == 0 {
		return false, nil
	}
	// Read-only, and owned by the orchestrator: when that is root, the
	// browser's user can't make it writable again. The entrypoint reads it
	// once, at start, so what happens to it later changes nothing.
	f, err := root.OpenFile(blockFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return false, err
	}
	_, err = f.Write(list)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err == nil, err
}

// allowAds turns blocking off in a running browser, for the rest of its
// session, and returns the session as it then is. If the browser can't be
// told, blocking stays on and the session says so.
func (m *Manager) allowAds(ctx context.Context, l *live) Session {
	s := m.snapshot(l)
	if err := m.control(ctx, s.Profile, http.MethodPost, "/proxy/blocking/off", nil, nil); err != nil {
		m.log.Warn("couldn't turn ad and tracker blocking off", "session", s.ID, "err", err)
		return s
	}
	_ = m.update(l, func(s *Session) { s.BlockAds = false })
	return m.snapshot(l)
}

// ExemptHost lets a session's browser reach host, and its subdomains,
// even when the ad and tracker list has it: an agent is opening a page
// there directly (see SPEC §3, Egress). It does nothing for a session that
// doesn't block.
func (m *Manager) ExemptHost(ctx context.Context, id, host string) error {
	l := m.find(id)
	if l == nil {
		return ErrNotFound
	}
	s := m.snapshot(l)
	if !s.BlockAds || !s.State.Live() {
		return nil
	}
	return m.control(ctx, s.Profile, http.MethodPost, "/proxy/exempt", map[string]string{"host": host}, nil)
}

// Blocked is how many connections the profile's running browser refused
// for the ad and tracker list. ok is false when it can't be read.
func (m *Manager) Blocked(ctx context.Context, profile string) (n int64, ok bool) {
	var stats map[string]int64
	if err := m.control(ctx, profile, http.MethodGet, "/proxy/stats", nil, &stats); err != nil {
		return 0, false
	}
	n, ok = stats["blocked"]
	return n, ok
}

// control calls the profile's browser on its control socket, with in as
// a JSON body and the answer decoded into out, each when not nil.
func (m *Manager) control(ctx context.Context, profile, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://control"+path, body)
	if err != nil {
		return err
	}
	resp, err := controlClient(m.layout.ControlSocket(profile)).Do(req) //nolint:gosec // our own control socket
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("the browser answered %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
	}
	return nil
}
