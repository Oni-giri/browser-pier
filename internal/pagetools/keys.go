package pagetools

import (
	"fmt"
	"strings"
)

// keyDef is how CDP's Input.dispatchKeyEvent names a key of a US
// keyboard: its DOM key and code, its Windows virtual key code, and the
// text it types (if any).
type keyDef struct {
	key, code string
	vk        int
	text      string
	shift     bool // typed with shift held
}

// Modifier bits, as Input.dispatchKeyEvent's modifiers take them.
const (
	modAlt   = 1
	modCtrl  = 2
	modMeta  = 4
	modShift = 8
)

var modifierKeys = map[string]struct {
	def keyDef
	bit int
}{
	"ctrl":    {keyDef{key: "Control", code: "ControlLeft", vk: 17}, modCtrl},
	"control": {keyDef{key: "Control", code: "ControlLeft", vk: 17}, modCtrl},
	"alt":     {keyDef{key: "Alt", code: "AltLeft", vk: 18}, modAlt},
	"option":  {keyDef{key: "Alt", code: "AltLeft", vk: 18}, modAlt},
	"shift":   {keyDef{key: "Shift", code: "ShiftLeft", vk: 16}, modShift},
	"meta":    {keyDef{key: "Meta", code: "MetaLeft", vk: 91}, modMeta},
	"cmd":     {keyDef{key: "Meta", code: "MetaLeft", vk: 91}, modMeta},
	"command": {keyDef{key: "Meta", code: "MetaLeft", vk: 91}, modMeta},
	"super":   {keyDef{key: "Meta", code: "MetaLeft", vk: 91}, modMeta},
	"win":     {keyDef{key: "Meta", code: "MetaLeft", vk: 91}, modMeta},
}

// namedKeys are the keys press takes by name: the subset of Visual
// mode's press_keys keysyms (X keysym names, with the same aliases) that
// CDP can send. Names are matched without case.
var namedKeys = map[string]keyDef{
	"return": {key: "Enter", code: "Enter", vk: 13, text: "\r"}, "enter": {key: "Enter", code: "Enter", vk: 13, text: "\r"},
	"tab":    {key: "Tab", code: "Tab", vk: 9},
	"escape": {key: "Escape", code: "Escape", vk: 27}, "esc": {key: "Escape", code: "Escape", vk: 27},
	"backspace": {key: "Backspace", code: "Backspace", vk: 8},
	"delete":    {key: "Delete", code: "Delete", vk: 46}, "del": {key: "Delete", code: "Delete", vk: 46},
	"insert": {key: "Insert", code: "Insert", vk: 45},
	"home":   {key: "Home", code: "Home", vk: 36}, "end": {key: "End", code: "End", vk: 35},
	"page_up": {key: "PageUp", code: "PageUp", vk: 33}, "pageup": {key: "PageUp", code: "PageUp", vk: 33}, "prior": {key: "PageUp", code: "PageUp", vk: 33}, "pgup": {key: "PageUp", code: "PageUp", vk: 33},
	"page_down": {key: "PageDown", code: "PageDown", vk: 34}, "pagedown": {key: "PageDown", code: "PageDown", vk: 34}, "next": {key: "PageDown", code: "PageDown", vk: 34}, "pgdn": {key: "PageDown", code: "PageDown", vk: 34},
	"up": {key: "ArrowUp", code: "ArrowUp", vk: 38}, "arrowup": {key: "ArrowUp", code: "ArrowUp", vk: 38},
	"down": {key: "ArrowDown", code: "ArrowDown", vk: 40}, "arrowdown": {key: "ArrowDown", code: "ArrowDown", vk: 40},
	"left": {key: "ArrowLeft", code: "ArrowLeft", vk: 37}, "arrowleft": {key: "ArrowLeft", code: "ArrowLeft", vk: 37},
	"right": {key: "ArrowRight", code: "ArrowRight", vk: 39}, "arrowright": {key: "ArrowRight", code: "ArrowRight", vk: 39},
	"space": {key: " ", code: "Space", vk: 32, text: " "},
}

func init() {
	for i := 1; i <= 12; i++ {
		namedKeys[fmt.Sprintf("f%d", i)] = keyDef{key: fmt.Sprintf("F%d", i), code: fmt.Sprintf("F%d", i), vk: 111 + i}
	}
}

