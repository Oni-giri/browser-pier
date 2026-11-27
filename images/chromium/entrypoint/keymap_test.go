package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeKeyboard is an X server's keyboard map: keycodes 8 to 255, four
// keysyms each, every one bound but the spare ones. A change copies the
// first two keysyms to the next two, as XKB does. Its opens, reads and
// changes go into the calls log as "keymap" calls, so tests see them in
// order with xdotool's runs.
type fakeKeyboard struct {
	calls      *[]call
	syms       []uint32
	mods       []byte
	openErr    error
	changeErr  error  // every change fails
	restoreErr error  // every change to NoSymbol (a restore) fails
	bindErr    error  // every other change fails
	bindsLeft  int    // with bindErr: this many other changes succeed first
	onChange   func() // called on each change
	ignore     bool   // changes are accepted and do nothing
	opened     int
	closed     int
}

const fakePerKey = 4

func newFakeKeyboard(calls *[]call, spare ...byte) *fakeKeyboard {
	f := &fakeKeyboard{calls: calls, syms: make([]uint32, 248*fakePerKey), mods: []byte{50, 62, 37}}
	for k := 8; k < 256; k++ {
		if !slices.Contains(spare, byte(k)) {
			f.syms[(k-8)*fakePerKey] = 0x1008ff00 + uint32(k) // some XF86 key
		}
	}
	return f
}

func (f *fakeKeyboard) log(args ...string) { *f.calls = append(*f.calls, call{"keymap", args}) }

func (f *fakeKeyboard) open(context.Context) (keymapConn, error) {
	f.log("open")
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.opened++
	return f, nil
}

func (f *fakeKeyboard) keycodes() (byte, int) { return 8, 248 }

// reopen is bind's and restore's new connection: the same map.
func (f *fakeKeyboard) reopen() (keymapConn, error) { return f.open(context.Background()) }

func (f *fakeKeyboard) mapping(first byte, count int) (int, []uint32, error) {
	i := int(first-8) * fakePerKey
	return fakePerKey, slices.Clone(f.syms[i : i+count*fakePerKey]), nil
}

func (f *fakeKeyboard) modifiers() ([]byte, error) { return f.mods, nil }

func (f *fakeKeyboard) change(first byte, perKey int, syms []uint32) error {
	f.log("change", strconv.Itoa(int(first)), strconv.Itoa(len(syms)/perKey), fmt.Sprintf("%x", syms[0]))
	if f.onChange != nil {
		f.onChange()
	}
	if f.changeErr != nil {
		return f.changeErr
	}
	if f.restoreErr != nil && syms[0] == 0 {
		return f.restoreErr
	}
	if f.bindErr != nil && syms[0] != 0 {
		if f.bindsLeft == 0 {
			return f.bindErr
		}
		f.bindsLeft--
	}
	if perKey != fakePerKey {
		return fmt.Errorf("perKey %d", perKey)
	}
	if f.ignore {
		return nil
	}
	for i := 0; i < len(syms); i += perKey {
		k := slices.Clone(syms[i : i+perKey])
		k[2], k[3] = k[0], k[1]
		copy(f.syms[int(first-8)*fakePerKey+i:], k)
	}
	return nil
}

func (f *fakeKeyboard) Close() error { f.closed++; return nil }

// key is a keycode's keysyms now.
func (f *fakeKeyboard) key(k byte) []uint32 {
	i := int(k-8) * fakePerKey
	return f.syms[i : i+fakePerKey]
}

func TestRuneKeysym(t *testing.T) {
	for r, want := range map[rune]uint32{'é': 0xe9, 'É': 0xc9, 'ā': 0x1000101, 'Ā': 0x1000100, '€': 0x10020ac, '😀': 0x101f600, 'A': 0x41} {
		if got, ok := runeKeysym(r); !ok || got != want {
			t.Errorf("runeKeysym(%q) = %x, %t; want %x", r, got, ok, want)
		}
	}
	for _, r := range "\r\x00\x1b\x7f\u0085\u009f" {
		if got, ok := runeKeysym(r); ok {
			t.Errorf("control character %U has keysym %x", r, got)
		}
	}
	for r, want := range map[rune]string{'a': "a", ' ': "space", '\n': "Return", 'é': "U00E9", 'ā': "U0101", '😀': "U1F600"} {
		got := keyName(r)
		if got != want || !keysymRE.MatchString(got) || xdotoolCommand(got) {
			t.Errorf("keyName(%q) = %q, want %q", r, got, want)
		}
	}
}

func TestKeyboardMapSpareKeys(t *testing.T) {
	var calls []call
	f := newFakeKeyboard(&calls, 8, 37, 93, 94, 153, 154, 197, 202, 217, 248)
	m, err := readKeyboardMap(f)
	if err != nil {
		t.Fatal(err)
	}
	// 37 has no keysyms, but a modifier's on it; Chromium reads 94 by
	// its US-layout key (below 136, and not on the quiet list), 197 as
	// F19, 202 as F24 and 217 as BassBoost.
	if got := m.spare(); !slices.Equal(got, []byte{8, 93, 153, 154, 248}) {
		t.Fatalf("spare %v", got)
	}
	f.key(40)[1] = 0xe9 // a key with é on its Shift level
	m, _ = readKeyboardMap(f)
	if !m.has(0xe9) || m.has(0xea) {
		t.Fatal("has")
	}
	for _, k := range []byte{8, 93, 120, 149, 154, 168, 178, 183, 184, 219, 222, 230, 248} {
		if !quietKeycode(k) {
			t.Errorf("keycode %d isn't quiet", k)
		}
	}
	// F13 to F24, the browser and media keys, the hardware fallback's
	// punctuation and keypad keys (97, 103, 132), and every key below 136
	// but the three: none.
	for _, k := range []byte{9, 23, 36, 94, 97, 103, 110, 119, 121, 124, 132, 135, 166, 167, 172, 180, 181, 191, 197, 202, 212, 217, 225, 232, 252} {
		if quietKeycode(k) {
			t.Errorf("keycode %d is quiet", k)
		}
	}
}

