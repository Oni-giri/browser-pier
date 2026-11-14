//go:build !race

package pagetext

const raceEnabled = false

// raceSlowdown allows for the race detector; it isn't on.
const raceSlowdown = 1
