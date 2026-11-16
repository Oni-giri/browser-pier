// Package motion makes an agent's pointer and keyboard move like a hand:
// a Fitts's-law travel time, a bowed path whose speed peaks early, a
// slight wobble, a corrective second leg on long throws, aim scattered
// around a target's middle, and the pauses of a person's clicking and
// typing (PROJ-113). It is pure maths, with no I/O: Visual mode's
// entrypoint turns its output into xdotool commands, and the API-mode
// page tools into CDP input events.
//
// None of it is fingerprint spoofing: it changes how input arrives, not
// what the browser says it is.
//
// This is a Go port of Motion.swift from agweb
// (Sources/AgwebCore/Motion.swift), with its constants kept:
//
//	Copyright (c) 2026 Paul Nameless
//	Licensed under the MIT License. Permission is hereby granted, free of
//	charge, to any person obtaining a copy of this software and associated
//	documentation files (the "Software"), to deal in the Software without
//	restriction, including without limitation the rights to use, copy,
//	modify, merge, publish, distribute, sublicense, and/or sell copies of
//	the Software, and to permit persons to whom the Software is furnished
//	to do so, subject to the following conditions: The above copyright
//	notice and this permission notice shall be included in all copies or
//	substantial portions of the Software. THE SOFTWARE IS PROVIDED "AS
//	IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED.
//
// See NOTICE.
package motion

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"strings"
	"time"
)

// Point is a position in pixels.
type Point struct{ X, Y float64 }

// Size is a target's or a viewport's size in pixels.
type Size struct{ W, H float64 }

// Step is one point of a pointer path and the pause that follows it.
type Step struct {
	X, Y  float64
	Pause time.Duration
}

// Tick is one wheel tick of a scroll and the pause that follows it.
type Tick struct {
	Delta float64
	Pause time.Duration
}

// tick is how far apart a path's points are: about 60 Hz, a mouse's
// usual report rate as a page sees it.
const tick = time.Second / 60

// Motion is a source of human-looking input. It is not safe for
// concurrent use; keep one per session (never one shared by every
// session, or two sessions' timings would be correlated).
type Motion struct {
	state uint64
}

// New returns a Motion with a fixed seed, which repeats the same paths
// and delays; tests use it.
func New(seed uint64) *Motion { return &Motion{state: seed} }

// NewRandom returns a Motion seeded from crypto/rand.
func NewRandom() *Motion {
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand panics instead)
	return New(binary.LittleEndian.Uint64(b[:]))
}

// Float64 is uniform in [0, 1), by SplitMix64.
func (m *Motion) Float64() float64 {
	m.state += 0x9E3779B97F4A7C15
	z := m.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return float64((z^(z>>31))>>11) / float64(uint64(1)<<53)
}

// Jitter is uniform in [base-spread, base+spread).
func (m *Motion) Jitter(base, spread float64) float64 {
	return base + (m.Float64()-0.5)*2*spread
}

// JitterDuration is Jitter in seconds, as a duration.
func (m *Motion) JitterDuration(base, spread float64) time.Duration {
	return seconds(m.Jitter(base, spread))
}

// Gaussian is a standard normal, by Box-Muller.
func (m *Motion) Gaussian() float64 {
	return math.Sqrt(-2*math.Log(math.Max(m.Float64(), 1e-12))) * math.Cos(2*math.Pi*m.Float64())
}

// LogNormal has this median: mostly near it, now and then much longer, as
// human timings are.
func (m *Motion) LogNormal(median, sigma float64) float64 {
	return median * math.Exp(sigma*m.Gaussian())
}

// Aim is where a person lands on a target: normally scattered around its
// middle, with a standard deviation of a sixth of its size on each axis
// (Meyer et al. 1988), and never more than 40% of the size from the
// middle, so always inside it.
func (m *Motion) Aim(center Point, size Size) Point {
	axis := func(side float64) float64 {
		return math.Min(math.Max(side/6*m.Gaussian(), -side*0.4), side*0.4)
	}
	return Point{center.X + axis(size.W), center.Y + axis(size.H)}
}

// Path is the pointer's way from one point to another: points about
// 60 Hz apart, each with the pause that follows it. The last point is
// exactly to. targetSize (pixels) is the target's smaller side, for
// Fitts's law.
//
// The travel time follows Fitts's law, 0.115 + 0.135·log2(2d/size + 1)
// seconds, clamped to 0.2–1.15 s (MacKenzie 1992's Shannon form). The
// harder the throw, the likelier it misses and a quick corrective leg
// follows (Meyer et al. 1988's optimised submovements); most such throws
// fall short, some overshoot.
func (m *Motion) Path(from, to Point, targetSize float64) []Step {
	dist := math.Hypot(to.X-from.X, to.Y-from.Y)
	if dist < 0.5 {
		return []Step{{to.X, to.Y, 0}}
	}
	index := math.Log2(2*dist/math.Min(math.Max(targetSize, 14), 320) + 1)
	secs := math.Min(math.Max(0.115+0.135*index, 0.2), 1.15)
	if m.Float64() >= math.Min(math.Max((index-2.5)/3, 0), 0.9) {
		return m.leg(from, to, secs)
	}
	f := 0.88 + 0.07*m.Float64() // short
	if m.Float64() < 0.3 {
		f = 1.03 + 0.05*m.Float64() // over
	}
	turn := Point{from.X + (to.X-from.X)*f + m.Jitter(0, dist*0.012), from.Y + (to.Y-from.Y)*f + m.Jitter(0, dist*0.012)}
	throw := m.leg(from, turn, secs*0.8)
	throw[len(throw)-1].Pause = m.JitterDuration(0.038, 0.014)
	return append(throw, m.leg(turn, to, math.Min(math.Max(secs*0.3, 0.11), 0.24))...)
}

