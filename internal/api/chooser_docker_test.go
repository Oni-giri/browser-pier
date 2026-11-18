//go:build docker

package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// chooserRig is a Visual-mode browser on the chooser page (PROJ-152).
type chooserRig struct {
	t      *testing.T
	a      agent
	site   *browsertest.PageSite
	id     string // the session
	name   string // the profile
	sw, sh int
}

func newChooserRig(t *testing.T, env ...string) *chooserRig {
	t.Helper()
	a := newRealAPIEnv(t, env...)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	name := fmt.Sprintf("ch%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "visual"}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	a.allowLocalNetwork(t, name)
	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || s.Mode != "visual" {
		t.Fatalf("start: %d %+v", code, s)
	}
	c := &chooserRig{t: t, a: a, site: site, id: s.ID, name: name}
	c.sw, c.sh = a.screenSize(t, s.ID)
	return c
}

// post sends a Visual action to path (click, type or computer) and
// returns the answer's headers.
func (c *chooserRig) post(path, body string) http.Header {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.a.base+"/api/sessions/"+c.id+path, strings.NewReader(body))
	req.Header = c.a.header()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		c.t.Fatalf("%s %s: %d %s", path, body, resp.StatusCode, raw)
	}
	return resp.Header
}

// at is a point of the page's band at frac of the window below the
// toolbar, for a click's JSON.
func (c *chooserRig) at(frac float64) string {
	top := 100
	vh := c.sh - 32 - top
	return fmt.Sprintf(`"x": %d, "y": %d`, c.sw/2, top+int(frac*float64(vh)))
}

// click clicks the band at frac, instantly.
func (c *chooserRig) click(frac float64) http.Header {
	return c.post("/click", `{`+c.at(frac)+`, "pace": "instant"}`)
}

// open opens the chooser page through the address bar, as a person does,
// and waits until it answers.
func (c *chooserRig) open() {
	c.t.Helper()
	for _, body := range []string{
		`{"action": "key", "keys": "ctrl+l", "pace": "instant"}`,
		fmt.Sprintf(`{"action": "type", "text": %q, "pace": "instant"}`, c.site.URL+"/chooser"),
		`{"action": "key", "keys": "Return", "pace": "instant"}`,
	} {
		c.post("/computer", body)
	}
	for deadline := time.Now().Add(30 * time.Second); !c.site.HasEvent("ping"); {
		c.click(0.87)
		if time.Now().After(deadline) {
			c.t.Fatal("the page never loaded")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (c *chooserRig) upload(name string, content []byte) {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.a.base+"/api/sessions/"+c.id+"/files?name="+url.QueryEscape(name), bytes.NewReader(content))
	req.Header = c.a.header()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		c.t.Fatalf("upload %s: %d", name, resp.StatusCode)
	}
}

func (c *chooserRig) choosers() []chooserJSON {
	c.t.Helper()
	var out struct {
		Choosers []chooserJSON `json:"choosers"`
		Text     string        `json:"text"`
	}
	if code := c.a.do(http.MethodGet, "/api/sessions/"+c.id+"/chooser", "", &out); code != http.StatusOK {
		c.t.Fatalf("GET chooser: %d", code)
	}
	return out.Choosers
}

// choosersText is GET …/chooser's text.
func (c *chooserRig) choosersText() string {
	c.t.Helper()
	var out struct {
		Text string `json:"text"`
	}
	if code := c.a.do(http.MethodGet, "/api/sessions/"+c.id+"/chooser", "", &out); code != http.StatusOK {
		c.t.Fatalf("GET chooser: %d", code)
	}
	return out.Text
}

// waitChooser waits for one chooser to be open, and returns it.
func (c *chooserRig) waitChooser() chooserJSON {
	c.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if cs := c.choosers(); len(cs) == 1 {
			return cs[0]
		} else if len(cs) > 1 {
			c.t.Fatalf("%d choosers open: %+v", len(cs), cs)
		}
		if time.Now().After(deadline) {
			c.t.Fatal("no file chooser opened")
		}
	}
}

