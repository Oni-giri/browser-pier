//go:build docker && measure

package chromium

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// TestMeasureTextAfterShortcut measures how often an address typed right
// after a shortcut that moves the focus to the browser's own UI (ctrl+l)
// goes to the page instead (PROJ-151). Chrome sends a page every key and
// moves the focus only once the page has answered that it left the key
// to the browser; text that arrives first is the page's. It's a
// measurement, not a check, so it only runs with the measure tag:
//
//	go test -tags 'docker measure' -run TestMeasureTextAfterShortcut -v ./images/chromium/
//
// Each run, on a /keys page (browsertest.KeySite) showing and focused, is
// the three calls an agent makes, straight to the control socket (the
// fastest client there can be): key ctrl+l, type the /arrived URL, key
// Return. A run is lost if /arrived never loads; it also counts the
// characters the page saw. MEASURE_RUNS sets the runs per shape (100),
// MEASURE_SHAPES a regexp of the shapes to run (all), MEASURE_PACE the
// pace of every call (instant), and MEASURE_SHOTS a directory for
// screenshots of the toolbar at start.
func TestMeasureTextAfterShortcut(t *testing.T) {
	image := testImage(t)
	host := browsertest.SiteHost(t)
	ks := browsertest.NewKeySite(t, host)
	runs := envInt("MEASURE_RUNS", 100)
	only := regexp.MustCompile(os.Getenv("MEASURE_SHAPES"))
	pace := os.Getenv("MEASURE_PACE")
	if pace == "" {
		pace = "instant"
	}
	var summary []string
	report := func(name string, res []keyOutcome) {
		lost, chars, leaked, focused := 0, 0, 0, 0
		var took []time.Duration
		for _, r := range res {
			if r.focused {
				focused++
			}
			if r.lost {
				lost++
				chars += r.chars
			} else if r.chars > 0 {
				leaked++
			}
			took = append(took, r.took)
		}
		line := fmt.Sprintf("%s [%s]: lost %d of %d", name, pace, lost, len(res))
		if lost > 0 {
			line += fmt.Sprintf(" (the page saw %.1f characters a lost run)", float64(chars)/float64(lost))
		}
		if leaked > 0 {
			line += fmt.Sprintf("; %d arrived but the page saw keys", leaked)
		}
		line += fmt.Sprintf("; the page had the keyboard in %d", focused)
		if len(took) > 0 {
			slices.Sort(took)
			line += fmt.Sprintf("; three calls took %v median", took[len(took)/2].Round(time.Millisecond))
		}
		summary = append(summary, line)
		t.Log(line)
	}
	defer func() {
		t.Logf("Chromium: %s; load: %s", chromiumVersion(image), loadAverage())
		for _, s := range summary {
			t.Log(s)
		}
	}()

	var k *keyRace
	session := func() *keyRace {
		if k == nil {
			k = &keyRace{t: t, ks: ks, pace: pace}
			k.b = startBrowser(t, image, newVolume(t))
			k.otherHost = browsertest.OtherAddress(t, k.b.name, host, ks.Port)
		}
		return k
	}
	type shape struct {
		name string
		slow int
		run  func(k *keyRace) keyOutcome
	}
	shortcut := func(keys string, gap time.Duration) func(k *keyRace) keyOutcome {
		return func(k *keyRace) keyOutcome { return k.navigate(keys, gap) }
	}
	var shapes []shape
	for _, slow := range []int{0, 50, 100, 200, 400} {
		shapes = append(shapes, shape{fmt.Sprintf("ctrl+l slow=%d field", slow), slow, shortcut("ctrl+l", 0)})
		if slow == 0 {
			shapes = append(shapes, shape{"ctrl+l slow=0 body", 0, shortcut("ctrl+l", 0)})
		}
	}
	for _, slow := range []int{0, 400} {
		shapes = append(shapes, shape{fmt.Sprintf("ctrl+l slow=%d cross-site frame", slow), slow, shortcut("ctrl+l", 0)})
	}
	shapes = append(shapes, shape{"list ctrl+l a b c slow=0", 0, (*keyRace).list}, shape{"list ctrl+l a b c slow=200", 200, (*keyRace).list})
	for _, slow := range []int{0, 200} {
		for _, keys := range []string{"alt+d", "F6", "ctrl+k", "ctrl+f", "ctrl+t"} {
			shapes = append(shapes, shape{fmt.Sprintf("other %s slow=%d", keys, slow), slow, func(k *keyRace) keyOutcome { return k.other(keys) }})
		}
	}
	shapes = append(shapes, shape{"click on the address bar slow=400", 400, func(k *keyRace) keyOutcome { return k.navigate("", 0) }})
	for _, slow := range []int{0, 50, 100, 200, 400} {
		for _, gap := range []int{25, 50, 100, 150, 250} {
			shapes = append(shapes, shape{fmt.Sprintf("gap %d ms slow=%d", gap, slow), slow, shortcut("ctrl+l", time.Duration(gap)*time.Millisecond)})
		}
	}

	for _, s := range shapes {
		if !only.MatchString(s.name) {
			continue
		}
		k := session()
		k.slow, k.focus, k.frame = s.slow, "field", false
		if strings.Contains(s.name, "body") {
			k.focus = "body"
		}
		k.frame = strings.Contains(s.name, "frame")
		n := runs
		if strings.HasPrefix(s.name, "other ") || strings.HasPrefix(s.name, "click ") {
			n = min(runs, 20)
		}
		k.reset()
		var res []keyOutcome
		for range n {
			res = append(res, s.run(k))
		}
		report(s.name, res)
	}
	if k != nil {
		docker(t, "rm", "-f", k.b.name)
	}

	// A browser restarted on its profile restores its tabs and loads them
	// as the agent's first calls arrive.
	// MEASURE_RESTART_LOAD makes the restored page take that long to
	// load, as a real site's may, so the agent's calls come while it loads.
	if only.MatchString("restarted") {
		vol := newVolume(t)
		k := &keyRace{t: t, ks: ks, pace: pace, slow: envInt("MEASURE_RESTART_SLOW", 0), focus: "field"}
		k.b = startBrowser(t, image, vol)
		k.reset()
		k.load = envInt("MEASURE_RESTART_LOAD", 0)
		var res []keyOutcome
		for i := range envInt("MEASURE_RESTARTS", 20) {
			old := k.b
			ks.Forget(k.cur)
			if code := old.stop(); code != "0" {
				t.Fatalf("exit code after stop = %s\n%s", code, old.logs())
			}
			k.b = startBrowser(t, image, vol)
			docker(t, "rm", "-f", old.name)
			if i == 0 {
				k.shot("restarted")
			}
			res = append(res, k.navigateAfterStart(envDuration("MEASURE_RESTART_GAP")))
		}
		report(fmt.Sprintf("restarted browser, slow=%d, page load %d ms, gap %v", k.slow, k.load, envDuration("MEASURE_RESTART_GAP")), res)
		docker(t, "rm", "-f", k.b.name)
	}

	// A fresh browser starts with no URL: where is the focus?
	if only.MatchString("fresh") {
		// Where the focus is once it has drawn, in a container of its own.
		if os.Getenv("MEASURE_SHOTS") != "" {
			k := &keyRace{t: t, ks: ks, b: startBrowser(t, image, newVolume(t))}
			time.Sleep(3 * time.Second)
			k.shot("fresh-drawn")
			docker(t, "rm", "-f", k.b.name)
		}
		var res []keyOutcome
		for i := range envInt("MEASURE_FRESH", 10) {
			k := &keyRace{t: t, ks: ks, pace: pace, slow: 0, focus: "field"}
			k.b = startBrowser(t, image, newVolume(t))
			if i == 0 {
				k.shot("fresh")
			}
			res = append(res, k.navigateAfterStart(0))
			docker(t, "rm", "-f", k.b.name)
		}
		report("fresh browser", res)
	}
}

