package pagetools

import (
	"fmt"
	"math"

	"github.com/Oni-giri/browser-pier/internal/screenshot"
)

// Cloudflare's challenge page (PROJ-120, for PROJ-25). Its checkbox is in
// a closed shadow root inside a cross-origin iframe, which neither a
// snapshot nor act can reach, and a click over CDP doesn't pass it; a
// native click on it does. So snapshot, navigate, find and page_content
// notice the page and tell the agent where the checkbox is on the screen,
// for a /computer click (which works on an API-mode session too) or a
// handoff. BrowserPier never clicks it by itself.
//
// The page is told by the challenge widget's hidden response input,
// whose id starts with cf-chl-widget-, inside a laid-out box: the widget,
// on a page with a mark of Cloudflare's challenge page (a #challenge-form
// or #challenge-stage element, a script or iframe from its
// /cdn-cgi/challenge-platform/, not the JavaScript-detections script
// Cloudflare adds to ordinary pages, or a title starting "Just a moment").
// A Turnstile on a site's own form has the same id shape: its input,
// named cf-turnstile-response, counts only inside the challenge page's
// #challenge-stage or #challenge-form, and a widget already answered (its
// input has a value) never counts. The look is part of jsPageInfo, the
// isolated-world read the tools already make: no domain, no script in
// the page's own world.

// The checkbox's place in the widget: 21 px from its left edge, halfway
// down its first 65 px (the widget is 65 px tall; a taller one has its
// checkbox at the same height).
const (
	cfCheckboxLeft = 21
	cfWidgetHeight = 65
)

// The browser window's place on the screen (SPEC §3, images/chromium's
// flags.go and jwmrc): at 0,0 with no frame, above a 32-pixel taskbar
// along the bottom. TestWindowGeometryMatchesTheImage ties these to the
// image, so a change there breaks a test instead of moving clicks.
const (
	windowX, windowY = 0, 0
	taskbarHeight    = 32
)

// cfInfo is what jsPageInfo reports of a challenge widget: its box (the
// input's parent) in the viewport's CSS pixels, and the window's
// geometry as the isolated world sees it. Every number is the page's to
// shape (its layout, its zoom), so what is made of them is clamped to the
// browser's content area.
type cfInfo struct {
	L   float64 `json:"l"`
	T   float64 `json:"t"`
	W   float64 `json:"w"`
	H   float64 `json:"h"`
	DPR float64 `json:"dpr"`
	// IW and IH are innerWidth and innerHeight (CSS pixels), OW and OH
	// outerWidth and outerHeight, SW and SH screen.width and height.
	IW float64 `json:"iw"`
	IH float64 `json:"ih"`
	OW float64 `json:"ow"`
	OH float64 `json:"oh"`
	SW float64 `json:"sw"`
	SH float64 `json:"sh"`
}

// jsChallenge finds the challenge widget, as part of jsPageInfo: null
// when there is none, it isn't laid out, or the page has no mark of a
// challenge page.
const jsChallenge = `(() => {
  const d = document;
  const mark = d.querySelector('#challenge-form, #challenge-stage, script[src*="/cdn-cgi/challenge-platform/"]:not([src*="/scripts/jsd/"]), iframe[src*="/cdn-cgi/challenge-platform/"]') ||
    /^just a moment/i.test(d.title);
  if (!mark) return null;
  let p = null;
  for (const i of d.querySelectorAll('input[id^="cf-chl-widget-"]')) {
    if (i.value || (i.name === 'cf-turnstile-response' && !i.closest('#challenge-stage, #challenge-form'))) continue;
    p = i.parentElement;
    break;
  }
  if (!p) return null;
  const r = p.getBoundingClientRect();
  return r.width > 0 && r.height > 0 ? {l: r.left, t: r.top, w: r.width, h: r.height, dpr: devicePixelRatio, iw: innerWidth, ih: innerHeight, ow: outerWidth, oh: outerHeight, sw: screen.width, sh: screen.height} : null;
})()`

// viewGeom is where the page's viewport sits on the screen, in screen
// pixels.
type viewGeom struct {
	dpr float64 // CSS pixels to screen pixels
	top float64 // the browser's own UI above the viewport (tabs, address bar)
	w   float64 // the viewport's size
	h   float64
	// screenW and screenH are the screen's size, taskbar included.
	screenW, screenH int
}

