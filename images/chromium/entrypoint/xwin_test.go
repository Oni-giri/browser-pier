package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeXServer is an X server with a window tree, answering the few requests
// xconn makes over a pipe.
type fakeXServer struct {
	root  uint32
	wins  map[uint32]*fakeWin
	atoms map[string]uint32
	// seen are the requests' opcodes, in order.
	seen []byte
	// gone answers BadWindow for these, as for a window that closed.
	gone map[uint32]bool
}

type fakeWin struct {
	parent   uint32
	kids     []uint32 // bottom of the stack first
	viewable bool
	override bool
	box      rect // relative to the parent
	props    map[string]string
}

func newFakeXServer() *fakeXServer {
	f := &fakeXServer{root: 0x100, wins: map[uint32]*fakeWin{}, atoms: map[string]uint32{"WM_STATE": 40, "WM_CLASS": 67, "WM_WINDOW_ROLE": 300}, gone: map[uint32]bool{}}
	f.wins[f.root] = &fakeWin{viewable: true, box: rect{0, 0, 1280, 800}}
	return f
}

// add puts window id on top of parent's children.
func (f *fakeXServer) add(id, parent uint32, w fakeWin) {
	w.parent = parent
	f.wins[id] = &w
	f.wins[parent].kids = append(f.wins[parent].kids, id)
}

// connect starts serving on one end of a pipe and returns a client on
// the other.
func (f *fakeXServer) connect(t *testing.T) *xconn {
	t.Helper()
	cli, srv := net.Pipe()
	t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })
	go f.serve(srv)
	x, err := xSetup(cli)
	if err != nil {
		t.Fatal(err)
	}
	if x.root != f.root {
		t.Fatalf("root %#x, want %#x", x.root, f.root)
	}
	return x
}

func (f *fakeXServer) serve(c net.Conn) {
	le := binary.LittleEndian
	setup := make([]byte, 12)
	if _, err := io.ReadFull(c, setup); err != nil || setup[0] != 'l' {
		return
	}
	// A vendor name of 5 bytes (padded to 8) and two pixmap formats, then
	// the screen, whose first field is the root.
	data := make([]byte, 32+8+16+40)
	le.PutUint16(data[16:18], 5)
	data[21] = 2
	copy(data[32:], "Fake!")
	le.PutUint32(data[56:60], f.root)
	head := make([]byte, 8)
	head[0] = 1
	le.PutUint16(head[2:4], 11)
	le.PutUint16(head[6:8], uint16(len(data)/4))
	_, _ = c.Write(append(head, data...))
	first := true
	for {
		h := make([]byte, 4)
		if _, err := io.ReadFull(c, h); err != nil {
			return
		}
		body := make([]byte, int(le.Uint16(h[2:4]))*4-4)
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		f.seen = append(f.seen, h[0])
		r := make([]byte, 32)
		r[0] = 1
		fail := func(code byte) { r[0], r[1], r[10] = 0, code, h[0] }
		var extra []byte
		w := f.wins[le.Uint32(body[0:4])]
		if w == nil && h[0] != 16 || f.gone[le.Uint32(body[0:4])] {
			fail(3)
		} else {
			switch h[0] {
			case 15: // QueryTree
				le.PutUint16(r[16:18], uint16(len(w.kids)))
				for _, k := range w.kids {
					extra = le.AppendUint32(extra, k)
				}
			case 3: // GetWindowAttributes
				extra = make([]byte, 12)
				if w.viewable {
					r[26] = 2
				}
				if w.override {
					r[27] = 1
				}
			case 14: // GetGeometry
				le.PutUint16(r[12:14], uint16(int16(w.box.X)))
				le.PutUint16(r[14:16], uint16(int16(w.box.Y)))
				le.PutUint16(r[16:18], uint16(w.box.W))
				le.PutUint16(r[18:20], uint16(w.box.H))
			case 40: // TranslateCoordinates to the root
				x, y := 0, 0
				for id := le.Uint32(body[0:4]); id != f.root; id = f.wins[id].parent {
					x, y = x+f.wins[id].box.X, y+f.wins[id].box.Y
				}
				le.PutUint16(r[12:14], uint16(int16(x)))
				le.PutUint16(r[14:16], uint16(int16(y)))
			case 16: // InternAtom
				n := int(le.Uint16(body[0:2]))
				le.PutUint32(r[8:12], f.atoms[string(body[4:4+n])])
			case 20: // GetProperty
				atom := le.Uint32(body[4:8])
				for name, a := range f.atoms {
					if v, ok := w.props[name]; ok && a == atom {
						r[1] = 8
						le.PutUint32(r[8:12], 31) // STRING
						le.PutUint32(r[16:20], uint32(len(v)))
						extra = append([]byte(v), make([]byte, pad4(len(v))-len(v))...)
					}
				}
			default:
				fail(1)
			}
		}
		le.PutUint32(r[4:8], uint32(len(extra)/4))
		if first {
			// An event before the first reply, as a server may send one,
			// which xconn skips.
			ev := make([]byte, 32)
			ev[0] = 12
			r = append(ev, r...)
			first = false
		}
		if _, err := c.Write(append(r, extra...)); err != nil {
			return
		}
	}
}

