//go:build race

package durable

// raceEnabled reports a -race build; latency acceptance thresholds are
// meaningless under the detector's ~10x slowdown and are skipped.
const raceEnabled = true