func TestPlanKeys(t *testing.T) {
	has := func(sym uint32) bool { return sym == 0xe0 } // à is on the map
	text := []rune("héllo à ūdens ū 😀é")
	plan, err := planKeys(text, has, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || string(plan[0].text) != string(text) || !slices.Equal(plan[0].bind, []uint32{0xe9, 0x100016b, 0x101f600}) {
		t.Fatalf("plan %+v", plan)
	}
	// ASCII binds nothing.
	if plan, err := planKeys([]rune("plain\ttext\n"), has, 0); err != nil || len(plan) != 1 || plan[0].bind != nil {
		t.Fatalf("ASCII: %+v %v", plan, err)
	}
	// Two spare keys: a new stretch where a third character comes.
	plan, err = planKeys([]rune("aébūcéd😀ūe"), has, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 || string(plan[0].text) != "aébūcéd" || !slices.Equal(plan[0].bind, []uint32{0xe9, 0x100016b}) ||
		string(plan[1].text) != "😀ūe" || !slices.Equal(plan[1].bind, []uint32{0x101f600, 0x100016b}) {
		t.Fatalf("batches %+v", plan)
	}
	if _, err := planKeys([]rune("abc é"), has, 0); !errors.Is(err, errNoSpareKeys) {
		t.Fatalf("no spare keys: %v", err)
	}
	if _, err := planKeys([]rune("é\x1b"), has, 3); err == nil || !strings.Contains(err.Error(), "character 2 is a control character") {
		t.Fatalf("control character: %v", err)
	}
	if i := untypeable([]rune("ok é\ta\r\x7fb")); i != 8 {
		t.Fatalf("untypeable = %d", i)
	}
	if untypeable([]rune("é😀\n\t ~")) != 0 || needsKeymap([]rune("ASCII ~\n")) || !needsKeymap([]rune("ASCII é")) {
		t.Fatal("untypeable or needsKeymap")
	}
}

func TestBindAndRestore(t *testing.T) {
	var calls []call
	f := newFakeKeyboard(&calls, 8, 153, 154, 248)
	m, _ := readKeyboardMap(f)
	before := slices.Clone(f.syms)
	b, err := bind(f, f.reopen, m, m.spare(), []uint32{0xe9, 0x101f600, 0x10000c9})
	if err != nil {
		t.Fatal(err)
	}
	// Both levels, so no Shift; 153 and 154 in one request.
	if !slices.Equal(f.key(8), []uint32{0xe9, 0xe9, 0xe9, 0xe9}) || !slices.Equal(f.key(153)[:2], []uint32{0x101f600, 0x101f600}) ||
		!slices.Equal(f.key(154)[:2], []uint32{0x10000c9, 0x10000c9}) || f.key(248)[0] != 0 {
		t.Fatalf("bound: %x %x %x %x", f.key(8), f.key(153), f.key(154), f.key(248))
	}
	if len(calls) != 2 || strings.Join(calls[1].args, " ") != "change 153 2 101f600" {
		t.Fatalf("changes %v", calls)
	}
	// Put back over a new connection.
	if err := b.restore(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.syms, before) || calls[2].args[0] != "open" {
		t.Fatalf("not restored, or not over a new connection: %v", calls)
	}

	// A refused change fails, and what was changed is put back.
	f.changeErr = errors.New("X error 2 in request 100")
	if _, err := bind(f, f.reopen, m, m.spare(), []uint32{0xe9}); err == nil || !strings.Contains(err.Error(), "X error 2") {
		t.Fatalf("refused: %v", err)
	}
	f.changeErr = nil
	// A change the server didn't take fails too.
	f.ignore = true
	if _, err := bind(f, f.reopen, m, m.spare(), []uint32{0xe9}); err == nil || !strings.Contains(err.Error(), "didn't take") {
		t.Fatalf("ignored: %v", err)
	}
	if _, err := bind(f, f.reopen, m, []byte{8}, []uint32{0xe9, 0xea}); !errors.Is(err, errNoSpareKeys) {
		t.Fatalf("too few keys: %v", err)
	}
}

// keymapVisual is a test visual whose keyboard map has the given spare
// keys.
func keymapVisual(t *testing.T, spare ...byte) (*visual, *[]call, *fakeKeyboard) {
	t.Helper()
	v, calls := newTestVisual(false)
	f := newFakeKeyboard(calls, spare...)
	v.keymap = f.open
	return v, calls, f
}

// events lists the calls as words: "keymap change 8 1 e9", "xdotool key".
func events(calls []call) []string {
	var out []string
	for _, c := range calls {
		if c.name == "keymap" {
			out = append(out, "keymap "+strings.Join(c.args, " "))
		} else {
			out = append(out, c.name+" "+c.args[0])
		}
	}
	return out
}

func TestTypeMappedAtInstantPace(t *testing.T) {
	v, calls, f := keymapVisual(t, 8, 153, 154, 248)
	before := slices.Clone(f.syms)
	body, _ := json.Marshal(map[string]string{"text": "héllo 😀", "pace": "instant"})
	if rec := serveVisual(v, http.MethodPost, "/input/type", string(body)); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d %s", rec.Code, rec.Body)
	}
	want := []string{"keymap open", "keymap change 8 1 e9", "keymap change 153 1 101f600", "xdotool key", "keymap open", "keymap change 8 1 0", "keymap change 153 1 0"}
	if got := events(*calls); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	key := (*calls)[3].args
	if got := strings.Join(key, " "); got != "key --clearmodifiers --delay 12 -- h U00E9 l l o space U1F600" {
		t.Fatalf("key run %q", got)
	}
	if !slices.Equal(f.syms, before) || f.closed != f.opened {
		t.Fatalf("map not put back, or a connection left open (%d of %d closed)", f.closed, f.opened)
	}
}

func TestTypeMappedAtHumanPace(t *testing.T) {
	v, calls, f := keymapVisual(t, 8, 153, 154, 248)
	text := "Rīga ūdens, Ārzemes"
	body, _ := json.Marshal(map[string]string{"text": text})
	if rec := serveVisual(v, http.MethodPost, "/input/type", string(body)); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d %s", rec.Code, rec.Body)
	}
	got := events(*calls)
	if !slices.Equal(got[:4], []string{"keymap open", "keymap change 8 1 100012b", "keymap change 153 2 100016b", "xdotool keydown"}) ||
		!slices.Equal(got[len(got)-3:], []string{"keymap open", "keymap change 8 1 0", "keymap change 153 2 0"}) {
		t.Fatalf("calls %q", got)
	}
	if typedText, _ := typed(t, *calls); typedText != text {
		t.Fatalf("typed %q, want %q", typedText, text)
	}
	if f.key(8)[0] != 0 || f.key(153)[0] != 0 || f.key(154)[0] != 0 {
		t.Fatal("map not put back")
	}
}

