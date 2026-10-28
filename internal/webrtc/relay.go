// Package webrtc relays each browser's H.264 stream to live viewers with
// pion, on one muxed UDP port (and the same port over TCP as a fallback),
// and carries their mouse and keyboard back over a data channel. No CDP is
// involved, so watching or helping doesn't change what a page can detect.
package webrtc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// Options say how viewers reach the relay.
type Options struct {
	// Port is the one UDP port (and TCP port, for networks that block UDP)
	// all live views use.
	Port int
	// IPs, if set, are the addresses to advertise to viewers: a VPS's
	// public IP or the Tailscale IP. Otherwise every local address is.
	IPs []string
}

// Relay serves live views.
type Relay struct {
	api     *webrtc.API
	socket  func(profile string) string // a profile's control socket
	log     *slog.Logger
	closers []io.Closer

	mu      sync.Mutex
	streams map[string]*stream // by profile, while someone watches
}

// New starts listening on the live-view port.
func New(o Options, socket func(profile string) string, log *slog.Logger) (*Relay, error) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	udp, err := ice.NewMultiUDPMuxFromPort(o.Port)
	if err != nil {
		return nil, fmt.Errorf("live view UDP port %d: %w", o.Port, err)
	}
	tcpLn, err := net.Listen("tcp", ":"+strconv.Itoa(o.Port))
	if err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("live view TCP port %d: %w", o.Port, err)
	}
	tcp := ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: tcpLn, ReadBufferSize: 8})
	se.SetICEUDPMux(udp)
	se.SetICETCPMux(tcp)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6})
	if len(o.IPs) > 0 {
		// Advertise these instead of the host's own addresses.
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{External: o.IPs, AsCandidateType: webrtc.ICECandidateTypeHost}); err != nil {
			_ = udp.Close()
			_ = tcp.Close()
			return nil, err
		}
	}
	return &Relay{
		api:     webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se)),
		socket:  socket,
		log:     log,
		closers: []io.Closer{udp, tcp},
		streams: map[string]*stream{},
	}, nil
}

// Close stops listening.
func (r *Relay) Close() error {
	var errs []error
	for _, c := range r.closers {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

// Viewer is one live view, connected or connecting.
type Viewer struct {
	pc     *webrtc.PeerConnection
	closed chan struct{}
}

// Done is closed when the viewer has gone.
func (v *Viewer) Done() <-chan struct{} { return v.closed }

// Close hangs up.
func (v *Viewer) Close() { _ = v.pc.Close() }

// Connect answers a viewer's SDP offer for profile's browser. Messages the
// viewer sends on its data channel are passed to input, in order, with a
// function that replies on the same channel.
func (r *Relay) Connect(ctx context.Context, profile string, offer string, input func(msg []byte, reply func(string))) (*Viewer, string, error) {
	pc, err := r.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, "", err
	}
	v := &Viewer{pc: pc, closed: make(chan struct{})}
	st := r.acquire(profile)
	var once sync.Once
	gone := func() {
		once.Do(func() {
			r.release(profile, st)
			close(v.closed)
		})
	}
	fail := func(err error) (*Viewer, string, error) {
		_ = pc.Close()
		gone()
		return nil, "", err
	}
	sender, err := pc.AddTrack(st.track)
	if err != nil {
		return fail(err)
	}
	go func() {
		// Read RTCP so the sender's interceptors run; it ends with the connection.
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		reply := func(s string) { _ = dc.SendText(s) }
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { input(msg.Data, reply) })
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			_ = pc.Close()
			gone()
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return fail(fmt.Errorf("bad offer: %w", err))
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fail(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return fail(err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	return v, pc.LocalDescription().SDP, nil
}

// stream is one browser's video, read while anyone watches.
type stream struct {
	track   *webrtc.TrackLocalStaticSample
	viewers int
	cancel  context.CancelFunc
}

func (r *Relay) acquire(profile string) *stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.streams[profile]
	if st == nil {
		track, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "browserpier-"+profile)
		ctx, cancel := context.WithCancel(context.Background())
		st = &stream{track: track, cancel: cancel}
		r.streams[profile] = st
		go r.pump(ctx, profile, st)
	}
	st.viewers++
	return st
}

func (r *Relay) release(profile string, st *stream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st.viewers--; st.viewers <= 0 && r.streams[profile] == st {
		st.cancel()
		delete(r.streams, profile)
	}
}

// pump reads the browser's H.264 stream and writes it to the track, one
// frame per sample, reconnecting if the stream drops, until ctx ends.
func (r *Relay) pump(ctx context.Context, profile string, st *stream) {
	for ctx.Err() == nil {
		err := r.pumpOnce(ctx, profile, st.track)
		if ctx.Err() != nil {
			return
		}
		r.log.Debug("live view stream ended; retrying", "profile", profile, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (r *Relay) pumpOnce(ctx context.Context, profile string, track *webrtc.TrackLocalStaticSample) error {
	sock := r.socket(profile)
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://control/stream", nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream: %s", resp.Status)
	}
	return writeFrames(resp.Body, track.WriteSample)
}

var startCode = []byte{0, 0, 0, 1}

// maxFrame caps one frame, so a misbehaving browser container can't make
// the orchestrator buffer without end.
const maxFrame = 8 << 20

// writeFrames splits an Annex-B stream into frames: each slice NAL, with
// the parameter sets and SEI before it, is one sample, timed by when it
// arrived. A NAL or frame over maxFrame ends the stream with an error.
func writeFrames(in io.Reader, write func(media.Sample) error) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 256<<10), maxFrame)
	sc.Split(newNALSplitter())
	var pending []byte
	last := time.Now()
	for sc.Scan() {
		nal := sc.Bytes()
		if len(nal) == 0 {
			continue
		}
		if len(pending)+len(startCode)+len(nal) > maxFrame {
			return errors.New("frame too large")
		}
		pending = append(append(pending, startCode...), nal...)
		if t := nal[0] & 0x1f; t != 1 && t != 5 { // not a slice: keep collecting
			continue
		}
		now := time.Now()
		if err := write(media.Sample{Data: pending, Duration: now.Sub(last)}); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
		pending, last = nil, now
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

// newNALSplitter returns a bufio.SplitFunc for Annex-B: each token is one
// NAL unit, without its start code (00 00 01 or 00 00 00 01). It remembers
// how far it has searched, so a NAL that trickles in a byte at a time is
// still scanned once, not again on every read.
func newNALSplitter() bufio.SplitFunc {
	code := []byte{0, 0, 1}
	searched := 0 // bytes after the current NAL's start code already searched
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		start := bytes.Index(data, code)
		if start < 0 {
			if atEOF {
				return len(data), nil, nil
			}
			// Keep the last two bytes: they may begin a start code.
			return max(0, len(data)-2), nil, nil
		}
		body := start + 3
		from := body + max(0, searched-2) // a start code may straddle the old end
		if from > len(data) {
			from = len(data)
		}
		if next := bytes.Index(data[from:], code); next >= 0 {
			searched = 0
			end := from + next
			return end, trimZeros(data[body:end]), nil
		}
		if atEOF {
			searched = 0
			return len(data), trimZeros(data[body:]), nil
		}
		searched = len(data) - body
		// Drop anything before the start code; the NAL then begins at 0.
		return start, nil, nil
	}
}

// trimZeros drops the zero that starts a four-byte start code, which
// belongs to the next NAL, not this one.
func trimZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}
