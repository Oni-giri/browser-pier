package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"time"
	"unicode"
)

// Typing characters the keyboard map lacks (PROJ-195).
//
// X sends a client keycodes, and each client turns them into characters
// with its own copy of the keyboard map. xdotool types a character the map
// lacks (é, ā, an emoji) by binding it to a spare keycode, pressing that
// and unbinding it at once. Chromium hears of each change (MappingNotify)
// and fetches the map again; when the fetch comes after the unbinding, the
// press means nothing to it and the character is lost without an error.
// So BrowserPier binds every such character of a text to a spare keycode
// before typing any of it, gives Chromium time to fetch the new map, types
// the whole text as ordinary key presses, gives Chromium time to read them,
// and only then puts the map back.

// keymapSettle is how long Chromium gets to take a change of the keyboard
// map: after the binding, before the first key, and after the last key,
// before the map is put back. Chromium fetches the map when it reads the
// change, and a fetch after the map is put back finds the keys unbound, so
// a binding must outlast Chromium's lag in reading its events; without
// the waits, one character typed alone would be bound for a few
// milliseconds, as with xdotool. On an idle browser no wait was needed
// (40 of 40 runs exact, and 20 of 20 for a lone character); this is the
// margin for a busy one, a page loading, say.
const keymapSettle = 150 * time.Millisecond

// keyboard reads and changes the X server's keyboard map.
type keyboard interface {
	// keycodes is the range of keycodes the map has.
	keycodes() (first byte, count int)
	// mapping reads count keycodes' keysyms from first, perKey of each.
	mapping(first byte, count int) (perKey int, syms []uint32, err error)
	// modifiers lists the keycodes bound to modifiers.
	modifiers() ([]byte, error)
	// change sets the keysyms of len(syms)/perKey keycodes from first.
	change(first byte, perKey int, syms []uint32) error
	Close() error
}

// keyboardMap is the keyboard map as it was read before a type.
type keyboardMap struct {
	first  byte
	perKey int
	syms   []uint32 // perKey keysyms for each keycode from first
	mods   map[byte]bool
}

func readKeyboardMap(kb keyboard) (keyboardMap, error) {
	first, count := kb.keycodes()
	perKey, syms, err := kb.mapping(first, count)
	if err != nil {
		return keyboardMap{}, err
	}
	if perKey < 1 || len(syms) != perKey*count {
		return keyboardMap{}, fmt.Errorf("the keyboard map came back with %d keysyms for %d keys", len(syms), count)
	}
	mods, err := kb.modifiers()
	if err != nil {
		return keyboardMap{}, err
	}
	m := keyboardMap{first: first, perKey: perKey, syms: syms, mods: map[byte]bool{}}
	for _, k := range mods {
		m.mods[k] = true
	}
	return m, nil
}

// key is keycode k's keysyms.
func (m keyboardMap) key(k byte) []uint32 {
	i := int(k-m.first) * m.perKey
	return m.syms[i : i+m.perKey]
}

func (m keyboardMap) count() int { return len(m.syms) / m.perKey }

// has reports whether a key types sym as it is, or with Shift: xdotool
// presses Shift for the second.
func (m keyboardMap) has(sym uint32) bool {
	for i := range m.count() {
		syms := m.key(m.first + byte(i))
		if slices.Contains(syms[:min(2, len(syms))], sym) {
			return true
		}
	}
	return false
}

// spare lists the keycodes with no keysyms that no modifier uses. In the
// image's map there are 19: evdev's codes (plus 8) of keys the US layout
// doesn't bind. Chromium puts a character typed on one into the page as
// text; the keydown, if there is one, has key "\x00" and an international
// or input-method key's code (IntlRo, IntlYen, Lang5) or none, or is
// "Process" (229), so the key itself does nothing. The Docker test
// TestVisualTypesNonASCII checks that.
func (m keyboardMap) spare() []byte {
	var out []byte
	for i := range m.count() {
		k := m.first + byte(i)
		if m.mods[k] {
			continue
		}
		if !slices.ContainsFunc(m.key(k), func(s uint32) bool { return s != 0 }) {
			out = append(out, k)
		}
	}
	return out
}

// runeKeysym is the X keysym that types r: Latin-1's own below U+0100,
// and 0x1000000 plus the code point above it, as Xlib reads "U00E9" or
// "U0101". Control characters type no character, so they have none.
func runeKeysym(r rune) (uint32, bool) {
	if r < 0x20 || (r >= 0x7f && r < 0xa0) || r > unicode.MaxRune || (r >= 0xd800 && r < 0xe000) {
		return 0, false
	}
	if r < 0x100 {
		return uint32(r), true
	}
	return 0x1000000 | uint32(r), true
}

// keyName is the name xdotool presses r's key by: charKeys's, or the
// Unicode name ("U00E9"), which Xlib turns into runeKeysym's keysym.
func keyName(r rune) string {
	if k, ok := keysym(r); ok {
		return k
	}
	return fmt.Sprintf("U%04X", r)
}

