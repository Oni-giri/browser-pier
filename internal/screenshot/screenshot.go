// Package screenshot is how the MCP tools shrink the browser's screen
// into the screenshots they show agents, and how a point moves between
// the two: the MCP tools take coordinates in the screenshot's pixels and
// scale them to the screen, and the page tools' Cloudflare note
// (PROJ-120) gives a screen point in the screenshot's pixels too. One
// function for both, so they can't drift apart.
package screenshot

// MaxW and MaxH bound the screenshots agents get over MCP: what models
// see best, and what costs them the fewest tokens.
const MaxW, MaxH = 1280, 720

// Screen is a session's screen size, in pixels.
type Screen struct{ W, H int }

// Size is the size screenshots are scaled to: the whole screen, within
// MaxW × MaxH. The height is worked out as the browser's screenshot
// does, from the width, so the two agree to the pixel.
func (sc Screen) Size() (int, int) {
	if sc.W <= MaxW && sc.H <= MaxH {
		return sc.W, sc.H
	}
	sw := min(MaxW, sc.W*MaxH/sc.H)
	return max(1, sw), max(1, sc.H*sw/sc.W)
}

// ToScreen turns a point of a screenshot into a point on the screen.
func (sc Screen) ToScreen(x, y int) (int, int) {
	x, y = sc.Scale(x, y)
	return min(x, sc.W-1), min(y, sc.H-1)
}

// Scale turns screenshot pixels into screen pixels, without keeping to
// the screen: for a region's far edge.
func (sc Screen) Scale(x, y int) (int, int) {
	sw, sh := sc.Size()
	return min(max(x, 0)*sc.W/sw, sc.W), min(max(y, 0)*sc.H/sh, sc.H)
}

// FromScreen turns a point on the screen into the screenshot's pixel
// nearest it, within the screenshot: the way back from ToScreen.
func (sc Screen) FromScreen(x, y int) (int, int) {
	sw, sh := sc.Size()
	x = (max(x, 0)*sw + sc.W/2) / sc.W
	y = (max(y, 0)*sh + sc.H/2) / sc.H
	return min(x, sw-1), min(y, sh-1)
}
