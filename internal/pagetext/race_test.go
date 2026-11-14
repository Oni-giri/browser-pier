//go:build race

package pagetext

const raceEnabled = true

// raceSlowdown allows for the race detector, which slows rendering about
// tenfold (readability's class matching more), and more again on a slow
// CI runner. A super-linear blow-up still takes far longer.
const raceSlowdown = 10
