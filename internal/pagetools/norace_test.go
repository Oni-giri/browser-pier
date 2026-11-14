//go:build !race

package pagetools

// raceSlowdown allows for the race detector; it isn't on.
const raceSlowdown = 1
