package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Oni-giri/browser-pier/internal/motion"
	"github.com/Oni-giri/browser-pier/internal/weburl"
)

// The open action (PROJ-164) opens an address as a person does: a click
// in the browser's address bar, everything there selected, the address
// typed, Return. A click on the browser's own UI moves the keyboard focus
// there at once, without asking the page, so a page whose key handler is
// busy can't take the address, as it can after ctrl+l (PROJ-151,
// images/chromium/ANTI-BOT.md). Nothing here touches CDP: the address
// bar's place comes from the window's fixed layout, and what covers it
// from the X display's window tree (xwin.go).

// Where the address bar is. BrowserPier's browser window sits at 0,0 with
// no frame, filling the screen above the taskbar (flags.go, jwmrc), and
// Chromium draws its tab strip and toolbar at a fixed size whatever the
// screen's (no scaling). Measured on Chromium 156 at 640x480, 1280x800 and
// 1920x1080 (TestOpenAtEveryScreenSize): the tab strip is 40 px tall, and
// the address bar runs from y = 46 to 80 and from x = 121 (after back,
// forward and reload) to the screen's width less 85 (the profile and
// menu buttons). Its ends hold buttons of their own (the site's security
// chip, "Not secure" on plain HTTP, about 110 px; the bookmark star and
// others), so the click aims at its middle.
//
// These hold for addressBarChromium only. Another Chromium may draw its
// toolbar elsewhere, and a click there would go to a tab or the page with
// the address after it, so open refuses on any other version until the
// layout is measured again and this constant moved.
const (
	addressBarY        = 63
	addressBarLeft     = 121
	addressBarRight    = 85 // from the screen's right edge
	addressBarChromium = 156
)

// chromiumVersionFile holds the installed Chromium package's version,
// written when the image is built (Dockerfile).
const chromiumVersionFile = "/etc/browserpier/chromium-version"

// installedChromium reads the Chromium major version from a Debian
// package version in file ("156.0.8078.79-1~deb13u1", maybe with an
// epoch), or 0 if it can't.
func installedChromium(file string) int {
	b, err := os.ReadFile(file) //nolint:gosec // a fixed path in the image, or a test's
	if err != nil {
		return 0
	}
	return chromiumMajor(string(b))
}

