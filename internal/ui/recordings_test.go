package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/recording"
)

const recSession = "0123456789abcdef01234567"

// recorded puts a two-segment recording of google on disk, with a gap
// for a person's control between, and loads it.
func (f *fixture) recorded() recording.Index {
	f.t.Helper()
	dir := filepath.Join(f.m.Layout().DataDir, "recordings", "google", recSession)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	ix := recording.Index{Profile: "google", Session: recSession,
		Segments: []recording.Segment{
			{File: "000001.mp4", Start: at, End: at.Add(20 * time.Second)},
			{File: "000002.mp4", Start: at.Add(60 * time.Second), End: at.Add(90 * time.Second)},
		},
		Gaps: []recording.Gap{{Start: at.Add(20 * time.Second), End: at.Add(60 * time.Second), Reason: recording.Person}}}
	b, _ := json.Marshal(ix)
	for name, body := range map[string]string{"index.json": string(b), "000001.mp4": "ftypMP4-one-0123456789", "000002.mp4": "ftypMP4-two"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := f.recordings.Reconcile([]string{"google"}); err != nil {
		f.t.Fatal(err)
	}
	got, err := f.recordings.Get("google", recSession)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func TestRecordingsListAndPlayer(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	f.recorded()
	page := f.get("/profiles/google")
	want(t, page, http.StatusOK, "Recordings", "50 s recorded", "/profiles/google/recordings/"+recSession, "Delete all", "kept 7 days")
	play := f.get("/profiles/google/recordings/" + recSession)
	want(t, play, http.StatusOK, "data-recording=", "/static/player.js", "50 s in 2 parts", "Delete recording")
	// The player's data: segment URLs and the gap's reason, nothing else.
	if !strings.Contains(play.body, "000002.mp4") || !strings.Contains(play.body, "A person had control") {
		t.Fatalf("player data: %.2000s", play.body)
	}

	seg := f.get("/profiles/google/recordings/" + recSession + "/000001.mp4")
	want(t, seg, http.StatusOK, "ftypMP4-one")
	for k, v := range map[string]string{"Content-Type": "video/mp4", "X-Content-Type-Options": "nosniff", "Cache-Control": "no-store"} {
		if got := seg.header.Get(k); got != v {
			t.Errorf("segment %s: %q, want %q", k, got, v)
		}
	}
	if cd := seg.header.Get("Content-Disposition"); cd != "" {
		t.Errorf("segment Content-Disposition %q", cd)
	}
	part := f.get("/profiles/google/recordings/"+recSession+"/000001.mp4", "Range", "bytes=4-6")
	if part.code != http.StatusPartialContent || part.body != "MP4" || part.header.Get("Content-Range") != "bytes 4-6/22" {
		t.Fatalf("range: %d %q %q", part.code, part.body, part.header.Get("Content-Range"))
	}
	for _, bad := range []string{"index.json", "..%2Findex.json", "000003.mp4", "000001.mp4.tmp"} {
		if r := f.get("/profiles/google/recordings/" + recSession + "/" + bad); r.code != http.StatusNotFound {
			t.Errorf("%s: %d", bad, r.code)
		}
	}

	if r := f.post("/profiles/google/recordings/"+recSession+"/delete", url.Values{}); r.code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if got := f.recordings.Sessions("google"); len(got) != 0 {
		t.Fatal("the recording is still listed after delete")
	}
	if r := f.get("/profiles/google/recordings/" + recSession); r.code != http.StatusNotFound {
		t.Fatalf("player after delete: %d", r.code)
	}
	f.recorded()
	if r := f.post("/profiles/google/recordings/delete", url.Values{}); r.code != http.StatusSeeOther {
		t.Fatalf("delete all: %d %s", r.code, r.body)
	}
	if got := f.recordings.Sessions("google"); len(got) != 0 {
		t.Fatal("recordings left after delete all")
	}
}

func TestRecordingsAreTheAdminsAlone(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	f.recorded()
	_, token, err := f.tokens.Create(context.Background(), "research-bot", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := "/profiles/google/recordings/" + recSession
	for _, r := range [][2]string{
		{http.MethodGet, base}, {http.MethodGet, base + "/000001.mp4"}, {http.MethodPost, base + "/delete"},
		{http.MethodPost, "/profiles/google/recordings/delete"}, {http.MethodGet, "/profiles/google/side"},
	} {
		for name, header := range map[string][]string{"agent token": {"Authorization", "Bearer " + token}, "signed out": nil} {
			req, _ := http.NewRequest(r[0], f.srv.URL+r[1], nil)
			for i := 0; i+1 < len(header); i += 2 {
				req.Header.Set(header[i], header[i+1])
			}
			resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			// The list is on the profile page, which sends anyone not
			// signed in to the sign-in page, as the rest of the UI does.
			signIn := (name == "signed out" || r[1] == "/profiles/google/side") &&
				resp.StatusCode == http.StatusSeeOther && strings.HasPrefix(resp.Header.Get("Location"), "/login")
			ok := resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound || signIn
			if !ok || strings.Contains(string(body), "MP4") || strings.Contains(string(body), "000001") {
				t.Errorf("%s %s with %s: %d %.200s", r[0], r[1], name, resp.StatusCode, body)
			}
			if name == "agent token" && r[1] != "/profiles/google/side" && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s with an agent token: %d, want 401 or 404", r[0], r[1], resp.StatusCode)
			}
		}
	}
	if got := f.recordings.Sessions("google"); len(got) != 1 {
		t.Fatal("a request without the admin's session deleted a recording")
	}
}

func TestRecordSessionsSettingAndLimits(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	if p, _ := f.m.Profiles().Get(context.Background(), "google"); p.Record {
		t.Fatal("a new profile records sessions")
	}
	edit := f.get("/profiles/google/edit")
	want(t, edit, http.StatusOK, `name="record"`, "Record sessions")
	form := url.Values{"default_mode": {"visual"}, "auto_stop": {"15"}, "screen": {"1440x900"}, "handoff_policy": {"pin"},
		"record_field": {"1"}, "record": {"1"}}
	if r := f.post("/profiles/google/edit", form); r.code != http.StatusSeeOther {
		t.Fatalf("save: %d %.400s", r.code, r.body)
	}
	if p, _ := f.m.Profiles().Get(context.Background(), "google"); !p.Record {
		t.Fatal("Record sessions wasn't saved")
	}
	want(t, f.get("/profiles/google"), http.StatusOK, "Recordings", "Nothing recorded yet")

	want(t, f.get("/settings"), http.StatusOK, `name="rec_days"`, `value="7"`, `name="rec_cap_mb"`, `value="2048"`)
	for _, bad := range []url.Values{{"rec_days": {"91"}}, {"rec_days": {"0"}}, {"rec_cap_mb": {"10"}}, {"rec_cap_mb": {"x"}}} {
		if r := f.post("/settings", bad); r.code != http.StatusUnprocessableEntity {
			t.Errorf("settings %v: %d", bad, r.code)
		}
	}
	if r := f.post("/settings", url.Values{"rec_days": {"30"}, "rec_cap_mb": {"512"}}); r.code != http.StatusSeeOther {
		t.Fatalf("settings: %d %.300s", r.code, r.body)
	}
	if l := f.recordings.Limits(); l.Retention != 30*24*time.Hour || l.Cap != 512<<20 {
		t.Fatalf("limits in force %+v", l)
	}
}

func TestTheSideSaysWhetherItRecords(t *testing.T) {
	f := newFixture(t)
	p := f.m.Profiles()
	f.profile("google")
	pr, _ := p.Get(context.Background(), "google")
	pr.Record = true
	if _, err := p.Update(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	s, _, err := f.m.Start(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	want(t, f.get("/profiles/google"), http.StatusOK, "Recording paused", "Resumes when an agent acts",
		// In the hint, which live.js rewrites, keeping the note while the
		// status says the profile records (PROJ-23).
		`<div id="hint" data-rec-note="Nothing is recorded while you have control." data-rec>`)
	rec := &uiRecorder{}
	f.m.SetRecorder(rec, f.recordings)
	_, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	want(t, f.get("/profiles/google/side"), http.StatusOK, "Recording now")
}

type uiRecorder struct{}

func (uiRecorder) Set(string, string, bool, recording.Reason) {}
func (uiRecorder) End(string)                                 {}
