package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// writeFile creates a file with its parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pinEmptyPath keeps the host's own Electron out of the test: resolution step 2
// reads PATH, and a developer machine that has one would otherwise decide the
// outcome.
func pinEmptyPath(t *testing.T) string {
	t.Helper()
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	return empty
}

// writePathShim puts the shim `npm install -g electron` leaves on PATH into dir
// and returns the path resolution must arrive at. On Windows the shim is
// electron.cmd, which starts cmd.exe — the black console window — so it is
// mapped onto the electron.exe beside it; elsewhere the shim is the executable
// itself, so there is nothing to map.
func writePathShim(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		writeFile(t, filepath.Join(dir, "electron.cmd"), "@echo off")
		binary := filepath.Join(dir, "node_modules", "electron", "dist", "electron.exe")
		writeFile(t, binary, "binary")
		return binary
	}
	// exec.LookPath takes a file for an executable on these platforms only when
	// it is marked executable, which is how npm leaves the shim.
	shim := filepath.Join(dir, "electron")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return shim
}

func TestResolveElectronRuntimePrefersTheSavedPath(t *testing.T) {
	pinEmptyPath(t)
	dataDir := t.TempDir()
	t.Setenv("WXTAP_DATA_DIR", dataDir)
	binary := filepath.Join(t.TempDir(), "electron.exe")
	writeFile(t, binary, "binary")
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"electron_path":`+jsonString(binary)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved := resolveElectronRuntime()
	if resolved.Path != binary || resolved.Source != electronSourceConfig || resolved.Error != "" {
		t.Fatalf("saved path must win: %+v", resolved)
	}
}

func TestResolveElectronRuntimeReportsABrokenSavedPath(t *testing.T) {
	pinEmptyPath(t)
	dataDir := t.TempDir()
	t.Setenv("WXTAP_DATA_DIR", dataDir)
	// A working Electron sits on PATH, but the operator asked for this one: the
	// report has to be about theirs, not a silent swap to the PATH install.
	prefix := t.TempDir()
	writePathShim(t, prefix)
	t.Setenv("PATH", prefix)
	missing := filepath.Join(t.TempDir(), "nope.exe")
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"electron_path":`+jsonString(missing)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved := resolveElectronRuntime()
	if resolved.Error == "" || resolved.Source != electronSourceConfig {
		t.Fatalf("a saved-but-broken path must be reported, not replaced: %+v", resolved)
	}
}

func TestResolveElectronRuntimeMapsTheNpmGlobalShimFromPath(t *testing.T) {
	prefix := t.TempDir()
	t.Setenv("PATH", prefix)
	binary := writePathShim(t, prefix)
	t.Setenv("WXTAP_DATA_DIR", t.TempDir())

	resolved := resolveElectronRuntime()
	if resolved.Path != binary || resolved.Source != electronSourcePath || resolved.Error != "" {
		t.Fatalf("the npm shim must resolve to the Electron it runs: %+v", resolved)
	}
}

func TestResolveElectronRuntimeFallsBackToWellKnownLocations(t *testing.T) {
	pinEmptyPath(t)
	t.Setenv("WXTAP_DATA_DIR", t.TempDir())
	// Which locations exist is this host's business, so detection is pinned
	// here; its per-platform lists are checked on their own below.
	binary := filepath.Join(t.TempDir(), "electron")
	writeFile(t, binary, "binary")
	pinElectronDetection(t, []string{binary})

	resolved := resolveElectronRuntime()
	if resolved.Path != binary || resolved.Source != electronSourceDetected || resolved.Error != "" {
		t.Fatalf("a well-known location must be found without PATH: %+v", resolved)
	}
}