func (c *chooserRig) answer(body string) (int, string) {
	c.t.Helper()
	var out map[string]string
	code := c.a.do(http.MethodPost, "/api/sessions/"+c.id+"/chooser", body, &out)
	return code, out["text"] + out["error"]
}

func (c *chooserRig) waitEvent(e string) {
	c.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !c.site.HasEvent(e); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			c.t.Fatalf("the page never said %q; it said %q", e, c.site.Events())
		}
	}
}

// eventsSince is the page's events after the first n.
func (c *chooserRig) eventsSince(n int) []string {
	ev := c.site.Events()
	if n > len(ev) {
		return nil
	}
	return slices.DeleteFunc(ev[n:], func(e string) bool { return e == "ping" })
}

// dialogs lists the native file dialogs on the display: there must never
// be one with the portal on.
func (c *chooserRig) dialogs() string {
	var found []string
	for _, title := range []string{"Open File", "Open Files", "Save File", "All Files", "Select"} {
		out, _ := exec.Command("docker", "exec", "browserpier-"+c.name, "xdotool", "search", "--onlyvisible", "--name", "^"+title).Output()
		if s := strings.TrimSpace(string(out)); s != "" {
			found = append(found, title+": "+s)
		}
	}
	return strings.Join(found, "; ")
}

func (c *chooserRig) noneAttached() {
	c.t.Helper()
	for _, tg := range targets(c.t, c.a.m.Layout().ControlSocket(c.name)) {
		if tg.Type == "page" && tg.Attached {
			c.t.Errorf("a CDP client is attached to %s", tg.URL)
		}
	}
}

func randomFile(t *testing.T, n int) ([]byte, string) {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

func TestChooserAnsweredByAnAgentVisual(t *testing.T) {
	c := newChooserRig(t)
	content, sum := randomFile(t, 100<<10)
	c.upload("my report #1.pdf", content)
	c.open()
	before := len(c.site.Events())

	start := time.Now()
	h := c.click(0.29) // the page's own button, which calls input.click()
	notice := h.Get(ChooserHeader)
	ch := c.waitChooser()
	t.Logf("click answered in %v; notice on it: %q; chooser listed %v after the click", time.Since(start).Round(time.Millisecond), notice, time.Since(start).Round(time.Millisecond))
	if notice == "" {
		// It may come an action late (SPEC §8): the next one says it.
		notice = c.post("/computer", `{"action": "wait", "ms": 10}`).Get(ChooserHeader)
	}
	if notice != fmt.Sprintf("a file chooser is open (%s): choose_files answers or cancels it", ch.ID) {
		t.Errorf("notice %q", notice)
	}
	if ch.Multiple {
		t.Errorf("chooser %+v, want one file and no accept list", ch)
	}
	if d := c.dialogs(); d != "" {
		t.Fatalf("a native dialog is on the display: %s", d)
	}
	if code, msg := c.answer(fmt.Sprintf(`{"id": %q, "files": ["my report #1.pdf"]}`, ch.ID)); code != http.StatusOK {
		t.Fatalf("answer: %d %s", code, msg)
	}
	c.waitEvent("b change 1")
	if cs := c.choosers(); len(cs) != 0 {
		t.Fatalf("still open after the answer: %+v", cs)
	}
	c.click(0.67) // Send
	for deadline := time.Now().Add(10 * time.Second); len(c.site.Uploads()) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the form never posted")
		}
	}
	got := c.site.Uploads()
	if len(got) != 1 || got[0].Field != "b" || got[0].Name != "my report #1.pdf" || got[0].Size != int64(len(content)) || got[0].SHA256 != sum {
		t.Fatalf("the site got %+v, want my report #1.pdf, %d bytes, sha256 %s", got, len(content), sum)
	}
	// What the page saw, as with Chromium's GTK dialog (ANTI-BOT.md):
	// the window loses the focus while the chooser is open, then the
	// files, then the focus comes back.
	ev := c.eventsSince(before)
	t.Logf("page events: %q", ev)
	if want := []string{"window blur", "b input", "b change 1", "window focus"}; !slices.Equal(ev[:min(len(ev), 4)], want) {
		t.Errorf("events %q, want %q first", ev, want)
	}
	if d := c.dialogs(); d != "" {
		t.Errorf("a native dialog is on the display: %s", d)
	}
	c.noneAttached()
	logged := map[string]bool{}
	for _, e := range c.a.m.Activity(c.id) {
		logged[e.What] = true
	}
	if !logged["The page opened a file chooser"] || !logged["Answered a file chooser with 1 file"] {
		t.Errorf("activity %+v", c.a.m.Activity(c.id))
	}
}

