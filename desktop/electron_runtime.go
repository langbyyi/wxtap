// Electron runtime resolution. WxTap does not ship an Electron either, so the
// DevTools window needs the user's own install — from the path saved in
// Settings, from whatever `electron` resolves to on PATH, or from a well-known
// install location. node_runtime.go is the model: everything that decides
// *which* Electron opens the window lives here, so the Settings page and the
// launch path cannot disagree about it.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/langbyyi/wxtap/desktop/internal/api/ipc"
)

// Where a resolved Electron came from. The Settings page renders the label, so
// an automatically chosen Electron is never a silent substitution.
const (
	electronSourceConfig   = "config"   // chosen in Settings
	electronSourcePath     = "path"     // the user's PATH
	electronSourceDetected = "detected" // a well-known install location
)

// configElectronPathKey is the config.json key the Settings page writes.
const configElectronPathKey = "electron_path"

// ElectronRuntime is the resolved Electron plus how it was found. The launch
// path refuses to open a window when Error is set, and the Settings page shows
// the same string, so the two surfaces never disagree about what is wrong.
type ElectronRuntime struct {
	Path   string `json:"path,omitempty"`
	Source string `json:"source,omitempty"`
	Error  string `json:"error,omitempty"`
	// Skipped lists candidates that were present but unusable. An Electron taken
	// from a later source is thus explained rather than silently swapped in.
	Skipped []string `json:"skipped,omitempty"`
}

