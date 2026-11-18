package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// xconn is the smallest X11 client the open action needs (PROJ-164): which
// windows show at a point of the screen, and what they are. xdotool
// 3.20160805 can't read a window's role (WM_WINDOW_ROLE), which is what
// tells Chromium's browser window from a pop-up window a page opened at
// the same place and size. It only reads: the window tree, geometry,
// attributes and properties. It sends no input and changes nothing, and
// it never talks to the page: X knows windows, not documents.
type xconn struct {
	c    net.Conn
	root uint32
}

// xTimeout bounds one look at the window tree.
const xTimeout = 3 * time.Second

// windowsAtPoint lists the windows showing at a point of the display
// (windowsAt), on a connection of its own.
func windowsAtPoint(ctx context.Context, px, py int) ([]shownWindow, error) {
	x, err := dialX(ctx, display)
	if err != nil {
		return nil, fmt.Errorf("the X display: %w", err)
	}
	defer x.close()
	return x.windowsAt(px, py)
}

// dialX connects to the X display's Unix socket (no authorisation: Xvfb
// runs with none, reachable only inside the container).
func dialX(ctx context.Context, display string) (*xconn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", "/tmp/.X11-unix/X"+strings.TrimPrefix(display, ":"))
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(xTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = c.SetDeadline(deadline)
	x, err := xSetup(c)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return x, nil
}

func (x *xconn) close() { _ = x.c.Close() }

// xSetup sends the connection setup (little-endian, protocol 11.0, no
// authorisation) and reads the root window from the reply.
func xSetup(c net.Conn) (*xconn, error) {
	if _, err := c.Write([]byte{'l', 0, 11, 0, 0, 0, 0, 0, 0, 0, 0, 0}); err != nil {
		return nil, err
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(c, head); err != nil {
		return nil, fmt.Errorf("x11 setup: %w", err)
	}
	n := int(binary.LittleEndian.Uint16(head[6:8])) * 4
	if n > 1<<20 {
		return nil, errors.New("x11 setup: reply too long")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(c, data); err != nil {
		return nil, fmt.Errorf("x11 setup: %w", err)
	}
	if head[0] != 1 {
		return nil, fmt.Errorf("x11 setup refused: %q", strings.TrimRight(string(data[:min(int(head[1]), len(data))]), "\x00"))
	}
	if len(data) < 32 {
		return nil, errors.New("x11 setup: short reply")
	}
	vendor := int(binary.LittleEndian.Uint16(data[16:18]))
	formats := int(data[21])
	off := 32 + pad4(vendor) + 8*formats
	if len(data) < off+4 {
		return nil, errors.New("x11 setup: no screen")
	}
	return &xconn{c: c, root: binary.LittleEndian.Uint32(data[off : off+4])}, nil
}

func pad4(n int) int { return (n + 3) &^ 3 }

// i16 reads a signed 16-bit field.
func i16(b []byte) int {
	v := int(binary.LittleEndian.Uint16(b))
	if v >= 1<<15 {
		v -= 1 << 16
	}
	return v
}

// maxRequest caps a request's length in bytes: every request here is a
// few fields, or an atom's name.
const maxRequest = 1 << 10

// xError is an error the X server answered a request with.
type xError struct{ code, opcode byte }

func (e xError) Error() string { return fmt.Sprintf("x11 error %d on request %d", e.code, e.opcode) }

// isBadWindow reports X's error for a window that doesn't exist (any
// more): one that closed while the tree was being read.
func isBadWindow(err error) bool {
	var xe xError
	return errors.As(err, &xe) && xe.code == 3
}

// request sends one request and reads its reply. Requests go one at a
// time, so the next reply or error is this request's; events (none are
// selected) are skipped.
func (x *xconn) request(opcode, b1 byte, body []byte) ([]byte, error) {
	if len(body) > maxRequest {
		return nil, errors.New("x11 request too long")
	}
	msg := make([]byte, 4+pad4(len(body)))
	msg[0], msg[1] = opcode, b1
	binary.LittleEndian.PutUint16(msg[2:4], uint16(len(msg)/4)) //nolint:gosec // at most maxRequest/4+1
	copy(msg[4:], body)
	if _, err := x.c.Write(msg); err != nil {
		return nil, err
	}
	for {
		r := make([]byte, 32)
		if _, err := io.ReadFull(x.c, r); err != nil {
			return nil, err
		}
		switch r[0] {
		case 0:
			return nil, xError{r[1], r[10]}
		case 1:
			n := binary.LittleEndian.Uint32(r[4:8])
			if n > 1<<18 {
				return nil, errors.New("x11 reply too long")
			}
			rest := make([]byte, n*4)
			if _, err := io.ReadFull(x.c, rest); err != nil {
				return nil, err
			}
			return append(r, rest...), nil
		case 35: // a generic event carries more
			if n := binary.LittleEndian.Uint32(r[4:8]); n > 0 {
				if _, err := io.CopyN(io.Discard, x.c, int64(n)*4); err != nil {
					return nil, err
				}
			}
		}
	}
}

func u32(v ...uint32) []byte {
	b := make([]byte, 4*len(v))
	for i, n := range v {
		binary.LittleEndian.PutUint32(b[4*i:], n)
	}
	return b
}

// children lists a window's children, bottom of the stack first.
func (x *xconn) children(w uint32) ([]uint32, error) {
	r, err := x.request(15, 0, u32(w)) // QueryTree
	if err != nil {
		return nil, err
	}
	n := int(binary.LittleEndian.Uint16(r[16:18]))
	if len(r) < 32+4*n {
		return nil, errors.New("x11 QueryTree: short reply")
	}
	kids := make([]uint32, n)
	for i := range kids {
		kids[i] = binary.LittleEndian.Uint32(r[32+4*i:])
	}
	return kids, nil
}

// attributes reports whether a window is viewable (it and its ancestors
// mapped) and whether it is override-redirect (a menu, bubble or tooltip
// the window manager doesn't manage).
func (x *xconn) attributes(w uint32) (viewable, override bool, err error) {
	r, err := x.request(3, 0, u32(w)) // GetWindowAttributes
	if err != nil {
		return false, false, err
	}
	if len(r) < 28 {
		return false, false, errors.New("x11 GetWindowAttributes: short reply")
	}
	return r[26] == 2, r[27] != 0, nil
}

// rect is a window's place on the screen.
type rect struct{ X, Y, W, H int }

func (r rect) contains(x, y int) bool { return x >= r.X && y >= r.Y && x < r.X+r.W && y < r.Y+r.H }

// geometry is a window's outer box (its border included) relative to its
// parent.
func (x *xconn) geometry(w uint32) (rect, error) {
	r, err := x.request(14, 0, u32(w)) // GetGeometry
	if err != nil {
		return rect{}, err
	}
	b := int(binary.LittleEndian.Uint16(r[20:22]))
	return rect{
		X: i16(r[12:14]),
		Y: i16(r[14:16]),
		W: int(binary.LittleEndian.Uint16(r[16:18])) + 2*b,
		H: int(binary.LittleEndian.Uint16(r[18:20])) + 2*b,
	}, nil
}

// onScreen is where a window's top left corner is on the screen.
func (x *xconn) onScreen(w uint32) (int, int, error) {
	body := append(u32(w, x.root), 0, 0, 0, 0)
	r, err := x.request(40, 0, body) // TranslateCoordinates
	if err != nil {
		return 0, 0, err
	}
	return i16(r[12:14]), i16(r[14:16]), nil
}

// atom is the atom named name, or 0 if the server has none.
func (x *xconn) atom(name string) (uint32, error) {
	if len(name) > maxRequest-4 {
		return 0, errors.New("x11 atom name too long")
	}
	body := make([]byte, 4+pad4(len(name)))
	binary.LittleEndian.PutUint16(body[0:2], uint16(len(name))) //nolint:gosec // checked above
	copy(body[4:], name)
	r, err := x.request(16, 1, body) // InternAtom, only if it exists
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(r[8:12]), nil
}

// property is a window's property, up to 1 KiB of it, and whether the
// window has it at all.
func (x *xconn) property(w, prop uint32) ([]byte, bool, error) {
	if prop == 0 {
		return nil, false, nil
	}
	r, err := x.request(20, 0, u32(w, prop, 0, 0, 256)) // GetProperty, any type
	if err != nil {
		return nil, false, err
	}
	format, typ := int(r[1]), binary.LittleEndian.Uint32(r[8:12])
	if typ == 0 {
		return nil, false, nil
	}
	n := int(binary.LittleEndian.Uint32(r[16:20])) * format / 8
	if len(r) < 32+n {
		return nil, false, errors.New("x11 GetProperty: short reply")
	}
	return r[32 : 32+n], true, nil
}

// shownWindow is a top-level window showing at a point of the screen.
type shownWindow struct {
	// Override: an override-redirect window (a menu, a bubble, a
	// tooltip, the taskbar), which the window manager doesn't manage and
	// which has no client.
	Override bool
	// Class and Role are the client window's WM_CLASS class and
	// WM_WINDOW_ROLE: Chromium names its tabbed window's role "browser"
	// and a pop-up window's "pop-up". Box is its place on the screen.
	Class, Role string
	Box         rect
}

// windowsAt lists the top-level windows showing at (px, py), top first:
// the override-redirect windows over it, down to and including the first
// managed one, which is described by its client (the window inside the
// window manager's frame that has WM_STATE). Empty: only the root is
// there.
func (x *xconn) windowsAt(px, py int) ([]shownWindow, error) {
	kids, err := x.children(x.root)
	if err != nil {
		return nil, err
	}
	var stack []shownWindow
	for i := len(kids) - 1; i >= 0; i-- {
		k := kids[i]
		viewable, override, err := x.attributes(k)
		if isBadWindow(err) {
			continue // gone since the tree was read
		}
		if err != nil {
			return nil, err
		}
		if !viewable {
			continue
		}
		g, err := x.geometry(k)
		if isBadWindow(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !g.contains(px, py) {
			continue
		}
		if override {
			stack = append(stack, shownWindow{Override: true, Box: g})
			continue
		}
		c, err := x.client(k)
		if err != nil {
			return nil, err
		}
		return append(stack, c), nil
	}
	return stack, nil
}

// client describes the client window of a top-level window: the first
// window with WM_STATE in it, itself included, at most two levels down.
func (x *xconn) client(top uint32) (shownWindow, error) {
	state, err := x.atom("WM_STATE")
	if err != nil {
		return shownWindow{}, err
	}
	level := []uint32{top}
	for range 3 {
		var next []uint32
		for _, w := range level {
			if _, ok, err := x.property(w, state); err != nil {
				return shownWindow{}, err
			} else if ok {
				return x.describe(w)
			}
			kids, err := x.children(w)
			if err != nil {
				return shownWindow{}, err
			}
			next = append(next, kids...)
		}
		level = next
	}
	// A managed window with no client: describe the frame, which has
	// neither class nor role.
	return x.describe(top)
}

func (x *xconn) describe(w uint32) (shownWindow, error) {
	var t shownWindow
	g, err := x.geometry(w)
	if err != nil {
		return t, err
	}
	gx, gy, err := x.onScreen(w)
	if err != nil {
		return t, err
	}
	t.Box = rect{gx, gy, g.W, g.H}
	if a, err := x.atom("WM_CLASS"); err != nil {
		return t, err
	} else if v, ok, err := x.property(w, a); err != nil {
		return t, err
	} else if ok {
		// instance\0class\0
		parts := strings.Split(strings.TrimRight(string(v), "\x00"), "\x00")
		t.Class = parts[len(parts)-1]
	}
	if a, err := x.atom("WM_WINDOW_ROLE"); err != nil {
		return t, err
	} else if v, ok, err := x.property(w, a); err != nil {
		return t, err
	} else if ok {
		t.Role = strings.TrimRight(string(v), "\x00")
	}
	return t, nil
}
