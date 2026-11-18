package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// theBrowser is what windowsAt finds at the address bar of a 1280x800
// screen as BrowserPier sets it up.
var theBrowser = []shownWindow{{Class: "Chromium-browser", Role: "browser", Box: rect{0, 0, 1280, 768}}}

// openVisual is a test visual whose address bar shows stack, every time
// it is looked at, or each of stacks in turn.
func openVisual(stacks ...[]shownWindow) (*visual, *[]call, *[]point) {
	v, calls := newTestVisual(false)
	var looked []point
	v.windows = func(_ context.Context, x, y int) ([]shownWindow, error) {
		looked = append(looked, point{x, y})
		s := stacks[min(len(looked), len(stacks))-1]
		return s, nil
	}
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
		if u, err := openURL(raw, maxTypeLen); err == nil {
			t.Errorf("%.40q: accepted as %.40q", raw, u)
		}
	}
	for raw, want := range map[string]string{
		"https://example.com/":                "https://example.com/",
		"  http://example.com/a b?q=1#top  ":  "http://example.com/a%20b?q=1#top",
		"https://example.com/--sync exec x":   "https://example.com/--sync%20exec%20x",
		"https://bücher.example/straße?x=é":   "https://b%C3%BCcher.example/stra%C3%9Fe?x=é",
		"https://ex\u2028ample.com/":          "https://ex%E2%80%A8ample.com/",
		"HTTPS://Example.COM:8443/path?a=b&c": "https://Example.COM:8443/path?a=b&c",
	} {
		if u, err := openURL(raw, maxTypeLen); err != nil || u != want {
			t.Errorf("%q: %q, %v; want %q", raw, u, err, want)
		}
	}
	// The length caps are type's: 500 characters at human pace.
	u := "https://example.com/" + strings.Repeat("a", maxHumanTypeLen)
	if _, err := openURL(u, maxHumanTypeLen); err == nil || !strings.HasPrefix(err.Error(), "url: ") {
		t.Errorf("%d characters at human pace: %v", len(u), err)
	}
	if _, err := openURL(u, maxTypeLen); err != nil {
		t.Errorf("%d characters at instant pace: %v", len(u), err)
	}
	v, calls, looked := openVisual(theBrowser)
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

