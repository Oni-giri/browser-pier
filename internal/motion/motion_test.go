package motion

import (
	"math"
	"slices"
	"testing"
	"time"
)

func total(path []Step) time.Duration {
	var d time.Duration
	for _, s := range path {
		d += s.Pause
	}
	return d
}

// corner is the index of a path's turning point (the step before a
// corrective leg, whose pause isn't a tick), or -1.
func corner(path []Step) int {
	for i, s := range path[:len(path)-1] {
		if s.Pause != tick {
			return i
		}
	}
	return -1
}

func TestPathEndsExactlyOnTargetWithinItsBox(t *testing.T) {
	for seed := range uint64(2000) {
		m := New(seed)
		for _, d := range []float64{0.2, 1, 12, 120, 259, 600, 1400, 4000} {
			// An arbitrary direction per seed.
			angle := float64(seed) * 0.7
			from := Point{100, 300}
			to := Point{from.X + d*math.Cos(angle), from.Y + d*math.Sin(angle)}
			path := m.Path(from, to, 44)
			if last := path[len(path)-1]; last.X != to.X || last.Y != to.Y {
				t.Fatalf("seed %d, %v px: ends at (%v, %v), want %v", seed, d, last.X, last.Y, to)
			}
			// The bounding box of from and to, plus 10% of the distance
			// (and the wobble's 1.5 px, which matters only on short moves).
			margin := 0.1*d + 1.5
			for _, s := range path {
				if s.X < math.Min(from.X, to.X)-margin || s.X > math.Max(from.X, to.X)+margin ||
					s.Y < math.Min(from.Y, to.Y)-margin || s.Y > math.Max(from.Y, to.Y)+margin {
					t.Fatalf("seed %d, %v px: step (%v, %v) strays from %v→%v", seed, d, s.X, s.Y, from, to)
				}
				if s.Pause < 0 {
					t.Fatalf("negative pause %v", s.Pause)
				}
			}
		}
	}
}

func TestPathTakesFittsTimeAt60Hz(t *testing.T) {
	for seed := range uint64(2000) {
		m := New(seed)
		for _, d := range []float64{1, 50, 200, 400, 1000, 2500, 4000} {
			path := m.Path(Point{0, 0}, Point{d, d / 3}, 44)
			c := corner(path)
			for i, s := range path {
				if i != c && s.Pause != tick {
					t.Fatalf("seed %d, %v px: step %d pauses %v, want a 60 Hz tick", seed, d, i, s.Pause)
				}
			}
			got := total(path)
			lo, hi := 200*time.Millisecond, 1150*time.Millisecond
			if c >= 0 {
				// The throw takes 80% of the time, then a pause and the
				// corrective leg (up to 0.24 s).
				lo, hi = 0, hi+240*time.Millisecond
			}
			if got < lo-tick || got > hi+tick {
				t.Fatalf("seed %d, %v px: %v, want %v–%v", seed, d, got, lo, hi)
			}
		}
	}
}

func TestLongThrowsMostlyFallShort(t *testing.T) {
	short, corrected := 0, 0
	for seed := range uint64(10000) {
		to := Point{1000, 0}
		path := New(seed).Path(Point{0, 0}, to, 20)
		c := corner(path)
		if c < 0 {
			continue
		}
		corrected++
		if path[c].X < to.X {
			short++
		}
	}
	if corrected < 5000 {
		t.Fatalf("only %d of 10,000 hard throws corrected", corrected)
	}
	if frac := float64(short) / float64(corrected); frac < 0.6 || frac > 0.8 {
		t.Fatalf("%.2f of corrections fall short, want most (about 0.7)", frac)
	}
}

// The hand is fastest early and slows into the target: by half the time,
// about two thirds of the way is done.
func TestSpeedPeaksBeforeHalfway(t *testing.T) {
	for seed := range uint64(200) {
		path := New(seed).Path(Point{0, 0}, Point{200, 0}, 40)
		if corner(path) >= 0 {
			continue
		}
		if half := path[len(path)/2]; half.X < 0.55*200 {
			t.Fatalf("seed %d: at half the time only %v of 200 px done", seed, half.X)
		}
	}
}

func TestPathsAreNotStraightLines(t *testing.T) {
	m := New(8)
	path := m.Path(Point{0, 0}, Point{400, 0}, 44)
	dev := 0.0
	for _, s := range path {
		dev = math.Max(dev, math.Abs(s.Y))
	}
	if dev < 2 || dev > 40 {
		t.Fatalf("largest deviation from the straight line %v px, want a few pixels to a few tens", dev)
	}
}

func TestAimStaysInsideAndScatters(t *testing.T) {
	m := New(6)
	const n, side = 10000, 120.0
	var sx, sxx, sy, syy float64
	for range n {
		p := m.Aim(Point{0, 0}, Size{side, side})
		if math.Abs(p.X) > side*0.4 || math.Abs(p.Y) > side*0.4 {
			t.Fatalf("aimed at %v, beyond 40%% of %v", p, side)
		}
		sx, sxx, sy, syy = sx+p.X, sxx+p.X*p.X, sy+p.Y, syy+p.Y*p.Y
	}
	for _, sd := range []float64{math.Sqrt(sxx/n - (sx/n)*(sx/n)), math.Sqrt(syy/n - (sy/n)*(sy/n))} {
		if want := side / 6; math.Abs(sd-want) > want*0.1 {
			t.Fatalf("SD %v, want within 10%% of %v", sd, want)
		}
	}
	// Tiny targets too.
	for range 1000 {
		if p := m.Aim(Point{50, 50}, Size{1, 1}); math.Abs(p.X-50) > 0.4 || math.Abs(p.Y-50) > 0.4 {
			t.Fatalf("aimed at %v for a 1 px target", p)
		}
	}
}

