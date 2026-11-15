//go:build !race

package scrub

// raceSlowdown allows for the race detector; it isn't on.
const raceSlowdown = 1
