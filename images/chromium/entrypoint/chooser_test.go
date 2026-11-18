package main

import (
	"encoding/json"
	"errors"
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

	"github.com/godbus/dbus/v5"
)

// fakeBus is the chooser's D-Bus side, recorded.
type fakeBus struct {
	mu       sync.Mutex
	answers  []fakeAnswer
	exported map[dbus.ObjectPath]func(string) *dbus.Error
}

type fakeAnswer struct {
	sender string
	path   dbus.ObjectPath
	code   uint32
	uris   []string
}

func (b *fakeBus) respond(sender string, path dbus.ObjectPath, code uint32, results map[string]dbus.Variant) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var uris []string
	if v, ok := results["uris"]; ok {
		uris, _ = v.Value().([]string)
	}
	b.answers = append(b.answers, fakeAnswer{sender, path, code, uris})
	return nil
}

func (b *fakeBus) exportRequest(path dbus.ObjectPath, close func(string) *dbus.Error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exported == nil {
		b.exported = map[dbus.ObjectPath]func(string) *dbus.Error{}
	}
	b.exported[path] = close
	return nil
}

func (b *fakeBus) unexportRequest(path dbus.ObjectPath) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.exported, path)
}

func (b *fakeBus) got() []fakeAnswer {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]fakeAnswer{}, b.answers...)
}