// browserDesktop is the tree BrowserPier's container has: the taskbar,
// and the browser window in JWM's frame.
func browserDesktop() *fakeXServer {
	f := newFakeXServer()
	f.add(0x200, f.root, fakeWin{viewable: true, override: true, box: rect{0, 768, 1280, 32}}) // the taskbar
	f.add(0x300, f.root, fakeWin{viewable: true, box: rect{0, 0, 1280, 768}})                  // JWM's frame
	f.add(0x400002, 0x300, fakeWin{viewable: true, box: rect{0, 0, 1280, 768},
		props: map[string]string{"WM_STATE": "\x01\x00\x00\x00", "WM_CLASS": "chromium-browser\x00Chromium-browser\x00", "WM_WINDOW_ROLE": "browser"}})
	return f
}

func TestWindowsAtFindsTheBrowserInItsFrame(t *testing.T) {
	f := browserDesktop()
	got, err := f.connect(t).windowsAt(658, 63)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []shownWindow{{Class: "Chromium-browser", Role: "browser", Box: rect{0, 0, 1280, 768}, ID: 0x400002}}) {
		t.Fatalf("%+v", got)
	}
	// Only reads: the tree, attributes, geometry, atoms and properties.
	for _, op := range f.seen {
		if !slices.Contains([]byte{3, 14, 15, 16, 20, 40}, op) {
			t.Fatalf("request %d", op)
		}
	}
}