// More characters the map lacks than spare keys: stretches, each typed
// with its own characters bound.
func TestTypeMappedInStretches(t *testing.T) {
	v, calls, f := keymapVisual(t, 8, 248)
	bound := map[string]bool{}
	v.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, args})
		for _, a := range args {
			if strings.HasPrefix(a, "U") && !bound[a] {
				t.Errorf("pressed %s, which isn't bound", a)
			}
		}
		return nil, nil
	}
	v.keymap = func(ctx context.Context) (keymapConn, error) {
		_, err := f.open(ctx)
		return &watchKeyboard{f, bound}, err
	}
	body, _ := json.Marshal(map[string]string{"text": "aébūcéd😀ūe", "pace": "instant"})
	if rec := serveVisual(v, http.MethodPost, "/input/type", string(body)); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d %s", rec.Code, rec.Body)
	}
	want := []string{"keymap open",
		"keymap change 8 1 e9", "keymap change 248 1 100016b", "xdotool key", "keymap open", "keymap change 8 1 0", "keymap change 248 1 0",
		"keymap change 8 1 101f600", "keymap change 248 1 100016b", "xdotool key", "keymap open", "keymap change 8 1 0", "keymap change 248 1 0"}
	if got := events(*calls); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if strings.Join((*calls)[3].args[5:], " ") != "a U00E9 b U016B c U00E9 d" || strings.Join((*calls)[9].args[5:], " ") != "U1F600 U016B e" {
		t.Fatalf("stretches %q %q", (*calls)[3].args, (*calls)[9].args)
	}
}

// watchKeyboard keeps the names of the keysyms bound now, as xdotool
// names them.
type watchKeyboard struct {
	*fakeKeyboard
	bound map[string]bool
}

func (w *watchKeyboard) change(first byte, perKey int, syms []uint32) error {
	if err := w.fakeKeyboard.change(first, perKey, syms); err != nil {
		return err
	}
	clear(w.bound)
	for k := byte(8); k != 0; k++ {
		if s := w.key(k)[0]; s != 0 && s < 0x1008ff00 {
			cp := s
			if s >= 0x1000000 {
				cp = s - 0x1000000
			}
			w.bound[fmt.Sprintf("U%04X", cp)] = true
		}
	}
	return nil
}

