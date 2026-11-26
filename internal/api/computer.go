package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/weburl"
)

// maxPause caps a wait action.
const maxPause = 10 * time.Second

type pointJSON struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// computerRequest is one Visual-mode action. Coordinates are screen pixels.
type computerRequest struct {
	Action string `json:"action"` // click, type, scroll, key, drag, open, wait or zoom
	X      *int   `json:"x"`
	Y      *int   `json:"y"`
	Button string `json:"button"`
	Double bool   `json:"double"`
	Text   string `json:"text"`
	DX     int    `json:"dx"`
	DY     int    `json:"dy"`
	// Keys is a combination like "ctrl+l", or a list of them.
	Keys json.RawMessage `json:"keys"`
	From *pointJSON      `json:"from"`
	To   *pointJSON      `json:"to"`
	// URL is the address open opens (PROJ-164).
	URL string `json:"url"`
	MS  int    `json:"ms"`
	W   int    `json:"w"`
	H   int    `json:"h"`
	// Screenshot returns the screen after the action, Width wide if set.
	Screenshot bool `json:"screenshot"`
	Width      int  `json:"width"`
	// Pace is human (the default: the pointer glides, aims and holds, and
	// keys come at a typist's rhythm) or instant (PROJ-113).
	Pace string `json:"pace"`
	// DownloadWaitMS is how long a click or key press waits for a
	// download it started to finish: 3,000 ms if unset, 0 not to wait,
	// at most 30,000 (PROJ-116). When set, it is also how long it looks
	// for one to start (files.Mark.Expect). Other actions ignore it.
	DownloadWaitMS *int `json:"download_wait_ms"`
}

// downloads reports whether an action may start a download, so its
// notice is looked for: a click on a link, a key (Return on one), or an
// address that is a file.
func (c computerRequest) downloads() bool {
	return c.Action == "click" || c.Action == "key" || c.Action == "open"
}

// inputBody is what the control socket takes for an input action, or an
// error saying what is missing.
func (c computerRequest) inputBody() (path string, body any, err error) {
	at := func() error {
		if c.X == nil || c.Y == nil {
			return fmt.Errorf("%s: want x and y", c.Action)
		}
		return nil
	}
	switch c.Pace {
	case "", "human", "instant":
	default:
		return "", nil, fmt.Errorf("pace %q: want human or instant", c.Pace)
	}
	switch c.Action {
	case "click":
		type click struct {
			X      int    `json:"x"`
			Y      int    `json:"y"`
			Button string `json:"button,omitempty"`
			Double bool   `json:"double,omitempty"`
			Pace   string `json:"pace,omitempty"`
		}
		if err := at(); err != nil {
			return "", nil, err
		}
		return "/input/click", click{*c.X, *c.Y, c.Button, c.Double, c.Pace}, nil
	case "type":
		return "/input/type", struct {
			Text string `json:"text"`
			Pace string `json:"pace,omitempty"`
		}{c.Text, c.Pace}, nil
	case "scroll":
		type scroll struct {
			X    int    `json:"x"`
			Y    int    `json:"y"`
			DX   int    `json:"dx"`
			DY   int    `json:"dy"`
			Pace string `json:"pace,omitempty"`
		}
		if err := at(); err != nil {
			return "", nil, err
		}
		return "/input/scroll", scroll{*c.X, *c.Y, c.DX, c.DY, c.Pace}, nil
	case "key":
		if len(c.Keys) == 0 {
			return "", nil, fmt.Errorf("key: want keys, like \"ctrl+l\" or [\"Tab\", \"Return\"]")
		}
		return "/input/key", struct {
			Keys json.RawMessage `json:"keys"`
			Pace string          `json:"pace,omitempty"`
		}{c.Keys, c.Pace}, nil
	case "drag":
		type drag struct {
			From   pointJSON `json:"from"`
			To     pointJSON `json:"to"`
			Button string    `json:"button,omitempty"`
			Pace   string    `json:"pace,omitempty"`
		}
		if c.From == nil || c.To == nil {
			return "", nil, fmt.Errorf("drag: want from and to, each {x, y}")
		}
		return "/input/drag", drag{*c.From, *c.To, c.Button, c.Pace}, nil
	case "open":
		u, err := weburl.Check(c.URL)
		if err != nil {
			return "", nil, err
		}
		return "/input/open", struct {
			URL  string `json:"url"`
			Pace string `json:"pace,omitempty"`
		}{u, c.Pace}, nil
	}
	return "", nil, fmt.Errorf("action %q: want click, type, scroll, key, drag, open, wait or zoom", c.Action)
}