// usKeys are the characters of a US keyboard, by the character typed.
var usKeys = func() map[rune]keyDef {
	m := map[rune]keyDef{}
	for c := 'a'; c <= 'z'; c++ {
		up := c - 'a' + 'A'
		m[c] = keyDef{key: string(c), code: "Key" + string(up), vk: int(up), text: string(c)}
		m[up] = keyDef{key: string(up), code: "Key" + string(up), vk: int(up), text: string(up), shift: true}
	}
	shifted := ")!@#$%^&*("
	for d := '0'; d <= '9'; d++ {
		m[d] = keyDef{key: string(d), code: "Digit" + string(d), vk: int(d), text: string(d)}
		s := rune(shifted[d-'0'])
		m[s] = keyDef{key: string(s), code: "Digit" + string(d), vk: int(d), text: string(s), shift: true}
	}
	for _, p := range []struct {
		plain, shift rune
		code         string
		vk           int
	}{
		{'-', '_', "Minus", 189}, {'=', '+', "Equal", 187}, {'[', '{', "BracketLeft", 219}, {']', '}', "BracketRight", 221},
		{'\\', '|', "Backslash", 220}, {';', ':', "Semicolon", 186}, {'\'', '"', "Quote", 222}, {',', '<', "Comma", 188},
		{'.', '>', "Period", 190}, {'/', '?', "Slash", 191}, {'`', '~', "Backquote", 192},
	} {
		m[p.plain] = keyDef{key: string(p.plain), code: p.code, vk: p.vk, text: string(p.plain)}
		m[p.shift] = keyDef{key: string(p.shift), code: p.code, vk: p.vk, text: string(p.shift), shift: true}
	}
	m[' '] = namedKeys["space"]
	m['\n'] = namedKeys["enter"]
	m['\t'] = namedKeys["tab"]
	return m
}()

// combo is one key press with its modifiers.
type combo struct {
	mods []keyDef
	bits int
	key  keyDef
	// name is how the activity log may show it; "" for one that types a
	// character, which could be part of a password.
	name string
}

// parseKeys reads press's keys: combinations like "ctrl+a" or "Enter",
// several separated by spaces, at most 10.
func parseKeys(s string) ([]combo, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 || len(fields) > 10 {
		return nil, fmt.Errorf("keys: want 1 to 10 keys or combinations, like \"Enter\" or \"ctrl+a Tab\"")
	}
	var out []combo
	for _, f := range fields {
		parts := strings.Split(f, "+")
		var c combo
		for i, p := range parts {
			last := i == len(parts)-1
			if p == "" && last && i > 0 { // "ctrl++"
				p = "+"
			}
			lower := strings.ToLower(p)
			if m, ok := modifierKeys[lower]; ok && !last {
				c.mods = append(c.mods, m.def)
				c.bits |= m.bit
				continue
			}
			if !last {
				return nil, fmt.Errorf("keys: %q isn't a modifier (ctrl, alt, shift, meta)", p)
			}
			if m, ok := modifierKeys[lower]; ok {
				c.key = m.def // a modifier pressed alone
			} else if k, ok := namedKeys[lower]; ok {
				c.key = k
			} else if r := []rune(p); len(r) == 1 {
				k, ok := usKeys[r[0]]
				if !ok {
					return nil, fmt.Errorf("keys: %q has no key on a US keyboard; use type for text", p)
				}
				c.key = k
			} else {
				return nil, fmt.Errorf("keys: unknown key %q", p)
			}
		}
		// A combination that types a character could be a password's.
		if c.key.text == "" || c.bits&(modCtrl|modAlt|modMeta) != 0 || c.key.key == "Enter" || c.key.key == " " {
			c.name = f
		}
		if c.key.shift {
			c.bits |= modShift
		}
		out = append(out, c)
	}
	return out, nil
}

// describeKeys names the keys for the activity log, or only counts them
// if any types a character.
func describeKeys(cs []combo) string {
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		if c.name == "" {
			if len(cs) == 1 {
				return "Pressed 1 key"
			}
			return fmt.Sprintf("Pressed %d keys", len(cs))
		}
		names = append(names, c.name)
	}
	return "Pressed " + strings.Join(names, ", ")
}
