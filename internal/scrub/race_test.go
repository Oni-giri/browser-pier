//go:build race

package scrub

// raceSlowdown scales the timing test's limit under the race detector,
// which instruments every table read of the scan (about 4 times slower
// here; CI runners are about twice as slow as a workstation).
const raceSlowdown = 3
