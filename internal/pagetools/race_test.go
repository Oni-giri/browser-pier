//go:build race

package pagetools

// raceSlowdown allows for the race detector, which slows these loops
// about tenfold.
const raceSlowdown = 10
