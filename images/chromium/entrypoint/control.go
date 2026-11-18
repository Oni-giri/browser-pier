package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

// newControlHandler serves the control socket:
//
//	GET /healthz   ok once Chromium answers on CDP
//	    /cdp/...   CDP over HTTP and WebSocket, relayed to Chromium's loopback port
//	GET /stream    the display as an H.264 Annex-B byte stream, one reader at a time
//	GET /record    the display as a fragmented MP4 made to be kept, one reader at a time
//	GET /screenshot, POST /input/{click,type,scroll,key,drag}   Visual mode (see visual.go)
//	POST /input/events, POST /input/release   the live view's mouse and keyboard (live.go)
//	GET /input/clipboard           the clipboard's text, for the live view's clipboard box (clipboard.go)
//	POST /input/clipboard          {"text"}: hold the text on the clipboard until replaced or emptied
//	POST /input/paste              {"text"}: the same, then Ctrl+V
//	DELETE /input/clipboard        empty CLIPBOARD and PRIMARY, as the person's control ends
//	GET /geo           the location the browser runs with
//	GET /proxy/stats   bytes through the upstream proxy, connections refused, and blocked for the ad list
//	POST /proxy/blocking/off, POST /proxy/exempt   the ad list's controls (block.go)
//	GET /chooser, POST /chooser/{id}   the page's file choosers (portal.go)
func newControlHandler(cdp string, stream, record http.Handler, vis *visual, fwd *forwarder, where geo, portal *portal, portalOff string) http.Handler {
	target := &url.URL{Scheme: "http", Host: cdp}
	relay := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		// SetURL also sets Host to the loopback address, which Chromium requires.
		pr.SetURL(target)
		// Chromium refuses CDP WebSockets that carry an Origin. Only the
		// orchestrator can reach this socket, so the request is trusted.
		pr.Out.Header.Del("Origin")
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/cdp/", http.StripPrefix("/cdp", relay))
	mux.Handle("GET /stream", stream)
	mux.Handle("GET /record", record)
	if vis != nil {
		vis.register(mux)
	}
	mux.HandleFunc("GET /geo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(where)
	})
	registerChooser(mux, portal, portalOff)
	if fwd != nil {
		mux.HandleFunc("GET /proxy/stats", fwd.stats)
		if fwd.block != nil {
			fwd.block.register(mux)
		}
	}
	return mux
}

// streamArgs is the GStreamer sidecar: it captures the display and writes
// H.264 (constrained baseline, Annex-B, no B-frames) to fd 3.
func streamArgs() []string {
	return []string{
		"-q",
		"ximagesrc", "display-name=" + display, "use-damage=false", "show-pointer=true", "!",
		"video/x-raw,framerate=30/1", "!",
		"videoconvert", "!", "video/x-raw,format=I420", "!",
		// One slice per frame (no sliced threads), so each slice NAL is a
		// whole frame for the relay.
		"x264enc", "tune=zerolatency", "speed-preset=ultrafast", "key-int-max=60", "bframes=0", "sliced-threads=false", "!",
		"video/x-h264,profile=constrained-baseline,stream-format=byte-stream", "!",
		"fdsink", "fd=3",
	}
}

// Recording (PROJ-23): a second pipeline, made to be kept rather than
// watched. At most recordFPS frames a second, scaled to 3/4 of the
// screen, H.264 at constant quality, muxed as fragmented MP4 so that
// every fragment (moof + mdat, about one frame) is whole on its own and
// the orchestrator can end a file at the last one it received. A still
// screen gives fragments of a few dozen bytes, which the orchestrator
// leaves out (internal/recording). No keyframe after the first: each
// reader gets a fresh pipeline, so each file starts with one.
const (
	recordFPS = 5
	// recordQuality is x264's constant-quality factor (pass=qual):
	// lower is sharper and larger.
	recordQuality = 28
)

// recordArgs is the recording pipeline for a width x height screen. Its
// only variable parts are the scaled size, computed here from the
// screen's own checked numbers; nothing comes from a request.
func recordArgs(width, height int) []string {
	w, h := recordSize(width, height)
	return []string{
		"-q",
		"ximagesrc", "display-name=" + display, "use-damage=false", "show-pointer=true", "!",
		"video/x-raw,framerate=" + strconv.Itoa(recordFPS) + "/1", "!",
		"videoconvert", "!", "video/x-raw,format=I420", "!",
		"videoscale", "!", "video/x-raw,width=" + strconv.Itoa(w) + ",height=" + strconv.Itoa(h), "!",
		// zerolatency and one thread: no look-ahead and no frame threads,
		// so a frame leaves the encoder as it comes. With frame threads
		// x264 holds about nine frames, almost two seconds at 5 fps, which
		// a recording would miss at each start.
		"x264enc", "pass=qual", "quantizer=" + strconv.Itoa(recordQuality), "speed-preset=veryfast", "tune=zerolatency",
		"sliced-threads=false", "threads=1", "key-int-max=1000", "bframes=0", "!",
		"video/x-h264,stream-format=avc,alignment=au,profile=high", "!",
		"mp4mux", "fragment-duration=200", "streamable=true", "!",
		"fdsink", "fd=3",
	}
}

// recordSize is 3/4 of the screen, rounded down to even numbers as I420
// needs.
func recordSize(width, height int) (int, int) {
	return width * 3 / 4 &^ 1, height * 3 / 4 &^ 1
}

// streamer runs a sidecar only while a client reads it, so an unwatched
// (or unrecorded) browser spends no CPU on encoding.
type streamer struct {
	busy        atomic.Bool
	command     func() *exec.Cmd // writes the stream to fd 3
	contentType string           // "" for video/h264
	// wait is how long a new reader waits for the last one's pipeline to
	// be gone before it is refused: the recorder opens the next segment's
	// stream as it closes the last.
	wait time.Duration
}

// claim takes the stream for this reader, waiting up to s.wait for the
// last reader's pipeline to exit.
func (s *streamer) claim(r *http.Request) bool {
	deadline := time.Now().Add(s.wait)
	for !s.busy.CompareAndSwap(false, true) {
		if !time.Now().Before(deadline) || r.Context().Err() != nil {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func (s *streamer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.claim(r) {
		http.Error(w, "stream already in use", http.StatusConflict)
		return
	}
	defer s.busy.Store(false)

	pr, pw, err := os.Pipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = pr.Close() }()
	cmd := s.command()
	cmd.ExtraFiles = []*os.File{pw}
	cmd.Stderr = os.Stderr
	exited, err := procs.start(cmd)
	_ = pw.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stop := func() { _ = cmd.Process.Signal(syscall.SIGTERM) }
	defer context.AfterFunc(r.Context(), stop)()

	ct := s.contentType
	if ct == "" {
		ct = "video/h264"
	}
	w.Header().Set("Content-Type", ct)
	rc := http.NewResponseController(w)
	buf := make([]byte, 64<<10)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			_ = rc.Flush()
		}
		if err != nil {
			break
		}
	}

	stop()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
}