// keysyms are the keys an open may press: the URL's characters' (from
// charKeys, or a letter or digit) and its own.
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
// after "--" of a type command that ends its run, or as keys from the
// fixed table, never as an argument xdotool could read as a command, at
// either pace.
func TestOpenTypesTheURLAsText(t *testing.T) {
	const u = "https://example.com/exec/type?key=--sync&window=x#sleep"
	for _, pace := range []string{"instant", "human"} {
		v, calls, looked := openVisual(theBrowser)
		rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "`+u+`", "pace": "`+pace+`"}`)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", pace, rec.Code, rec.Body)
		}
		var text strings.Builder
		for _, c := range *calls {
			a := c.args
			for i := 0; i < len(a); i++ {
				switch a[i] {
				case "type":
					if a[len(a)-2] != "--" || !slices.Contains(a[i:], "--") {
						t.Fatalf("%s: type isn't last with its text after --: %q", pace, a)
					}
					text.WriteString(a[len(a)-1])
					i = len(a)
				case "keydown":
					if !pressable(a[i+1]) {
						t.Fatalf("%s: pressed %q", pace, a[i+1])
					}
					if a[i+1] != "ctrl+a" && a[i+1] != "Delete" && a[i+1] != "Return" {
						text.WriteString(keyText(a[i+1]))
					}
					i++
				case "key":
					// Instant: key --clearmodifiers [--delay 50] -- keys…, then
					// a sleep or the end.
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
		if len(*looked) == 0 {
			t.Errorf("%s: never looked at the address bar", pace)
		}
	}
}

// keyText is the character a keysym from charKeys, a letter or a digit
// types.
func keyText(k string) string {
	for r, name := range charKeys {
		if name == k {
			return string(r)
		}
	}
	return k
}

func TestInstantOpenClicksSelectsTypesAndEnters(t *testing.T) {
	v, calls, looked := openVisual(theBrowser)
	if rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "instant"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("open: %d %s", rec.Code, rec.Body)
	}
	var got []string
	for _, c := range *calls {
		got = append(got, strings.Join(c.args, " "))
	}
	want := []string{
		"mousemove 658 63 click 1 sleep 0.05 key --clearmodifiers -- ctrl+a sleep 0.150",
		"type --clearmodifiers --delay 12 -- https://example.com/",
		"sleep 0.1 key --clearmodifiers --delay 50 -- Delete Return",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("runs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Equal(*looked, []point{{658, 63}}) {
		t.Fatalf("looked at %v", *looked)
	}
}

func TestHumanOpenGlidesPressesAndTypes(t *testing.T) {
	v, calls, looked := openVisual(theBrowser)
	if rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("open: %d %s", rec.Code, rec.Body)
	}
	// The pointer starts at (100, 700) (newTestVisual): a glide up to the
	// address bar, scattered around its middle.
	pts, rest := moves(t, (*calls)[0].args)
	end := pts[len(pts)-1]
	if len(pts) < 5 || len(rest) != 0 || abs(end.X-658) > 26 || abs(end.Y-63) > 5 {
		t.Fatalf("glide %v, then %q", pts, rest)
	}
	press := strings.Join((*calls)[1].args, " ")
	for _, want := range []string{"mousedown 1 sleep", "mouseup 1 sleep", "keydown ctrl+a sleep", "keyup ctrl+a sleep"} {
		if !strings.Contains(press, want) {
			t.Fatalf("press %q lacks %q", press, want)
		}
	}
	if strings.HasPrefix(press, "mousemove") {
		t.Fatalf("the press moves: %q", press)
	}
	text, _ := typed(t, (*calls)[2:])
	if text != "https://example.com/Delete\n" {
		t.Fatalf("typed %q", text)
	}
	// Looked before moving and again where it landed, before pressing.
	if len(*looked) != 2 || (*looked)[0] != (point{658, 63}) || (*looked)[1] != end {
		t.Fatalf("looked at %v; landed at %v", *looked, end)
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
	popup := []shownWindow{{Role: "pop-up", Box: rect{0, 0, 1280, 768}}}
	for _, pace := range []string{"instant", "human"} {
		v, calls, _ := openVisual(popup)
		rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "`+pace+`"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "pop-up") {
			t.Fatalf("%s: %d %s", pace, rec.Code, rec.Body)
		}
		if len(*calls) != 0 {
			t.Fatalf("%s: ran %v", pace, *calls)
		}
	}
	// A pop-up that opens while the pointer glides there: the glide has
	// run, nothing is pressed.
	v, calls, looked := openVisual(theBrowser, popup)
	rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/"}`)
	if rec.Code != http.StatusConflict || len(*looked) != 2 {
		t.Fatalf("%d %s, looked %v", rec.Code, rec.Body, *looked)
	}
	for _, c := range *calls {
		if a := strings.Join(c.args, " "); strings.Contains(a, "mousedown") || strings.Contains(a, "key") || strings.Contains(a, "type") {
			t.Fatalf("pressed after the pop-up came: %q", a)
		}
	}
	// The display can't be read: an error, nothing pressed.
	v, calls, _ = openVisual(theBrowser)
	v.windows = func(context.Context, int, int) ([]shownWindow, error) { return nil, errors.New("no display") }
	if rec := serveVisual(v, http.MethodPost, "/input/open", `{"url": "https://example.com/", "pace": "instant"}`); rec.Code != http.StatusInternalServerError || len(*calls) != 0 {
		t.Fatalf("no display: %d %s, ran %v", rec.Code, rec.Body, *calls)
	}
}

func TestOpenWaitsForTheShortcutGapAndDoesntArmIt(t *testing.T) {
	c := newClockedVisual()
	c.windows = func(context.Context, int, int) ([]shownWindow, error) { return theBrowser, nil }
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
	v, calls, _ := openVisual(theBrowser)
	n := 0
	v.run = func(ctx context.Context, _ string, args ...string) ([]byte, error) {
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