// leg is one movement along a bowed cubic Bézier, on minimum-jerk timing.
func (m *Motion) leg(a, b Point, secs float64) []Step {
	dx, dy := b.X-a.X, b.Y-a.Y
	dist := math.Hypot(dx, dy)
	if dist < 0.5 {
		return []Step{{b.X, b.Y, 0}}
	}
	bow := dist * 0.055 * (0.55 + 0.9*m.Float64())
	if m.Float64() < 0.5 {
		bow = -bow
	}
	nx, ny := -dy/dist*bow, dx/dist*bow
	p1 := Point{a.X + dx*0.33 + nx, a.Y + dy*0.33 + ny}
	p2 := Point{a.X + dx*0.66 + nx*0.55, a.Y + dy*0.66 + ny*0.55}
	count := max(1, int(math.Round(secs*60)))
	// A wobble of about a pixel, smooth and gone at both ends, so the leg
	// still lands exactly: bot checks flag mathematically smooth curves.
	wx, wy := 0.5+m.Float64(), 0.5+m.Float64()
	fx, fy := 1.5+1.5*m.Float64(), 1.5+1.5*m.Float64()
	px, py := 2*math.Pi*m.Float64(), 2*math.Pi*m.Float64()
	steps := make([]Step, count)
	for i := 1; i <= count; i++ {
		if i == count {
			steps[i-1] = Step{b.X, b.Y, tick}
			break
		}
		u := float64(i) / float64(count)
		// Minimum jerk (Flash & Hogan 1985) on warped time: the speed
		// peaks at 40% of the way, not halfway, and the hand slows into
		// the target, as aimed movements do.
		w := math.Pow(u, 0.756)
		e := w * w * w * (10 + w*(-15+6*w))
		r := 1 - e
		c0, c1, c2, c3 := r*r*r, 3*r*r*e, 3*r*e*e, e*e*e
		fade := math.Sin(math.Pi * u)
		steps[i-1] = Step{
			X:     c0*a.X + c1*p1.X + c2*p2.X + c3*b.X + wx*fade*math.Sin(2*math.Pi*fx*u+px),
			Y:     c0*a.Y + c1*p1.Y + c2*p2.Y + c3*b.Y + wy*fade*math.Sin(2*math.Pi*fy*u+py),
			Pause: tick,
		}
	}
	return steps
}

// KeyDelay is the time from pressing char to pressing next (0 if not
// known), as measured in 136M keystrokes (Dhakal et al., CHI 2018):
// log-normal around 0.2 s with a long tail, longer after spaces and
// punctuation, a little shorter when the hands alternate, and now and
// then (5%) a hesitation. It is between 0.06 and 2 s.
func (m *Motion) KeyDelay(char, next rune) time.Duration {
	delay := m.LogNormal(0.2, 0.42)
	switch {
	case char == ' ':
		delay += m.Jitter(0.06, 0.03)
	case strings.ContainsRune(".,@!?:;_-/", char):
		delay += m.Jitter(0.14, 0.06)
	}
	if next != 0 {
		if a, ok := hand(char); ok {
			if b, ok := hand(next); ok {
				if a == b {
					delay += 0.01
				} else {
					delay -= 0.015
				}
			}
		}
	}
	if m.Float64() < 0.05 {
		delay += m.LogNormal(0.4, 0.35)
	}
	return seconds(math.Min(math.Max(delay, 0.06), 2))
}

// Dwell is the pause between the pointer stopping and the button going
// down: usually short (median 0.16 s), now and then a long look; between
// 0.04 and 1 s.
func (m *Motion) Dwell() time.Duration {
	return seconds(math.Min(math.Max(m.LogNormal(0.16, 0.6), 0.04), 1))
}

// KeyHold is how long a key or button stays down: about 116 ms (SD
// 24 ms), between 60 and 200 ms (Dhakal et al. 2018).
func (m *Motion) KeyHold() time.Duration {
	return seconds(math.Min(math.Max(0.116+0.024*m.Gaussian(), 0.06), 0.2))
}

// Entry is where a pointer first appears: just inside the bottom, right
// or left edge, as if the hand came in from outside the window.
func (m *Motion) Entry(viewport Size) Point {
	along := 0.15 + 0.7*m.Float64()
	switch int(m.Float64() * 3) {
	case 0:
		return Point{viewport.W * along, viewport.H - 2}
	case 1:
		return Point{viewport.W - 2, viewport.H * along}
	default:
		return Point{2, viewport.H * along}
	}
}

// WheelTicks splits a wheel scroll of total pixels into ticks of about
// 60 px that speed up and slow down, each with the pause after it. The
// deltas add up to total.
func (m *Motion) WheelTicks(total float64) []Tick {
	count := max(1, int(math.Ceil(math.Abs(total)/60)))
	weights := make([]float64, count)
	sum := 0.0
	for i := range weights {
		weights[i] = math.Sin(math.Pi*(float64(i)+0.5)/float64(count)) + 0.3
		sum += weights[i]
	}
	ticks := make([]Tick, count)
	for i, w := range weights {
		ticks[i] = Tick{total * w / sum, m.JitterDuration(0.03, 0.012)}
	}
	return ticks
}

// hand is 0 for a key of the left hand, 1 for the right, on a QWERTY
// keyboard.
func hand(char rune) (int, bool) {
	c := []rune(strings.ToLower(string(char)))[0]
	switch {
	case strings.ContainsRune("qwertasdfgzxcvb12345`~!@#$%", c):
		return 0, true
	case strings.ContainsRune("yuiophjklnm67890-=[];',./\\^&*()_+{}:\"<>?|", c):
		return 1, true
	}
	return 0, false
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