// untypeable is the 1-based position of the first character of text that
// no key types (a control character other than \n, \r and \t), or 0.
func untypeable(text []rune) int {
	for i, r := range text {
		if _, ok := keysym(r); ok {
			continue
		}
		if _, ok := runeKeysym(r); !ok {
			return i + 1
		}
	}
	return 0
}

// needsKeymap reports whether text has a character charKeys has no key
// for, which only a binding types.
func needsKeymap(text []rune) bool {
	return slices.ContainsFunc(text, func(r rune) bool { _, ok := keysym(r); return !ok })
}

// keyBatch is a stretch of text and the keysyms to bind before typing it.
type keyBatch struct {
	text []rune
	bind []uint32
}

var errNoSpareKeys = errors.New("the keyboard map has no spare key to bind a character to")

// planKeys splits text into stretches that each need at most spare keys
// bound (one, unless the text has more distinct characters the map lacks
// than there are spare keys), and lists those keysyms for each.
func planKeys(text []rune, has func(uint32) bool, spare int) ([]keyBatch, error) {
	var out []keyBatch
	var cur keyBatch
	bound := map[uint32]bool{}
	for i, r := range text {
		if _, ok := keysym(r); !ok {
			sym, ok := runeKeysym(r)
			if !ok {
				return nil, fmt.Errorf("character %d is a control character, which no key types", i+1)
			}
			if !has(sym) && !bound[sym] {
				if spare < 1 {
					return nil, errNoSpareKeys
				}
				if len(cur.bind) == spare {
					out, cur, bound = append(out, cur), keyBatch{}, map[uint32]bool{}
				}
				cur.bind = append(cur.bind, sym)
				bound[sym] = true
			}
		}
		cur.text = append(cur.text, r)
	}
	if len(cur.text) > 0 {
		out = append(out, cur)
	}
	return out, nil
}

// keyRun is a stretch of keycodes changed together: one request, so
// Chromium fetches the map once for it.
type keyRun struct {
	first         byte
	before, after []uint32
}

// binding is spare keys bound to keysyms; restore puts them back.
type binding struct {
	kb     keyboard
	perKey int
	runs   []keyRun
}

// bind binds syms to the first of keys, each on both levels (so no Shift
// is needed, and an upper-case letter isn't taken for its lower case),
// then reads them back: a refused change shows as an error there, and
// the reply proves the server has the new map before any key is pressed.
func bind(kb keyboard, m keyboardMap, keys []byte, syms []uint32) (*binding, error) {
	if len(syms) > len(keys) {
		return nil, errNoSpareKeys
	}
	perKey := m.perKey
	b := &binding{kb: kb, perKey: perKey}
	for i, sym := range syms {
		k := keys[i]
		after := make([]uint32, perKey)
		after[0] = sym
		if perKey > 1 {
			after[1] = sym
		}
		if n := len(b.runs); n > 0 && int(b.runs[n-1].first)+len(b.runs[n-1].after)/perKey == int(k) {
			r := &b.runs[n-1]
			r.before = append(r.before, m.key(k)...)
			r.after = append(r.after, after...)
			continue
		}
		b.runs = append(b.runs, keyRun{first: k, before: slices.Clone(m.key(k)), after: after})
	}
	for i, r := range b.runs {
		if err := kb.change(r.first, perKey, r.after); err != nil {
			b.runs = b.runs[:i+1] // the last may be half done
			return nil, errors.Join(err, b.restore())
		}
	}
	if err := b.check(func(r keyRun) []uint32 { return r.after }); err != nil {
		return nil, errors.Join(err, b.restore())
	}
	return b, nil
}

// check reads every run back and compares each key's first two keysyms,
// which type, with want's. (The server fills in the rest: XKB copies them
// to the second group, say.)
func (b *binding) check(want func(keyRun) []uint32) error {
	for _, r := range b.runs {
		perKey, got, err := b.kb.mapping(r.first, len(r.after)/b.perKey)
		if err != nil {
			return err
		}
		w := want(r)
		if perKey != b.perKey || len(got) != len(w) {
			return fmt.Errorf("the keyboard map changed shape at keycode %d", r.first)
		}
		for i := 0; i < len(w); i += perKey {
			if n := min(2, perKey); !slices.Equal(got[i:i+n], w[i:i+n]) {
				return fmt.Errorf("the keyboard map didn't take the change at keycode %d", int(r.first)+i/perKey)
			}
		}
	}
	return nil
}