func TestTypeMappedFailsLoudly(t *testing.T) {
	text, _ := json.Marshal(map[string]string{"text": "abc é", "pace": "instant"})
	ran := func(calls []call) bool {
		return slices.ContainsFunc(calls, func(c call) bool { return c.name == "xdotool" })
	}

	// No spare key: nothing typed.
	v, calls, _ := keymapVisual(t)
	rec := serveVisual(v, http.MethodPost, "/input/type", string(text))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "before anything was typed") || !strings.Contains(rec.Body.String(), "no spare key") || ran(*calls) ||
		rec.Header().Get(nothingTypedHeader) != "1" {
		t.Fatalf("no spare key: %d %s %v %v", rec.Code, rec.Body, rec.Header(), *calls)
	}
	// No keyboard map.
	v, calls, f := keymapVisual(t, 8)
	f.openErr = errors.New("dial unix /tmp/.X11-unix/X99: connect: no such file or directory")
	rec = serveVisual(v, http.MethodPost, "/input/type", string(text))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "keyboard map") || ran(*calls) {
		t.Fatalf("no map: %d %s", rec.Code, rec.Body)
	}
	// The binding refused.
	v, calls, f = keymapVisual(t, 8)
	f.changeErr = errors.New("X error 11 in request 100")
	rec = serveVisual(v, http.MethodPost, "/input/type", string(text))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "before anything was typed") || !strings.Contains(rec.Body.String(), "couldn't bind 1 characters") || ran(*calls) {
		t.Fatalf("refused: %d %s", rec.Code, rec.Body)
	}
	// xdotool failing half way: a failure, its keys let go, and the map
	// put back all the same.
	v, calls, f = keymapVisual(t, 8)
	v.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, args})
		if args[0] == "key" {
			return nil, errors.New("killed")
		}
		return nil, nil
	}
	rec = serveVisual(v, http.MethodPost, "/input/type", string(text))
	got := events(*calls)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "may have been typed") || rec.Header().Get(nothingTypedHeader) != "" ||
		!slices.Equal(got[len(got)-4:], []string{"xdotool key", "xdotool keyup", "keymap open", "keymap change 8 1 0"}) || f.key(8)[0] != 0 {
		t.Fatalf("xdotool failing: %d %s %q", rec.Code, rec.Body, got)
	}
	if strings.Join((*calls)[len(*calls)-3].args, " ") != "keyup a b c space U00E9" {
		t.Fatalf("release %q", (*calls)[len(*calls)-3].args)
	}
	// A control character: refused before anything, at either pace, and
	// the message doesn't quote the text.
	for _, pace := range []string{"instant", "human"} {
		v, calls, _ = keymapVisual(t, 8)
		body, _ := json.Marshal(map[string]string{"text": "secret\x1b", "pace": pace})
		rec = serveVisual(v, http.MethodPost, "/input/type", string(body))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "character 7 is a control character") || strings.Contains(rec.Body.String(), "secret") || len(*calls) != 0 {
			t.Fatalf("%s control character: %d %s %v", pace, rec.Code, rec.Body, *calls)
		}
	}
}

// ASCII keeps the paths it had: type at instant pace, charKeys's keys at
// human pace, and the keyboard map isn't opened.
func TestASCIIKeepsItsKeys(t *testing.T) {
	v, calls, f := keymapVisual(t, 8)
	serveVisual(v, http.MethodPost, "/input/type", `{"text": "Hi, there!\n", "pace": "instant"}`)
	if got := strings.Join((*calls)[0].args, " "); len(*calls) != 1 || got != "type --clearmodifiers --delay 12 -- Hi, there!\n" {
		t.Fatalf("instant: %q", got)
	}
	*calls = nil
	serveVisual(v, http.MethodPost, "/input/type", `{"text": "Hi, there!\n"}`)
	if text, _ := typed(t, *calls); text != "Hi, there!\n" {
		t.Fatalf("human: %q", text)
	}
	// \r is Return, as xdotool's type types it.
	*calls = nil
	serveVisual(v, http.MethodPost, "/input/type", `{"text": "a\r\nb"}`)
	if text, _ := typed(t, *calls); text != "a\n\nb" {
		t.Fatalf("human \\r: %q", text)
	}
	if f.opened != 0 || slices.ContainsFunc(*calls, func(c call) bool { return c.name == "keymap" }) {
		t.Fatal("ASCII opened the keyboard map")
	}
}

// While a binding is in place, no other input runs: not an agent's (v.mu)
// nor the live view's (li.mu).
func TestTypeMappedHoldsTheInputLocks(t *testing.T) {
	v, calls, _ := keymapVisual(t, 8)
	v.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, args})
		if v.mu.TryLock() || v.live.mu.TryLock() {
			t.Error("an input lock was free while typing with a binding")
		}
		return nil, nil
	}
	if rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "é", "pace": "instant"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d", rec.Code)
	}
	if !v.mu.TryLock() || !v.live.mu.TryLock() {
		t.Fatal("locks left held")
	}
}

// xServer plays an X server's side of a connection, one exchange at a
// time.
type xServer struct {
	t    *testing.T
	conn net.Conn
}

func (s xServer) read(n int) []byte {
	s.t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(s.conn, b); err != nil {
		s.t.Error(err)
	}
	return b
}

func (s xServer) write(b []byte) {
	if _, err := s.conn.Write(b); err != nil {
		s.t.Error(err)
	}
}