func TestWindowsAtSeesWhatIsOnTop(t *testing.T) {
	f := browserDesktop()
	// A pop-up window of the same size, framed, over the browser.
	f.add(0x310, f.root, fakeWin{viewable: true, box: rect{0, 0, 1280, 768}})
	f.add(0x400019, 0x310, fakeWin{viewable: true, box: rect{0, 0, 1280, 768},
		props: map[string]string{"WM_STATE": "\x01", "WM_CLASS": "chromium-browser\x00Chromium-browser\x00", "WM_WINDOW_ROLE": "pop-up"}})
	// A menu over that, and an unmapped window over it all.
	f.add(0x500, f.root, fakeWin{viewable: true, override: true, box: rect{600, 20, 411, 458}})
	f.add(0x600, f.root, fakeWin{viewable: false, box: rect{0, 0, 1280, 800}})
	x := f.connect(t)
	got, err := x.windowsAt(658, 63)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Override || got[0].Box != (rect{600, 20, 411, 458}) || got[1].Role != "pop-up" {
		t.Fatalf("%+v", got)
	}
	// Left of the menu: the pop-up alone.
	if got, err := x.windowsAt(300, 63); err != nil || len(got) != 1 || got[0].Role != "pop-up" {
		t.Fatalf("%+v %v", got, err)
	}
	// The taskbar is all there is at the bottom.
	if got, err := x.windowsAt(640, 790); err != nil || len(got) != 1 || !got[0].Override {
		t.Fatalf("%+v %v", got, err)
	}
	// A window that closed while the tree was read is passed over.
	f.gone[0x500] = true
	if got, err := x.windowsAt(658, 63); err != nil || len(got) != 1 || got[0].Role != "pop-up" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestWindowsAtOnAnEmptyScreen(t *testing.T) {
	f := newFakeXServer()
	got, err := f.connect(t).windowsAt(10, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestXSetupRefused(t *testing.T) {
	cli, srv := net.Pipe()
	defer func() { _ = cli.Close() }()
	go func() {
		_, _ = io.ReadFull(srv, make([]byte, 12))
		reason := "No protocol specified"
		head := []byte{0, byte(len(reason)), 11, 0, 0, 0, byte(pad4(len(reason)) / 4), 0}
		_, _ = srv.Write(append(head, append([]byte(reason), 0, 0, 0)...))
	}()
	if _, err := xSetup(cli); err == nil || !strings.Contains(err.Error(), "No protocol specified") {
		t.Fatalf("%v", err)
	}
}

// scripted serves one setup, then answers the first request with reply
// (and closes, or with stall, never answers, until the test's cleanup
// closes the pipe).
func scripted(t *testing.T, reply []byte, stall bool) *xconn {
	t.Helper()
	cli, srv := net.Pipe()
	t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })
	go func() {
		le := binary.LittleEndian
		_, _ = io.ReadFull(srv, make([]byte, 12))
		data := make([]byte, 40)
		le.PutUint32(data[32:36], 0x100) // no vendor, no formats: the root
		head := []byte{1, 0, 11, 0, 0, 0, byte(len(data) / 4), 0}
		_, _ = srv.Write(append(head, data...))
		h := make([]byte, 4)
		if _, err := io.ReadFull(srv, h); err != nil {
			return
		}
		_, _ = io.CopyN(io.Discard, srv, int64(le.Uint16(h[2:4]))*4-4)
		if stall {
			// Until the cleanup closes the pipe, so the goroutine ends.
			_, _ = io.Copy(io.Discard, srv)
			return
		}
		_, _ = srv.Write(reply)
		_ = srv.Close()
	}()
	x, err := xSetup(cli)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// TestMalformedXRepliesAreErrors: a reply that claims more than it
// carries, more than the cap, or never comes is an error, never a panic
// or a hang.
func TestMalformedXRepliesAreErrors(t *testing.T) {
	le := binary.LittleEndian
	reply := func(words uint32, body []byte, set func(r []byte)) []byte {
		r := make([]byte, 32)
		r[0] = 1
		le.PutUint32(r[4:8], words)
		if set != nil {
			set(r)
		}
		return append(r, body...)
	}
	for _, c := range []struct {
		name string
		resp []byte
		do   func(x *xconn) error
	}{
		{"over the cap", reply(1<<20, nil, nil), func(x *xconn) error { _, err := x.children(1); return err }},
		{"cut short", reply(10, make([]byte, 8), nil), func(x *xconn) error { _, err := x.children(1); return err }},
		{"more children than sent", reply(1, make([]byte, 4), func(r []byte) { le.PutUint16(r[16:18], 100) }),
			func(x *xconn) error { _, err := x.children(1); return err }},
		{"a longer property than sent", reply(1, make([]byte, 4), func(r []byte) {
			r[1] = 8
			le.PutUint32(r[8:12], 31)
			le.PutUint32(r[16:20], 1000)
		}), func(x *xconn) error { _, _, err := x.property(1, 2); return err }},
		{"a short attributes reply", reply(0, nil, nil), func(x *xconn) error { _, _, err := x.attributes(1); return err }},
		{"an error", []byte{0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			func(x *xconn) error { _, err := x.children(1); return err }},
	} {
		x := scripted(t, c.resp, false)
		if err := c.do(x); err == nil {
			t.Errorf("%s: no error", c.name)
		}
	}
	// A server that stops answering: the deadline ends the read.
	x := scripted(t, nil, true)
	_ = x.c.SetDeadline(time.Now().Add(100 * time.Millisecond))
	start := time.Now()
	if _, err := x.children(1); err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("stalled: %v after %v", err, time.Since(start))
	}
}

// TestACancelledLookEndsAtOnce: a handoff cancels the request, which
// closes the connection rather than waiting out the deadline.
func TestACancelledLookEndsAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "X9")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer func() { _ = c.Close() }()
			_, _ = io.Copy(io.Discard, c) // reads the setup, never answers
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	if _, err := dialXAt(ctx, path, 10*time.Second); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}