// restore puts the keys back as they were, and reads them back.
func (b *binding) restore() error {
	var errs []error
	for _, r := range b.runs {
		errs = append(errs, b.kb.change(r.first, b.perKey, r.before))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return b.check(func(r keyRun) []uint32 { return r.before })
}

// xKeyboard is a minimal X11 client over the display's Unix socket: the
// handful of core requests that read and change the keyboard map. xdotool
// can't keep a binding, and xmodmap's package (x11-xserver-utils) would
// add the C preprocessor, 35 MB, to the image; this is the standard
// library.
type xKeyboard struct {
	conn           net.Conn
	minKey, maxKey byte
}

// xTimeout bounds one exchange with the X server.
const xTimeout = 5 * time.Second

var xOrder = binary.LittleEndian

func dialKeyboard(ctx context.Context) (keyboard, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", "/tmp/.X11-unix/X"+display[1:])
	if err != nil {
		return nil, err
	}
	x := &xKeyboard{conn: conn}
	if err := x.setup(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return x, nil
}

// setup opens the connection: little-endian, protocol 11.0, and no
// authorization, as for xdotool (Xvfb runs without -auth).
func (x *xKeyboard) setup() error {
	_ = x.conn.SetDeadline(time.Now().Add(xTimeout))
	if _, err := x.conn.Write([]byte{'l', 0, 11, 0, 0, 0, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(x.conn, head); err != nil {
		return err
	}
	body := make([]byte, int(xOrder.Uint16(head[6:8]))*4)
	if _, err := io.ReadFull(x.conn, body); err != nil {
		return err
	}
	switch head[0] {
	case 1:
		if len(body) < 28 {
			return errors.New("the X server's setup reply is short")
		}
		x.minKey, x.maxKey = body[26], body[27]
		if x.maxKey < x.minKey {
			return errors.New("the X server has no keycodes")
		}
		return nil
	case 0:
		return fmt.Errorf("the X server refused the connection: %s", body[:min(int(head[1]), len(body))])
	}
	return errors.New("the X server wants authentication")
}

func (x *xKeyboard) Close() error { return x.conn.Close() }

func (x *xKeyboard) keycodes() (byte, int) { return x.minKey, int(x.maxKey-x.minKey) + 1 }

// send writes one request; with reply, it returns the reply, skipping
// events (every client gets MappingNotify) and failing on an X error,
// which comes before the reply to any later request.
func (x *xKeyboard) send(req []byte, reply bool) ([]byte, error) {
	_ = x.conn.SetDeadline(time.Now().Add(xTimeout))
	if _, err := x.conn.Write(req); err != nil {
		return nil, err
	}
	for reply {
		b := make([]byte, 32)
		if _, err := io.ReadFull(x.conn, b); err != nil {
			return nil, err
		}
		switch b[0] {
		case 0:
			return nil, fmt.Errorf("the X server sent error %d for request %d", b[1], b[10])
		case 1:
			n := xOrder.Uint32(b[4:8])
			if n > 1<<16 {
				return nil, fmt.Errorf("the X server sent a %d-word reply", n)
			}
			more := make([]byte, int(n)*4)
			if _, err := io.ReadFull(x.conn, more); err != nil {
				return nil, err
			}
			return append(b, more...), nil
		case 35, 35 | 0x80:
			// A GenericEvent, longer than 32 bytes; none is selected,
			// but skipping one keeps the stream in step.
			if _, err := io.CopyN(io.Discard, x.conn, int64(xOrder.Uint32(b[4:8]))*4); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}

func (x *xKeyboard) mapping(first byte, count int) (int, []uint32, error) {
	if count < 1 || int(first)+count > 256 {
		return 0, nil, fmt.Errorf("keycodes %d to %d: out of range", first, int(first)+count-1)
	}
	// GetKeyboardMapping (101).
	b, err := x.send([]byte{101, 0, 2, 0, first, byte(count), 0, 0}, true) //nolint:gosec // count is checked above
	if err != nil {
		return 0, nil, err
	}
	perKey := int(b[1])
	syms := make([]uint32, (len(b)-32)/4)
	for i := range syms {
		syms[i] = xOrder.Uint32(b[32+4*i:])
	}
	return perKey, syms, nil
}

func (x *xKeyboard) modifiers() ([]byte, error) {
	// GetModifierMapping (119): 8 modifiers of b[1] keycodes each.
	b, err := x.send([]byte{119, 0, 1, 0}, true)
	if err != nil {
		return nil, err
	}
	n := min(8*int(b[1]), len(b)-32)
	var out []byte
	for _, k := range b[32 : 32+n] {
		if k != 0 {
			out = append(out, k)
		}
	}
	return out, nil
}

func (x *xKeyboard) change(first byte, perKey int, syms []uint32) error {
	n := len(syms) / max(perKey, 1)
	if perKey < 1 || perKey > 255 || n < 1 || n*perKey != len(syms) || int(first)+n > 256 {
		return fmt.Errorf("%d keysyms, %d a key, from keycode %d: not whole keys in range", len(syms), perKey, first)
	}
	// ChangeKeyboardMapping (100), which has no reply.
	req := make([]byte, 8, 8+4*len(syms))
	req[0], req[1] = 100, byte(n)                  //nolint:gosec // n and perKey are checked above
	xOrder.PutUint16(req[2:], uint16(2+len(syms))) //nolint:gosec // at most 248 keys of 255 keysyms
	req[4], req[5] = first, byte(perKey)
	for _, s := range syms {
		req = xOrder.AppendUint32(req, s)
	}
	_, err := x.send(req, false)
	return err
}