// keyRace drives one browser through the runs of a shape.
type keyRace struct {
	t         *testing.T
	ks        *browsertest.KeySite
	b         *browser
	pace      string
	otherHost string // another address of this machine, for a cross-site frame

	slow  int
	focus string
	frame bool

	cur string // the run whose /keys page is showing
	// load makes the site take that long to answer a /keys page.
	load int
}

type keyOutcome struct {
	lost  bool
	chars int // keys the page saw that weren't the shortcut's
	took  time.Duration
	// focused: the page (the framed one, in a frame) saw a key, so it
	// had the keyboard.
	focused bool
}

// keyRuns numbers the runs of every shape: they share one site.
var keyRuns int

func (k *keyRace) id() string { keyRuns++; return fmt.Sprintf("r%d", keyRuns) }

// page is the /keys page for run n in the current shape: in a frame, the
// framed page is n and the page around it "o"+n.
func (k *keyRace) page(n string) string {
	if k.frame {
		inner := strings.Replace(k.ks.Keys(n, k.slow, "field"), k.ks.URL, k.ks.At(k.otherHost), 1)
		return k.ks.Framed("o"+n, inner)
	}
	if k.load > 0 {
		return browsertest.Loading(k.ks.Keys(n, k.slow, k.focus), k.load)
	}
	return k.ks.Keys(n, k.slow, k.focus)
}

