package sessions

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// maxPreview caps a preview screenshot.
	maxPreview = 8 << 20
	// previewTimeout is how long a preview may take; a slow one is skipped.
	previewTimeout = 10 * time.Second
)

// Screenshot takes a PNG of a profile's screen for the web UI, scaled
// down to width pixels across (0 for full size). Unlike an agent's
// screenshot it doesn't count as use: the UI polls previews, and they
// mustn't keep idle browsers from stopping. It needs the profile's
// session to be running or with a person (Needs help).
func (m *Manager) Screenshot(ctx context.Context, profile string, width int) ([]byte, error) {
	if width < 0 || (width > 0 && width < 16) {
		return nil, fmt.Errorf("width %d: want 0 for full size, or at least 16 pixels", width)
	}
	m.mu.Lock()
	l, ok := m.live[profile]
	ok = ok && (l.sess.State == Running || l.sess.State == NeedsHelp)
	m.mu.Unlock()
	if !ok {
		return nil, ErrNotRunning
	}
	ctx, cancel := context.WithTimeout(ctx, previewTimeout)
	defer cancel()
	target := "http://control/screenshot"
	if width > 0 {
		target += "?width=" + strconv.Itoa(width)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	// The host is the profile's Unix socket; the path is fixed.
	resp, err := controlClient(m.layout.ControlSocket(profile)).Do(req) //nolint:gosec // see above
	if err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("screenshot: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	png, err := io.ReadAll(io.LimitReader(resp.Body, maxPreview+1))
	if err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	if len(png) > maxPreview {
		return nil, fmt.Errorf("screenshot: over %d bytes", maxPreview)
	}
	return png, nil
}
