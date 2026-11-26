//go:build docker

package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// nonASCII has accented Latin letters typed lower and upper case, Latvian
// ones from outside Latin-1, and an emoji from outside the BMP.
const nonASCII = "héllo Rīga ūdens naïve ĀČĒ ģķļ šž 😀"

// cyrillic has 34 distinct letters the keyboard map lacks, more than
// there are spare keys, so it is typed in stretches and every spare key
// is used.
const cyrillic = "Съешь же ещё этих мягких французских булок, да выпей же чаю"

// TestVisualTypesNonASCII is PROJ-195's check of Visual-mode typing of
// characters the keyboard map lacks. For each text it measures, over
// several calls: that each call answers 204 and the page's textarea then
// settles, for a second, on exactly the text (no character missing or
// extra); and that every keydown the page sees is one of the text's own
// ASCII keys (by its code, whatever its key says) or a quiet one: no code
// or Lang5's, and keyCode 0, 229 or 244, so no key that acts, no keypad
// key and no F-key.
// The page's own script reports what its textarea holds: nothing reads
// it over CDP. It types the Latin text 5 times at instant pace and 2 at
// human pace, one accent alone and the Cyrillic text 5 times each at
// instant pace; BROWSERPIER_TEST_TYPING_RUNS=20 types each that many
// times.
func TestVisualTypesNonASCII(t *testing.T) {
	instantRuns, humanRuns := 5, 2
	if s := os.Getenv("BROWSERPIER_TEST_TYPING_RUNS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("BROWSERPIER_TEST_TYPING_RUNS=%q: want a number of runs", s)
		}
		instantRuns, humanRuns = n, n
	}
	a := newRealAPI(t)
	det := browsertest.NewDetector(t, browsertest.SiteHost(t))
	name := fmt.Sprintf("u%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "visual"}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	a.allowLocalNetwork(t, name)
	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || s.Mode != "visual" {
		t.Fatalf("start: %d %+v", code, s)
	}
	// computer sends one action and returns its status and error text.
	computer := func(body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, a.base+"/api/sessions/"+s.ID+"/computer", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+a.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		msg, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(msg))
	}
	must := func(body string) {
		t.Helper()
		if code, msg := computer(body); code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", body, code, msg)
		}
	}
	sw, sh := a.screenSize(t, s.ID)
	for _, body := range []string{
		`{"action": "key", "keys": "ctrl+l", "pace": "instant"}`,
		fmt.Sprintf(`{"action": "type", "text": %q, "pace": "instant"}`, det.InputURL()),
		`{"action": "key", "keys": "Return", "pace": "instant"}`,
	} {
		must(body)
	}
	// Once the page has loaded, its recorder sees a click; then a click
	// in the textarea, which fills the top of the page, focuses it.
	x0, y0 := sw/2, sh*3/4
	var offY float64
	for deadline := time.Now().Add(30 * time.Second); ; {
		must(fmt.Sprintf(`{"action": "click", "x": %d, "y": %d, "pace": "instant"}`, x0, y0))
		time.Sleep(500 * time.Millisecond)
		if downs := ofType(det.Events(), "mousedown"); len(downs) > 0 {
			offY = float64(y0) - downs[0].Y
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the input recorder never loaded")
		}
	}
	must(fmt.Sprintf(`{"action": "click", "x": %d, "y": %d, "pace": "instant"}`, sw/2, int(offY)+40))

	// value returns what the textarea holds once it has stayed the same
	// for a second: as soon as that is want, or after 6 s whatever it is.
	// A late extra character shows.
	value := func(want string) string {
		deadline := time.Now().Add(6 * time.Second)
		got, still := det.Value(), time.Now()
		for {
			time.Sleep(50 * time.Millisecond)
			if v := det.Value(); v != got {
				got, still = v, time.Now()
			}
			if time.Since(still) >= time.Second && (got == want || time.Now().After(deadline)) {
				return got
			}
		}
	}
	// One character alone too: its key is bound for the shortest time.
	for _, pace := range []struct {
		name, text string
		runs       int
	}{{"instant", nonASCII, instantRuns}, {"human", nonASCII, humanRuns}, {"instant", "ž", instantRuns}, {"instant", cyrillic, instantRuns}} {
		// The codes of the text's own keys: its ASCII characters', and
		// Shift's for a capital.
		ownCodes := map[string]bool{}
		for _, r := range pace.text {
			if r >= 0x80 {
				continue
			}
			c, ok := usCode(r)
			if !ok {
				t.Fatalf("no US key code for %q here", r)
			}
			ownCodes[c] = true
			if r >= 'A' && r <= 'Z' {
				ownCodes["ShiftLeft"] = true
			}
		}
		exact, failed := 0, 0
		var slowest time.Duration
		for i := range pace.runs {
			must(`{"action": "key", "keys": ["ctrl+a", "BackSpace"], "pace": "instant"}`)
			if got := value(""); got != "" {
				t.Fatalf("%s run %d: the textarea didn't empty: %q", pace.name, i+1, got)
			}
			time.Sleep(200 * time.Millisecond)
			det.ResetEvents()
			start := time.Now()
			code, msg := computer(fmt.Sprintf(`{"action": "type", "text": %q, "pace": %q}`, pace.text, pace.name))
			took := time.Since(start)
			slowest = max(slowest, took)
			got := value(pace.text)
			switch {
			case code != http.StatusNoContent:
				failed++
				t.Errorf("%s run %d: the call failed after %s: %d %s (the textarea has %q)", pace.name, i+1, took.Round(time.Millisecond), code, msg, got)
			case got != pace.text:
				t.Errorf("%s run %d: typed %q, want %q", pace.name, i+1, got, pace.text)
			default:
				exact++
			}
			// Every keydown is one of the text's own keys, whatever its
			// key says, or a character on a spare key, which reaches the
			// page as text: no code, or an input-method key's, and a key
			// code that is none, an input method's (229) or Lang5's (244).
			// Never a key that acts, a keypad key (108, Numpad*) or an
			// F-key.
			time.Sleep(200 * time.Millisecond)
			seen := map[string]bool{}
			for _, e := range ofType(det.Events(), "keydown") {
				k := fmt.Sprintf("key %q code %q keyCode %d", e.Key, e.Code, e.KeyCode)
				quiet := (e.Code == "" || e.Code == "Lang5") && (e.KeyCode == 0 || e.KeyCode == 229 || e.KeyCode == 244)
				if !ownCodes[e.Code] && !quiet && !seen[k] {
					t.Errorf("%s run %d: a keydown with %s", pace.name, i+1, k)
				}
				if i == 0 && !seen[k] {
					t.Logf("%s pace: a keydown with %s", pace.name, k)
				}
				seen[k] = true
			}
		}
		t.Logf("%s pace, %d characters: %d of %d runs exact, %d calls failed; the slowest call took %s", pace.name, utf8.RuneCountInString(pace.text), exact, pace.runs, failed, slowest.Round(time.Millisecond))
	}
}

// usCode is the code of the US keyboard's key for an ASCII character the
// texts here have.
func usCode(r rune) (string, bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return "Key" + strings.ToUpper(string(r)), true
	case r >= 'A' && r <= 'Z':
		return "Key" + string(r), true
	case r >= '0' && r <= '9':
		return "Digit" + string(r), true
	}
	c, ok := map[rune]string{' ': "Space", ',': "Comma", '.': "Period"}[r]
	return c, ok
}
