package api

import (
	"bytes"
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
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// maxPause caps a wait action.
const maxPause = 10 * time.Second

type pointJSON struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// computerRequest is one Visual-mode action. Coordinates are screen pixels.
type computerRequest struct {
	Action string `json:"action"` // click, type, scroll, key, drag, wait or zoom
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
	MS   int             `json:"ms"`
	W    int             `json:"w"`
	H    int             `json:"h"`
	// Screenshot returns the screen after the action, Width wide if set.
	Screenshot bool `json:"screenshot"`
	Width      int  `json:"width"`
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
	switch c.Action {
	case "click":
		type click struct {
			X      int    `json:"x"`
			Y      int    `json:"y"`
			Button string `json:"button,omitempty"`
			Double bool   `json:"double,omitempty"`
		}
		if err := at(); err != nil {
			return "", nil, err
		}
		return "/input/click", click{*c.X, *c.Y, c.Button, c.Double}, nil
	case "type":
		return "/input/type", struct {
			Text string `json:"text"`
		}{c.Text}, nil
	case "scroll":
		type scroll struct {
			X  int `json:"x"`
			Y  int `json:"y"`
			DX int `json:"dx"`
			DY int `json:"dy"`
		}
		if err := at(); err != nil {
			return "", nil, err
		}
		return "/input/scroll", scroll{*c.X, *c.Y, c.DX, c.DY}, nil
	case "key":
		if len(c.Keys) == 0 {
			return "", nil, fmt.Errorf("key: want keys, like \"ctrl+l\" or [\"Tab\", \"Return\"]")
		}
		return "/input/key", struct {
			Keys json.RawMessage `json:"keys"`
		}{c.Keys}, nil
	case "drag":
		type drag struct {
			From   pointJSON `json:"from"`
			To     pointJSON `json:"to"`
			Button string    `json:"button,omitempty"`
		}
		if c.From == nil || c.To == nil {
			return "", nil, fmt.Errorf("drag: want from and to, each {x, y}")
		}
		return "/input/drag", drag{*c.From, *c.To, c.Button}, nil
	}
	return "", nil, fmt.Errorf("action %q: want click, type, scroll, key, drag, wait or zoom", c.Action)
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
		a.m.Note(s.ID, tok.Name, "Waited "+strconv.FormatFloat(d.Seconds(), 'f', -1, 64)+" s")
		a.done(w, r, s, c)
		return
	}
	path, body, err := c.inputBody()
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
	if a.act(w, r, s, path, raw) {
		a.done(w, r, s, c)
	}
}

// done answers an action that ran: with the screen if asked, else 204.
// The action ran either way, so a screenshot that fails doesn't fail the
// call (an agent would retry it, clicking or paying twice): it answers
// 204 with X-Screenshot-Error.
func (a *API) done(w http.ResponseWriter, r *http.Request, s sessions.Session, c computerRequest) {
	if !c.Screenshot {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	target := "/screenshot"
	if c.Width > 0 {
		target += "?width=" + strconv.Itoa(c.Width)
	}
	resp, err := a.control(r.Context(), s.Profile, http.MethodGet, target, nil)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("the browser answered %d", resp.StatusCode)
		_ = resp.Body.Close()
	}
	if err != nil {
		w.Header().Set("X-Screenshot-Error", strings.ReplaceAll(err.Error(), "\n", " "))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	relay(w, resp, "Content-Type", "X-Screen-Width", "X-Screen-Height")
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
	a.m.Note(s.ID, tok.Name, "Zoomed into a region")
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