// computer runs one Visual-mode action: input through xdotool, a wait,
// or a zoom into part of the screen. With screenshot set it answers with
// the screen after the action instead of 204.
func (a *API) computer(w http.ResponseWriter, r *http.Request) {
	var c computerRequest
	if !readJSON(w, r, &c) {
		return
	}
	if c.Screenshot && c.Width != 0 && c.Width < 16 {
		writeError(w, http.StatusBadRequest, "width: want a number of pixels, at least 16")
		return
	}
	tok, _ := auth.TokenFromContext(r.Context())
	switch c.Action {
	case "zoom":
		a.zoom(w, r, c)
		return
	case "wait":
		if c.MS < 0 {
			writeError(w, http.StatusBadRequest, "ms: want a number of milliseconds")
			return
		}
		s, ok := a.running(w, r, false)
		if !ok {
			return
		}
		d := time.Duration(min(c.MS, int(maxPause/time.Millisecond))) * time.Millisecond
		a.m.Touch(s.ID)
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
		a.m.Touch(s.ID)
		a.m.NoteFor(r.Context(), s.ID, tok.Name, "Waited "+strconv.FormatFloat(d.Seconds(), 'f', -1, 64)+" s")
		a.done(w, r, s, c)
		return
	}
	path, body, err := c.inputBody()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	wait, err := files.DownloadWait(c.DownloadWaitMS)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s, ok := a.running(w, r, true)
	if !ok {
		return
	}
	raw, err := json.Marshal(body)
	if err != nil {
		a.internal(w, "encoding an action", err)
		return
	}
	if err := checkInput(path, raw); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var first func(ctx context.Context)
	if c.Action == "open" {
		u, _ := weburl.Check(c.URL) // checked by inputBody
		// Once the agent has the input: not while a person has the browser.
		first = func(ctx context.Context) { a.openingDirectly(ctx, s, u) }
	}
	var mark *files.Mark
	if c.downloads() {
		mark = a.markDownloads(s)
		if c.DownloadWaitMS != nil {
			mark.Expect(wait) // the caller expects a download: look as long for it to start
		}
	}
	if !a.actAfter(w, r, s, path, raw, first) {
		return
	}
	mark.Ended()
	a.doneWith(w, r, s, c, func() {
		if mark != nil {
			a.reportDownloads(w, r, s, mark, wait)
		}
	})
}

// maxScreenshot caps the screenshot an action answers with (a variable
// for tests).
var maxScreenshot int64 = 64 << 20

// done answers an action that ran: with the screen if asked, else 204.
// The action ran either way, so a screenshot that fails doesn't fail the
// call (an agent would retry it, clicking or paying twice): it answers
// 204 with X-Screenshot-Error.
func (a *API) done(w http.ResponseWriter, r *http.Request, s sessions.Session, c computerRequest) {
	a.doneWith(w, r, s, c, func() {})
}