func TestKeyDelayIsSkewedLikeAPersons(t *testing.T) {
	m := New(5)
	const n = 10000
	gaps := make([]time.Duration, n)
	var afterA, afterSpace, afterComma time.Duration
	for i := range gaps {
		gaps[i] = m.KeyDelay('a', 0)
		afterA += gaps[i]
		afterSpace += m.KeyDelay(' ', 0)
		afterComma += m.KeyDelay(',', 0)
	}
	for _, g := range gaps {
		if g < 60*time.Millisecond || g > 2*time.Second {
			t.Fatalf("gap %v outside 0.06–2 s", g)
		}
	}
	slices.Sort(gaps)
	median := gaps[n/2]
	if median < 180*time.Millisecond || median > 240*time.Millisecond {
		t.Fatalf("median gap %v, want 0.18–0.24 s", median)
	}
	if mean := afterA / n; mean < median+15*time.Millisecond {
		t.Fatalf("mean %v not above the median %v: no long tail", mean, median)
	}
	if afterSpace <= afterA || afterComma <= afterSpace {
		t.Fatalf("mean after 'a' %v, ' ' %v, ',' %v: want longer after spaces and longer still after punctuation", afterA/n, afterSpace/n, afterComma/n)
	}
}

func TestAlternatingHandsTypeFaster(t *testing.T) {
	a, b := New(9), New(9)
	var alternating, same time.Duration
	for range 500 {
		alternating += a.KeyDelay('a', 'k')
		same += b.KeyDelay('a', 's')
	}
	if alternating >= same {
		t.Fatalf("alternating %v, same hand %v", alternating, same)
	}
}

func TestKeyHold(t *testing.T) {
	m := New(2)
	var sum time.Duration
	for range 2000 {
		h := m.KeyHold()
		if h < 60*time.Millisecond || h > 200*time.Millisecond {
			t.Fatalf("hold %v outside 0.06–0.2 s", h)
		}
		sum += h
	}
	if mean := sum / 2000; mean < 111*time.Millisecond || mean > 121*time.Millisecond {
		t.Fatalf("mean hold %v, want about 116 ms", mean)
	}
}

func TestDwell(t *testing.T) {
	m := New(12)
	quick, long := 0, 0
	for range 2000 {
		d := m.Dwell()
		if d < 40*time.Millisecond || d > time.Second {
			t.Fatalf("dwell %v outside 0.04–1 s", d)
		}
		if d < 100*time.Millisecond {
			quick++
		}
		if d > 350*time.Millisecond {
			long++
		}
	}
	if quick < 100 || long < 50 {
		t.Fatalf("%d quick and %d long dwells of 2000: want both", quick, long)
	}
}

func TestWheelTicksAddUpAndRamp(t *testing.T) {
	m := New(14)
	for _, want := range []float64{600, -600, 61, 1, 2000} {
		ticks := m.WheelTicks(want)
		sum := 0.0
		for _, tk := range ticks {
			sum += tk.Delta
			if tk.Pause < 18*time.Millisecond || tk.Pause > 42*time.Millisecond {
				t.Fatalf("pause %v", tk.Pause)
			}
		}
		if math.Abs(sum-want) > 1e-9 {
			t.Fatalf("ticks of %v add up to %v", want, sum)
		}
		if n := len(ticks); n >= 3 {
			mid := math.Abs(ticks[n/2].Delta)
			if mid <= math.Abs(ticks[0].Delta) || mid <= math.Abs(ticks[n-1].Delta) {
				t.Fatalf("%v: middle tick %v not larger than the ends %v, %v", want, mid, ticks[0].Delta, ticks[n-1].Delta)
			}
		}
	}
}

func TestEntryIsAtAnEdge(t *testing.T) {
	m := New(13)
	for range 200 {
		p := m.Entry(Size{800, 600})
		if p.X > 2 && p.X < 798 && p.Y < 598 {
			t.Fatalf("entered at %v, not at an edge", p)
		}
	}
}

func TestFloat64StaysInUnitInterval(t *testing.T) {
	m := New(1)
	for range 10000 {
		if f := m.Float64(); f < 0 || f >= 1 {
			t.Fatal(f)
		}
	}
}

func TestSeeds(t *testing.T) {
	path := func(seed uint64) []Step { return New(seed).Path(Point{0, 0}, Point{300, 80}, 30) }
	if !slices.Equal(path(42), path(42)) {
		t.Fatal("the same seed gave different paths")
	}
	if slices.Equal(path(42), path(43)) {
		t.Fatal("different seeds gave the same path")
	}
	if a, b := NewRandom(), NewRandom(); a.state == b.state {
		t.Fatal("two random sources share a seed")
	}
}
