package pagetools

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/screenshot"
)

// CSS pixels to screen pixels: × devicePixelRatio, below the browser's UI.
func TestCSSToScreen(t *testing.T) {
	for _, tc := range []struct {
		dpr, top float64
		x, y     int
	}{
		{1, 0, 100, 200},
		{1, 80, 100, 280},
		{2, 0, 200, 400},
		{2, 80, 200, 480},
	} {
		g := viewGeom{dpr: tc.dpr, top: tc.top, w: 1280 * tc.dpr, h: 688 * tc.dpr, screenW: int(1280 * tc.dpr), screenH: int(800 * tc.dpr)}
		x, y, in := g.toScreen(100, 200)
		if x != tc.x || y != tc.y || !in {
			t.Errorf("dPR %g, UI %g: (100, 200) → (%d, %d) in %t; want (%d, %d)", tc.dpr, tc.top, x, y, in, tc.x, tc.y)
		}
	}
}

// What the page reports is clamped to the content area: never the
// browser's UI above it, the taskbar below it, or off the screen.
func TestCSSToScreenClamps(t *testing.T) {
	g := viewGeom{dpr: 1, top: 80, w: 1280, h: 688, screenW: 1280, screenH: 800}
	for _, tc := range []struct {
		name   string
		x, y   float64
		sx, sy int
	}{
		{"above the viewport, in the address bar", 100, -40, 100, 80},
		{"below the viewport, in the taskbar", 100, 700, 100, 767},
		{"left of it", -500, 10, 0, 90},
		{"right of it", 5000, 10, 1279, 90},
		{"not a number", math.NaN(), 10, 0, 0},
		{"infinite", 10, math.Inf(1), 0, 0},
	} {
		x, y, in := g.toScreen(tc.x, tc.y)
		if in {
			t.Errorf("%s: in the viewport", tc.name)
		}
		if tc.name != "not a number" && tc.name != "infinite" && (x != tc.sx || y != tc.sy) {
			t.Errorf("%s: (%d, %d), want (%d, %d)", tc.name, x, y, tc.sx, tc.sy)
		}
	}
	// A viewport that would reach into the taskbar stops above it.
	tall := viewGeom{dpr: 1, top: 80, w: 1280, h: 1000, screenW: 1280, screenH: 800}
	if _, y, in := tall.toScreen(10, 700); in || y != 767 {
		t.Errorf("into the taskbar: y %d, in %t", y, in)
	}
}

// The viewport's place from the isolated world's numbers: at 100% the
// browser's UI is outerHeight − innerHeight; zoomed, innerHeight shrinks
// and the UI stays the same.
func TestGeometry(t *testing.T) {
	g, ok := geometry(cfInfo{DPR: 1, IW: 1280, IH: 688, OW: 1280, OH: 768, SW: 1280, SH: 800})
	if !ok || g.top != 80 || g.w != 1280 || g.h != 688 || g.screenW != 1280 || g.screenH != 800 {
		t.Errorf("100%%: %+v %t", g, ok)
	}
	g, ok = geometry(cfInfo{DPR: 2, IW: 640, IH: 344, OW: 1280, OH: 768, SW: 1280, SH: 800})
	if !ok || g.top != 80 || g.w != 1280 || g.h != 688 {
		t.Errorf("200%%: %+v %t", g, ok)
	}
	for _, bad := range []cfInfo{
		{},
		{DPR: math.NaN(), IW: 1280, IH: 688, OH: 768, SW: 1280, SH: 800},
		{DPR: 1, IW: 1280, IH: 688, OH: 768, SW: 1280, SH: 20},
		{DPR: 100, IW: 1280, IH: 688, OH: 768, SW: 1280, SH: 800},
	} {
		if _, ok := geometry(bad); ok {
			t.Errorf("%+v passes", bad)
		}
	}
}

// The window's place in SPEC §3 is the image's: a change there breaks this
// test rather than shifting the note's clicks.
func TestWindowGeometryMatchesTheImage(t *testing.T) {
	src, err := os.ReadFile("../../images/chromium/entrypoint/flags.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf(`"--window-position=%d,%d"`, windowX, windowY),
		fmt.Sprintf("const panelHeight = %d\n", taskbarHeight),
		`fmt.Sprintf("--window-size=%d,%d", c.Width, c.Height-panelHeight)`,
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("the image's flags.go no longer has %s", want)
		}
	}
}

// challengeAt is what jsPageInfo reports of a widget at l, t on a
// 1440×900 screen at 100%.
func challengeAt(l, t float64) map[string]any {
	return map[string]any{"l": l, "t": t, "w": 300, "h": 65, "dpr": 1, "iw": 1440, "ih": 788, "ow": 1440, "oh": 868, "sw": 1440, "sh": 900}
}