// post sends an action to the control socket. Unlike browser.input it
// allows a slow one: typing an address at human pace takes about 12 s.
func (k *keyRace) post(path, body string) {
	k.t.Helper()
	resp, err := k.b.http.Post("http://control"+path, "application/json", strings.NewReader(body))
	if err != nil {
		k.t.Fatal(err)
	}
	msg, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		k.t.Fatalf("%s %s: %d %s", path, body, resp.StatusCode, msg)
	}
}

func (k *keyRace) key(keys string) {
	k.post("/input/key", fmt.Sprintf(`{"keys": %q, "pace": %q}`, keys, k.pace))
}

func (k *keyRace) typ(text string) {
	k.post("/input/type", fmt.Sprintf(`{"text": %q, "pace": %q}`, text, k.pace))
}

// seen counts the keydowns run n's pages saw other than the shortcut's
// own (modifiers, and a key pressed with ctrl or alt, or an F key).
// had reports whether run n's own page saw any key.
func (k *keyRace) had(n string) bool { return len(k.ks.Run(n).Keys) > 0 }

func (k *keyRace) seen(n string) int {
	count := 0
	for _, run := range []string{n, "o" + n} {
		for _, e := range k.ks.Run(run).Keys {
			switch {
			case e.Ctrl, e.Alt, e.Key == "Control", e.Key == "Alt", e.Key == "Shift", len(e.Key) >= 2 && len(e.Key) <= 3 && e.Key[0] == 'F' && e.Key[1] >= '1' && e.Key[1] <= '9':
			default:
				count++
			}
		}
	}
	return count
}

// ready waits for run n's page to have drawn; in a frame, it then clicks
// into the frame's field, which a cross-site frame doesn't autofocus.
func (k *keyRace) ready(n string, wait time.Duration) bool {
	if _, ok := k.ks.WaitRun(n, wait, func(r browsertest.KeyRun) bool { return r.Ready }); !ok {
		return false
	}
	if k.frame {
		// Chrome drops a click on a page it hasn't painted yet.
		time.Sleep(time.Second)
		k.post("/input/click", `{"x": 640, "y": 300, "pace": "instant"}`)
		time.Sleep(300 * time.Millisecond)
		if os.Getenv("MEASURE_FRAME_SHOT") != "" {
			k.shot("frame-clicked")
		}
	}
	k.cur = n
	return true
}

