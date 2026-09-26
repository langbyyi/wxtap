//go:build windows

package main

import (
	"context"
	"testing"
)

// The version probe is the one child process that is easy to forget: it runs
// once per candidate, so an unhidden console means a black window flashing for
// each of them on every engine start, every Settings open, and every
// "auto-detect". The other spawns all go through these helpers already.
//
// The console half of the assertion is Windows-only: syscall.SysProcAttr has no
// HideWindow or CreationFlags on other platforms, so a runtime.GOOS guard would
// not compile there — the file itself has to be the guard.
func TestNodeVersionProbeRunsWithoutAConsoleWindow(t *testing.T) {
	cmd := nodeVersionCommand(context.Background(), fakeNode(t, "22.0.0"))
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("probe would allocate a console window: %+v", cmd.SysProcAttr)
	}
}
