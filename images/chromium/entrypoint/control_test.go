package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeCDP checks what the relay sends on, and echoes bytes over an upgraded connection.
func fakeCDP(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != srv.Listener.Addr().String() {
			t.Errorf("Host = %q, want the loopback address", r.Host)
		}
		if o := r.Header.Get("Origin"); o != "" {
			t.Errorf("Origin = %q, want none", o)
		}
		switch r.URL.Path {
		case "/json/version":
			_, _ = io.WriteString(w, `{"Browser":"fake"}`)
		case "/devtools/browser/x":
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = rw.Flush()
			line, _ := rw.ReadString('\n')
			_, _ = rw.WriteString("echo " + line)
			_ = rw.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestControlRelaysCDP(t *testing.T) {
	cdp := fakeCDP(t)
	ctl := httptest.NewServer(newControlHandler(cdp.Listener.Addr().String(), http.NotFoundHandler(), http.NotFoundHandler(), nil, nil, geo{}))
	defer ctl.Close()

	req, _ := http.NewRequest(http.MethodGet, ctl.URL+"/cdp/json/version", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"Browser":"fake"}` {
		t.Fatalf("GET /cdp/json/version = %d %q", resp.StatusCode, body)
	}
}

func TestControlRelaysWebSocketUpgrade(t *testing.T) {
	cdp := fakeCDP(t)
	ctl := httptest.NewServer(newControlHandler(cdp.Listener.Addr().String(), http.NotFoundHandler(), http.NotFoundHandler(), nil, nil, geo{}))
	defer ctl.Close()

	conn, err := net.Dial("tcp", ctl.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET /cdp/devtools/browser/x HTTP/1.1\r\nHost: control\r\nOrigin: https://evil.example\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	_, _ = io.WriteString(conn, "hello\n")
	if line, _ := br.ReadString('\n'); line != "echo hello\n" {
		t.Fatalf("got %q over the upgraded connection", line)
	}
}

func TestStreamOneReaderAtATime(t *testing.T) {
	s := &streamer{command: func() *exec.Cmd {
		return exec.Command("sh", "-c", `printf 'frame' >&3; exec sleep 30`)
	}}
	ctl := httptest.NewServer(newControlHandler("127.0.0.1:1", s, http.NotFoundHandler(), nil, nil, geo{}))
	defer ctl.Close()

	resp, err := http.Get(ctl.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "frame" {
		t.Fatalf("read %q, %v", buf, err)
	}

	second, err := http.Get(ctl.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second reader got %d, want 409", second.StatusCode)
	}

	// Disconnecting stops the sidecar and frees the stream for the next reader.
	_ = resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for s.busy.Load() {
		if time.Now().After(deadline) {
			t.Fatal("stream still busy 5s after the reader left")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStreamArgsWriteH264ToFD3(t *testing.T) {
	args := strings.Join(streamArgs(), " ")
	for _, want := range []string{"ximagesrc display-name=:99", "x264enc", "stream-format=byte-stream", "fdsink fd=3"} {
		if !strings.Contains(args, want) {
			t.Errorf("pipeline %q lacks %q", args, want)
		}
	}
}

func TestRecordOneReaderAtATime(t *testing.T) {
	rec := &streamer{contentType: "video/mp4", command: func() *exec.Cmd {
		return exec.Command("sh", "-c", `printf 'ftyp' >&3; exec sleep 30`)
	}}
	ctl := httptest.NewServer(newControlHandler("127.0.0.1:1", http.NotFoundHandler(), rec, nil, nil, geo{}))
	defer ctl.Close()

	resp, err := http.Get(ctl.URL + "/record")
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", ct)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "ftyp" {
		t.Fatalf("read %q, %v", buf, err)
	}
	// The live stream is its own pipeline: recording doesn't hold it.
	second, err := http.Get(ctl.URL + "/record")
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second reader got %d, want 409", second.StatusCode)
	}

	_ = resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for rec.busy.Load() {
		if time.Now().After(deadline) {
			t.Fatal("recording pipeline still running 5s after the reader left")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRecordArgsAreFixedWordsAndTheScreenSize(t *testing.T) {
	args := recordArgs(1440, 900)
	joined := strings.Join(args, " ")
	for _, want := range []string{"ximagesrc display-name=:99", "show-pointer=true", "framerate=5/1",
		"width=1080,height=674", "x264enc pass=qual", "mp4mux fragment-duration=200 streamable=true", "fdsink fd=3"} {
		if !strings.Contains(joined, want) {
			t.Errorf("pipeline %q lacks %q", joined, want)
		}
	}
	// Only the size varies, and only with the screen: words from a fixed
	// alphabet, nothing a request could put there.
	word := regexp.MustCompile(`^[-a-zA-Z0-9=:/,!_.]+$`)
	for _, a := range args {
		if !word.MatchString(a) {
			t.Errorf("argument %q isn't a fixed word", a)
		}
	}
	if w, h := recordSize(1281, 721); w != 960 || h != 540 {
		t.Errorf("recordSize(1281, 721) = %d, %d, want even 960, 540", w, h)
	}
}