func TestResolveElectronRuntimeExplainsAMissingInstall(t *testing.T) {
	pinEmptyPath(t)
	t.Setenv("WXTAP_DATA_DIR", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	// The POSIX locations are absolute paths (/Applications/Electron.app,
	// /usr/bin/electron) that no environment variable can empty, so a host with
	// Electron installed would otherwise resolve it and this case would be
	// untestable there.
	pinElectronDetection(t, nil)

	resolved := resolveElectronRuntime()
	if resolved.Path != "" || resolved.Error == "" {
		t.Fatalf("nothing to find must produce the missing-install message: %+v", resolved)
	}
	if resolved.Error != electronMissingMessage() {
		t.Fatalf("error = %q, want the shared wording", resolved.Error)
	}
}

func TestUsableElectronAcceptsABundleAndRejectsADirectoryWithoutOne(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "Electron.app")
	writeFile(t, filepath.Join(bundle, "Contents", "MacOS", "Electron"), "binary")
	if got, err := usableElectron(bundle); err != nil || got != bundle {
		t.Fatalf("a bundle must be usable as-is: %q %v", got, err)
	}

	empty := t.TempDir()
	if _, err := usableElectron(empty); err == nil {
		t.Fatal("a directory holding no Electron binary must be rejected")
	}
}

func TestUsableElectronRejectsACommandShimWithNoBinaryBesideIt(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "electron.cmd")
	writeFile(t, shim, "@echo off")
	if _, err := usableElectron(shim); err == nil {
		t.Fatal("a .cmd shim with no electron.exe beside it must be rejected")
	}
}

func TestProbeElectronCandidatesListsPathFirstThenWellKnown(t *testing.T) {
	prefix := t.TempDir()
	t.Setenv("PATH", prefix)
	fromPath := writePathShim(t, prefix)
	detected := filepath.Join(t.TempDir(), "electron")
	writeFile(t, detected, "binary")
	pinElectronDetection(t, []string{detected})
	t.Setenv("WXTAP_DATA_DIR", t.TempDir())

	candidates := probeElectronCandidates()
	if len(candidates) != 2 {
		t.Fatalf("want the PATH install then the well-known one, got %+v", candidates)
	}
	if candidates[0].Path != fromPath || candidates[0].Source != electronSourcePath {
		t.Fatalf("PATH must come first: %+v", candidates[0])
	}
	if candidates[1].Path != detected || candidates[1].Source != electronSourceDetected {
		t.Fatalf("the install location must come second: %+v", candidates[1])
	}
}

// The well-known lists are data, so taking the platform as a parameter means the
// Windows and macOS ones are driven on every host rather than only on the one
// that can run them.
func TestDetectElectronCandidatesFollowsThePlatform(t *testing.T) {
	appData := t.TempDir()
	home := t.TempDir()
	t.Setenv("APPDATA", appData)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("HOME", home)

	npmGlobal := filepath.Join(appData, "npm", "node_modules", "electron", "dist", "electron.exe")
	writeFile(t, npmGlobal, "binary")
	bundle := filepath.Join(home, "Applications", "Electron.app")
	writeFile(t, filepath.Join(bundle, "Contents", "MacOS", "Electron"), "binary")

	// Both Windows locations come from the environment and both are pinned, so
	// this list is exact wherever the test runs.
	if found := detectElectronCandidates("windows"); !slices.Equal(found, []string{npmGlobal}) {
		t.Fatalf(`detectElectronCandidates("windows") = %v, want just %q`, found, npmGlobal)
	}
	// The macOS list also holds absolute paths (/Applications, /usr/local) that a
	// test cannot empty, so membership is what is checkable there: a Mac with
	// Electron installed legitimately finds more than the bundle written above.
	if found := detectElectronCandidates("darwin"); !slices.Contains(found, bundle) {
		t.Fatalf(`detectElectronCandidates("darwin") = %v, want %q among them`, found, bundle)
	}
	// The unix list is absolute paths for the same reason, in reverse: the check
	// that holds everywhere is that no other platform's prefix bleeds into it.
	for _, candidate := range detectElectronCandidates("linux") {
		if slices.Contains([]string{npmGlobal, bundle}, candidate) {
			t.Fatalf(`detectElectronCandidates("linux") must not hold %q`, candidate)
		}
	}
}

// jsonString quotes a path for the config.json fixtures above.
func jsonString(path string) string {
	quoted := make([]byte, 0, len(path)+2)
	quoted = append(quoted, '"')
	for index := 0; index < len(path); index++ {
		if path[index] == '\\' {
			quoted = append(quoted, '\\', '\\')
			continue
		}
		quoted = append(quoted, path[index])
	}
	return string(append(quoted, '"'))
}