// setup answers a connection's setup with keycodes min to 255.
func (s xServer) setup(min byte) {
	if got := s.read(12); !slices.Equal(got, []byte{'l', 0, 11, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
		s.t.Errorf("setup %v", got)
	}
	body := make([]byte, 32)
	body[26], body[27] = min, 255
	s.write(append([]byte{1, 0, 11, 0, 0, 0, 8, 0}, body...))
}

// reply is a reply to request seq with b[1] = one and data after it.
func xReply(seq uint16, one byte, data []byte) []byte {
	r := make([]byte, 32, 32+len(data))
	r[0], r[1] = 1, one
	xOrder.PutUint16(r[2:], seq)
	xOrder.PutUint32(r[4:], uint32(len(data)/4))
	return append(r, data...)
}

// xErrorPacket is an X error for request seq.
func xErrorPacket(seq uint16, code, major byte) []byte {
	e := make([]byte, 32)
	e[1], e[10] = code, major
	xOrder.PutUint16(e[2:], seq)
	return e
}

func syms(s ...uint32) []byte {
	var b []byte
	for _, x := range s {
		b = xOrder.AppendUint32(b, x)
	}
	return b
}

func TestXKeyboardSpeaksX(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	x := &xKeyboard{conn: client}
	s := xServer{t, server}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.setup(8)
		// 1, GetKeyboardMapping: answered after an event (MappingNotify)
		// and a stale reply, which are skipped.
		if got := s.read(8); !slices.Equal(got, []byte{101, 0, 2, 0, 9, 2, 0, 0}) {
			t.Errorf("GetKeyboardMapping %v", got)
		}
		s.write(append([]byte{34}, make([]byte, 31)...))
		s.write(xReply(0, 2, syms(1, 2, 3, 4)))
		s.write(xReply(1, 2, syms(0x31, 0x21, 0xe9, 0)))
		// 2, GetModifierMapping: two keycodes a modifier.
		if got := s.read(4); !slices.Equal(got, []byte{119, 0, 1, 0}) {
			t.Errorf("GetModifierMapping %v", got)
		}
		s.write(xReply(2, 2, []byte{50, 62, 66, 0, 37, 105, 0, 0, 64, 108, 77, 0, 0, 0, 92, 0}))
		// 3, ChangeKeyboardMapping, which has no reply; then 4,
		// GetKeyboardMapping, answered as a server would: the change's
		// error first, then the reply still due.
		if got := s.read(16); !slices.Equal(got, []byte{100, 1, 4, 0, 8, 2, 0, 0, 0x01, 0xf6, 0x01, 0x01, 0x01, 0xf6, 0x01, 0x01}) {
			t.Errorf("ChangeKeyboardMapping %x", got)
		}
		s.read(8)
		s.write(xErrorPacket(3, 2, 100))
		s.write(xReply(4, 2, syms(0x101f601, 0x101f601)))
	}()
	if err := x.setup(); err != nil {
		t.Fatal(err)
	}
	if first, n := x.keycodes(); first != 8 || n != 248 {
		t.Fatalf("keycodes %d, %d", first, n)
	}
	perKey, got, err := x.mapping(9, 2)
	if err != nil || perKey != 2 || !slices.Equal(got, []uint32{0x31, 0x21, 0xe9, 0}) {
		t.Fatalf("mapping: %d %x %v", perKey, got, err)
	}
	mods, err := x.modifiers()
	if err != nil || !slices.Equal(mods, []byte{50, 62, 66, 37, 105, 64, 108, 77, 92}) {
		t.Fatalf("modifiers %v %v", mods, err)
	}
	if err := x.change(8, 2, []uint32{0x101f601, 0x101f601}); err != nil {
		t.Fatal(err)
	}
	// The change's error fails the read after it, which still reads its
	// own reply, so nothing stale is left for a later read.
	if _, _, err := x.mapping(8, 1); err == nil || err.Error() != "the X server sent error 2 for request 100" {
		t.Fatalf("the change's error: %v", err)
	}
	<-done
	// From then on the connection is refused, and nothing more is sent on
	// it: restore uses a new one.
	errc := make(chan error, 1)
	go func() { _, err := x.modifiers(); errc <- err }()
	_ = server.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _ := server.Read(make([]byte, 4)); n != 0 {
		t.Fatal("a request went out on a connection after an X error")
	}
	if err := <-errc; err == nil || err.Error() != "the X server sent error 2 for request 100" {
		t.Fatalf("after an error: %v", err)
	}
	if err := x.change(8, 2, []uint32{1, 2, 3}); err == nil {
		t.Fatal("a ragged change was sent")
	}
	if _, _, err := x.mapping(250, 10); err == nil {
		t.Fatal("keycodes past 255 asked for")
	}
}

// Replies of the wrong length, and keycodes below 8, are refused.
func TestXKeyboardChecksLengths(t *testing.T) {
	for name, c := range map[string]struct {
		min   byte
		reply func(s xServer)
		call  func(x *xKeyboard) error
	}{
		"short modifier map": {8, func(s xServer) {
			s.read(4)
			s.write(xReply(1, 2, []byte{50, 62, 66, 0, 37, 105, 0, 0})) // 8 bytes for 16
		}, func(x *xKeyboard) error { _, err := x.modifiers(); return err }},
		"long modifier map": {8, func(s xServer) {
			s.read(4)
			s.write(xReply(1, 1, []byte{50, 62, 66, 0, 37, 105, 0, 0, 1, 2, 3, 4}))
		}, func(x *xKeyboard) error { _, err := x.modifiers(); return err }},
		"short keyboard map": {8, func(s xServer) {
			s.read(8)
			s.write(xReply(1, 2, syms(1, 2))) // 1 key for 2
		}, func(x *xKeyboard) error { _, _, err := x.mapping(8, 2); return err }},
		"no keysyms a key": {8, func(s xServer) {
			s.read(8)
			s.write(xReply(1, 0, nil))
		}, func(x *xKeyboard) error { _, _, err := x.mapping(8, 2); return err }},
		"keycodes from 7": {7, func(xServer) {}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			s := xServer{t, server}
			go func() {
				s.setup(c.min)
				c.reply(s)
			}()
			x := &xKeyboard{conn: client}
			err := x.setup()
			if c.call == nil {
				if err == nil {
					t.Fatal("setup took keycodes from 7")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := c.call(x); err == nil {
				t.Fatal("taken")
			}
		})
	}
}

func TestXKeyboardRefused(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		s := xServer{t, server}
		s.read(12)
		s.write(append([]byte{0, 21, 11, 0, 0, 0, 6, 0}, []byte("No protocol specified\x00\x00\x00")...))
	}()
	x := &xKeyboard{conn: client}
	if err := x.setup(); err == nil || err.Error() != "the X server refused the connection: No protocol specified" {
		t.Fatalf("refused: %v", err)
	}
}

