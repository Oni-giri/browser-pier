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
	if first < 8 || count < 1 || int(first)+count > 256 {
		return keyboardMap{}, fmt.Errorf("the X server has keycodes %d to %d", first, int(first)+count-1)
	}
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

// spare lists the keycodes with no keysyms that no modifier uses and
// that Chromium reads as no key that acts (quietKeycode). How many there
// are depends on the XKB layout data the X server has: with the image's
// (Debian's xkb-data, evdev, us), 13 of the 19 unbound keycodes.
func (m keyboardMap) spare() []byte {
	var out []byte
	for i := range m.count() {
		k := m.first + byte(i)
		if m.mods[k] || !quietKeycode(k) {
			continue
		}
		if !slices.ContainsFunc(m.key(k), func(s uint32) bool { return s != 0 }) {
			out = append(out, k)
		}
	}
	return out
}

// quietKeycode reports whether a keycode is one Chromium reads as no key
// that acts, whatever character it types. Chromium gives a key event the
// DOM code its table has for the keycode, and, when the keysym has no key
// code of its own (as a Unicode one hasn't), the US layout's key code for
// the keycode (DefaultKeyboardCodeFromHardwareKeycode: a table of
// keycodes below 136, and from 136 a few browser keys). An unbound key
// can still be an F-key there (197 is F19, 202 F24, in the image's map),
// so a binding on it would press F19. Quiet means both say nothing, or an
// input-method key: below 136, 8 (no DOM code, VKEY_UNKNOWN), 93 (Lang5,
// VKEY_DBE_DBCSCHAR) and 120 (none, VKEY_UNSUPPORTED); not 97 (IntlRo,
// VKEY_OEM_102), 132 (IntlYen, VKEY_OEM_5) or 103 (none, but
// VKEY_SEPARATOR, the keypad's), whose key codes are a punctuation or a
// keypad key's. From 136 up, every keycode that neither table names.
// (Chromium's keysym tables come before the hardware fallback, and know
// Latin-1 keysyms and some above; that would make the key code depend on
// the character, not the key. Measured, the image's Chromium gives
// keyCode 0 for every character bound here, as for xdotool's own binding
// before; TestVisualTypesNonASCII checks a sample.) Source: Chromium's ui/events/keycodes/dom/dom_code_data.inc (blob
// 3c5ef403df15) and keyboard_code_conversion_x.cc (kHardwareKeycodeMap
// and DefaultKeyboardCodeFromHardwareKeycode, blob eae62840c180), main of
// 2026-11-26; TestVisualTypesNonASCII checks the page sees no other key.
func quietKeycode(k byte) bool {
	if k < 136 {
		return k == 8 || k == 93 || k == 120
	}
	return !chromiumKeys[k]
}

// chromiumKeys are the keycodes from 136 up that either table names:
// browser, media, launch, editing and power keys, F13 to F24. (The
// hardware fallback's own, 166, 167, 181, 212, 232 and 233, are among
// dom_code_data.inc's.)
var chromiumKeys = map[byte]bool{}

func init() {
	for _, k := range []byte{136, 137, 139, 140, 141, 142, 143, 144, 145, 146, 148, 150, 151, 152, 158, 160,
		163, 164, 166, 167, 169, 171, 172, 173, 174, 175, 176, 177, 179, 180, 181, 182, 187, 188, 189, 190,
		191, 192, 193, 194, 195, 196, 197, 198, 199, 200, 201, 202, 209, 212, 214, 215, 216, 217, 218, 225,
		232, 233, 235, 236, 237, 238, 239, 240, 241, 242, 243, 252} {
		chromiumKeys[k] = true
	}
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
	open   func() (keyboard, error)
	perKey int
	runs   []keyRun
}

// bind binds syms to the first of keys, each on both levels (so no Shift
// is needed, and an upper-case letter isn't taken for its lower case),
// then reads them back: a refused change shows as an error there, and
// the reply proves the server has the new map before any key is pressed.
// open opens a new connection, which restore uses: after an X error, kb's
// may be out of step. On an error, a non-nil binding is keys that may
// still be bound.
func bind(kb keyboard, open func() (keyboard, error), m keyboardMap, keys []byte, syms []uint32) (*binding, error) {
	if len(syms) > len(keys) {
		return nil, errNoSpareKeys
	}
	perKey := m.perKey
	b := &binding{kb: kb, open: open, perKey: perKey}
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
			return b.rollback(err)
		}
	}
	if err := check(kb, perKey, b.runs, func(r keyRun) []uint32 { return r.after }); err != nil {
		return b.rollback(err)
	}
	return b, nil
}