// doneWith is done, with headers set by before just ahead of the answer:
// after the screenshot is taken, so that a download notice gets the
// screenshot's time to see a download start.
func (a *API) doneWith(w http.ResponseWriter, r *http.Request, s sessions.Session, c computerRequest, before func()) {
	before = func(set func()) func() {
		return func() { set(); a.setChooserNotice(w, r, s) }
	}(before)
	if !c.Screenshot {
		before()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	target := "/screenshot"
	if c.Width > 0 {
		target += "?width=" + strconv.Itoa(c.Width)
	}
	var shot []byte
	resp, err := a.control(r.Context(), s.Profile, http.MethodGet, target, nil)
	if err == nil {
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("the browser answered %d", resp.StatusCode)
		} else {
			shot, err = io.ReadAll(io.LimitReader(resp.Body, maxScreenshot+1))
			if err == nil && int64(len(shot)) > maxScreenshot {
				// Never a cut-off image passed off as the screen.
				shot, err = nil, fmt.Errorf("the screenshot is over %d MiB", maxScreenshot>>20)
			}
		}
		_ = resp.Body.Close()
	}
	before()
	if err != nil {
		w.Header().Set("X-Screenshot-Error", strings.ReplaceAll(err.Error(), "\n", " "))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for _, h := range []string{"Content-Type", "X-Screen-Width", "X-Screen-Height"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(shot)
}

// zoom answers with a region of the screen at full resolution. The region
// starts on screen and is cut off at the screen's edges.
func (a *API) zoom(w http.ResponseWriter, r *http.Request, c computerRequest) {
	if c.X == nil || c.Y == nil || c.W <= 0 || c.H <= 0 {
		writeError(w, http.StatusBadRequest, "zoom: want a region x, y, w, h in screen pixels")
		return
	}
	s, ok := a.running(w, r, false)
	if !ok {
		return
	}
	a.m.Touch(s.ID)
	resp, err := a.control(r.Context(), s.Profile, http.MethodGet, "/screenshot", nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "screenshot failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		relay(w, resp, "Content-Type")
		return
	}
	img, err := png.Decode(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "screenshot failed: "+err.Error())
		return
	}
	screen := img.Bounds()
	at := image.Pt(*c.X, *c.Y)
	if !at.In(screen) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("(%d, %d) is outside the %dx%d screen", at.X, at.Y, screen.Dx(), screen.Dy()))
		return
	}
	region := image.Rectangle{at, at.Add(image.Pt(min(c.W, screen.Dx()), min(c.H, screen.Dy())))}.Intersect(screen)
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		a.internal(w, "cropping a screenshot", fmt.Errorf("can't crop a %T", img))
		return
	}
	var out bytes.Buffer
	if err := png.Encode(&out, sub.SubImage(region)); err != nil {
		a.internal(w, "encoding a screenshot", err)
		return
	}
	tok, _ := auth.TokenFromContext(r.Context())
	a.m.NoteFor(r.Context(), s.ID, tok.Name, "Zoomed into a region")
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Screen-Width", strconv.Itoa(screen.Dx()))
	w.Header().Set("X-Screen-Height", strconv.Itoa(screen.Dy()))
	_, _ = w.Write(out.Bytes())
}

// namedKey matches keys whose names say nothing about what was typed.
var namedKey = regexp.MustCompile(`(?i)^(ctrl|control|alt|option|shift|cmd|command|meta|super|win|enter|return|esc|escape|tab|space|backspace|delete|del|insert|home|end|pageup|pgup|pagedown|pgdn|prior|next|up|down|left|right|arrowup|arrowdown|arrowleft|arrowright|f[1-9]|f[12][0-9]|f3[0-5])$`)

// commandKey matches modifiers that make a combination a command, not text.
var commandKey = regexp.MustCompile(`(?i)^(ctrl|control|alt|option|cmd|command|meta|super|win)$`)

// describeKeys names the combinations pressed, like "Pressed ctrl+l". A
// key that types a character could be part of a password, so if any is
// pressed on its own (or with shift) only the count is logged.
func describeKeys(raw json.RawMessage) string {
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		var one string
		_ = json.Unmarshal(raw, &one)
		list = []string{one}
	}
	shown := make([]string, len(list))
	for i, combo := range list {
		parts := strings.Split(combo, "+")
		command, named := false, true
		for j, p := range parts {
			parts[j] = strings.TrimSpace(p)
			command = command || commandKey.MatchString(parts[j])
			named = named && namedKey.MatchString(parts[j])
		}
		if !command && !named {
			if len(list) == 1 {
				return "Pressed 1 key"
			}
			return fmt.Sprintf("Pressed %d keys", len(list))
		}
		shown[i] = strings.Join(parts, "+")
	}
	return "Pressed " + strings.Join(shown, ", ")
}

// openingDirectly lets the host of an address the agent opens through
// the address bar past the session's ad and tracker list, as navigate
// does (SPEC §3, Egress): BrowserPier knows the address was asked for.
func (a *API) openingDirectly(ctx context.Context, s sessions.Session, raw string) {
	if !s.BlockAds {
		return
	}
	if err := a.m.ExemptURL(ctx, s.ID, raw); err != nil {
		a.log.Debug("couldn't exempt a host from the ad and tracker list", "session", s.ID, "err", err)
	}
}
