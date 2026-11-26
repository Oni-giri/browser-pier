package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// browserID is the browser window's client in these tests.
const browserID = 0x400002

// theBrowser is what windowsAt finds at the address bar of a 1280x800
// screen as BrowserPier sets it up.
var theBrowser = []shownWindow{{Class: "Chromium-browser", Role: "browser", Box: rect{0, 0, 1280, 768}, ID: browserID}}

// atRest is the screen as BrowserPier sets it up: the browser on top at
// the address bar, with the keyboard.
var atRest = screenLook{theBrowser, browserID}

// openVisual is a test visual whose screen looks like looks[i] the i-th
// time it is looked at, and like the last one after.
func openVisual(looks ...screenLook) (*visual, *[]call, *[]point) {
	v, calls := newTestVisual(false)
	var looked []point
	v.look = func(_ context.Context, x, y int) (screenLook, error) {
		looked = append(looked, point{x, y})
		return looks[min(len(looked), len(looks))-1], nil
	}
	v.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return v, calls, &looked
}

func TestAddressBarIsInTheMiddleOfTheToolbar(t *testing.T) {
	for _, c := range []struct {
		w, h, x int
		aim     float64
	}{
		{640, 480, 338, 54}, // 434 px of address bar
		{1280, 800, 658, 64},
		{1920, 1080, 978, 64},
		{7680, 4320, 3858, 64},
	} {
		v := newVisual(c.w, c.h)
		at, size := v.addressBar()
		if at != (point{c.x, 63}) || size.W != c.aim || size.H != 12 {
			t.Errorf("%dx%d: %+v %+v, want (%d, 63), %v wide", c.w, c.h, at, size, c.x, c.aim)
		}
		// Clear of the security chip (about 110 px at its left end) and of
		// the bookmark star and the other buttons at its right end.
		if lo, hi := at.X-int(0.4*size.W), at.X+int(0.4*size.W); lo < addressBarLeft+130 || hi > c.w-addressBarRight-80 {
			t.Errorf("%dx%d: the aim spans %d to %d, too near an end", c.w, c.h, lo, hi)
		}
	}
}