// rollback puts back what a failed bind may have changed. If that fails
// too, it returns the binding along with the error, so the caller can
// keep its keys for a later restore (v.leftover).
func (b *binding) rollback(err error) (*binding, error) {
	if rerr := b.restore(); rerr != nil {
		return b, errors.Join(err, fmt.Errorf("couldn't put the keyboard map back: %w", rerr))
	}
	return nil, err
}

// check reads runs back and compares each key's first two keysyms, which
// type, with want's. (The server fills in the rest: XKB copies them to
// the second group, say.)
func check(kb keyboard, perKey int, runs []keyRun, want func(keyRun) []uint32) error {
	for _, r := range runs {
		got, err := mappingOf(kb, perKey, r)
		if err != nil {
			return err
		}
		w := want(r)
		if len(got) != len(w) {
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

func mappingOf(kb keyboard, perKey int, r keyRun) ([]uint32, error) {
	n, got, err := kb.mapping(r.first, len(r.after)/perKey)
	if err == nil && n != perKey {
		err = fmt.Errorf("the keyboard map changed shape at keycode %d", r.first)
	}
	return got, err
}

// restore puts the keys back as they were, over a new connection, and
// reads them back.
func (b *binding) restore() error { return restoreRuns(b.open, b.perKey, b.runs) }

// restoreRuns puts runs' keys back as they were before, over a new
// connection, and reads them back.
func restoreRuns(open func() (keyboard, error), perKey int, runs []keyRun) error {
	kb, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = kb.Close() }()
	for _, r := range runs {
		if err := kb.change(r.first, perKey, r.before); err != nil {
			return err
		}
	}
	return check(kb, perKey, runs, func(r keyRun) []uint32 { return r.before })
}

// xKeyboard is a minimal X11 client over the display's Unix socket: the
// handful of core requests that read and change the keyboard map. xdotool
// can't keep a binding, and xmodmap's package (x11-xserver-utils) would
// add the C preprocessor, 35 MB, to the image; this is the standard
// library.
type xKeyboard struct {
	conn           net.Conn
	minKey, maxKey byte
	// seq is the last request's sequence number, which the server's
	// replies and errors carry (its low 16 bits).
	seq uint16
	// err, once set, is why the connection can't be trusted any more: an
	// X error (for this request or an earlier one, ChangeKeyboardMapping
	// having no reply) or a broken exchange. Every later call returns it.
	err error
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
		if x.minKey < 8 || x.maxKey < x.minKey {
			return fmt.Errorf("the X server has keycodes %d to %d", x.minKey, x.maxKey)
		}
		return nil
	case 0:
		return fmt.Errorf("the X server refused the connection: %s", body[:min(int(head[1]), len(body))])
	}
	return errors.New("the X server wants authentication")
}

func (x *xKeyboard) Close() error { return x.conn.Close() }

func (x *xKeyboard) keycodes() (byte, int) { return x.minKey, int(x.maxKey) - int(x.minKey) + 1 }

// fail marks the connection as untrustworthy, keeping the first reason.
func (x *xKeyboard) fail(err error) error {
	if x.err == nil {
		x.err = err
	}
	return x.err
}

// send writes one request and returns its sequence number.
func (x *xKeyboard) send(req []byte) (uint16, error) {
	if x.err != nil {
		return 0, x.err
	}
	_ = x.conn.SetDeadline(time.Now().Add(xTimeout))
	if _, err := x.conn.Write(req); err != nil {
		return 0, x.fail(err)
	}
	x.seq++
	return x.seq, nil
}

// await reads until the reply to request seq, skipping events (every
// client gets MappingNotify) and any stale reply. An X error, for seq or
// for an earlier request, fails it and every later call; one for an
// earlier request is read past, so the stream stays in step.
func (x *xKeyboard) await(seq uint16) ([]byte, error) {
	for {
		b := make([]byte, 32)
		if _, err := io.ReadFull(x.conn, b); err != nil {
			return nil, x.fail(err)
		}
		got := xOrder.Uint16(b[2:4])
		switch b[0] {
		case 0:
			err := x.fail(fmt.Errorf("the X server sent error %d for request %d", b[1], b[10]))
			if got == seq {
				return nil, err
			}
		case 1:
			n := xOrder.Uint32(b[4:8])
			if n > 1<<16 {
				return nil, x.fail(fmt.Errorf("the X server sent a %d-word reply", n))
			}
			more := make([]byte, int(n)*4)
			if _, err := io.ReadFull(x.conn, more); err != nil {
				return nil, x.fail(err)
			}
			if got != seq {
				continue // a stale reply
			}
			if x.err != nil {
				return nil, x.err
			}
			return append(b, more...), nil
		case 35, 35 | 0x80:
			// A GenericEvent, longer than 32 bytes; none is selected,
			// but skipping one keeps the stream in step.
			n := xOrder.Uint32(b[4:8])
			if n > 1<<16 {
				return nil, x.fail(fmt.Errorf("the X server sent a %d-word event", n))
			}
			if _, err := io.CopyN(io.Discard, x.conn, int64(n)*4); err != nil {
				return nil, x.fail(err)
			}
		}
	}
}

// call sends a request and returns its reply.
func (x *xKeyboard) call(req []byte) ([]byte, error) {
	seq, err := x.send(req)
	if err != nil {
		return nil, err
	}
	return x.await(seq)
}

func (x *xKeyboard) mapping(first byte, count int) (int, []uint32, error) {
	if first < 8 || count < 1 || count > 248 || int(first)+count > 256 {
		return 0, nil, fmt.Errorf("keycodes %d to %d: out of range", first, int(first)+count-1)
	}
	// GetKeyboardMapping (101).
	b, err := x.call([]byte{101, 0, 2, 0, first, byte(count), 0, 0}) //nolint:gosec // count is checked above
	if err != nil {
		return 0, nil, err
	}
	perKey := int(b[1])
	if perKey < 1 || len(b)-32 != 4*count*perKey {
		return 0, nil, x.fail(fmt.Errorf("the X server sent %d bytes of keysyms for %d keys of %d", len(b)-32, count, perKey))
	}
	syms := make([]uint32, count*perKey)
	for i := range syms {
		syms[i] = xOrder.Uint32(b[32+4*i:])
	}
	return perKey, syms, nil
}

func (x *xKeyboard) modifiers() ([]byte, error) {
	// GetModifierMapping (119): 8 modifiers of b[1] keycodes each.
	b, err := x.call([]byte{119, 0, 1, 0})
	if err != nil {
		return nil, err
	}
	n := 8 * int(b[1])
	if len(b)-32 != n {
		return nil, x.fail(fmt.Errorf("the X server sent %d bytes of modifier keys, want %d", len(b)-32, n))
	}
	var out []byte
	for _, k := range b[32:] {
		if k != 0 {
			out = append(out, k)
		}
	}
	return out, nil
}

func (x *xKeyboard) change(first byte, perKey int, syms []uint32) error {
	n := len(syms) / max(perKey, 1)
	if first < 8 || perKey < 1 || perKey > 255 || n < 1 || n*perKey != len(syms) || int(first)+n > 256 {
		return fmt.Errorf("%d keysyms, %d a key, from keycode %d: not whole keys in range", len(syms), perKey, first)
	}
	// ChangeKeyboardMapping (100), which has no reply: an error comes
	// before the reply to a later request (await).
	req := make([]byte, 8, 8+4*len(syms))
	req[0], req[1] = 100, byte(n)                  //nolint:gosec // n and perKey are checked above
	xOrder.PutUint16(req[2:], uint16(2+len(syms))) //nolint:gosec // at most 248 keys of 255 keysyms
	req[4], req[5] = first, byte(perKey)
	for _, sym := range syms {
		req = xOrder.AppendUint32(req, sym)
	}
	_, err := x.send(req)
	return err
}