// quiet waits until run n's pages stop reporting keys: a busy page works
// through the keys it was sent.
func (k *keyRace) quiet(n string) {
	last, since := -1, time.Now()
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		got := len(k.ks.Run(n).Keys) + len(k.ks.Run("o"+n).Keys)
		if got != last {
			last, since = got, time.Now()
		} else if time.Since(since) > 800*time.Millisecond {
			return
		}
	}
}

// reset opens a new /keys page with care: a click in the address bar
// (handled by the browser, not the page), all of it selected, the URL
// and Return, each after a pause.
func (k *keyRace) reset() {
	k.t.Helper()
	if k.cur != "" {
		k.quiet(k.cur)
	}
	n := k.id()
	for range 3 {
		k.post("/input/click", `{"x": 500, "y": 63, "pace": "instant"}`)
		time.Sleep(300 * time.Millisecond)
		k.post("/input/key", `{"keys": "ctrl+a", "pace": "instant"}`)
		time.Sleep(200 * time.Millisecond)
		k.post("/input/type", fmt.Sprintf(`{"text": %q, "pace": "instant"}`, k.page(n)))
		time.Sleep(200 * time.Millisecond)
		k.post("/input/key", `{"keys": "Return", "pace": "instant"}`)
		if k.ready(n, 15*time.Second) {
			return
		}
		k.quiet(n)
	}
	k.t.Fatalf("the /keys page never loaded; tabs %v", k.b.pageURLs())
}

// navigate is one run: keys (or a click on the address bar if keys is
// empty), gap, the /arrived URL, Return.
func (k *keyRace) navigate(keys string, gap time.Duration) keyOutcome {
	n, next := k.cur, k.id()
	to := k.ks.Arrived("a"+n, k.page(next))
	start := time.Now()
	if keys == "" {
		k.post("/input/click", fmt.Sprintf(`{"x": 500, "y": 63, "pace": %q}`, k.pace))
	} else {
		k.key(keys)
	}
	time.Sleep(gap)
	k.typ(to)
	k.key("Return")
	took := time.Since(start) - gap
	return k.finish(n, next, took)
}

func (k *keyRace) finish(n, next string, took time.Duration) keyOutcome {
	// /arrived loads in about 0.3 s; a lost run's address went elsewhere.
	_, arrived := k.ks.WaitRun("a"+n, 2500*time.Millisecond, func(r browsertest.KeyRun) bool { return r.Arrived })
	if arrived && k.ready(next, 15*time.Second) {
		return keyOutcome{chars: k.seen(n), took: took, focused: k.had(n)}
	}
	k.quiet(n)
	out := keyOutcome{lost: !arrived, chars: k.seen(n), took: took, focused: k.had(n)}
	if out.lost {
		r := k.ks.Run(n)
		k.t.Logf("lost: run %s, the page saw %d keys (%d in all: %s), value %q; arrived later %t; tabs %v", n, out.chars, len(r.Keys), keyNames(r.Keys), r.Value, k.ks.Run("a"+n).Arrived, k.b.pageURLs())
		k.shot("lost-" + n)
	}
	k.reset()
	return out
}

// list sends one key call with ctrl+l and three letters: are they the
// page's?
func (k *keyRace) list() keyOutcome {
	n := k.cur
	start := time.Now()
	k.post("/input/key", fmt.Sprintf(`{"keys": ["ctrl+l", "a", "b", "c"], "pace": %q}`, k.pace))
	took := time.Since(start)
	k.quiet(n)
	out := keyOutcome{chars: k.seen(n), took: took, focused: k.had(n)}
	out.lost = out.chars > 0
	k.post("/input/key", `{"keys": "Escape", "pace": "instant"}`)
	k.reset()
	return out
}