func TestOpenChecksTheURL(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", 8200)
	for _, raw := range []string{
		"", "example.com", "ftp://example.com/", "javascript:alert(1)", "file:///etc/passwd", "data:text/html,hi",
		"chrome://settings", "https://user:pass@example.com/", "https://user@example.com/", "https:///path",
		"https://example.com/a\nb", "https://example.com/\x00", "https://exa mple.com/",
		long,
	} {
		if u, err := openURL(raw); err == nil {
			t.Errorf("%.40q: accepted as %.40q", raw, u)
		}
	}
	for raw, want := range map[string]string{
		"https://example.com/":                "https://example.com/",
		"  http://example.com/a b?q=1#top  ":  "http://example.com/a%20b?q=1#top",
		"https://example.com/?q=hello world":  "https://example.com/?q=hello%20world",
		"https://example.com/--sync exec x":   "https://example.com/--sync%20exec%20x",
		"https://bücher.example/straße?x=é":   "https://b%C3%BCcher.example/stra%C3%9Fe?x=%C3%A9",
		"https://ex ample.com/":               "https://ex%E2%80%A8ample.com/",
		"HTTPS://Example.COM:8443/path?a=b&c": "https://Example.COM:8443/path?a=b&c",
	} {
		if u, err := openURL(raw); err != nil || u != want {
			t.Errorf("%q: %q, %v; want %q", raw, u, err, want)
		}
	}
	// Capped as an instant type is, at either pace.
	if _, err := openURL("https://example.com/" + strings.Repeat("a", maxTypeLen)); err == nil || !strings.HasPrefix(err.Error(), "url: ") {
		t.Errorf("over %d characters: %v", maxTypeLen, err)
	}
	if _, err := openURL("https://example.com/" + strings.Repeat("a", maxHumanTypeLen)); err != nil {
		t.Errorf("%d characters: %v", maxHumanTypeLen+20, err)
	}
	v, calls, looked := openVisual(atRest)
	for _, body := range []string{`{"url": "javascript:alert(1)"}`, `{"url": "https://a@example.com/"}`, `{"url": ""}`, `{}`,
		`{"url": "https://example.com/", "pace": "fast"}`, `{"url": "https://example.com/", "x": 1}`} {
		if rec := serveVisual(v, http.MethodPost, "/input/open", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if len(*calls) != 0 || len(*looked) != 0 {
		t.Fatalf("a refused URL ran %v and looked at %v", *calls, *looked)
	}
}

// pressable reports whether k is a key an open may press: one of the
// URL's characters' (from charKeys, or a letter or digit) or its own.
func pressable(k string) bool {
	if k == "ctrl+a" || k == "Delete" || k == "Return" {
		return true
	}
	for _, name := range charKeys {
		if k == name {
			return true
		}
	}
	return len(k) == 1 && (k[0] >= 'a' && k[0] <= 'z' || k[0] >= 'A' && k[0] <= 'Z' || k[0] >= '0' && k[0] <= '9')
}

// TestOpenTypesTheURLAsText: the URL reaches xdotool only as the text
// after "--" of a type command that ends its run, never as an argument
// xdotool could read as a command, at either pace. A long one is typed
// in runs of openChunk characters, the keyboard looked at before each.
func TestOpenTypesTheURLAsText(t *testing.T) {
	u := "https://example.com/exec/type?key=--sync&window=x#sleep" + strings.Repeat("-exec", 30)
	for _, pace := range []string{"instant", "human"} {
		v, calls, looked := openVisual(atRest)
		rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "`+u+`", "pace": "`+pace+`"}`)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", pace, rec.Code, rec.Body)
		}
		var text strings.Builder
		runs := 0
		for _, c := range *calls {
			a := c.args
			for i := 0; i < len(a); i++ {
				switch a[i] {
				case "type":
					if !slices.Equal(a[i:len(a)-1], []string{"type", "--clearmodifiers", "--delay", "12", "--"}) {
						t.Fatalf("%s: type isn't last with its text after --: %q", pace, a)
					}
					if n := len([]rune(a[len(a)-1])); n > openChunk {
						t.Fatalf("%s: one run types %d characters", pace, n)
					}
					text.WriteString(a[len(a)-1])
					runs++
					i = len(a)
				case "keydown":
					if !pressable(a[i+1]) || a[i+1] != "ctrl+a" && a[i+1] != "Delete" && a[i+1] != "Return" {
						t.Fatalf("%s: pressed %q", pace, a[i+1])
					}
					i++
				case "key":
					j := slices.Index(a[i:], "--") + i
					for j++; j < len(a) && a[j] != "sleep"; j++ {
						if !pressable(a[j]) {
							t.Fatalf("%s: key %q", pace, a[j])
						}
					}
					i = j - 1
				case "mousemove", "sleep", "keyup", "mousedown", "mouseup", "click":
					// their arguments are numbers, buttons or the keys above
				default:
					if strings.Contains(u, a[i]) && len(a[i]) > 3 {
						t.Fatalf("%s: part of the URL, %q, is an argument of its own: %q", pace, a[i], a)
					}
				}
			}
		}
		if text.String() != u {
			t.Errorf("%s: typed %q, want %q", pace, text.String(), u)
		}
		want := (len(u) + openChunk - 1) / openChunk
		// Two looks before the press, one before each run, and one before
		// each of Delete and Return.
		if runs != want || len(*looked) != 2+want+2 {
			t.Errorf("%s: %d runs (want %d), %d looks", pace, runs, want, len(*looked))
		}
	}
}

func TestInstantOpenClicksSelectsTypesAndEnters(t *testing.T) {
	v, calls, looked := openVisual(atRest)
	if rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "instant"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("open: %d %s", rec.Code, rec.Body)
	}
	var got []string
	for _, c := range *calls {
		got = append(got, strings.Join(c.args, " "))
	}
	want := []string{
		"mousemove 658 63 sleep 0.05",
		"click 1 sleep 0.05 key --clearmodifiers -- ctrl+a sleep 0.150",
		"type --clearmodifiers --delay 12 -- https://example.com/",
		"key --clearmodifiers -- Delete",
		"key --clearmodifiers -- Return",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("runs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Equal(*looked, []point{{658, 63}, {658, 63}, {658, 63}, {658, 63}, {658, 63}}) {
		t.Fatalf("looked at %v", *looked)
	}
}

func TestHumanOpenGlidesDwellsLooksThenPresses(t *testing.T) {
	v, calls, looked := openVisual(atRest)
	var order []string
	v.look = func(_ context.Context, x, y int) (screenLook, error) {
		*looked = append(*looked, point{x, y})
		order = append(order, "look")
		return atRest, nil
	}
	run := v.run
	v.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		order = append(order, args[0])
		return run(ctx, name, args...)
	}
	if rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("open: %d %s", rec.Code, rec.Body)
	}
	// The pointer starts at (100, 700) (newTestVisual): a glide up to the
	// address bar, scattered around its middle, then the hand's pause.
	glide := (*calls)[0].args
	pts, rest := moves(t, glide)
	end := pts[len(pts)-1]
	if len(pts) < 5 || len(rest) != 2 || rest[0] != "sleep" || abs(end.X-658) > 26 || abs(end.Y-63) > 5 {
		t.Fatalf("glide %v, then %q", pts, rest)
	}
	// Looked before moving, then where it landed after the pause, and at
	// once the press, which starts with the button.
	press := strings.Join((*calls)[1].args, " ")
	if !strings.HasPrefix(press, "mousedown 1 sleep") || !strings.Contains(press, "keydown ctrl+a sleep") {
		t.Fatalf("press %q", press)
	}
	if !slices.Equal(order[:4], []string{"look", "mousemove", "look", "mousedown"}) {
		t.Fatalf("order %v", order)
	}
	if (*looked)[0] != (point{658, 63}) || (*looked)[1] != end {
		t.Fatalf("looked at %v; landed at %v", *looked, end)
	}
	// The address at 12 ms a key, then Delete and Return held.
	if got := strings.Join((*calls)[2].args, " "); got != "type --clearmodifiers --delay 12 -- https://example.com/" {
		t.Fatalf("typed with %q", got)
	}
	text, _ := typed(t, (*calls)[3:])
	if text != "Delete\n" {
		t.Fatalf("then %q", text)
	}
}

func TestHumanOpenWithThePointerThere(t *testing.T) {
	v, calls, looked := openVisual(atRest)
	v.output = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("x:658 y:63 screen:0 window:4194306\n"), nil
	}
	v.moved = true
	v.mot.Float64() // any aim lands within a few px; the look follows it
	if rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("open: %d %s", rec.Code, rec.Body)
	}
	if first := (*calls)[0].args; first[len(first)-2] != "sleep" {
		t.Fatalf("no pause before the look: %q", first)
	}
	if len(*looked) < 2 || !strings.HasPrefix(strings.Join((*calls)[1].args, " "), "mousedown 1") {
		t.Fatalf("looked %v; runs %v", *looked, *calls)
	}
}

func TestOpenRefusesWhatCoversTheAddressBar(t *testing.T) {
	v := newVisual(1280, 800)
	menu := shownWindow{Override: true, Box: rect{600, 20, 411, 458}}
	for _, c := range []struct {
		name  string
		stack []shownWindow
		want  string // in the refusal; "" if it opens
	}{
		{"the browser", theBrowser, ""},
		{"a menu over it", append([]shownWindow{menu}, theBrowser...), "menu or a bubble"},
		{"a pop-up the size of the window", []shownWindow{{Class: "Chromium-browser", Role: "pop-up", Box: rect{0, 0, 1280, 768}}}, "pop-up window"},
		{"full screen", []shownWindow{{Class: "Chromium-browser", Role: "browser", Box: rect{0, 0, 1280, 800}}}, "full screen"},
		{"full screen under its bubble", []shownWindow{{Override: true, Box: rect{441, 45, 397, 51}}, {Role: "browser", Box: rect{0, 0, 1280, 800}}}, "full screen"},
		{"a dialog", []shownWindow{{Class: "Chromium-browser", Role: "GtkFileChooserDialog", Box: rect{200, 30, 800, 600}}}, "another window"},
		{"a frame with no client", []shownWindow{{Box: rect{0, 0, 1280, 768}}}, "another window"},
		{"nothing", nil, "no browser window"},
		{"only the taskbar's kind", []shownWindow{menu}, "menu or a bubble"},
		{"a window moved", []shownWindow{{Role: "browser", Box: rect{30, 20, 1200, 700}}}, "not where BrowserPier keeps it"},
	} {
		got := v.addressBarCovered(c.stack)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestARefusedOpenPressesNothing(t *testing.T) {
	popup := screenLook{[]shownWindow{{Role: "pop-up", Box: rect{0, 0, 1280, 768}, ID: 0x400019}}, 0x400019}
	for _, pace := range []string{"instant", "human"} {
		v, calls, _ := openVisual(popup)
		rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "`+pace+`"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "pop-up") {
			t.Fatalf("%s: %d %s", pace, rec.Code, rec.Body)
		}
		if len(*calls) != 0 {
			t.Fatalf("%s: ran %v", pace, *calls)
		}
		// A pop-up that opens while the pointer goes there: the move has
		// run, nothing is pressed.
		v, calls, looked := openVisual(atRest, popup)
		rec = serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "`+pace+`"}`)
		if rec.Code != http.StatusConflict || len(*looked) != 2 || len(*calls) != 1 || (*calls)[0].args[0] != "mousemove" {
			t.Fatalf("%s: %d %s, looked %v, ran %v", pace, rec.Code, rec.Body, *looked, *calls)
		}
	}
	// The display can't be read: an error, nothing pressed.
	v, calls, _ := openVisual(atRest)
	v.look = func(context.Context, int, int) (screenLook, error) { return screenLook{}, errors.New("no display") }
	if rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "instant"}`); rec.Code != http.StatusInternalServerError || len(*calls) != 0 {
		t.Fatalf("no display: %d %s, ran %v", rec.Code, rec.Body, *calls)
	}
}

// TestOpenStopsTypingWhenAnotherWindowTakesTheKeyboard: a window that
// opens after the click (a pop-up, a file chooser) and takes the focus
// gets no more of the address, and no Return.
func TestOpenStopsTypingWhenAnotherWindowTakesTheKeyboard(t *testing.T) {
	u := "https://example.com/?token=" + strings.Repeat("s", 2*openChunk)
	stolen := screenLook{theBrowser, 0x500001}
	for _, pace := range []string{"instant", "human"} {
		// Looks: two before the press, one before the first run; the one
		// before the second run finds the keyboard gone.
		v, calls, _ := openVisual(atRest, atRest, atRest, stolen)
		rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "`+u+`", "pace": "`+pace+`"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "another window took the keyboard") ||
			!strings.Contains(rec.Body.String(), "64 of its") {
			t.Fatalf("%s: %d %s", pace, rec.Code, rec.Body)
		}
		var typedText string
		for _, c := range *calls {
			a := strings.Join(c.args, " ")
			if strings.Contains(a, "Return") || strings.Contains(a, "Delete") {
				t.Fatalf("%s: pressed %q after the keyboard went", pace, a)
			}
			if c.args[0] == "type" {
				typedText += c.args[len(c.args)-1]
			}
		}
		if typedText != u[:openChunk] {
			t.Fatalf("%s: typed %q", pace, typedText)
		}
	}
	// No focus at all (None) is no better.
	v, _, _ := openVisual(atRest, atRest, screenLook{theBrowser, 0})
	if rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "instant"}`); rec.Code != http.StatusConflict {
		t.Fatalf("no focus: %d %s", rec.Code, rec.Body)
	}
}