// A map that can't be put back fails the call, saying text may have been
// typed; the next call puts those keys back before anything else, or, if
// it still can't, fails before typing anything.
func TestTypeMappedRestoreFailure(t *testing.T) {
	v, calls, f := keymapVisual(t, 8, 248)
	f.restoreErr = errors.New("the X server sent error 2 for request 100")
	body := `{"text": "é", "pace": "instant"}`
	rec := serveVisual(v, http.MethodPost, "/input/type", body)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "may have been typed") || !strings.Contains(rec.Body.String(), "couldn't put the keyboard map back") {
		t.Fatalf("restore failing: %d %s", rec.Code, rec.Body)
	}
	if f.key(8)[0] != 0xe9 || len(v.leftover) != 1 {
		t.Fatalf("key 8 %x, leftover %v", f.key(8), v.leftover)
	}
	// Still failing: the next call types nothing.
	*calls = nil
	rec = serveVisual(v, http.MethodPost, "/input/type", `{"text": "ž", "pace": "instant"}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "before anything was typed") ||
		slices.ContainsFunc(*calls, func(c call) bool { return c.name == "xdotool" }) {
		t.Fatalf("leftover not put back: %d %s %v", rec.Code, rec.Body, *calls)
	}
	// Once it works, the leftover goes first, and key 8 is spare again.
	f.restoreErr = nil
	*calls = nil
	rec = serveVisual(v, http.MethodPost, "/input/type", `{"text": "ž", "pace": "instant"}`)
	got := events(*calls)
	if rec.Code != http.StatusNoContent || !slices.Equal(got[:4], []string{"keymap open", "keymap change 8 1 0", "keymap open", "keymap change 8 1 100017e"}) ||
		len(v.leftover) != 0 || f.key(8)[0] != 0 {
		t.Fatalf("after: %d %s %q", rec.Code, rec.Body, got)
	}
}

// A panic while typing still puts the map back, and lets go of the locks.
func TestTypeMappedPanicRestores(t *testing.T) {
	v, _, f := keymapVisual(t, 8)
	v.run = func(context.Context, string, ...string) ([]byte, error) { panic("boom") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("no panic")
			}
		}()
		serveVisual(v, http.MethodPost, "/input/type", `{"text": "é", "pace": "instant"}`)
	}()
	if f.key(8)[0] != 0 || !v.mu.TryLock() || !v.live.mu.TryLock() {
		t.Fatalf("after a panic: key 8 %x, or a lock still held", f.key(8))
	}
}

// A binding refused for the second stretch fails the call as one that
// has typed the first.
func TestTypeMappedSecondStretchRefused(t *testing.T) {
	v, calls, f := keymapVisual(t, 8)
	v.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, args})
		if args[0] == "key" {
			f.bindErr = errors.New("the X server sent error 11 for request 100")
		}
		return nil, nil
	}
	rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "éž", "pace": "instant"}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "may have been typed") || !strings.Contains(rec.Body.String(), "couldn't bind") {
		t.Fatalf("second stretch: %d %s", rec.Code, rec.Body)
	}
}

func TestTypeRefusesTheReplacementCharacter(t *testing.T) {
	for _, body := range []string{`{"text": "ok\ud800", "pace": "instant"}`, `{"text": "ok�"}`, "{\"text\": \"ok\xff\"}"} {
		v, calls, _ := keymapVisual(t, 8)
		rec := serveVisual(v, http.MethodPost, "/input/type", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "character 3 is U+FFFD") || len(*calls) != 0 {
			t.Errorf("%q: %d %s %v", body, rec.Code, rec.Body, *calls)
		}
	}
}

// Before a binding, the live view's xdotool is ended, so a key the person
// sent has run, not still queued.
func TestTypeMappedDrainsTheLiveView(t *testing.T) {
	v, _, _ := keymapVisual(t, 8)
	x := &closingXdotool{}
	v.live.start = func() (ioWriteCloser, error) { return x, nil }
	if rec := post(v.live.events, `[{"t":"keydown","k":"U00E9"}]`); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "é", "pace": "instant"}`); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if x.closed != 1 || v.live.stdin != nil {
		t.Fatalf("closed %d, stdin %v", x.closed, v.live.stdin)
	}
}

type closingXdotool struct {
	fakeXdotool
	closed int
}

func (c *closingXdotool) Close() error { c.closed++; return nil }

