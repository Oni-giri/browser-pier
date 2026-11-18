package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
const (
	addressBarY     = 63
	addressBarLeft  = 121
	addressBarRight = 85 // from the screen's right edge
)

// addressBar is the point the open action clicks, and the size of the
// target its aim scatters over: at most 64 px wide (within about 26 px of
// the middle), less on a narrow screen, and 12 px tall.
func (v *visual) addressBar() (point, motion.Size) {
	left, right := addressBarLeft, v.width-addressBarRight
	return point{(left + right) / 2, addressBarY}, motion.Size{W: float64(min(64, (right-left)/8)), H: 12}
}

// refusal is an error that answers 409: the action was refused as the
// screen stands, and nothing was pressed.
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

// checkAddressBar refuses the action if something covers the address bar
// at at.
func (v *visual) checkAddressBar(ctx context.Context, at point) error {
	stack, err := v.windows(ctx, at.X, at.Y)
	if err != nil {
		return err
	}
	if why := v.addressBarCovered(stack); why != "" {
		return refusal(why)
	}
	return nil
}

type openRequest struct {
	URL  string
	Pace string
}

// openURL checks the address (weburl.Check, as navigate does) and that it
// types as itself: no space, line break or control character, any of
// which xdotool would type as a key. most caps its characters, as type's.
func openURL(raw string, most int) (string, error) {
	u, err := weburl.Check(raw)
	if err != nil {
		return "", err
	}
	if strings.ContainsFunc(u, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", errors.New("url: has a space or a control character")
	}
	if err := checkText(u, most); err != nil {
		return "", errors.New("url" + strings.TrimPrefix(err.Error(), "text"))
	}
	return u, nil
}

// instantOpen is the open action at instant pace: the click, select all,
// the address at 12 ms a key ("type --" takes the rest as text, so it
// ends its run), a pause for the address bar's suggestions, then Delete,
// which drops a completion the address bar added after the text, and
// Return.
func (v *visual) instantOpen(at point, u string) []xcmd {
	return []xcmd{
		{check: func(ctx context.Context) error { return v.checkAddressBar(ctx, at) }},
		{args: []string{"mousemove", strconv.Itoa(at.X), strconv.Itoa(at.Y), "click", "1", "sleep", "0.05",
			"key", "--clearmodifiers", "--", "ctrl+a", "sleep", secs(shortcutGap)}, release: []string{"mouseup", "1", "keyup", "ctrl+a"}},
		{args: []string{"type", "--clearmodifiers", "--delay", "12", "--", u}},
		{args: []string{"sleep", "0.1", "key", "--clearmodifiers", "--delay", "50", "--", "Delete", "Return"}, release: []string{"keyup", "Delete", "Return"}},
	}
}

// humanOpen is the open action at human pace: the pointer glides to the
// address bar, which is looked at again once it is there (a pop-up may
// have opened meanwhile), presses and holds, then select all, the address
// at a typist's rhythm, Delete and Return, each held, with a hand's
// pauses between.
func (v *visual) humanOpen(from hand, at point, size motion.Size, u string) []xcmd {
	aim := v.mot.Aim(motion.Point{X: float64(at.X), Y: float64(at.Y)}, size)
	there := v.screenPoint(aim)
	check := func(p point) xcmd {
		return xcmd{check: func(ctx context.Context) error { return v.checkAddressBar(ctx, p) }}
	}
	cmds := []xcmd{check(at)}
	// No moves if the pointer is there already, and xdotool takes no
	// empty run.
	if g := v.glide(nil, from, aim); len(g) > 0 {
		cmds = append(cmds, xcmd{args: g}, check(there))
	}
	press := []string{"sleep", secs(v.mot.Dwell()), "mousedown", "1", "sleep", secs(v.mot.KeyHold()), "mouseup", "1",
		"sleep", secs(v.mot.JitterDuration(0.25, 0.08)),
		"keydown", "ctrl+a", "sleep", secs(v.mot.KeyHold()), "keyup", "ctrl+a",
		"sleep", secs(max(v.mot.JitterDuration(0.25, 0.08), shortcutGap))}
	cmds = append(cmds, xcmd{args: press, release: []string{"mouseup", "1", "keyup", "ctrl+a"}})
	cmds = append(cmds, v.humanType(u)...)
	enter := []string{"sleep", secs(v.mot.JitterDuration(0.3, 0.1)), "keydown", "Delete", "sleep", secs(v.mot.KeyHold()), "keyup", "Delete",
		"sleep", secs(v.mot.JitterDuration(0.2, 0.06)), "keydown", "Return", "sleep", secs(v.mot.KeyHold()), "keyup", "Return"}
	return append(cmds, xcmd{args: enter, release: []string{"keyup", "Delete", "Return"}})
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
	most := maxHumanTypeLen
	if now {
		most = maxTypeLen
	}
	u, err := openURL(o.URL, most)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	at, size := v.addressBar()
	v.perform(w, r, keysAction, func(from func() hand) []xcmd {
		if now {
			return v.instantOpen(at, u)
		}
		return v.humanOpen(from(), at, size, u)
	})
}