func chromiumMajor(version string) int {
	v := strings.TrimSpace(version)
	if i := strings.IndexByte(v, ':'); i >= 0 {
		v = v[i+1:]
	}
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// layoutKnown refuses the action on a Chromium whose address bar hasn't
// been measured.
func (v *visual) layoutKnown() error {
	if v.chromium == addressBarChromium {
		return nil
	}
	have := "an unknown version"
	if v.chromium > 0 {
		have = "Chromium " + strconv.Itoa(v.chromium)
	}
	return refusal(fmt.Sprintf("the address bar's place is known for Chromium %d only, and this browser is %s: open the address with the key ctrl+l and typing it instead", addressBarChromium, have))
}

// addressBar is the point the open action clicks, and the size of the
// target its aim scatters over: at most 64 px wide (within about 26 px of
// the middle), less on a narrow screen, and 12 px tall.
func (v *visual) addressBar() (point, motion.Size) {
	left, right := addressBarLeft, v.width-addressBarRight
	return point{(left + right) / 2, addressBarY}, motion.Size{W: float64(min(64, (right-left)/8)), H: 12}
}

// refusal is an error that answers 409: the action was refused as the
// screen stands, and nothing more was pressed.
type refusal string

func (r refusal) Error() string { return string(r) }

// addressBarCovered says why the address bar can't be clicked, given the
// windows showing at its point (windowsAt, top first), or "" if it can:
// the top window there must be Chromium's tabbed browser window, where
// BrowserPier keeps it.
func (v *visual) addressBarCovered(stack []shownWindow) string {
	var managed *shownWindow
	for i := range stack {
		if !stack[i].Override {
			managed = &stack[i]
			break
		}
	}
	full := rect{0, 0, v.width, v.height}
	switch {
	// Before what covers it: full screen puts a bubble of its own there.
	case managed != nil && managed.Role == "browser" && managed.Box == full:
		return "the browser is full screen, so it shows no address bar: press F11 to leave it (Escape, if a page went full screen), then try again"
	case len(stack) > 0 && stack[0].Override:
		return "a menu or a bubble is open over the address bar: take a screenshot, close it (Escape, or a click away from it), then try again"
	case managed == nil:
		return "no browser window shows at the address bar"
	case managed.Role == "pop-up":
		return "a pop-up window covers the browser's address bar: finish in it or close it (ctrl+w while it is in front), then try again"
	case managed.Role != "browser":
		return "another window covers the browser's address bar (a dialog?): take a screenshot and close it, then try again"
	case managed.Box != (rect{0, 0, v.width, v.height - panelHeight}):
		return fmt.Sprintf("the browser window is at %d,%d, %dx%d, not where BrowserPier keeps it, so its address bar can't be found", managed.Box.X, managed.Box.Y, managed.Box.W, managed.Box.H)
	}
	return ""
}

// opening is one open action's state between its runs: the browser
// window whose address bar it clicks, which must keep the keyboard
// until Return.
type opening struct {
	v       *visual
	at      point
	browser uint32
}

// clear looks at the address bar's point just before the press: the
// browser window must be on top there. It remembers that window.
func (o *opening) clear(ctx context.Context) error {
	l, err := o.v.look(ctx, o.at.X, o.at.Y)
	if err != nil {
		return err
	}
	if why := o.v.addressBarCovered(l.Stack); why != "" {
		return refusal(why)
	}
	o.browser = l.Stack[len(l.Stack)-1].ID
	return nil
}

// still checks, after the click, that the browser window clicked still
// has the keyboard: a window that opened meanwhile (a pop-up, a file
// chooser) would get the rest of the address and Return. done says how
// far the open got, for the refusal.
func (o *opening) still(done string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		l, err := o.v.look(ctx, o.at.X, o.at.Y)
		if err != nil {
			return err
		}
		if l.Focus == 0 || l.Focus != o.browser {
			return refusal("another window took the keyboard while the address was being typed (" + done + ", Return not pressed): take a screenshot")
		}
		return nil
	}
}

// typedOf says how much of the address was typed, for still.
func typedOf(typed, of int) string {
	return fmt.Sprintf("%d of its %d characters typed", typed, of)
}

// pause waits d between runs, or until the request ends: a pause in Go
// rather than in an xdotool run, so the keyboard can be looked at after
// it, just before the next press.
func (o *opening) pause(d time.Duration) func(ctx context.Context) error {
	return func(ctx context.Context) error { return o.v.sleep(ctx, d) }
}

// enter is the end of an open, at either pace: a pause for the address
// bar's suggestions, Delete (which drops a completion the address bar
// added after the text), a pause, Return. Each press comes right after a
// look at the keyboard (still), which follows the pause before it, so a
// window that takes the keyboard during a pause gets neither key; one that
// takes it in the few milliseconds between a look and its press still
// gets that press. press makes a key's run.
func (o *opening) enter(of int, suggest, between time.Duration, press func(key string) []string) []xcmd {
	all := typedOf(of, of)
	return []xcmd{
		{check: o.pause(suggest)},
		{check: o.still(all)},
		{args: press("Delete"), release: []string{"keyup", "Delete"}},
		{check: o.pause(between)},
		{check: o.still(all + ", Delete pressed")},
		{args: press("Return"), release: []string{"keyup", "Return"}},
	}
}

type openRequest struct {
	URL  string
	Pace string
}

// openURL checks the address (weburl.Check, as navigate does) and that it
// types as itself: no space, line break or control character, any of
// which xdotool would type as a key. Its characters are capped as an
// instant type's.
func openURL(raw string) (string, error) {
	u, err := weburl.Check(raw)
	if err != nil {
		return "", err
	}
	if strings.ContainsFunc(u, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", errors.New("url: has a space or a control character")
	}
	if err := checkText(u, maxTypeLen); err != nil {
		return "", errors.New("url" + strings.TrimPrefix(err.Error(), "text"))
	}
	return u, nil
}