// TestOpenLooksRightBeforeDeleteAndReturn: the pause before Delete, and
// the one between Delete and Return, come before a look at the keyboard,
// and each key is pressed right after its look. A window that takes the
// keyboard during either pause gets no key: after Delete, no Return.
func TestOpenLooksRightBeforeDeleteAndReturn(t *testing.T) {
	stolen := screenLook{theBrowser, 0x500001}
	for _, pace := range []string{"instant", "human"} {
		v, _, _ := openVisual(atRest)
		var order []string
		look := v.look
		v.look = func(ctx context.Context, x, y int) (screenLook, error) {
			order = append(order, "look")
			return look(ctx, x, y)
		}
		v.sleep = func(ctx context.Context, _ time.Duration) error {
			order = append(order, "pause")
			return ctx.Err()
		}
		run := v.run
		v.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if k := slices.IndexFunc(args, func(a string) bool { return a == "Delete" || a == "Return" }); k >= 0 {
				order = append(order, args[k])
			} else {
				order = append(order, args[0])
			}
			return run(ctx, name, args...)
		}
		if rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "`+pace+`"}`); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: open: %d %s", pace, rec.Code, rec.Body)
		}
		if tail := order[len(order)-6:]; !slices.Equal(tail, []string{"pause", "look", "Delete", "pause", "look", "Return"}) {
			t.Fatalf("%s: the open ends %v (all: %v)", pace, tail, order)
		}
		// The window takes the keyboard after Delete: Return isn't pressed.
		// Looks: two before the press, one before the address, one
		// before Delete; the one before Return finds the keyboard gone.
		v, calls, _ := openVisual(atRest, atRest, atRest, atRest, stolen)
		rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "`+pace+`"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Delete pressed, Return not pressed") {
			t.Fatalf("%s: %d %s", pace, rec.Code, rec.Body)
		}
		deleted := false
		for _, c := range *calls {
			a := strings.Join(c.args, " ")
			if strings.Contains(a, "Return") {
				t.Fatalf("%s: pressed %q after the keyboard went", pace, a)
			}
			deleted = deleted || strings.Contains(a, "Delete")
		}
		if !deleted {
			t.Fatalf("%s: Delete wasn't pressed: %v", pace, *calls)
		}
		// Taken during the pause before Delete: neither key.
		v, calls, _ = openVisual(atRest, atRest, atRest, stolen)
		if rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "`+pace+`"}`); rec.Code != http.StatusConflict {
			t.Fatalf("%s: %d %s", pace, rec.Code, rec.Body)
		}
		for _, c := range *calls {
			if a := strings.Join(c.args, " "); strings.Contains(a, "Delete") || strings.Contains(a, "Return") {
				t.Fatalf("%s: pressed %q after the keyboard went", pace, a)
			}
		}
	}
}