// fakeClock runs timers when told.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { was := !t.stopped; t.stopped = true; return was }

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *fakeClock) after(d time.Duration, f func()) stopper {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.t.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.stopped && !t.at.After(c.t) {
			t.stopped = true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

type countFocus struct {
	mu           sync.Mutex
	aways, backs int
}

func (f *countFocus) away() { f.mu.Lock(); f.aways++; f.mu.Unlock() }
func (f *countFocus) back() { f.mu.Lock(); f.backs++; f.mu.Unlock() }

const browserSender = ":1.2"

// testChooser is a chooser over a fake bus and clock, with files under a
// temporary folder, answering only browserSender.
func testChooser(t *testing.T) (*chooser, *fakeBus, *fakeClock, *countFocus) {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"uploads", "downloads"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bus := &fakeBus{}
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	focus := &countFocus{}
	check := func(sender string) error {
		if sender != browserSender {
			return os.ErrPermission
		}
		return nil
	}
	c := newChooser(slog.New(slog.NewTextHandler(io.Discard, nil)), bus, check, focus, 5*time.Minute)
	c.now, c.after = clock.now, clock.after
	c.uploads, c.downloads = filepath.Join(dir, "uploads"), filepath.Join(dir, "downloads")
	return c, bus, clock, focus
}

func opts(kv ...any) map[string]dbus.Variant {
	m := map[string]dbus.Variant{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = dbus.MakeVariant(kv[i+1])
	}
	return m
}

func upload(t *testing.T, c *chooser, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.uploads, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestChooserRefusesACallerThatIsntTheBrowser(t *testing.T) {
	c, bus, _, _ := testChooser(t)
	if _, err := c.openFile(":1.9", opts("handle_token", "t1")); err == nil {
		t.Fatal("a caller that isn't the browser opened a chooser")
	}
	if _, err := c.saveFile(":1.9", opts("current_name", "x.txt")); err == nil {
		t.Fatal("a caller that isn't the browser saved")
	}
	if len(c.list()) != 0 || len(bus.exported) != 0 {
		t.Fatal("a refused request was kept")
	}
	if ents, _ := os.ReadDir(c.downloads); len(ents) != 0 {
		t.Fatalf("a refused save created %v", ents)
	}
}

func TestChooserHoldsAtMostFour(t *testing.T) {
	c, _, _, _ := testChooser(t)
	for i := range maxOpenChoosers {
		if _, err := c.openFile(browserSender, opts("handle_token", "t"+string(rune('a'+i)))); err != nil {
			t.Fatalf("chooser %d: %v", i+1, err)
		}
	}
	if _, err := c.openFile(browserSender, opts("handle_token", "te")); err == nil {
		t.Fatal("a fifth chooser was held")
	}
	if n := len(c.list()); n != maxOpenChoosers {
		t.Fatalf("%d open, want %d", n, maxOpenChoosers)
	}
}

func TestChooserRefusesFolders(t *testing.T) {
	c, _, _, _ := testChooser(t)
	if _, err := c.openFile(browserSender, opts("handle_token", "t1", "directory", true)); err == nil {
		t.Fatal("a folder chooser was held")
	}
	if len(c.list()) != 0 {
		t.Fatal("a folder chooser is listed")
	}
}

func TestChooserRefusesSaveFiles(t *testing.T) {
	if _, err := (fileChooser{}).SaveFiles(browserSender, "", "", nil); err == nil || err.Name != "org.freedesktop.portal.Error.NotAllowed" {
		t.Fatalf("SaveFiles = %v, want NotAllowed", err)
	}
}

func TestPortalPropertiesAnswerOnlyFileChooser(t *testing.T) {
	v, err := properties{}.Get(fileChooserIntf, "version")
	if err != nil || v.Value() != portalVersion {
		t.Fatalf("version = %v, %v", v, err)
	}
	for _, intf := range []string{"org.freedesktop.portal.Settings", "org.freedesktop.portal.Notification", "org.freedesktop.portal.ScreenCast"} {
		if _, err := (properties{}).Get(intf, "version"); err == nil || err.Name != "org.freedesktop.DBus.Error.UnknownInterface" {
			t.Errorf("Get(%s) = %v, want UnknownInterface", intf, err)
		}
		if _, err := (properties{}).GetAll(intf); err == nil || err.Name != "org.freedesktop.DBus.Error.UnknownInterface" {
			t.Errorf("GetAll(%s) = %v, want UnknownInterface", intf, err)
		}
	}
	if err := (properties{}).Set(fileChooserIntf, "version", dbus.MakeVariant(uint32(9))); err == nil {
		t.Error("version was writable")
	}
}

func TestChooserCloseOnlyFromTheCallerThatAsked(t *testing.T) {
	c, bus, _, focus := testChooser(t)
	p, err := c.openFile(browserSender, opts("handle_token", "tok"))
	if err != nil {
		t.Fatal(err)
	}
	if p != "/org/freedesktop/portal/desktop/request/1_2/tok" {
		t.Fatalf("request path %s", p)
	}
	closeFn := bus.exported[p]
	if err := closeFn(":1.7"); err == nil {
		t.Fatal("another caller closed the request")
	}
	if len(c.list()) != 1 {
		t.Fatal("the request ended on another caller's Close")
	}
	if err := closeFn(browserSender); err != nil {
		t.Fatal(err)
	}
	if len(c.list()) != 0 {
		t.Fatal("the request outlived its caller's Close")
	}
	if len(bus.got()) != 0 {
		t.Fatal("a closed request was answered")
	}
	if focus.aways != 1 || focus.backs != 1 {
		t.Fatalf("focus moved away %d and back %d times, want 1 and 1", focus.aways, focus.backs)
	}
}

func TestChooserTimesOut(t *testing.T) {
	c, bus, clock, _ := testChooser(t)
	p, err := c.openFile(browserSender, opts("handle_token", "tok"))
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(5*time.Minute - time.Second)
	if len(c.list()) != 1 {
		t.Fatal("cancelled before the time limit")
	}
	clock.advance(time.Second)
	if len(c.list()) != 0 {
		t.Fatal("still open after the time limit")
	}
	got := bus.got()
	if len(got) != 1 || got[0].code != respCancelled || got[0].path != p || got[0].sender != browserSender {
		t.Fatalf("answers %+v, want one cancel to the caller", got)
	}
}

func TestChooserRequestsEndWithTheBrowser(t *testing.T) {
	c, bus, _, _ := testChooser(t)
	for _, tok := range []string{"a", "b", "c"} {
		if _, err := c.openFile(browserSender, opts("handle_token", tok)); err != nil {
			t.Fatal(err)
		}
	}
	c.browserExited()
	if len(c.list()) != 0 || len(bus.exported) != 0 {
		t.Fatal("requests outlived the browser")
	}
	if _, err := c.openFile(browserSender, opts("handle_token", "d")); err != nil {
		t.Fatal(err) // the check, not the chooser, refuses once the browser is gone
	}
	c.gone(browserSender)
	if len(c.list()) != 0 {
		t.Fatal("requests outlived their caller leaving the bus")
	}
}

func TestChooserAnswerChecks(t *testing.T) {
	c, bus, _, _ := testChooser(t)
	upload(t, c, "report.pdf", "%PDF")
	upload(t, c, "b.txt", "b")
	if err := os.Symlink("/etc/passwd", filepath.Join(c.uploads, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(c.uploads, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.openFile(browserSender, opts("handle_token", "one")); err != nil {
		t.Fatal(err)
	}
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = "report.pdf"
	}
	for _, tc := range []struct {
		names  []string
		status int
	}{
		{[]string{"../x"}, 400},
		{[]string{"/etc/passwd"}, 400},
		{[]string{"a/b"}, 400},
		{[]string{".hidden"}, 400},
		{[]string{""}, 400},
		{[]string{strings.Repeat("a", 256)}, 400},
		{[]string{"link.txt"}, 422},
		{[]string{"folder"}, 422},
		{[]string{"missing.txt"}, 422},
		{[]string{"report.pdf", "b.txt"}, 422},
		{eleven, 422},
		{nil, 400},
	} {
		err := c.answer("c1", tc.names)
		var ae *answerError
		if !errors.As(err, &ae) || ae.status != tc.status {
			t.Errorf("answer(%q) = %v, want status %d", tc.names, err, tc.status)
		}
	}
	if got := bus.got(); len(got) != 0 {
		t.Fatalf("refused answers reached the browser: %+v", got)
	}
	if err := c.answer("c9", []string{"report.pdf"}); !errors.Is(err, errNoChooser) {
		t.Fatalf("an unknown id: %v", err)
	}
	// The uploads folder itself a link (to the profile, say): refused.
	real := c.uploads
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "Cookies"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.uploads = filepath.Join(t.TempDir(), "uploads")
	if err := os.Symlink(elsewhere, c.uploads); err != nil {
		t.Fatal(err)
	}
	var ae *answerError
	if err := c.answer("c1", []string{"Cookies"}); !errors.As(err, &ae) || ae.status != 422 {
		t.Fatalf("a linked uploads folder: %v", err)
	}
	if _, err := createSaveFile(c.uploads, "x.txt"); err == nil {
		t.Fatal("a save went into a linked folder")
	}
	c.uploads = real
	if got := bus.got(); len(got) != 0 {
		t.Fatalf("a linked folder's file reached the browser: %+v", got)
	}
	if err := c.answer("c1", []string{"report.pdf"}); err != nil {
		t.Fatal(err)
	}
	got := bus.got()
	if len(got) != 1 || got[0].code != respOK || len(got[0].uris) != 1 || got[0].uris[0] != "file://"+c.uploads+"/report.pdf" {
		t.Fatalf("answers %+v", got)
	}
	if err := c.answer("c1", []string{"report.pdf"}); !errors.Is(err, errNoChooser) {
		t.Fatal("a chooser was answered twice")
	}
}

func TestChooserAnswerURI(t *testing.T) {
	c, _, _, _ := testChooser(t)
	if d := newChooser(nil, nil, nil, nil, 0); d.uploads != "/files/uploads" || d.downloads != "/files/downloads" {
		t.Fatalf("folders %s, %s", d.uploads, d.downloads)
	}
	name := "my report #1 (final)?.pdf"
	upload(t, c, name, "x")
	uris, err := c.uploadURIs([]string{name}, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "file://" + c.uploads + "/my%20report%20%231%20%28final%29%3F.pdf"; uris[0] != want {
		t.Fatalf("uri %s, want %s", uris[0], want)
	}
}

func TestChooserIDsAreNeverReused(t *testing.T) {
	c, _, _, _ := testChooser(t)
	seen := map[string]bool{}
	for i := range 6 {
		if _, err := c.openFile(browserSender, opts("handle_token", "t"+string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
		l := c.list()
		id := l[len(l)-1].ID
		if seen[id] {
			t.Fatalf("id %s given twice", id)
		}
		seen[id] = true
		if err := c.cancel(id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChooserAcceptPatterns(t *testing.T) {
	type pat struct {
		Kind    uint32
		Pattern string
	}
	type filter struct {
		Name     string
		Patterns []pat
	}
	many := []pat{}
	for i := range 40 {
		many = append(many, pat{0, "*." + strings.Repeat("x", i+1)})
	}
	filters := []filter{
		{"Custom Files", []pat{{0, "*.[pP][dD][fF]"}, {0, "*.[pP][nN][gG]"}, {1, "image/png"}, {0, "*.[pP][dD][fF]"}}},
		{"<script>All Files", []pat{{0, "*.*"}, {0, "*"}}},
		{"x", []pat{{0, "*.\u200bcsv\n"}, {0, strings.Repeat("y", 65)}}},
		{"y", many},
	}
	got := acceptPatterns(map[string]dbus.Variant{"filters": dbus.MakeVariant(filters)})
	if len(got) != maxPatterns {
		t.Fatalf("%d patterns, want %d: %q", len(got), maxPatterns, got)
	}
	want := []string{"*.pdf", "*.png", "image/png", "*.csv"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("patterns %q, want %q first", got, want)
		}
	}
	for _, p := range got {
		if len(p) > maxPatternLen || strings.ContainsAny(p, "\n\u200b<") {
			t.Fatalf("pattern %q", p)
		}
	}
}

func TestSaveNames(t *testing.T) {
	c, bus, clock, _ := testChooser(t)
	if err := os.WriteFile(filepath.Join(c.downloads, "taken.pdf"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Symlink(outside, filepath.Join(c.downloads, "planted.txt")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ suggested, want string }{
		{"../report.pdf", "_report.pdf"},
		{"a\nb.txt", "ab.txt"},
		{"", "download"},
		{".bashrc", "bashrc"},
		{"taken.pdf", "taken (2).pdf"},
		{strings.Repeat("n", 300) + ".pdf", strings.Repeat("n", maxSaveName-4) + ".pdf"},
		{"planted.txt", "planted (2).txt"},
	} {
		if _, err := c.saveFile(browserSender, opts("current_name", tc.suggested, "handle_token", "s")); err != nil {
			t.Fatalf("%q: %v", tc.suggested, err)
		}
		clock.advance(saveDelay)
		got := bus.got()
		last := got[len(got)-1]
		if last.code != respOK || len(last.uris) != 1 {
			t.Fatalf("%q: answer %+v", tc.suggested, last)
		}
		fi, err := os.Lstat(filepath.Join(c.downloads, tc.want))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 {
			t.Errorf("%q: want a new empty file %q directly in the downloads: %v", tc.suggested, tc.want, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(c.downloads, "taken.pdf")); string(b) != "old" {
		t.Error("a save replaced a taken name's file")
	}
	if _, err := os.Lstat(outside); err == nil {
		t.Error("a save followed a planted link")
	}
}

func TestSavesAreCapped(t *testing.T) {
	c, _, clock, _ := testChooser(t)
	for i := range maxSaves {
		if _, err := c.saveFile(browserSender, opts("current_name", "f.txt")); err != nil {
			t.Fatalf("save %d: %v", i+1, err)
		}
	}
	if _, err := c.saveFile(browserSender, opts("current_name", "f.txt")); err == nil {
		t.Fatal("a save past the cap was answered")
	}
	clock.advance(saveWindow)
	if _, err := c.saveFile(browserSender, opts("current_name", "f.txt")); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestChooserRoutes(t *testing.T) {
	c, bus, _, _ := testChooser(t)
	upload(t, c, "a.txt", "a")
	mux := http.NewServeMux()
	registerChooser(mux, &portal{chooser: c})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, err := c.openFile(browserSender, opts("handle_token", "t1", "multiple", true,
		"filters", []any{[]any{"Custom", []any{[]any{uint32(0), "*.[tT][xX][tT]"}}}})); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv.URL + "/chooser")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Choosers []map[string]any `json:"choosers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	if len(out.Choosers) != 1 || out.Choosers[0]["id"] != "c1" || out.Choosers[0]["multiple"] != true {
		t.Fatalf("GET /chooser: %+v", out)
	}
	for _, k := range []string{"id", "multiple", "accept", "age_ms"} {
		if _, ok := out.Choosers[0][k]; !ok {
			t.Errorf("no %s in %v", k, out.Choosers[0])
		}
	}
	post := func(id, body string) int {
		resp, err := http.Post(srv.URL+"/chooser/"+id, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if s := post("c9", `{"files":["a.txt"]}`); s != 404 {
		t.Fatalf("unknown id: %d", s)
	}
	if s := post("c1", `{"files":["a.txt"],"cancel":true}`); s != 400 {
		t.Fatalf("files and cancel: %d", s)
	}
	if s := post("c1", `{"files":["a.txt"]}`); s != 204 {
		t.Fatalf("answer: %d", s)
	}
	if s := post("c1", `{"cancel":true}`); s != 404 {
		t.Fatalf("an answered id: %d", s)
	}
	if len(bus.got()) != 1 {
		t.Fatal("not answered once")
	}

	off := http.NewServeMux()
	registerChooser(off, nil)
	srv2 := httptest.NewServer(off)
	defer srv2.Close()
	resp, err = http.Get(srv2.URL + "/chooser")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no portal: %d", resp.StatusCode)
	}
}