func TestCloudflareNote(t *testing.T) {
	if n := cloudflareNote(nil); n != "" {
		t.Errorf("no challenge: %q", n)
	}
	cf := &cfInfo{L: 570, T: 300, W: 300, H: 65, DPR: 1, IW: 1440, IH: 788, OW: 1440, OH: 868, SW: 1440, SH: 900}
	n := cloudflareNote(cf)
	// 570 + 21, and 300 + 65/2 below an 80 px UI.
	sx, sy := 591, 413
	if math.Round(300+32.5+80) != float64(sy) {
		t.Fatal("the test's arithmetic")
	}
	// The screenshot's pixels come from the MCP tools' own scaling.
	mx, my := screenshot.Screen{W: 1440, H: 900}.FromScreen(sx, sy)
	for _, want := range []string{
		"Cloudflare challenge. If it is still here after about 10 s,",
		fmt.Sprintf("computer click at screen x=%d, y=%d (MCP", sx, sy),
		fmt.Sprintf("MCP: click at %d, %d in screenshot pixels", mx, my),
		"request_handoff with reason captcha",
		"A named profile keeps the pass.",
		"where the page's layout puts it",
	} {
		if !strings.Contains(n, want) {
			t.Errorf("no %q in %q", want, n)
		}
	}
	if mx == sx {
		t.Error("the screenshot's pixels aren't scaled")
	}
	// Visual mode's human-paced click lands within about 3 px, well inside
	// the box, so the note doesn't send the agent to instant pace.
	if strings.Contains(n, "instant") {
		t.Errorf("the note asks for instant pace: %q", n)
	}
	// A taller widget: the checkbox is halfway down its first 65 px.
	tall := *cf
	tall.H = 400
	if n := cloudflareNote(&tall); !strings.Contains(n, fmt.Sprintf("x=%d, y=%d", sx, sy)) {
		t.Errorf("a tall widget: %q", n)
	}
	// Scrolled out of view: no coordinates, but what to do.
	away := *cf
	away.T = -500
	if n := cloudflareNote(&away); strings.Contains(n, "x=") || !strings.Contains(n, "outside the visible area") || !strings.Contains(n, "captcha") {
		t.Errorf("out of view: %q", n)
	}
	// Numbers no window gives: no coordinates either.
	odd := *cf
	odd.DPR = 0
	if n := cloudflareNote(&odd); strings.Contains(n, "x=") || !strings.Contains(n, "Cloudflare challenge") {
		t.Errorf("odd numbers: %q", n)
	}
}

// snapshot, navigate, find and page_content add the note, in the fence;
// a page without the widget gets none, and act doesn't add it.
func TestCloudflareNoteInTools(t *testing.T) {
	f := newFixture(t)
	f.fb.challenge = map[string]map[string]any{"https://shop.example/": challengeAt(570, 300)}
	f.fb.pages = func(string) *fnode {
		return el("#document", "RootWebArea", "", box(0, 0, 1440, 788), el("p", "paragraph", "", box(0, 0, 400, 20), txt("Verifying you are human", box(0, 0, 400, 20))))
	}
	f.fb.addTab("https://plain.example/", f.fb.pages(""), true)
	ctx := context.Background()
	inFence := func(name, out string) {
		t.Helper()
		i, start, end := strings.Index(out, "Cloudflare challenge."), strings.Index(out, "<untrusted-content-"), strings.LastIndex(out, "</untrusted-content-")
		if i < 0 || i > end || start < 0 || start > i {
			t.Errorf("%s: no note in the fence:\n%s", name, out)
		}
		if !strings.Contains(out, "computer click at screen x=591, y=413 (MCP") {
			t.Errorf("%s: coordinates:\n%s", name, out)
		}
	}
	none := func(name, out string, err error) {
		t.Helper()
		if err != nil || strings.Contains(out, "Cloudflare") {
			t.Errorf("%s on a plain page: %v\n%s", name, err, out)
		}
	}
	out, err := f.tools.Snapshot(ctx, f.session(), f.who, SnapshotIn{})
	none("snapshot", out, err)
	out, err = f.tools.Find(ctx, f.session(), f.who, FindIn{Text: "human"})
	none("find", out, err)
	out, err = f.text(TextIn{Format: "markdown"})
	none("markdown", out, err)

	out, err = f.tools.Navigate(ctx, f.session(), f.who, NavigateIn{URL: "https://shop.example/"})
	if err != nil {
		t.Fatal(err)
	}
	inFence("navigate", out)
	for _, tc := range []struct {
		name string
		in   SnapshotIn
	}{{"snapshot", SnapshotIn{}}, {"snapshot with no budget", SnapshotIn{MaxChars: new(0)}}, {"snapshot diff", SnapshotIn{Diff: true}}} {
		out, err := f.tools.Snapshot(ctx, f.session(), f.who, tc.in)
		if err != nil {
			t.Fatal(tc.name, err)
		}
		if tc.in.Diff && !strings.Contains(out, "no changes since the last snapshot") {
			t.Errorf("not a diff:\n%s", out)
		}
		inFence(tc.name, out)
	}
	out, err = f.tools.Find(ctx, f.session(), f.who, FindIn{Text: "human"})
	if err != nil {
		t.Fatal(err)
	}
	inFence("find", out)
	out, err = f.tools.Find(ctx, f.session(), f.who, FindIn{Text: "absent"})
	if err != nil {
		t.Fatal(err)
	}
	inFence("find with no match", out)
	for _, format := range []string{"markdown", "readability"} {
		out, err := f.text(TextIn{Format: format})
		if err != nil {
			t.Fatal(format, err)
		}
		inFence(format, out)
	}
	// The note says nothing in the activity log.
	for _, a := range f.m.Activity(f.sess.ID) {
		if strings.Contains(a.What, "Cloudflare") {
			t.Errorf("activity: %q", a.What)
		}
	}
	f.closedAfter()
}