func TestChooserMultipleAndAccept(t *testing.T) {
	c := newChooserRig(t)
	pdf, pdfSum := randomFile(t, 5000)
	png, pngSum := randomFile(t, 7000)
	c.upload("a.pdf", pdf)
	c.upload("b.png", png)
	c.open()
	c.click(0.49)
	ch := c.waitChooser()
	text := c.choosersText()
	if !ch.Multiple || !strings.Contains(text, "*.pdf") || !strings.Contains(text, "*.png") || !strings.Contains(text, "<untrusted-content-") {
		t.Fatalf("chooser %+v, %s: want multiple, with *.pdf and *.png fenced", ch, text)
	}
	if code, msg := c.answer(fmt.Sprintf(`{"id": %q, "files": ["a.pdf", "b.png"]}`, ch.ID)); code != http.StatusOK {
		t.Fatalf("answer: %d %s", code, msg)
	}
	c.waitEvent("c change 2")
	c.click(0.67)
	for deadline := time.Now().Add(10 * time.Second); len(c.site.Uploads()) < 2; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the form posted %+v", c.site.Uploads())
		}
	}
	got := map[string]string{}
	for _, u := range c.site.Uploads() {
		if u.Field == "c" {
			got[u.Name] = u.SHA256
		}
	}
	if got["a.pdf"] != pdfSum || got["b.png"] != pngSum {
		t.Fatalf("the site got %+v", c.site.Uploads())
	}
}

func TestChooserCancelAndTimeout(t *testing.T) {
	c := newChooserRig(t, "BROWSERPIER_CHOOSER_TIMEOUT=3s")
	c.open()
	pings := func() int {
		n := 0
		for _, e := range c.site.Events() {
			if e == "ping" {
				n++
			}
		}
		return n
	}
	// Cancelled: the input stays empty and the page goes on answering.
	c.click(0.09)
	ch := c.waitChooser()
	if code, msg := c.answer(fmt.Sprintf(`{"id": %q, "cancel": true}`, ch.ID)); code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, msg)
	}
	c.waitEvent("a cancel")
	if c.site.HasEvent("a change 1") {
		t.Fatal("a cancelled chooser gave files")
	}
	n := pings()
	c.click(0.87)
	for deadline := time.Now().Add(5 * time.Second); pings() == n; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the page doesn't answer after a cancel")
		}
	}
	if code, _ := c.answer(fmt.Sprintf(`{"id": %q, "cancel": true}`, ch.ID)); code != http.StatusConflict {
		t.Fatalf("a closed chooser: %d, want 409", code)
	}
	// Timed out: the same, after the (shortened) time limit.
	c.click(0.29)
	c.waitChooser()
	start := time.Now()
	for len(c.choosers()) > 0 {
		if time.Since(start) > 10*time.Second {
			t.Fatal("the chooser outlived its time limit")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("cancelled %v after it was listed", time.Since(start).Round(100*time.Millisecond))
	c.waitEvent("b cancel")
	n = pings()
	c.click(0.87)
	for deadline := time.Now().Add(5 * time.Second); pings() == n; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the page doesn't answer after a time-out")
		}
	}
	if d := c.dialogs(); d != "" {
		t.Errorf("a native dialog is on the display: %s", d)
	}
}