// other is a run with another shortcut. alt+d and F6 go to the address
// bar, so the run opens /arrived; ctrl+k (search in the address bar) and
// ctrl+f (the find bar) take the text but don't open it, so the run is
// lost if the page saw any of it; ctrl+t opens /arrived in a new tab,
// which is then closed.
func (k *keyRace) other(keys string) keyOutcome {
	switch keys {
	case "alt+d", "F6":
		return k.navigate(keys, 0)
	}
	n := k.cur
	to := k.ks.Arrived("a"+n, "")
	start := time.Now()
	k.key(keys)
	k.typ(to)
	took := time.Since(start)
	var out keyOutcome
	if keys == "ctrl+t" {
		k.key("Return")
		_, arrived := k.ks.WaitRun("a"+n, 5*time.Second, func(r browsertest.KeyRun) bool { return r.Arrived })
		k.quiet(n)
		out = keyOutcome{lost: !arrived, chars: k.seen(n), took: took, focused: k.had(n)}
		time.Sleep(300 * time.Millisecond)
		if len(k.b.pageURLs()) > 1 {
			k.post("/input/key", `{"keys": "ctrl+w", "pace": "instant"}`)
			time.Sleep(500 * time.Millisecond)
		}
	} else {
		k.quiet(n)
		out = keyOutcome{chars: k.seen(n), took: took, focused: k.had(n)}
		out.lost = out.chars > 0
		k.post("/input/key", `{"keys": "Escape", "pace": "instant"}`)
		time.Sleep(200 * time.Millisecond)
	}
	k.reset()
	return out
}

// navigateAfterStart is a run on a browser whose control socket has just
// answered, after gap: the page it shows (restored, or none) may still be
// loading.
func (k *keyRace) navigateAfterStart(gap time.Duration) keyOutcome {
	n := k.cur
	if n == "" {
		n = k.id()
	}
	next := k.id()
	to := k.ks.Arrived("a"+n, k.page(next))
	ready := k.ks.Run(n).Ready
	start := time.Now()
	time.Sleep(gap)
	k.key("ctrl+l")
	k.typ(to)
	k.key("Return")
	took := time.Since(start) - gap
	_, arrived := k.ks.WaitRun("a"+n, 10*time.Second, func(r browsertest.KeyRun) bool { return r.Arrived })
	k.t.Logf("after start: the page was ready %t when ctrl+l was sent; arrived %t", ready, arrived)
	if arrived && k.ready(next, 15*time.Second) {
		return keyOutcome{chars: k.seen(n), took: took, focused: k.had(n)}
	}
	k.cur = n
	k.quiet(n)
	out := keyOutcome{lost: !arrived, chars: k.seen(n), took: took, focused: k.had(n)}
	k.t.Logf("lost after start: the page saw %d keys; tabs %v", out.chars, k.b.pageURLs())
	k.reset()
	return out
}

// shot saves the top of the screen, toolbar and all, in MEASURE_SHOTS.
func (k *keyRace) shot(name string) {
	dir := os.Getenv("MEASURE_SHOTS")
	if dir == "" {
		return
	}
	resp, err := k.b.http.Get("http://control/screenshot")
	if err != nil {
		k.t.Log(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	png, _ := io.ReadAll(resp.Body)
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%d.png", name, time.Now().Unix())), png, 0o644)
}

func keyNames(keys []browsertest.KeyEvent) string {
	var b strings.Builder
	for _, e := range keys {
		fmt.Fprintf(&b, "%s@%.0f ", e.Key, e.T)
	}
	return b.String()
}

func envDuration(name string) time.Duration {
	return time.Duration(envInt(name, 0)) * time.Millisecond
}

func chromiumVersion(image string) string {
	out, err := exec.Command("docker", "run", "--rm", "--entrypoint", "chromium", image, "--version").Output()
	if err != nil {
		return "unknown: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

func loadAverage() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unknown"
	}
	return strings.Join(strings.Fields(string(b))[:3], " ") + fmt.Sprintf(" (1, 5, 15 min) on %d CPUs", runtime.NumCPU())
}