// usableElectron turns one candidate into something electronLaunch can run, or
// explains why it cannot. There is no version floor to enforce the way Node has
// one, so existence and shape are the whole check — a broken install still
// reports itself when the window fails to open.
func usableElectron(candidate string) (string, error) {
	resolved, err := resolveElectronPath(candidate)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%s 不存在", resolved)
	}
	if !info.IsDir() {
		return resolved, nil
	}
	// A directory is either a macOS bundle or a dist directory holding the
	// binary; electronLaunch does the platform-specific mapping itself, so the
	// directory is the right thing to hand back.
	for _, inner := range []string{
		filepath.Join(resolved, "Contents", "MacOS", "Electron"),
		filepath.Join(resolved, "Electron"),
		filepath.Join(resolved, "electron.exe"),
	} {
		if innerInfo, err := os.Stat(inner); err == nil && !innerInfo.IsDir() {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("%s 不是可运行的 Electron：请指向 Electron.app、其 Contents/MacOS 目录或可执行文件", resolved)
}

// resolveElectronRuntime picks the Electron that opens the DevTools window, in
// this order:
//
//  1. the path saved in Settings — the operator asked for that one, so its
//     problem is reported rather than falling through to another install.
//  2. whatever `electron` resolves to on PATH, which is what the user's own
//     shell would run. On Windows that is npm's .cmd shim, so it is mapped to
//     the real binary before use.
//  3. well-known install locations, for a machine where `npm i -g electron`
//     never put a shim on PATH (the usual reason a fresh install "isn't seen").
//
// At steps 2 and 3 a present-but-unusable candidate does continue to the next
// one, because there the goal is simply to find a working Electron; the
// substitution is recorded in Skipped.
func resolveElectronRuntime() ElectronRuntime {
	var skipped []string
	consider := func(candidate, source string) (ElectronRuntime, bool) {
		if strings.TrimSpace(candidate) == "" {
			return ElectronRuntime{}, false
		}
		resolved, err := usableElectron(candidate)
		if err != nil {
			skipped = append(skipped, err.Error())
			return ElectronRuntime{Path: candidate, Source: source, Error: err.Error(), Skipped: skipped}, false
		}
		return ElectronRuntime{Path: resolved, Source: source, Skipped: skipped}, true
	}

	if configured := configElectronPath(); configured != "" {
		probed, _ := consider(configured, electronSourceConfig)
		probed.Skipped = skipped
		return probed
	}
	if fromPath, err := exec.LookPath("electron"); err == nil {
		if probed, ok := consider(fromPath, electronSourcePath); ok {
			return probed
		}
	}
	for _, candidate := range electronRuntimeCandidates() {
		if probed, ok := consider(candidate, electronSourceDetected); ok {
			return probed
		}
	}
	return ElectronRuntime{Skipped: skipped, Error: electronMissingMessage()}
}

// probeElectronCandidates lists every usable Electron this machine has, in the
// same shape node.detect answers with: the PATH shim first (that is the one the
// user's own shell would run), then the well-known install locations, each
// resolved to a binary and de-duplicated by it.
func probeElectronCandidates() []ElectronRuntime {
	candidates := []ElectronRuntime{}
	seen := map[string]bool{}
	consider := func(candidate, source string) {
		resolved, err := usableElectron(candidate)
		if err != nil {
			return
		}
		key := strings.ToLower(resolved)
		if seen[key] {
			return
		}
		seen[key] = true
		candidates = append(candidates, ElectronRuntime{Path: resolved, Source: source})
	}
	if fromPath, err := exec.LookPath("electron"); err == nil {
		consider(fromPath, electronSourcePath)
	}
	for _, candidate := range electronRuntimeCandidates() {
		consider(candidate, electronSourceDetected)
	}
	return candidates
}

// electronMissingMessage is the one wording for "there is no usable Electron",
// used by the Settings panel and by the launch failure alike.
func electronMissingMessage() string {
	return "未找到可用的 Electron：WxTap 不自带 Electron，需要系统中已安装（npm install -g electron），" +
		"或在「设置 → Electron 路径」里指定可执行文件路径"
}

// configElectronPath returns the Electron the operator picked in Settings, or "".
func configElectronPath() string {
	base, err := userBaseDir()
	if err != nil {
		return ""
	}
	config, err := ipc.NewConfigStore(base).Load()
	if err != nil {
		return ""
	}
	value, _ := config[configElectronPathKey].(string)
	return strings.TrimSpace(value)
}

// electronRuntimeCandidates returns the well-known install locations detection
// probes. It is a variable so tests can pin the list: the real answer depends on
// where this host installed Electron, which no test can control. It must not be
// reassigned from a parallel test.
var electronRuntimeCandidates = func() []string { return detectElectronCandidates(runtime.GOOS) }

// detectElectronCandidates lists the well-known locations for goos that
// actually hold a file. The per-user entries are derived from the environment
// rather than hardcoded, so a relocated install and a test that pins those
// variables both work. goos is a parameter rather than read from runtime.GOOS
// here so that a test run on any one host still covers every platform's list —
// the list is data, the way layoutFor's is in internal/update.
func detectElectronCandidates(goos string) []string {
	var candidates []string
	add := func(parts ...string) {
		for _, part := range parts {
			if part == "" {
				return
			}
		}
		candidates = append(candidates, filepath.Join(parts...))
	}

	switch goos {
	case "windows":
		// npm's default global prefix. A relocated prefix is covered by the PATH
		// step above, whose shim names the prefix it lives in.
		add(os.Getenv("APPDATA"), "npm", "node_modules", "electron", "dist", "electron.exe")
		add(os.Getenv("LOCALAPPDATA"), "Programs", "electron", "electron.exe")
	case "darwin":
		add("/Applications", "Electron.app")
		add(os.Getenv("HOME"), "Applications", "Electron.app")
		add("/usr/local/lib", "node_modules", "electron", "dist", "Electron.app")
		// Apple-Silicon Homebrew's prefix, the counterpart of Node's
		// /opt/homebrew/bin/node: /usr/local is Intel Homebrew only, so without
		// this a `brew install electron` on an M-series Mac is invisible.
		add("/opt/homebrew/lib", "node_modules", "electron", "dist", "Electron.app")
		add(os.Getenv("HOME"), ".npm-global", "lib", "node_modules", "electron", "dist", "Electron.app")
	default:
		add("/usr/bin/electron")
		add("/usr/local/bin/electron")
		add("/opt/Electron/electron")
	}

	found := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		key := strings.ToLower(candidate)
		if seen[key] {
			continue
		}
		seen[key] = true
		// Unlike Node's list this keeps directories too: on macOS the install is
		// an .app bundle, and usableElectron decides what inside it counts.
		if _, err := os.Stat(candidate); err == nil {
			found = append(found, candidate)
		}
	}
	return found
}
