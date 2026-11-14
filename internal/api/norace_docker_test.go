//go:build docker && !race

package api

// raceSlowdown allows for the race detector; it isn't on.
const raceSlowdown = 1
