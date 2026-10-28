package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// visual serves Visual mode: screenshots of the X display and OS-level
// input through xdotool. Nothing here touches CDP, so a page can't tell
// an agent from a person by a debugger being attached.
type visual struct {
	width, height int
	// run runs a command, returning what it wrote to fd 3.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
	mu  sync.Mutex // one input action at a time, so two never interleave
}

const maxTypeLen = 2000

func newVisual(width, height int) *visual {
	return &visual{width: width, height: height, run: runCommand}
}

func (v *visual) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /screenshot", v.screenshot)
	mux.HandleFunc("POST /input/click", v.click)
	mux.HandleFunc("POST /input/type", v.typeText)
}

// screenshotArgs captures one frame as PNG to fd 3, scaled to width (and
// the matching height) when width is smaller than the screen.
func (v *visual) screenshotArgs(width int) []string {
	args := []string{"-q", "ximagesrc", "display-name=" + display, "num-buffers=1", "use-damage=false", "show-pointer=false", "!", "videoconvert", "!"}
	if width > 0 && width < v.width {
		height := v.height * width / v.width
		args = append(args, "videoscale", "!", fmt.Sprintf("video/x-raw,width=%d,height=%d", width, height), "!")
	}
	return append(args, "pngenc", "!", "fdsink", "fd=3")
}

func (v *visual) screenshot(w http.ResponseWriter, r *http.Request) {
	width := 0
	if s := r.URL.Query().Get("width"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 16 {
			http.Error(w, "width: want a number of pixels, at least 16", http.StatusBadRequest)
			return
		}
		width = n
	}
	png, err := v.run(r.Context(), "gst-launch-1.0", v.screenshotArgs(width)...)
	if err != nil {
		http.Error(w, "screenshot failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Screen-Width", strconv.Itoa(v.width))
	w.Header().Set("X-Screen-Height", strconv.Itoa(v.height))
	_, _ = w.Write(png)
}

type clickRequest struct {
	X, Y   int
	Button string // left (default), middle or right
	Double bool
}

var buttons = map[string]string{"": "1", "left": "1", "middle": "2", "right": "3"}

func (v *visual) clickArgs(c clickRequest) ([]string, error) {
	if c.X < 0 || c.Y < 0 || c.X >= v.width || c.Y >= v.height {
		return nil, fmt.Errorf("(%d, %d) is outside the %dx%d screen", c.X, c.Y, v.width, v.height)
	}
	b, ok := buttons[c.Button]
	if !ok {
		return nil, fmt.Errorf("button %q: want left, middle or right", c.Button)
	}
	args := []string{"mousemove", "--sync", strconv.Itoa(c.X), strconv.Itoa(c.Y), "click"}
	if c.Double {
		args = append(args, "--repeat", "2", "--delay", "80")
	}
	return append(args, b), nil
}

func (v *visual) click(w http.ResponseWriter, r *http.Request) {
	var c clickRequest
	if !decode(w, r, &c) {
		return
	}
	args, err := v.clickArgs(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v.input(w, r, args)
}

type typeRequest struct{ Text string }

func typeArgs(text string) ([]string, error) {
	if text == "" || !utf8.ValidString(text) {
		return nil, fmt.Errorf("text: want non-empty UTF-8")
	}
	if n := utf8.RuneCountInString(text); n > maxTypeLen {
		return nil, fmt.Errorf("text: %d characters, at most %d per call", n, maxTypeLen)
	}
	// "--" so text starting with a dash is typed, not read as an option.
	return []string{"type", "--clearmodifiers", "--delay", "12", "--", text}, nil
}

func (v *visual) typeText(w http.ResponseWriter, r *http.Request) {
	var t typeRequest
	if !decode(w, r, &t) {
		return
	}
	args, err := typeArgs(t.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v.input(w, r, args)
}

func (v *visual) input(w http.ResponseWriter, r *http.Request, args []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, err := v.run(r.Context(), "xdotool", args...); err != nil {
		http.Error(w, "input failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// runCommand runs a command through the reaper, killing it after 30 s or
// when ctx ends, and returns what it wrote to fd 3.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = pr.Close() }()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	cmd.ExtraFiles = []*os.File{pw}
	cmd.Stderr = os.Stderr
	exited, err := procs.start(cmd)
	_ = pw.Close()
	if err != nil {
		return nil, err
	}
	defer context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })()
	out, readErr := io.ReadAll(io.LimitReader(pr, 64<<20))
	ws := <-exited
	if !ws.Exited() || ws.ExitStatus() != 0 {
		return nil, fmt.Errorf("%s: %s", name, describe(ws))
	}
	return out, readErr
}