// xdotool's stderr never reaches the log, only how much it wrote, and it
// is an *os.File, as the reaper requires: a fake xdotool here writes a
// typed key's name to it.
func TestWithheldStderr(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"(symbol) No such key name 'U00E9'. Ignoring it.\" >&2\n"
	if err := os.WriteFile(dir+"/xdotool", []byte(script), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	logged := make(chan int64, 1)
	old := withheldLog
	withheldLog = func(_ string, n int64) { logged <- n }
	defer func() { withheldLog = old }()
	if _, err := runCommand(context.Background(), "xdotool", "keydown", "U00E9"); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-logged:
		if n != 48 {
			t.Fatalf("%d bytes", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stderr pipe was never read to its end")
	}
}

// The reaper refuses a command whose stdout or stderr isn't a file: no
// one would wait for os/exec's copying.
func TestReaperRefusesWriters(t *testing.T) {
	cmd := exec.Command("true")
	cmd.Stderr = &strings.Builder{}
	if _, err := procs.start(cmd); err == nil {
		t.Fatal("started with a strings.Builder for stderr")
	}
	cmd = exec.Command("true")
	cmd.Stdout = io.Discard
	if _, err := procs.start(cmd); err == nil {
		t.Fatal("started with io.Discard for stdout")
	}
}

// A bind that changed some keys, then failed, and couldn't put them back
// leaves them for the next type to put back.
func TestTypeMappedHalfFailedBind(t *testing.T) {
	v, calls, f := keymapVisual(t, 8, 248)
	f.bindErr, f.bindsLeft = errors.New("the X server sent error 11 for request 100"), 1
	f.restoreErr = errors.New("the X server sent error 2 for request 100")
	rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "éž", "pace": "instant"}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "before anything was typed") ||
		slices.ContainsFunc(*calls, func(c call) bool { return c.name == "xdotool" }) {
		t.Fatalf("half-failed bind: %d %s", rec.Code, rec.Body)
	}
	if f.key(8)[0] != 0xe9 || len(v.leftover) != 2 {
		t.Fatalf("key 8 %x, leftover %v", f.key(8), v.leftover)
	}
	f.bindErr, f.restoreErr = nil, nil
	if rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "ž", "pace": "instant"}`); rec.Code != http.StatusNoContent || f.key(8)[0] != 0 || f.key(248)[0] != 0 || len(v.leftover) != 0 {
		t.Fatalf("after: %d %s, keys %x %x", rec.Code, rec.Body, f.key(8), f.key(248))
	}
}

// An ASCII type puts back keys an earlier type left bound before typing,
// and fails before typing if it can't, at either pace.
func TestASCIITypeRestoresLeftoverKeys(t *testing.T) {
	for _, pace := range []string{"instant", "human"} {
		v, calls, f := keymapVisual(t, 8)
		f.restoreErr = errors.New("the X server sent error 2 for request 100")
		serveVisual(v, http.MethodPost, "/input/type", `{"text": "é", "pace": "instant"}`)
		*calls = nil
		body := `{"text": "abc", "pace": "` + pace + `"}`
		rec := serveVisual(v, http.MethodPost, "/input/type", body)
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "before anything was typed") ||
			slices.ContainsFunc(*calls, func(c call) bool { return c.name == "xdotool" }) {
			t.Fatalf("%s, leftover stuck: %d %s %v", pace, rec.Code, rec.Body, *calls)
		}
		f.restoreErr = nil
		*calls = nil
		rec = serveVisual(v, http.MethodPost, "/input/type", body)
		if got := events(*calls); rec.Code != http.StatusNoContent || len(got) < 3 || got[0] != "keymap open" || got[1] != "keymap change 8 1 0" || f.key(8)[0] != 0 {
			t.Fatalf("%s: %d %s %q", pace, rec.Code, rec.Body, got)
		}
	}
}

// killedXdotool is a live xdotool that had to be killed with commands
// still queued.
type killedXdotool struct{ fakeXdotool }

func (*killedXdotool) Close() error {
	return errors.New("xdotool was killed with commands still queued")
}

// A live xdotool killed with a release still queued may not have run it:
// before typing, everything held is let go by a new xdotool. One that
// exited cleanly ran its releases, and they're done.
func TestTypeMappedLetsGoOfLiveKeysAfterAKill(t *testing.T) {
	v, calls, _ := keymapVisual(t, 8)
	v.live.start = func() (ioWriteCloser, error) { return &killedXdotool{}, nil }
	if rec := post(v.live.events, `[{"t":"keydown","k":"Control_L"},{"t":"down","b":1},{"t":"wheel","dy":3},{"t":"keyup","k":"Control_L"}]`); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "é", "pace": "instant"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d %s", rec.Code, rec.Body)
	}
	got := events(*calls)
	if len(got) < 2 || got[0] != "xdotool mouseup" || strings.Join((*calls)[0].args, " ") != "mouseup 1 keyup Control_L" || got[1] != "keymap open" {
		t.Fatalf("calls %q, first %v", got, (*calls)[0].args)
	}
	if len(v.live.held) != 0 {
		t.Fatalf("still held: %v", v.live.held)
	}

	// The new xdotool can't run either: nothing is typed.
	v, calls, _ = keymapVisual(t, 8)
	v.live.start = func() (ioWriteCloser, error) { return &killedXdotool{}, nil }
	v.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, args})
		return nil, errors.New("xdotool: exit status 1")
	}
	post(v.live.events, `[{"t":"keydown","k":"Control_L"},{"t":"keyup","k":"Control_L"}]`)
	rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "é", "pace": "instant"}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "before anything was typed") || !strings.Contains(rec.Body.String(), "let go of the live view's keys") {
		t.Fatalf("release failing: %d %s", rec.Code, rec.Body)
	}

	// Exited cleanly: its releases ran, so Control isn't released again;
	// Shift, still down, is let go before the map changes (clean means
	// every key held had its release run).
	v, calls, _ = keymapVisual(t, 8)
	x := &closingXdotool{}
	v.live.start = func() (ioWriteCloser, error) { return x, nil }
	post(v.live.events, `[{"t":"keydown","k":"Control_L"},{"t":"keyup","k":"Control_L"},{"t":"keydown","k":"Shift_L"}]`)
	serveVisual(v, http.MethodPost, "/input/type", `{"text": "é", "pace": "instant"}`)
	if got := events(*calls); got[0] != "xdotool keyup" || strings.Join((*calls)[0].args, " ") != "keyup Shift_L" || got[1] != "keymap open" || len(v.live.held) != 0 {
		t.Fatalf("clean: %q, held %v", got, v.live.held)
	}
}

// An ASCII type that puts leftover keys back does it as typeMapped
// changes the map: with the live view's lock, once its xdotool has run
// what it was sent. With nothing to put back, it drains nothing.
func TestASCIITypeDrainsTheLiveViewOnlyToRestore(t *testing.T) {
	v, _, f := keymapVisual(t, 8)
	x := &closingXdotool{}
	v.live.start = func() (ioWriteCloser, error) { return x, nil }
	post(v.live.events, `[{"t":"keydown","k":"a"},{"t":"keyup","k":"a"}]`)
	serveVisual(v, http.MethodPost, "/input/type", `{"text": "abc", "pace": "instant"}`)
	if x.closed != 0 || v.live.stdin == nil {
		t.Fatalf("an ASCII type with nothing to put back drained the live view (%d)", x.closed)
	}
	v.leftover, v.leftoverPerKey = []keyRun{{first: 8, before: []uint32{0, 0, 0, 0}, after: []uint32{0xe9, 0xe9, 0xe9, 0xe9}}}, fakePerKey
	copy(f.key(8), []uint32{0xe9, 0xe9, 0xe9, 0xe9})
	f.onChange = func() {
		if v.live.stdin != nil {
			t.Error("the map changed with the live view's xdotool running")
		}
		if v.live.mu.TryLock() {
			v.live.mu.Unlock()
			t.Error("the map changed with the live view's lock free")
		}
	}
	if rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "abc", "pace": "instant"}`); rec.Code != http.StatusNoContent || x.closed != 1 || f.key(8)[0] != 0 {
		t.Fatalf("restore: %d %s, closed %d, key 8 %x", rec.Code, rec.Body, x.closed, f.key(8))
	}
}

