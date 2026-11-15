package screenshot

import "testing"

func TestScreenToScreenshotScaling(t *testing.T) {
	sc := Screen{1440, 900}
	if w, h := sc.Size(); w != 1152 || h != 720 {
		t.Fatalf("shot %dx%d", w, h)
	}
	if x, y := sc.ToScreen(576, 360); x != 720 || y != 450 {
		t.Fatalf("centre maps to %d,%d", x, y)
	}
	if x, y := sc.ToScreen(5000, 5000); x != 1439 || y != 899 {
		t.Fatalf("off the edge maps to %d,%d", x, y)
	}
	if w, h := (Screen{1280, 720}).Size(); w != 1280 || h != 720 {
		t.Fatalf("a small screen is scaled: %dx%d", w, h)
	}
	// A tall screen: the height comes out as the browser computes it.
	tall := Screen{640, 728}
	if w, h := tall.Size(); w != 632 || h != 728*632/640 {
		t.Fatalf("tall shot %dx%d", w, h)
	}
	// A region's far edge can reach the screen's.
	if x, y := sc.Scale(1152, 720); x != 1440 || y != 900 {
		t.Fatalf("far edge maps to %d,%d", x, y)
	}
}

// FromScreen is the way back: a screen point goes to the screenshot
// pixel that ToScreen takes nearest to it, never off the screenshot.
func TestFromScreen(t *testing.T) {
	sc := Screen{1440, 900}
	if x, y := sc.FromScreen(720, 450); x != 576 || y != 360 {
		t.Fatalf("centre maps to %d,%d", x, y)
	}
	if x, y := sc.FromScreen(1439, 899); x != 1151 || y != 719 {
		t.Fatalf("the far corner maps to %d,%d", x, y)
	}
	if x, y := sc.FromScreen(-5, 5000); x != 0 || y != 719 {
		t.Fatalf("off the screen maps to %d,%d", x, y)
	}
	if x, y := (Screen{1280, 720}).FromScreen(231, 307); x != 231 || y != 307 {
		t.Fatalf("an unscaled screen moves the point to %d,%d", x, y)
	}
	for _, s := range []Screen{{1440, 900}, {1920, 1080}, {2560, 1440}, {640, 728}, {1366, 768}} {
		for x := 0; x < s.W; x += 7 {
			y := x * s.H / s.W
			sx, sy := s.FromScreen(x, y)
			bx, by := s.ToScreen(sx, sy)
			// One screenshot pixel is W/sw screen pixels: back within it.
			sw, _ := s.Size()
			tol := (s.W + sw - 1) / sw
			if d := bx - x; d < -tol || d > tol {
				t.Fatalf("%v: x %d → %d → %d", s, x, sx, bx)
			}
			if d := by - y; d < -tol-1 || d > tol+1 {
				t.Fatalf("%v: y %d → %d → %d", s, y, sy, by)
			}
		}
	}
}
