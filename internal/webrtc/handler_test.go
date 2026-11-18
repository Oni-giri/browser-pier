package webrtc

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

// offerFixture is a running fake browser and a Handler for it.
func offerFixture(t *testing.T) (*Handler, *sessions.Manager, sessions.Session) {
	t.Helper()
	return offerFixtureWith(t, nil, slog.New(slog.DiscardHandler))
}

// offerFixtureWith is offerFixture with routes of its own on the fake
// container's control socket, and log as the logger.
func offerFixtureWith(t *testing.T, routes func(*http.ServeMux), log *slog.Logger) (*Handler, *sessions.Manager, sessions.Session) {
	t.Helper()
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "of")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, err := db.Open(ctx, filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	rt := sessionstest.New(dir)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	if routes != nil {
		routes(mux)
	}
	rt.Control = mux
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if _, err := m.Profiles().Create(ctx, profiles.New("google")); err != nil {
		t.Fatal(err)
	}
	s, _, err := m.Start(ctx, "google", "")
	if err != nil {
		t.Fatal(err)
	}
	relay, err := New(Options{Port: 0, IPs: []string{"127.0.0.1"}}, m.Layout().ControlSocket, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	return NewHandler(relay, m, nil, log), m, s
}

func offer(h *Handler, o OfferOptions, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.Offer(rec, httptest.NewRequest(http.MethodPost, "/offer", strings.NewReader(body)), "google", o)
	return rec
}

func TestOfferWithControlThatFailsToConnectGivesControlBack(t *testing.T) {
	h, m, _ := offerFixture(t)
	if rec := offer(h, OfferOptions{Control: true, Holder: "viewer"}, `{"sdp": "not an offer"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad SDP: %d %s", rec.Code, rec.Body)
	}
	if s, _ := m.Live("google"); s.HumanControl {
		t.Fatal("a viewer that never connected kept control")
	}
}

func TestOfferWithControlIsRefusedWhileSomeoneElseHasIt(t *testing.T) {
	h, m, s := offerFixture(t)
	if err := m.TakeControl(s.ID, "person"); err != nil {
		t.Fatal(err)
	}
	if rec := offer(h, OfferOptions{Control: true, Holder: "viewer"}, `{"sdp": "not an offer"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Someone else") {
		t.Fatalf("control while someone else has it: %d %s", rec.Code, rec.Body)
	}
	if s, _ := m.Live("google"); !s.HumanControl || s.Viewers != 0 {
		t.Fatalf("the refused viewer changed control or still watches: %+v", s)
	}
}

// TestViewerTakesControlAsItsHolder (PROJ-184): Take control from a live
// view records that view's holder (the signed-in admin's, or the
// handoff link's), so the file chooser routes answer it and no other.
func TestViewerTakesControlAsItsHolder(t *testing.T) {
	h, m, s := offerFixture(t)
	admin := sessions.AdminHolder("abc")
	v := &viewer{h: h, session: s.ID, holder: admin, sock: m.Layout().ControlSocket("google")}
	var said []string
	v.message([]byte(`{"type":"take"}`), func(m string) { said = append(said, m) })
	if len(said) != 1 || !strings.Contains(said[0], `"on":true`) {
		t.Fatalf("take: %q", said)
	}
	if !m.ControlledBy(s.ID, admin) || m.ControlledBy(s.ID, sessions.HandoffHolder("h1")) {
		t.Fatal("control isn't the viewer's holder's alone")
	}
	v.message([]byte(`{"type":"release"}`), func(string) {})
	if m.ControlledBy(s.ID, admin) {
		t.Fatal("still in control after release")
	}
}
