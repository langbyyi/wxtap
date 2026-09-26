//go:build windows

package main

import (
	"os/exec"
	"testing"
)

func TestConfigureBackgroundProcessHidesConsoleWindow(t *testing.T) {
	cmd := exec.Command("node", "core.js")
	configureBackgroundProcess(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Fatal("background Core process must hide its console window")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("CREATE_NO_WINDOW missing: %#x", cmd.SysProcAttr.CreationFlags)
	}

	visible := exec.Command("electron", "devtools.js")
	configureGUIProcess(visible)
	if visible.SysProcAttr == nil || visible.SysProcAttr.HideWindow {
		t.Fatal("a DevTools window must not be hidden")
	}
	if visible.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatal("a DevTools window must still suppress the console")
	}
}
