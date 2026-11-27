package keyplan

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	for _, ok := range []string{"plain\ttext\r\n", "héllo Rīga 😀", "ĀČĒ ~"} {
		if err := Check(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for text, want := range map[string]string{
		"secret\x1b": "character 7 is a control character",
		"ab\x7f":     "character 3 is a control character",
		"é\u0085":    "character 2 is a control character",
		"ok�":        "character 3 is U+FFFD",
		"ok\xff":     "character 3 is U+FFFD", // invalid UTF-8 ranges as U+FFFD
	} {
		err := Check(text)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: %v, want %q", text, err, want)
		}
	}
}

func TestPlan(t *testing.T) {
	needs := func(r rune) bool { return !OnKeyboard(r) && r != 'à' } // à is on the map
	plan, err := Plan([]rune("héllo à ūdens ū 😀é"), needs, 10)
	if err != nil || len(plan) != 1 || string(plan[0].Bind) != "éū😀" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	// Two spare keys: a new stretch where a third character comes, and a
	// character an earlier stretch bound is bound again.
	plan, _ = Plan([]rune("aébūcéd😀ūe"), needs, 2)
	if len(plan) != 2 || string(plan[0].Text) != "aébūcéd" || string(plan[0].Bind) != "éū" ||
		string(plan[1].Text) != "😀ūe" || string(plan[1].Bind) != "😀ū" {
		t.Fatalf("stretches %+v", plan)
	}
	if _, err := Plan([]rune("é"), needs, 0); !errors.Is(err, ErrNoSpareKeys) {
		t.Fatalf("no spare keys: %v", err)
	}
	if plan, err := Plan([]rune("ASCII"), needs, 0); err != nil || len(plan) != 1 || plan[0].Bind != nil {
		t.Fatalf("ASCII: %+v %v", plan, err)
	}
}

// prose is ordinary Russian, whose letters come back after more than 13
// others again and again.
const prose = "Мой дядя самых честных правил, когда не в шутку занемог, он уважать себя заставил и лучше выдумать не мог. Его пример другим наука; но, боже мой, какая скука с больным сидеть и день и ночь, не отходя ни шагу прочь! "

func TestEstimate(t *testing.T) {
	// 14 letters over and over: every stretch binds 13, so the 14th
	// starts a new one, 153 in all, not the 2 that 14 distinct letters
	// would suggest.
	if d, n := Estimate(strings.Repeat("абвгдежзийклмн", 142), false); n != 153 || d != 1988*InstantKey+153*StretchTime {
		t.Fatalf("repeated: %s, %d stretches", d, n)
	}
	// About 2,000 characters of prose: some 86 stretches, not 3.
	d, n := Estimate(strings.Repeat(prose, 9), false)
	if n < 75 || d < 60*time.Second {
		t.Fatalf("prose: %s, %d stretches", d, n)
	}
	t.Logf("%d characters of prose: %s, %d stretches", len([]rune(strings.Repeat(prose, 9))), d, n)
	if d, n := Estimate("Съешь же ещё этих мягких французских булок, да выпей же чаю", false); n != 4 || d > 3*time.Second {
		t.Fatalf("a Cyrillic sentence: %s, %d stretches", d, n)
	}
	if d, n := Estimate(strings.Repeat("a", 150), true); n != 0 || d != 150*HumanKey {
		t.Fatalf("ASCII at human pace: %s, %d", d, n)
	}
}