// openChunk is how many characters of the address one xdotool run types,
// at 12 ms a key: under a second, after which the keyboard is looked at
// again.
const openChunk = 64

// typeAddress types the address at 12 ms a key, at either pace: no page
// sees these keys or their rhythm, they go to the address bar, and a
// person pastes an address as often as they type it (but a paste would
// overwrite the profile's clipboard). Before each run, the keyboard is
// looked at (still); enter looks again after the last.
func (o *opening) typeAddress(u string) []xcmd {
	runes := []rune(u)
	var cmds []xcmd
	for i := 0; i < len(runes); i += openChunk {
		cmds = append(cmds, xcmd{check: o.still(typedOf(i, len(runes)))},
			xcmd{args: []string{"type", "--clearmodifiers", "--delay", "12", "--", string(runes[i:min(i+openChunk, len(runes))])}})
	}
	return cmds
}

// instantOpen is the open action at instant pace: the pointer moves to
// the address bar, which is looked at once it is there; the click and
// select all; the address; then Delete and Return (enter), 100 ms after
// the address and 50 ms apart.
func (o *opening) instantOpen(u string) []xcmd {
	x, y := strconv.Itoa(o.at.X), strconv.Itoa(o.at.Y)
	cmds := []xcmd{
		{check: o.clear},
		{args: []string{"mousemove", x, y, "sleep", "0.05"}},
		{check: o.clear},
		{args: []string{"click", "1", "sleep", "0.05", "key", "--clearmodifiers", "--", "ctrl+a", "sleep", secs(shortcutGap)},
			release: []string{"mouseup", "1", "keyup", "ctrl+a"}},
	}
	cmds = append(cmds, o.typeAddress(u)...)
	return append(cmds, o.enter(len([]rune(u)), 100*time.Millisecond, 50*time.Millisecond, func(key string) []string {
		return []string{"key", "--clearmodifiers", "--", key}
	})...)
}

// humanOpen is the open action at human pace: the pointer glides to the
// address bar and pauses there as a hand does; the address bar is looked
// at again only then, just before the press; the button and ctrl+a are
// held; the address goes in as at instant pace (typeAddress); Delete and
// Return are held, after a hand's pauses (enter).
func (o *opening) humanOpen(from hand, size motion.Size, u string) []xcmd {
	v := o.v
	first := &opening{v: v, at: o.at} // the middle, before the pointer moves
	aim := v.mot.Aim(motion.Point{X: float64(o.at.X), Y: float64(o.at.Y)}, size)
	o.at = v.screenPoint(aim)
	// The dwell comes before the last look, so the press follows it at once.
	glide := append(v.glide(nil, from, aim), "sleep", secs(v.mot.Dwell()))
	cmds := []xcmd{{check: first.clear}, {args: glide}, {check: o.clear}}
	press := []string{"mousedown", "1", "sleep", secs(v.mot.KeyHold()), "mouseup", "1",
		"sleep", secs(v.mot.JitterDuration(0.25, 0.08)),
		"keydown", "ctrl+a", "sleep", secs(v.mot.KeyHold()), "keyup", "ctrl+a",
		"sleep", secs(max(v.mot.JitterDuration(0.25, 0.08), shortcutGap))}
	cmds = append(cmds, xcmd{args: press, release: []string{"mouseup", "1", "keyup", "ctrl+a"}})
	cmds = append(cmds, o.typeAddress(u)...)
	return append(cmds, o.enter(len([]rune(u)), v.mot.JitterDuration(0.3, 0.1), v.mot.JitterDuration(0.2, 0.06), func(key string) []string {
		return []string{"keydown", key, "sleep", secs(v.mot.KeyHold()), "keyup", key}
	})...)
}

func (v *visual) open(w http.ResponseWriter, r *http.Request) {
	var o openRequest
	if !decode(w, r, &o) {
		return
	}
	now, err := instant(o.Pace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u, err := openURL(o.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := v.layoutKnown(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	at, size := v.addressBar()
	v.perform(w, r, keysAction, func(from func() hand) []xcmd {
		op := &opening{v: v, at: at}
		if now {
			return op.instantOpen(u)
		}
		return op.humanOpen(from(), size, u)
	})
}
