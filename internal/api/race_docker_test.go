//go:build docker && race

package api

// raceSlowdown scales the Docker tests' time limits under the race
// detector, which slows the orchestrator's side of a read (decoding the
// DOM snapshot, rendering it). CI runs these tests only under it, on
// runners about twice as slow as a workstation, where reading the stress
// pages took up to 1.6 times the no-race limit (the hidden chain of
// 190,000 elements: 16 s against 10 s). Tripled, the limits leave room
// for that; a super-linear blow-up would still take minutes.
const raceSlowdown = 3
