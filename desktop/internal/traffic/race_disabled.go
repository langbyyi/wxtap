//go:build !race

package traffic

// raceEnabled is false for ordinary builds (see race_enabled.go).
const raceEnabled = false
