package browsertest

import (
	"slices"
	"testing"
)

func TestSortHits(t *testing.T) {
	hits := []Hit{{Kind: "print", Where: "main"}, {Kind: "console-slow", Where: "worker"}, {Kind: "console-stack", Where: "worker"}, {Kind: "print", Where: "worker"}}
	other, slow := sortHits(hits, []string{"print@main"})
	if len(slow) != 1 || slow[0].Where != "worker" {
		t.Errorf("slow: %v", slow)
	}
	if kinds(other)[0] != "console-stack@worker" || kinds(other)[1] != "print@worker" || len(other) != 2 {
		t.Errorf("other: %v", other)
	}
}

func TestSlowNotIn(t *testing.T) {
	m, w := Hit{Kind: "console-slow", Where: "main"}, Hit{Kind: "console-slow", Where: "worker"}
	for _, c := range []struct {
		batch, control []Hit
		want           []string
	}{
		{[]Hit{w}, []Hit{w}, nil},
		{[]Hit{w, w}, []Hit{w, m}, nil},
		{[]Hit{m, w}, []Hit{w}, []string{"main"}},
		{[]Hit{m, w, m}, nil, []string{"main", "worker"}},
		{[]Hit{w}, []Hit{m}, []string{"worker"}},
	} {
		if got := slowNotIn(c.batch, c.control); !slices.Equal(got, c.want) {
			t.Errorf("%v against %v: %v, want %v", c.batch, c.control, got, c.want)
		}
	}
}

func TestSlowness(t *testing.T) {
	got := slowness([]Hit{{Kind: "console-slow", Where: "worker", Detail: "7.5 ms vs 0.5 ms at load"},
		{Kind: "console-slow", Where: "main", Detail: "8.4 ms vs 1.6 ms at load"},
		{Kind: "console-slow", Where: "worker", Detail: "12.0 ms vs 0.5 ms at load"}})
	if want := "console-slow@main 1× (worst 8.4 ms vs 1.6 ms at load), console-slow@worker 2× (worst 12.0 ms vs 0.5 ms at load)"; got != want {
		t.Errorf("%s\nwant %s", got, want)
	}
}
