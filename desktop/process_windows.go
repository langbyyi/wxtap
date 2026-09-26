//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"unsafe"
)

const createNoWindow = 0x08000000

const (
	messageBoxOK        = 0x00000000
	messageBoxIconError = 0x00000010
)

var messageBoxW = syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")

// showFatalMessage puts a message in front of the user when there is no window
// to show it in. A -H windowsgui binary has no console, so println() output goes
// nowhere and a startup failure looks like the app simply not launching.
//
// The result is deliberately ignored: every caller is already on a failure path,
// so a dialog that cannot open must not become the error being reported.
func showFatalMessage(title, message string) {
	titleUTF16, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	messageUTF16, err := syscall.UTF16PtrFromString(message)
	if err != nil {
		return
	}
	_, _, _ = messageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(messageUTF16)),
		uintptr(unsafe.Pointer(titleUTF16)),
		uintptr(messageBoxOK|messageBoxIconError),
	)
}

// configureBackgroundProcess hides a console helper such as Node or reg.
// HideWindow also hides a GUI window, so it must not be used for Electron or a browser.
func configureBackgroundProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}

// configureGUIProcess stops Windows from allocating a console for the child
// without hiding the window the user is supposed to see.
func configureGUIProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}