// Typing characters the keyboard map lacks waits out an agent's shortcut
// gap, as ASCII typing does (PROJ-151): ctrl+l, then "éxample.com". The
// live view's Type button doesn't wait for it, and types such characters
// too (PROJ-195).
func TestKeymapTypingAndTheShortcutGap(t *testing.T) {
	for _, pace := range []string{"instant", "human"} {
		c := newClockedVisual()
		var kcalls []call
		c.keymap = newFakeKeyboard(&kcalls, 8).open
		key := c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+l", "pace": "`+pace+`"}`)
		ended := key + xdotoolTakes
		typed := c.do(t, http.MethodPost, "/input/type", `{"text": "éxample.com", "pace": "`+pace+`"}`)
		if typed < ended+shortcutGap {
			t.Errorf("%s: the text started %v after the key call ended, want at least %v", pace, typed-ended, shortcutGap)
		}
		if len(c.slept) != 1 || c.slept[0] != shortcutGap {
			t.Errorf("%s: slept %v, want one %v", pace, c.slept, shortcutGap)
		}
		if !slices.ContainsFunc(kcalls, func(cl call) bool { return strings.Join(cl.args, " ") == "change 8 1 e9" }) {
			t.Errorf("%s: é wasn't bound: %v", pace, kcalls)
		}
	}
	c := newClockedVisual()
	var kcalls []call
	c.keymap = newFakeKeyboard(&kcalls, 8).open
	c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+l", "pace": "instant"}`)
	n := len(c.calls)
	c.do(t, http.MethodPost, "/input/live-type", `{"text": "héllo"}`)
	if len(c.slept) != 0 {
		t.Fatalf("the person's text waited %v", c.slept)
	}
	if got := strings.Join(c.calls[n].args, " "); got != "key --clearmodifiers --delay 12 -- h U00E9 l l o" {
		t.Fatalf("the person's text typed with %q", got)
	}
	for _, body := range []string{`{"text": "hi\u001b"}`, `{"text": "hi\ud800"}`} {
		if rec := serveVisual(c.visual, http.MethodPost, "/input/live-type", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
}

// A cancelled request (a handoff) breaks off an exchange with a stalled X
// server at once, rather than after keymapTimeout, with the input locks
// held. Here the server takes the setup and never answers.
func TestKeymapConnectionEndsWithItsRequest(t *testing.T) {
	path := t.TempDir() + "/X0"
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _ = io.Copy(io.Discard, c) // reads, never answers
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := dialKeyboardAt(ctx, path); err == nil {
		t.Fatal("a server that never answered set up")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the cancel took %s", d)
	}
}

// The spare-key check is one of the entrypoint's checks.
func TestSpareKeysIsACheck(t *testing.T) {
	if checkMode([]string{"entrypoint", spareKeysArg}) == nil {
		t.Fatal("spare-keys isn't a check")
	}
}

// A type cancelled while it types (the person's control ended, their view
// went) stops: the stretch under way is cut, the map is put back, the
// stretches after it don't run, and the answer says part may have been
// typed.
func TestTypeMappedStopsWhenCancelled(t *testing.T) {
	v, calls, f := keymapVisual(t, 8)
	ctx, cancel := context.WithCancel(context.Background())
	v.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, args})
		if args[0] == "key" {
			cancel()
			return nil, context.Canceled
		}
		return nil, nil
	}
	mux := http.NewServeMux()
	v.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodPost, "/input/live-type", strings.NewReader(`{"text": "éžāū"}`)))
	keys := 0
	for _, c := range *calls {
		if c.name == "xdotool" && c.args[0] == "key" {
			keys++
		}
	}
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "may have been typed") || keys != 1 || f.key(8)[0] != 0 {
		t.Fatalf("cancelled: %d %s, %d key runs, key 8 %x", rec.Code, rec.Body, keys, f.key(8))
	}
}
