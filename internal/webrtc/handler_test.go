package webrtc

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

// offerFixture is a running fake browser and a Handler for it.
func offerFixture(t *testing.T) (*Handler, *sessions.Manager, sessions.Session) {
	t.Helper()
	r := newOfferRig(t, nil, slog.New(slog.DiscardHandler))
	return r.h, r.m, r.s
}

// offerFixtureWith is offerFixture with routes of its own on the fake
// container's control socket, and log as the logger.
func offerFixtureWith(t *testing.T, routes func(*http.ServeMux), log *slog.Logger) (*Handler, *sessions.Manager, sessions.Session) {
	t.Helper()
	r := newOfferRig(t, routes, log)
	return r.h, r.m, r.s
}

// offerRig is offerFixture's parts; register adds routes to the fake
// browser's control socket, and log is the logger.
type offerRig struct {
	h     *Handler
	m     *sessions.Manager
	s     sessions.Session
	d     *sql.DB
	relay *Relay
}

func newOfferRig(t *testing.T, register func(*http.ServeMux), log *slog.Logger) *offerRig {
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
	if register != nil {
		register(mux)
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
	return &offerRig{h: NewHandler(relay, m, nil, log), m: m, s: s, d: d, relay: relay}
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
	admin := sessions.AdminHolder
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

// inputLog records what reached the fake browser's /input routes.
type inputLog struct {
	mu   sync.Mutex
	hits []string
}

func (l *inputLog) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /input/{what}", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.hits = append(l.hits, r.PathValue("what"))
		l.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
}

func (l *inputLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.hits
	l.hits = nil
	return out
}

// TestStaleViewerLeavesTheNextHolderAlone (PROJ-184, review S1 and N1): a
// live view whose control ended behind its back (a handoff finished, or
// Hand back from another page) and someone else took it, whether the
// handoff link or another admin view: that view's input, lift, release,
// take and closing neither reach the browser nor end the new holder's
// control.
func TestStaleViewerLeavesTheNextHolderAlone(t *testing.T) {
	var in inputLog
	r := newOfferRig(t, in.register, slog.New(slog.DiscardHandler))
	for _, next := range []string{sessions.HandoffHolder("h1"), sessions.AdminHolder} {
		a := &viewer{h: r.h, session: r.s.ID, holder: sessions.AdminHolder, sock: r.m.Layout().ControlSocket("google")}
		var said []string
		send := func(msg string) {
			a.message([]byte(msg), func(m string) { said = append(said, m) })
		}
		send(`{"type":"take"}`)
		if !r.m.ControlledBy(r.s.ID, sessions.AdminHolder) {
			t.Fatalf("take: %q", said)
		}
		stale := a.turn
		r.m.ReleaseControl(r.s.ID) // the handoff finished, say
		if err := r.m.TakeControl(r.s.ID, next); err != nil {
			t.Fatal(err)
		}
		in.take()
		said = nil

		// Each on its own: the first that notices the view is stale
		// makes it forget its control.
		for _, msg := range []string{`{"type":"lift"}`, `{"type":"release"}`, "close", `{"type":"input","events":[{"t":"key","k":"a"}]}`, `{"type":"text","text":"hello"}`, `{"type":"take"}`} {
			a.control, a.turn = true, stale
			if msg == "close" {
				a.handBack()
			} else {
				send(msg)
			}
		}
		if got := in.take(); len(got) != 0 {
			t.Fatalf("next holder %q: the stale view reached the browser: %q", next, got)
		}
		if !r.m.ControlledBy(r.s.ID, next) {
			t.Fatalf("next holder %q lost control to the stale view", next)
		}
		if all := strings.Join(said, " "); !strings.Contains(all, `"on":false`) || !strings.Contains(all, "Someone else has control") || strings.Contains(all, `"on":true`) {
			t.Fatalf("the stale view was told %q", said)
		}
		r.m.ReleaseControl(r.s.ID)
	}

	// Its own control it still releases, keys lifted.
	a := &viewer{h: r.h, session: r.s.ID, holder: sessions.AdminHolder, sock: r.m.Layout().ControlSocket("google")}
	a.message([]byte(`{"type":"take"}`), func(string) {})
	a.handBack()
	if r.m.ControlledBy(r.s.ID, sessions.AdminHolder) {
		t.Fatal("its own control kept after closing")
	}
	if got := in.take(); len(got) != 1 || got[0] != "release" {
		t.Fatalf("closing with control: %q", got)
	}
}

// TestViewerWithNoHolderSaysSo (review N3): a wiring mistake that offers
// control with no holder answers what went wrong, not "isn't running".
func TestViewerWithNoHolderSaysSo(t *testing.T) {
	h, _, s := offerFixture(t)
	if rec := offer(h, OfferOptions{Control: true}, `{"sdp": "not an offer"}`); rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "nobody") {
		t.Fatalf("offer with no holder: %d %s", rec.Code, rec.Body)
	}
	v := &viewer{h: h, session: s.ID}
	var said string
	v.message([]byte(`{"type":"take"}`), func(m string) { said = m })
	if !strings.Contains(said, "nobody") {
		t.Fatalf("take with no holder: %s", said)
	}
}

// TestAdminOfferTakesControlAsTheAdmin (review N1): the signed-in admin's
// /live/{profile}/offer, through a real WebRTC connection: Take control
// over its data channel holds control as AdminHolder, the holder the
// admin's file chooser routes ask for.
func TestAdminOfferTakesControlAsTheAdmin(t *testing.T) {
	r := newOfferRig(t, nil, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	admin, err := auth.NewAdmin(ctx, r.d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	web := auth.NewWeb(admin, auth.NewTokens(r.d), log, auth.WebOptions{CookieSecure: "auto"})
	mux := http.NewServeMux()
	NewHandler(r.relay, r.m, web, log).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cookie, _, err := admin.NewSession(ctx, false, "test")
	if err != nil {
		t.Fatal(err)
	}

	pc, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(pion.RTPCodecTypeVideo, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	dc, err := pc.CreateDataChannel("input", nil)
	if err != nil {
		t.Fatal(err)
	}
	replies := make(chan string, 8)
	dc.OnMessage(func(m pion.DataChannelMessage) { replies <- string(m.Data) })
	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })
	o, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := pion.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(o); err != nil {
		t.Fatal(err)
	}
	<-gathered
	body, _ := json.Marshal(map[string]string{"sdp": pc.LocalDescription().SDP})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/live/google/offer", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var ans struct {
		SDP     string `json:"sdp"`
		Control bool   `json:"control"`
	}
	err = json.NewDecoder(resp.Body).Decode(&ans)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || ans.Control {
		t.Fatalf("offer: %d %+v %v", resp.StatusCode, ans, err)
	}
	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: ans.SDP}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("data channel never opened")
	}
	if err := dc.SendText(`{"type":"take"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-replies:
		if !strings.Contains(m, `"on":true`) {
			t.Fatalf("take: %s", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer to take")
	}
	if !r.m.ControlledBy(r.s.ID, sessions.AdminHolder) {
		t.Fatal("the admin's live view doesn't hold control as AdminHolder")
	}
}
