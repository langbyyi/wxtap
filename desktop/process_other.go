//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

func configureBackgroundProcess(*exec.Cmd) {}

func configureGUIProcess(*exec.Cmd) {}

// showFatalMessage puts a message in front of the user when there is no window
// to show it in. A .app launched from Finder has no terminal attached, so
// println() output goes nowhere and a startup failure looks like the app simply
// not launching. osascript is macOS's own scripting interface, so this needs no
// new dependency; Linux is not a product platform and gets nothing.
//
// Errors are deliberately ignored: every caller is already on a failure path,
// so a dialog that cannot open must not become the error being reported.
func showFatalMessage(title, message string) {
	if runtime.GOOS != "darwin" {
		return
	}
	script := "display dialog " + appleScriptLiteral(message) +
		" with title " + appleScriptLiteral(title) +
		` buttons {"好"} default button 1 with icon stop`
	_ = exec.Command("osascript", "-e", script).Run()
}