// geometry works the viewport's place out from what the isolated world
// reports. The device scale factor is 1 on the container's display, so a
// devicePixelRatio other than 1 is the page's zoom: outerHeight stays in
// screen pixels while innerHeight shrinks by the zoom, and the browser's
// UI is outerHeight − innerHeight × devicePixelRatio (at 100%, as SPEC §3
// has it, outerHeight − innerHeight). ok is false for numbers no real
// window gives.
func geometry(cf cfInfo) (viewGeom, bool) {
	for _, v := range []float64{cf.DPR, cf.IW, cf.IH, cf.OW, cf.OH, cf.SW, cf.SH} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return viewGeom{}, false
		}
	}
	if cf.DPR < 0.2 || cf.DPR > 10 || cf.IW < 1 || cf.IH < 1 || cf.OH < 1 || cf.SW < 1 || cf.SH <= taskbarHeight {
		return viewGeom{}, false
	}
	g := viewGeom{dpr: cf.DPR, w: cf.IW * cf.DPR, h: cf.IH * cf.DPR, screenW: int(math.Round(cf.SW)), screenH: int(math.Round(cf.SH))}
	g.top = math.Max(0, cf.OH-g.h)
	return g, true
}

// toScreen turns a point of the viewport, in CSS pixels, into the screen
// pixel it is shown at: the window sits at 0,0 (SPEC §3), so
// x × devicePixelRatio, and y × devicePixelRatio below the browser's UI.
// in is false when the point isn't in the viewport; the pixel is always
// clamped to the browser's content area, never its UI or the taskbar.
func (g viewGeom) toScreen(x, y float64) (sx, sy int, in bool) {
	if math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
		return 0, 0, false
	}
	fx, fy := windowX+x*g.dpr, windowY+g.top+y*g.dpr
	// The content area: below the UI, above the taskbar, on the screen.
	left, right := float64(windowX), math.Min(windowX+g.w, float64(g.screenW))
	top, bottom := windowY+g.top, math.Min(windowY+g.top+g.h, float64(g.screenH-taskbarHeight))
	in = fx >= left && fx < right && fy >= top && fy < bottom
	sx = clampInt(int(math.Round(fx)), int(math.Ceil(left)), int(math.Floor(right))-1)
	sy = clampInt(int(math.Round(fy)), int(math.Ceil(top)), int(math.Floor(bottom))-1)
	return sx, sy, in
}

func clampInt(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}

// cloudflareNote is the line snapshot, navigate, find and page_content
// add when the page is a Cloudflare challenge, "" when it isn't. Its
// words are BrowserPier's, so it goes after the fence, not in it: the
// only part from the page is the point, two integers clamped to the
// browser's content area, and the line says it was read from the page's
// layout.
func cloudflareNote(cf *cfInfo) string {
	if cf == nil {
		return ""
	}
	const lead = "Cloudflare challenge. If it is still here after about 10 s, "
	const tail = " or request_handoff with reason captcha. A named profile keeps the pass."
	g, ok := geometry(*cf)
	if !ok {
		return lead + "click its checkbox, which snapshot can't see, with a real click (computer click on a screenshot)," + tail
	}
	sx, sy, in := g.toScreen(cf.L+cfCheckboxLeft, cf.T+math.Min(cf.H, cfWidgetHeight)/2)
	if !in {
		return lead + "click its checkbox, which snapshot can't see, with a real click; the page has it outside the visible area, so scroll to it and look again," + tail
	}
	mcp := ""
	if g.screenW > 0 && g.screenH > 0 {
		mx, my := screenshot.Screen{W: g.screenW, H: g.screenH}.FromScreen(sx, sy)
		mcp = fmt.Sprintf(" (MCP: click at %d, %d in screenshot pixels)", mx, my)
	}
	// The point comes from the page's layout, and the browser's own
	// prompts (a permission bubble, say) can sit over the page: the agent
	// looks before it clicks.
	return lead + fmt.Sprintf("take a screenshot and check its checkbox, which snapshot can't see, is at this point (position read from the page's layout; the browser's own prompts can cover the page), then click it with a real click: computer click at screen x=%d, y=%d%s,", sx, sy, mcp) + tail
}

// noteChallenge adds the Cloudflare note to the call's notes, which its
// result puts after the fence.
func (c *call) noteChallenge(cf *cfInfo) {
	if n := cloudflareNote(cf); n != "" {
		c.notes = append(c.notes, n)
	}
}
