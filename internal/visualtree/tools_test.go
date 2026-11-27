package visualtree

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

const smallTree = `{"doc": {"app": ":1.0", "id": "/d", "url": "https://example.com/", "title": "A page", "x": 0, "y": 87, "w": 1440, "h": 781},
"screen_width": 1440, "screen_height": 900, "nodes": [
 {"id": "/n/1", "parent": -1, "depth": 1, "role": 43, "states": 1124073728, "name": "Go", "x": 8, "y": 200, "w": 100, "h": 22}
]}`

// toolsFixture is a Visual-mode session on a fake browser whose /tree
// waits for hold, when set, before it answers.
func toolsFixture(t *testing.T, hold chan struct{}, reads chan struct{}) (*sessions.Manager, *Tools, sessions.Session) {
	t.Helper()
	dir, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, err := db.Open(context.Background(), filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /tree", func(w http.ResponseWriter, r *http.Request) {
		if reads != nil {
			reads <- struct{}{}
		}
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, smallTree)
	})
	rt := sessionstest.New(dir)
	rt.Control = mux
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	p := profiles.New("shot")
	p.DefaultMode = profiles.ModeVisual
	if _, err := m.Profiles().Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	s, _, err := m.Start(context.Background(), "shot", "")
	if err != nil {
		t.Fatal(err)
	}
	return m, New(m), s
}

func isRevoked(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// TestARevocationAfterTheReadWins: a token revoked once the tree has
// been read, while it is formatted, gets the revocation, not the page.
func TestARevocationAfterTheReadWins(t *testing.T) {
	m, tools, s := toolsFixture(t, nil, nil)
	tools.afterRead = func() { m.DisconnectToken("doomed") }
	out, err := tools.Snapshot(context.Background(), s, Caller{TokenID: "doomed", Agent: "a"}, SnapshotIn{})
	if !isRevoked(err) || out != "" {
		t.Fatalf("revoked while formatting: %v, %q", err, out)
	}
	tools.afterRead = nil
	if _, err := tools.Snapshot(context.Background(), s, Caller{TokenID: "other", Agent: "b"}, SnapshotIn{}); err != nil {
		t.Fatalf("another token: %v", err)
	}
}

// TestARevocationEndsAReadWaitingItsTurn: a read waiting for another to
// finish is its token's already, and a revocation ends the wait.
func TestARevocationEndsAReadWaitingItsTurn(t *testing.T) {
	hold, reads := make(chan struct{}), make(chan struct{}, 4)
	m, tools, s := toolsFixture(t, hold, reads)
	defer close(hold)
	first := make(chan error, 1)
	go func() {
		_, err := tools.Snapshot(context.Background(), s, Caller{TokenID: "keeper", Agent: "a"}, SnapshotIn{})
		first <- err
	}()
	<-reads // the first read holds the turn
	second := make(chan error, 1)
	go func() {
		_, err := tools.Snapshot(context.Background(), s, Caller{TokenID: "doomed", Agent: "b"}, SnapshotIn{})
		second <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !contains(m.ConnectedTokens(), "doomed") {
		if time.Now().After(deadline) {
			t.Fatal("the waiting read's token isn't listed: the revocation watcher wouldn't see it")
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.DisconnectToken("doomed")
	select {
	case err := <-second:
		if !isRevoked(err) {
			t.Fatalf("revoked while waiting: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting read went on waiting after its token was revoked")
	}
	select {
	case err := <-first:
		t.Fatalf("the other token's read ended: %v", err)
	default:
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