func TestOpenNeedsTheMeasuredChromium(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]int{
		"156.0.8078.79-1~deb13u1\n": 156,
		"1:157.0.1.2-1":             157,
		"garbage":                   0,
		"":                          0,
	} {
		f := filepath.Join(dir, "v")
		if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := installedChromium(f); got != want {
			t.Errorf("%q: %d, want %d", content, got, want)
		}
	}
	if got := installedChromium(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("no file: %d", got)
	}
	for _, have := range []int{157, 0} {
		v, calls, looked := openVisual(atRest)
		v.chromium = have
		rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "instant"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "known for Chromium 156 only") || !strings.Contains(rec.Body.String(), "ctrl+l") {
			t.Errorf("Chromium %d: %d %s", have, rec.Code, rec.Body)
		}
		if len(*calls) != 0 || len(*looked) != 0 {
			t.Errorf("Chromium %d: ran %v, looked %v", have, *calls, *looked)
		}
	}
}

func TestOpenWaitsForTheShortcutGapAndDoesntArmIt(t *testing.T) {
	c := newClockedVisual()
	c.look = func(context.Context, int, int) (screenLook, error) { return atRest, nil }
	key := c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+l", "pace": "instant"}`)
	opened := c.do(t, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "instant"}`)
	if opened < key+xdotoolTakes+shortcutGap {
		t.Errorf("the open started %v after the shortcut ended", opened-key-xdotoolTakes)
	}
	c.slept = nil
	c.do(t, http.MethodPost, "/input/type", `{"text": "a", "pace": "instant"}`)
	if len(c.slept) != 0 {
		t.Errorf("text after an open waited %v", c.slept)
	}
}

func TestACutShortOpenLetsGoOfItsKeys(t *testing.T) {
	v, calls, _ := openVisual(atRest)
	n := 0
	v.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{"xdotool", args})
		if n++; n == 2 { // the press: cut short
			return nil, context.Canceled
		}
		return nil, nil
	}
	rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	last := strings.Join((*calls)[len(*calls)-1].args, " ")
	if last != "mouseup 1 keyup ctrl+a" {
		t.Fatalf("after the cut: %q (runs %d)", last, len(*calls))
	}
}