func TestChooserOnlyFromUploads(t *testing.T) {
	c := newChooserRig(t)
	c.open()
	filesDir := c.a.m.Layout().FilesDir(c.name)
	for _, d := range []string{files.Uploads, files.Downloads} {
		if err := os.MkdirAll(filepath.Join(filesDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(filesDir, files.Downloads, "dl.pdf"), []byte("downloaded"), 0o644); err != nil {
		t.Fatal(err)
	}
	// /profile, as the browser's container sees it: its cookies.
	if err := os.Symlink("/profile/Default/Cookies", filepath.Join(filesDir, files.Uploads, "cookies.pdf")); err != nil {
		t.Fatal(err)
	}
	c.click(0.09)
	ch := c.waitChooser()
	// The orchestrator refuses both itself (its words, not the
	// browser's: nothing went to the browser).
	for _, name := range []string{"dl.pdf", "cookies.pdf", "../downloads/dl.pdf"} {
		code, msg := c.answer(fmt.Sprintf(`{"id": %q, "files": [%q]}`, ch.ID, name))
		if (code != http.StatusUnprocessableEntity || !strings.Contains(msg, "upload_file")) && (code != http.StatusBadRequest || !strings.Contains(msg, "like report.pdf")) {
			t.Errorf("%s through the API: %d %s", name, code, msg)
		}
	}
	// And so does the browser, asked directly past the orchestrator.
	control := browsertest.ControlClient(c.a.m.Layout().ControlSocket(c.name))
	for _, name := range []string{"dl.pdf", "cookies.pdf", "../downloads/dl.pdf", "/profile/Default/Cookies"} {
		body, _ := json.Marshal(map[string]any{"files": []string{name}})
		resp, err := control.Post("http://control/chooser/"+ch.ID, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		msg, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s to the browser: %d %s", name, resp.StatusCode, msg)
		}
	}
	time.Sleep(time.Second)
	for _, e := range c.site.Events() {
		if strings.HasPrefix(e, "a change") || strings.HasPrefix(e, "a input") {
			t.Fatalf("the page received files: %q", c.site.Events())
		}
	}
	if cs := c.choosers(); len(cs) != 1 {
		t.Fatalf("the refusals closed the chooser: %+v", cs)
	}
	if code, _ := c.answer(fmt.Sprintf(`{"id": %q, "cancel": true}`, ch.ID)); code != http.StatusOK {
		t.Fatalf("cancel: %d", code)
	}
}

func TestChooserDuringHandoff(t *testing.T) {
	c := newChooserRig(t)
	// Link only, so the handoff needs no PIN (only a person can lower it).
	p, err := c.a.m.Profiles().Get(context.Background(), c.name)
	if err != nil {
		t.Fatal(err)
	}
	p.HandoffPolicy = profiles.PolicyLink
	if _, err := c.a.m.Profiles().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	content, _ := randomFile(t, 1000)
	c.upload("cv.pdf", content)
	c.open()
	c.click(0.09)
	ch := c.waitChooser()
	var h handoffJSON
	if code := c.a.do(http.MethodPost, "/api/sessions/"+c.id+"/handoff", `{"reason": "file_upload"}`, &h); code != http.StatusCreated {
		t.Fatalf("handoff: %d %+v", code, h)
	}
	// The chooser passes to the person: still open, listed, the agent's
	// answer refused.
	if cs := c.choosers(); len(cs) != 1 || cs[0].ID != ch.ID {
		t.Fatalf("during the handoff: %+v", cs)
	}
	if code, msg := c.answer(fmt.Sprintf(`{"id": %q, "files": ["cv.pdf"]}`, ch.ID)); code != http.StatusConflict {
		t.Fatalf("the agent's answer during a handoff: %d %s", code, msg)
	}
	hh, err := c.a.ho.Get(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.a.ho.Finish(context.Background(), hh, "done", ""); err != nil {
		t.Fatal(err)
	}
	// Still open at hand-back; the agent answers it now.
	if code, msg := c.answer(fmt.Sprintf(`{"id": %q, "files": ["cv.pdf"]}`, ch.ID)); code != http.StatusOK {
		t.Fatalf("after hand-back: %d %s", code, msg)
	}
	c.waitEvent("a change 1")
}

func TestSaveAsLandsInDownloads(t *testing.T) {
	c := newChooserRig(t)
	c.open()
	before := c.a.listed(t, c.name)
	c.post("/computer", `{"action": "key", "keys": "ctrl+s", "pace": "instant"}`)
	var saved string
	for deadline := time.Now().Add(15 * time.Second); saved == ""; time.Sleep(100 * time.Millisecond) {
		for n, size := range c.a.listed(t, c.name) {
			if _, ok := before[n]; !ok && strings.HasSuffix(n, ".html") && size > 0 {
				saved = n
			}
		}
		if d := c.dialogs(); d != "" {
			t.Fatalf("a native dialog is on the display: %s", d)
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing saved; downloads %v", c.a.listed(t, c.name))
		}
	}
	b, err := os.ReadFile(filepath.Join(c.a.downloadsDir(c.name), saved))
	if err != nil || !bytes.Contains(b, []byte("Choose a file")) {
		t.Fatalf("saved %s: %v %.200s", saved, err, b)
	}
	t.Logf("saved as %s (%d bytes)", saved, len(b))
}

// TestChooserStaysQuiet: the detector, on its main thread and in a
// worker, sees nothing new over 20 rounds of a click on its file input
// and an answer from the uploads, and still catches its positive control.
func TestChooserStaysQuiet(t *testing.T) {
	c := newChooserRig(t)
	content, _ := randomFile(t, 1000)
	c.upload("doc.pdf", content)
	w := browsertest.NewWatch(t, browsertest.SiteHost(t))
	for _, body := range []string{
		`{"action": "key", "keys": "ctrl+l", "pace": "instant"}`,
		fmt.Sprintf(`{"action": "type", "text": %q, "pace": "instant"}`, w.URL),
		`{"action": "key", "keys": "Return", "pace": "instant"}`,
	} {
		c.post("/computer", body)
	}
	w.Ready(t)
	// Where the file input is on the screen, read once over CDP before
	// the batch (the detector's hits from it are reset away).
	p := browsertest.Page(t, browsertest.ControlClient(c.a.m.Layout().ControlSocket(c.name)))
	pt, _ := p.Eval(`(() => { const r = document.querySelector('#doc').getBoundingClientRect();
	  return [Math.round(r.x + 20 + screenX + (outerWidth - innerWidth)), Math.round(r.y + r.height / 2 + screenY + (outerHeight - innerHeight))]; })()`).([]any)
	p.Close()
	w.Settle() // its own hits arrive before the batch's Reset
	if len(pt) != 2 {
		t.Fatalf("no #doc: %v", pt)
	}
	at := fmt.Sprintf(`"x": %v, "y": %v`, pt[0], pt[1])
	round := func() {
		c.post("/click", `{`+at+`}`)
		ch := c.waitChooser()
		if code, msg := c.answer(fmt.Sprintf(`{"id": %q, "files": ["doc.pdf"]}`, ch.ID)); code != http.StatusOK {
			t.Fatalf("answer: %d %s", code, msg)
		}
		for deadline := time.Now().Add(5 * time.Second); len(c.choosers()) > 0; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("still open")
			}
		}
	}
	_, quiet := w.Quiet(t, "20 × open a file chooser and answer it", 20, round)
	if d := c.dialogs(); d != "" {
		t.Errorf("a native dialog is on the display: %s", d)
	}
	c.noneAttached()
	w.Reset()
	p = browsertest.Page(t, browsertest.ControlClient(c.a.m.Layout().ControlSocket(c.name)))
	browsertest.WatchPositiveControl(t, p)
	w.Settle()
	p.Close()
	t.Logf("positive control: %v", w.Kinds())
	if !w.HasKind("main-world-call") || !w.HasKind("automation-globals") {
		t.Errorf("the detector missed its positive control (%v); its silence above means nothing", w.Kinds())
	}
	if !quiet {
		t.Log("see the batch's hits above")
	}
}
