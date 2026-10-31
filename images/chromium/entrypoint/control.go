package main

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// newControlHandler serves the control socket:
//
//	GET /healthz   ok once Chromium answers on CDP
//	    /cdp/...   CDP over HTTP and WebSocket, relayed to Chromium's loopback port
//	GET /stream    the display as an H.264 Annex-B byte stream, one reader at a time
//	GET /screenshot, POST /input/click, POST /input/type   Visual mode (see visual.go)
func newControlHandler(cdp string, stream http.Handler, vis *visual) http.Handler {
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
	if vis != nil {
		vis.register(mux)
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
		"x264enc", "tune=zerolatency", "speed-preset=ultrafast", "key-int-max=60", "bframes=0", "!",
		"video/x-h264,profile=constrained-baseline,stream-format=byte-stream", "!",
		"fdsink", "fd=3",
	}
}

// streamer runs the sidecar only while a client reads the stream, so an
// unwatched browser spends no CPU on encoding.
type streamer struct {
	busy    atomic.Bool
	command func() *exec.Cmd // writes the stream to fd 3
}

func (s *streamer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.busy.CompareAndSwap(false, true) {
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

	w.Header().Set("Content-Type", "video/h264")
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
