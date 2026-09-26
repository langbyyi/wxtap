//go:build race

package traffic

// raceEnabled reports whether this binary was built with the Go race
// detector. Wall-clock regression budgets are meaningless under it, so the
// perf tests opt out instead of reporting false regressions.
const raceEnabled = true
