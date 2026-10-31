package webrtc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// fakeBrowser serves /stream on a control socket: synthetic H.264 frames
// (parameter sets, then a slice), forever, until the reader goes away.
type fakeBrowser struct {
	sock    string
	readers atomic.Int32
}

func newFakeBrowser(t *testing.T) *fakeBrowser {
	t.Helper()
	dir, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	fb := &fakeBrowser{sock: filepath.Join(dir, "c.sock")}
	ln, err := net.Listen("unix", fb.sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		fb.readers.Add(1)
		defer fb.readers.Add(-1)
		frame := func(t byte) []byte { return append([]byte{0, 0, 0, 1, t}, bytes.Repeat([]byte{0x80}, 200)...) }
		for i := 0; r.Context().Err() == nil; i++ {
			var b []byte
			if i%30 == 0 {
				b = append(append(append(b, frame(0x67)...), frame(0x68)...), frame(0x65)...) // SPS, PPS, IDR
			} else {
				b = frame(0x41) // non-IDR slice
			}
			if _, err := w.Write(b); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(33 * time.Millisecond)
		}
	})
	srv := &http.Server{Handler: mux} //nolint:gosec // test server
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return fb
}

func TestRelayStreamsVideoAndCarriesInput(t *testing.T) {
	fb := newFakeBrowser(t)
	relay, err := New(Options{Port: 0, IPs: []string{"127.0.0.1"}}, func(string) string { return fb.sock }, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()

	viewer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = viewer.Close() }()
	if _, err := viewer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	dc, err := viewer.CreateDataChannel("input", nil)
	if err != nil {
		t.Fatal(err)
	}
	gotVideo := make(chan string, 1)
	viewer.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if _, _, err := tr.ReadRTP(); err == nil {
			gotVideo <- tr.Codec().MimeType
		}
	})
	offer, err := viewer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(viewer)
	if err := viewer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered

	var mu sync.Mutex
	var inputs []string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, answer, err := relay.Connect(ctx, "google", viewer.LocalDescription().SDP, func(b []byte, _ func(string)) {
		mu.Lock()
		inputs = append(inputs, string(b))
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := viewer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}

	select {
	case mime := <-gotVideo:
		if mime != webrtc.MimeTypeH264 {
			t.Fatalf("codec %s", mime)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no video reached the viewer")
	}

	opened := make(chan struct{})
	var openOnce sync.Once
	dc.OnOpen(func() { openOnce.Do(func() { close(opened) }) })
	if dc.ReadyState() == webrtc.DataChannelStateOpen {
		openOnce.Do(func() { close(opened) })
	}
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("data channel never opened")
	}
	for _, m := range []string{`[{"t":"move","x":1,"y":2}]`, `[{"t":"down","b":1}]`} {
		if err := dc.SendText(m); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(inputs)
		mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("inputs %v", inputs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if inputs[0] != `[{"t":"move","x":1,"y":2}]` {
		t.Fatalf("inputs out of order: %v", inputs)
	}

	// The last viewer leaving stops the stream being read, so the
	// browser stops encoding.
	v.Close()
	<-v.Done()
	deadline = time.Now().Add(5 * time.Second)
	for fb.readers.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("still reading the stream with no viewer")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWriteFramesGroupsParameterSetsWithTheirFrame(t *testing.T) {
	// Real NAL units never end in a zero byte (their trailing bits end in a 1).
	nal := func(t byte, n int) []byte { return append([]byte{0, 0, 0, 1, t}, bytes.Repeat([]byte{0x80}, n)...) }
	var in []byte
	in = append(in, nal(0x67, 4)...) // SPS
	in = append(in, nal(0x68, 3)...) // PPS
	in = append(in, nal(0x65, 10)...)
	in = append(in, nal(0x41, 6)...)
	var samples []media.Sample
	err := writeFrames(&onceReader{b: in}, func(s media.Sample) error { samples = append(samples, s); return nil })
	if !errors.Is(err, io.EOF) {
		t.Fatalf("writeFrames = %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("%d samples, want 2 frames", len(samples))
	}
	if got := len(samples[0].Data); got != (4+5)+(4+4)+(4+11) {
		t.Fatalf("first frame is %d bytes; want SPS, PPS and IDR with start codes", got)
	}
}

type onceReader struct{ b []byte }

func (o *onceReader) Read(p []byte) (int, error) {
	if len(o.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, o.b)
	o.b = o.b[n:]
	return n, nil
}

func TestWriteFramesRefusesAnEndlessNAL(t *testing.T) {
	// A NAL that never ends (no start code after it) must not be buffered forever.
	endless := io.MultiReader(bytes.NewReader([]byte{0, 0, 0, 1, 0x06}), &zeros{n: maxFrame + 1<<20})
	err := writeFrames(endless, func(media.Sample) error { return nil })
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("writeFrames = %v, want an error", err)
	}
}

// zeros yields n non-start-code bytes.
type zeros struct{ n int }

func (z *zeros) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), z.n)
	for i := range p[:n] {
		p[i] = 0xAB
	}
	z.n -= n
	return n, nil
}

func TestSplitNALsHandlesBothStartCodes(t *testing.T) {
	in := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 0, 0, 0, 1, 0x65, 4, 5}
	var samples []media.Sample
	_ = writeFrames(bytes.NewReader(in), func(s media.Sample) error { samples = append(samples, s); return nil })
	want := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 0, 1, 0x68, 3, 0, 0, 0, 1, 0x65, 4, 5}
	if len(samples) != 1 || !bytes.Equal(samples[0].Data, want) {
		t.Fatalf("%d samples (first %x), want one %x", len(samples), func() []byte {
			if len(samples) > 0 {
				return samples[0].Data
			}
			return nil
		}(), want)
	}
}

// trickle hands out one byte per Read, the worst case for a splitter that
// rescans on every call.
type trickle struct{ r io.Reader }

func (t trickle) Read(p []byte) (int, error) { return t.r.Read(p[:1]) }

func TestSplitterIsLinearOnATrickle(t *testing.T) {
	nal := append([]byte{0, 0, 0, 1, 0x65}, bytes.Repeat([]byte{0x80}, 2<<20)...)
	in := append(append([]byte{}, nal...), 0, 0, 0, 1, 0x41, 0x80)
	start := time.Now()
	var got int
	err := writeFrames(trickle{bytes.NewReader(in)}, func(s media.Sample) error { got++; return nil })
	if !errors.Is(err, io.EOF) || got != 2 {
		t.Fatalf("writeFrames = %v with %d frames", err, got)
	}
	// Linear scanning takes well under a second; rescanning a 2 MiB NAL
	// on each of its 2M reads would take hours.
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %s", d)
	}
}
