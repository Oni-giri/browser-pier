// Package keyplan is how Visual-mode typing handles characters the
// keyboard map lacks (PROJ-195), shared by the browser's entrypoint,
// which types them, and the orchestrator, which checks text before a call
// takes its turn and estimates how long typing it takes.
//
// The display's US map has keys for printable ASCII, Return and Tab.
// Every other character is bound to a spare key before it is typed, as
// many at a time as there are spare keys; text with more distinct such
// characters is typed in stretches, each bound in turn.
package keyplan

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// OnKeyboard reports whether the US keyboard map has a key for r:
// printable ASCII, \n and \r (Return) and \t (Tab).
func OnKeyboard(r rune) bool {
	return r >= 0x20 && r < 0x7f || r == '\n' || r == '\r' || r == '\t'
}

// Check refuses text with a character no key types: a control character
// other than \n, \r and \t, which xdotool would leave out, or U+FFFD,
// which stands for input that didn't decode (an unpaired surrogate escape
// in JSON, say), not for a character anyone meant. The error names the
// character's position, never the text.
func Check(text string) error {
	i := 0
	for _, r := range text {
		i++
		switch {
		case r == utf8.RuneError:
			return fmt.Errorf("text: character %d is U+FFFD, which stands for invalid input; it isn't typed", i)
		case OnKeyboard(r):
		case r < 0x20 || r >= 0x7f && r < 0xa0:
			return fmt.Errorf("text: character %d is a control character, which no key types; only \\n and \\r (Return) and \\t (Tab) are typed", i)
		}
	}
	return nil
}

// Batch is a stretch of text and the characters to bind before typing it.
type Batch struct {
	Text []rune
	Bind []rune
}

// ErrNoSpareKeys is Plan's error when text needs a binding and there is
// no spare key.
var ErrNoSpareKeys = errors.New("the keyboard map has no spare key to bind a character to")

// Plan splits text into stretches that each need at most spare keys
// bound, and lists those characters for each, in the order they come.
// needs reports a character that must be bound. A new stretch starts
// where a character comes that would be one too many; it binds afresh, so
// a character an earlier stretch bound is bound again if it comes back.
func Plan(text []rune, needs func(rune) bool, spare int) ([]Batch, error) {
	var out []Batch
	var cur Batch
	bound := map[rune]bool{}
	for _, r := range text {
		if needs(r) && !bound[r] {
			if spare < 1 {
				return nil, ErrNoSpareKeys
			}
			if len(cur.Bind) == spare {
				out, cur, bound = append(out, cur), Batch{}, map[rune]bool{}
			}
			cur.Bind = append(cur.Bind, r)
			bound[r] = true
		}
		cur.Text = append(cur.Text, r)
	}
	if len(cur.Text) > 0 {
		out = append(out, cur)
	}
	return out, nil
}

// The time model of a type call, measured on the browser image: 12 ms a
// key at instant pace, about 0.25 s at human pace, and about 0.55 s for
// each stretch on top of its keys (two 150 ms waits for Chromium, binding
// and putting back the map, xdotool's start). SpareKeys is how many spare
// keys the image's layout data leaves (13; another X server's could leave
// fewer, and its stretches would be shorter and more).
const (
	SpareKeys   = 13
	InstantKey  = 12 * time.Millisecond
	HumanKey    = 250 * time.Millisecond
	StretchTime = 550 * time.Millisecond
)

// Estimate is how long typing text takes, by Plan with SpareKeys spare
// keys, and how many stretches that takes.
func Estimate(text string, human bool) (time.Duration, int) {
	runes := []rune(text)
	batches, _ := Plan(runes, func(r rune) bool { return !OnKeyboard(r) }, SpareKeys)
	stretches := 0
	for _, b := range batches {
		if len(b.Bind) > 0 {
			stretches++
		}
	}
	per := InstantKey
	if human {
		per = HumanKey
	}
	return time.Duration(len(runes))*per + time.Duration(stretches)*StretchTime, stretches
}
