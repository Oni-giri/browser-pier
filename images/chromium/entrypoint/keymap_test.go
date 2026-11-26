package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fakeKeyboard is an X server's keyboard map: keycodes 8 to 255, four
// keysyms each, every one bound but the spare ones. A change copies the
// first two keysyms to the next two, as XKB does. Its opens, reads and
// changes go into the calls log as "keymap" calls, so tests see them in
// order with xdotool's runs.
type fakeKeyboard struct {
	calls     *[]call
	syms      []uint32
	mods      []byte
	openErr   error
	changeErr error // every change fails
	ignore    bool  // changes are accepted and do nothing
	opened    int
	closed    int
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

func (f *fakeKeyboard) open(context.Context) (keyboard, error) {
	f.log("open")
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.opened++
	return f, nil
}

func (f *fakeKeyboard) keycodes() (byte, int) { return 8, 248 }

func (f *fakeKeyboard) mapping(first byte, count int) (int, []uint32, error) {
	i := int(first-8) * fakePerKey
	return fakePerKey, slices.Clone(f.syms[i : i+count*fakePerKey]), nil
}

func (f *fakeKeyboard) modifiers() ([]byte, error) { return f.mods, nil }

func (f *fakeKeyboard) change(first byte, perKey int, syms []uint32) error {
	f.log("change", strconv.Itoa(int(first)), strconv.Itoa(len(syms)/perKey), fmt.Sprintf("%x", syms[0]))
	if f.changeErr != nil {
		return f.changeErr
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
	f := newFakeKeyboard(&calls, 8, 37, 93, 94, 248)
	m, err := readKeyboardMap(f)
	if err != nil {
		t.Fatal(err)
	}
	// 37 has no keysyms, but a modifier's on it.
	if got := m.spare(); !slices.Equal(got, []byte{8, 93, 94, 248}) {
		t.Fatalf("spare %v", got)
	}
	f.key(40)[1] = 0xe9 // a key with é on its Shift level
	m, _ = readKeyboardMap(f)
	if !m.has(0xe9) || m.has(0xea) {
		t.Fatal("has")
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
	f := newFakeKeyboard(&calls, 8, 93, 94, 248)
	m, _ := readKeyboardMap(f)
	before := slices.Clone(f.syms)
	b, err := bind(f, m, m.spare(), []uint32{0xe9, 0x101f600, 0x10000c9})
	if err != nil {
		t.Fatal(err)
	}
	// Both levels, so no Shift; 93 and 94 in one request.
	if !slices.Equal(f.key(8), []uint32{0xe9, 0xe9, 0xe9, 0xe9}) || !slices.Equal(f.key(93)[:2], []uint32{0x101f600, 0x101f600}) ||
		!slices.Equal(f.key(94)[:2], []uint32{0x10000c9, 0x10000c9}) || f.key(248)[0] != 0 {
		t.Fatalf("bound: %x %x %x %x", f.key(8), f.key(93), f.key(94), f.key(248))
	}
	if len(calls) != 2 || strings.Join(calls[1].args, " ") != "change 93 2 101f600" {
		t.Fatalf("changes %v", calls)
	}
	if err := b.restore(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.syms, before) {
		t.Fatal("not restored")
	}

	// A refused change fails, and what was changed is put back.
	f.changeErr = errors.New("X error 2 in request 100")
	if _, err := bind(f, m, m.spare(), []uint32{0xe9}); err == nil || !strings.Contains(err.Error(), "X error 2") {
		t.Fatalf("refused: %v", err)
	}
	f.changeErr = nil
	// A change the server didn't take fails too.
	f.ignore = true
	if _, err := bind(f, m, m.spare(), []uint32{0xe9}); err == nil || !strings.Contains(err.Error(), "didn't take") {
		t.Fatalf("ignored: %v", err)
	}
	if _, err := bind(f, m, []byte{8}, []uint32{0xe9, 0xea}); !errors.Is(err, errNoSpareKeys) {
		t.Fatalf("too few keys: %v", err)
	}
}

// keymapVisual is a test visual whose keyboard map has the given spare
// keys.
func keymapVisual(t *testing.T, spare ...byte) (*visual, *[]call, *fakeKeyboard) {
	t.Helper()
	v, calls := newTestVisual(false)
	f := newFakeKeyboard(calls, spare...)
	v.keyboard = f.open
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
	v, calls, f := keymapVisual(t, 8, 93, 94, 248)
	before := slices.Clone(f.syms)
	body, _ := json.Marshal(map[string]string{"text": "héllo 😀", "pace": "instant"})
	if rec := serveVisual(v, http.MethodPost, "/input/type", string(body)); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d %s", rec.Code, rec.Body)
	}
	want := []string{"keymap open", "keymap change 8 1 e9", "keymap change 93 1 101f600", "xdotool key", "keymap change 8 1 0", "keymap change 93 1 0"}
	if got := events(*calls); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	key := (*calls)[3].args
	if got := strings.Join(key, " "); got != "key --clearmodifiers --delay 12 -- h U00E9 l l o space U1F600" {
		t.Fatalf("key run %q", got)
	}
	if !slices.Equal(f.syms, before) || f.closed != 1 {
		t.Fatalf("map not put back, or not closed (%d)", f.closed)
	}
}

func TestTypeMappedAtHumanPace(t *testing.T) {
	v, calls, f := keymapVisual(t, 8, 93, 94, 248)
	text := "Rīga ūdens, Ārzemes"
	body, _ := json.Marshal(map[string]string{"text": text})
	if rec := serveVisual(v, http.MethodPost, "/input/type", string(body)); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d %s", rec.Code, rec.Body)
	}
	got := events(*calls)
	if !slices.Equal(got[:4], []string{"keymap open", "keymap change 8 1 100012b", "keymap change 93 2 100016b", "xdotool keydown"}) ||
		!slices.Equal(got[len(got)-2:], []string{"keymap change 8 1 0", "keymap change 93 2 0"}) {
		t.Fatalf("calls %q", got)
	}
	if typedText, _ := typed(t, *calls); typedText != text {
		t.Fatalf("typed %q, want %q", typedText, text)
	}
	if f.key(8)[0] != 0 || f.key(93)[0] != 0 || f.key(94)[0] != 0 {
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
	v.keyboard = func(ctx context.Context) (keyboard, error) {
		_, err := f.open(ctx)
		return &watchKeyboard{f, bound}, err
	}
	body, _ := json.Marshal(map[string]string{"text": "aébūcéd😀ūe", "pace": "instant"})
	if rec := serveVisual(v, http.MethodPost, "/input/type", string(body)); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d %s", rec.Code, rec.Body)
	}
	want := []string{"keymap open",
		"keymap change 8 1 e9", "keymap change 248 1 100016b", "xdotool key", "keymap change 8 1 0", "keymap change 248 1 0",
		"keymap change 8 1 101f600", "keymap change 248 1 100016b", "xdotool key", "keymap change 8 1 0", "keymap change 248 1 0"}
	if got := events(*calls); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if strings.Join((*calls)[3].args[5:], " ") != "a U00E9 b U016B c U00E9 d" || strings.Join((*calls)[8].args[5:], " ") != "U1F600 U016B e" {
		t.Fatalf("stretches %q %q", (*calls)[3].args, (*calls)[8].args)
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
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "no spare key") || ran(*calls) {
		t.Fatalf("no spare key: %d %s %v", rec.Code, rec.Body, *calls)
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
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "couldn't bind 1 characters") || ran(*calls) {
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
	if rec.Code != http.StatusInternalServerError || !slices.Equal(got[len(got)-3:], []string{"xdotool key", "xdotool keyup", "keymap change 8 1 0"}) || f.key(8)[0] != 0 {
		t.Fatalf("xdotool failing: %d %s %q", rec.Code, rec.Body, got)
	}
	if strings.Join((*calls)[len(*calls)-2].args, " ") != "keyup a b c space U00E9" {
		t.Fatalf("release %q", (*calls)[len(*calls)-2].args)
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

func TestXKeyboardSpeaksX(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	x := &xKeyboard{conn: client}
	s := xServer{t, server}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if got := s.read(12); !slices.Equal(got, []byte{'l', 0, 11, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
			t.Errorf("setup %v", got)
		}
		body := make([]byte, 32)
		body[26], body[27] = 8, 255
		s.write(append([]byte{1, 0, 11, 0, 0, 0, 8, 0}, body...))

		// GetKeyboardMapping, answered after an event (MappingNotify).
		if got := s.read(8); !slices.Equal(got, []byte{101, 0, 2, 0, 9, 2, 0, 0}) {
			t.Errorf("GetKeyboardMapping %v", got)
		}
		s.write(append([]byte{34}, make([]byte, 31)...))
		reply := make([]byte, 32, 48)
		reply[0], reply[1] = 1, 2
		xOrder.PutUint32(reply[4:], 4)
		for _, sym := range []uint32{0x31, 0x21, 0xe9, 0} {
			reply = xOrder.AppendUint32(reply, sym)
		}
		s.write(reply)

		// ChangeKeyboardMapping, then GetModifierMapping answered with
		// the error the change caused first.
		if got := s.read(16); !slices.Equal(got, []byte{100, 1, 4, 0, 8, 2, 0, 0, 0x01, 0xf6, 0x01, 0x01, 0x01, 0xf6, 0x01, 0x01}) {
			t.Errorf("ChangeKeyboardMapping %x", got)
		}
		if got := s.read(4); !slices.Equal(got, []byte{119, 0, 1, 0}) {
			t.Errorf("GetModifierMapping %v", got)
		}
		e := make([]byte, 32)
		e[1], e[10] = 2, 100
		s.write(e)

		// GetModifierMapping: two keycodes a modifier.
		s.read(4)
		reply = make([]byte, 32, 48)
		reply[0], reply[1] = 1, 2
		xOrder.PutUint32(reply[4:], 4)
		s.write(append(reply, 50, 62, 66, 0, 37, 105, 0, 0, 64, 108, 77, 0, 0, 0, 92, 0))
	}()
	if err := x.setup(); err != nil {
		t.Fatal(err)
	}
	if first, n := x.keycodes(); first != 8 || n != 248 {
		t.Fatalf("keycodes %d, %d", first, n)
	}
	perKey, syms, err := x.mapping(9, 2)
	if err != nil || perKey != 2 || !slices.Equal(syms, []uint32{0x31, 0x21, 0xe9, 0}) {
		t.Fatalf("mapping: %d %x %v", perKey, syms, err)
	}
	if err := x.change(8, 2, []uint32{0x101f601, 0x101f601}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.modifiers(); err == nil || err.Error() != "the X server sent error 2 for request 100" {
		t.Fatalf("the change's error: %v", err)
	}
	mods, err := x.modifiers()
	if err != nil || !slices.Equal(mods, []byte{50, 62, 66, 37, 105, 64, 108, 77, 92}) {
		t.Fatalf("modifiers %v %v", mods, err)
	}
	<-done
	if err := x.change(8, 2, []uint32{1, 2, 3}); err == nil {
		t.Fatal("a ragged change was sent")
	}
	if _, _, err := x.mapping(250, 10); err == nil {
		t.Fatal("keycodes past 255 asked for")
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
