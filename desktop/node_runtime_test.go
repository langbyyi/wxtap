package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeNode writes a stand-in that reports version on stdout, so the Node gate
// can be tested without depending on whatever Node the host happens to have.
func fakeNode(t *testing.T, version string) string {
	t.Helper()
	name := "node"
	if runtime.GOOS == "windows" {
		name = "node.cmd"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, fakeNodeBody(t, version), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeNodeBody is fakeNode's script body, for the tests that need the stand-in
// at a specific path (a PATH entry, an install location).
func fakeNodeBody(t *testing.T, version string) []byte {
	t.Helper()
	if runtime.GOOS == "windows" {
		return []byte("@echo off\r\necho " + version + "\r\n")
	}
	return []byte("#!/bin/sh\necho " + version + "\n")
}

// isolateFromInstalledNode removes every way resolveNodeRuntime could find a
// Node, so "nothing usable is installed" is testable on a machine that has Node.
func isolateFromInstalledNode(t *testing.T) {
	t.Helper()
	empty := t.TempDir()
	t.Setenv("WXTAP_CORE_CMD", "")
	t.Setenv("WXTAP_DATA_DIR", empty) // no saved choice
	t.Setenv("PATH", empty)
	// The host's real install locations are outside the test's control, so the
	// detection seam is pinned rather than trying to hide them one by one.
	pinNodeDetection(t, nil)
}

// pinNodeDetection replaces the well-known-location list for one test.
func pinNodeDetection(t *testing.T, candidates []string) {
	t.Helper()
	original := nodeRuntimeCandidates
	nodeRuntimeCandidates = func() []string { return candidates }
	t.Cleanup(func() { nodeRuntimeCandidates = original })
}

// writeConfig puts a config.json under a fresh data dir and points the shell at
// it, which is how the Settings page's saved Node path reaches resolution.
func writeConfig(t *testing.T, config map[string]any) {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WXTAP_DATA_DIR", dir)
}

func TestNodeMajorVersion(t *testing.T) {
	for input, want := range map[string]int{
		"v24.19.0":   24,
		"20.11.1":    20,
		"22":         22,
		" v18.0.0\n": 18,
	} {
		got, err := nodeMajorVersion(input)
		if err != nil || got != want {
			t.Errorf("nodeMajorVersion(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "v", "abc", "node"} {
		if _, err := nodeMajorVersion(input); err == nil {
			t.Errorf("nodeMajorVersion(%q) should fail", input)
		}
	}
}

func TestNodeVersionOf(t *testing.T) {
	if version, err := nodeVersionOf(fakeNode(t, "24.19.0")); err != nil || version != "24.19.0" {
		t.Fatalf("version = %q, err = %v", version, err)
	}
	// The floor itself must pass: it is the number the build scripts enforce.
	if _, err := nodeVersionOf(fakeNode(t, "22.0.0")); err != nil {
		t.Fatalf("the floor itself should pass: %v", err)
	}
	if _, err := nodeVersionOf(fakeNode(t, "16.20.2")); err == nil || !strings.Contains(err.Error(), "版本过低") {
		t.Fatalf("an unsupported Node should be refused with a version complaint: %v", err)
	}
	broken := filepath.Join(t.TempDir(), "node.exe")
	if err := os.WriteFile(broken, []byte("not an executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeVersionOf(broken); err == nil || !strings.Contains(err.Error(), "无法运行") {
		t.Fatalf("an unusable binary should be reported as such: %v", err)
	}
}

// The probe's argument shape is the same everywhere; whether it also suppresses
// a console window is Windows-only and lives in node_runtime_windows_test.go.
func TestNodeVersionProbeArguments(t *testing.T) {
	cmd := nodeVersionCommand(context.Background(), fakeNode(t, "22.0.0"))
	if got := strings.Join(cmd.Args[1:], " "); got != "-p process.versions.node" {
		t.Fatalf("probe arguments = %q, want %q", got, "-p process.versions.node")
	}
}

func TestResolveNodeRuntimePrefersTheExplicitOverride(t *testing.T) {
	override := fakeNode(t, "24.19.0")
	t.Setenv("WXTAP_CORE_CMD", override)
	pinNodeDetection(t, []string{fakeNode(t, "23.0.0")})

	node := resolveNodeRuntime()
	if node.Error != "" || node.Source != nodeSourceEnv || node.Path != override {
		t.Fatalf("override should win outright: %+v", node)
	}
	if node.Version != "24.19.0" {
		t.Fatalf("version: %q", node.Version)
	}
}

// A pinned override that does not work must be reported, not quietly replaced by
// some other Node the operator did not ask for.
func TestResolveNodeRuntimeReportsABrokenOverride(t *testing.T) {
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")
	pinNodeDetection(t, []string{fakeNode(t, "24.19.0")})

	node := resolveNodeRuntime()
	if node.Error == "" || node.Source != nodeSourceEnv {
		t.Fatalf("a broken override must fail as itself: %+v", node)
	}
	if node.Version != "" {
		t.Fatalf("a failed probe must not report a version: %+v", node)
	}
}

func TestResolveNodeRuntimeUsesTheConfiguredPath(t *testing.T) {
	configured := fakeNode(t, "24.19.0")
	t.Setenv("WXTAP_CORE_CMD", "")
	writeConfig(t, map[string]any{configNodePathKey: configured})

	node := resolveNodeRuntime()
	if node.Error != "" || node.Source != nodeSourceConfig || node.Path != configured {
		t.Fatalf("configured path should win over PATH: %+v", node)
	}
}

func TestResolveNodeRuntimeFallsBackToPath(t *testing.T) {
	binDir := t.TempDir()
	name := "node"
	if runtime.GOOS == "windows" {
		name = "node.cmd"
	}
	onPath := filepath.Join(binDir, name)
	if err := os.WriteFile(onPath, []byte(fakeNodeBody(t, "24.19.0")), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WXTAP_CORE_CMD", "")
	writeConfig(t, map[string]any{})
	t.Setenv("PATH", binDir)
	pinNodeDetection(t, nil)

	node := resolveNodeRuntime()
	if node.Error != "" || node.Source != nodeSourcePath {
		t.Fatalf("PATH node should be used: %+v", node)
	}
	if !strings.EqualFold(node.Path, onPath) {
		t.Fatalf("path = %q, want %q", node.Path, onPath)
	}
}

func TestResolveNodeRuntimeDetectsAKnownInstall(t *testing.T) {
	detected := fakeNode(t, "24.19.0")
	t.Setenv("WXTAP_CORE_CMD", "")
	writeConfig(t, map[string]any{})
	t.Setenv("PATH", t.TempDir())
	pinNodeDetection(t, []string{detected})

	node := resolveNodeRuntime()
	if node.Error != "" || node.Source != nodeSourceDetected || node.Path != detected {
		t.Fatalf("a detected Node should be used: %+v", node)
	}
}

// A Node that exists but is unusable is recorded, so picking a different one is
// visible in the settings panel instead of silent.
func TestResolveNodeRuntimeRecordsWhatItSkipped(t *testing.T) {
	old := fakeNode(t, "16.20.2")
	usable := fakeNode(t, "24.19.0")
	t.Setenv("WXTAP_CORE_CMD", "")
	writeConfig(t, map[string]any{})
	t.Setenv("PATH", t.TempDir())
	pinNodeDetection(t, []string{old, usable})

	node := resolveNodeRuntime()
	if node.Error != "" || node.Path != usable {
		t.Fatalf("the usable Node should win: %+v", node)
	}
	if len(node.Skipped) != 1 || !strings.Contains(node.Skipped[0], "版本过低") {
		t.Fatalf("the skipped Node should be reported: %+v", node.Skipped)
	}
}

// These two strings reach a mac user on first run, where Node is not bundled
// and the missing-Node advice is the only guidance they get: naming node.exe or
// telling them to re-log into Windows reads as instructions for another machine.
func TestNodeBinaryNameMatchesThePlatform(t *testing.T) {
	want := "node"
	if runtime.GOOS == "windows" {
		want = "node.exe"
	}
	if got := nodeBinaryName(); got != want {
		t.Fatalf("nodeBinaryName() = %q, want %q", got, want)
	}
}

func TestNodeMissingMessageNamesNoPlatform(t *testing.T) {
	if message := nodeMissingMessage(); strings.Contains(message, "Windows") {
		t.Fatalf("the missing-Node advice must not name an operating system: %q", message)
	}
}

func TestResolveNodeRuntimeExplainsWhenNothingIsInstalled(t *testing.T) {
	isolateFromInstalledNode(t)

	node := resolveNodeRuntime()
	if node.Error == "" || node.Path != "" || node.Source != "" {
		t.Fatalf("no Node should be an error with nothing resolved: %+v", node)
	}
	for _, want := range []string{"nodejs.org", "设置 → Node 运行时"} {
		if !strings.Contains(node.Error, want) {
			t.Fatalf("error should mention %q: %v", want, node.Error)
		}
	}
}

func TestDetectNodeCandidatesReturnsExistingFilesOnly(t *testing.T) {
	if runtime.GOOS != "windows" {
		// The unix locations are a fixed list of absolute paths with no seam to
		// pin, so this one is only meaningful where the list is env-derived.
		t.Skip("detection locations are env-derived on Windows only")
	}
	root := t.TempDir()
	binDir := filepath.Join(root, "nodejs")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	present := filepath.Join(binDir, "node.exe")
	if err := os.WriteFile(present, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ProgramFiles", root)

	found := detectNodeCandidates()
	if len(found) == 0 || !strings.EqualFold(found[0], present) {
		t.Fatalf("detectNodeCandidates = %v, want %q first", found, present)
	}
	// A directory named node.exe must not be offered as an interpreter.
	if err := os.Remove(present); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(present, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range detectNodeCandidates() {
		if strings.EqualFold(candidate, present) {
			t.Fatalf("a directory was offered as a Node: %v", candidate)
		}
	}
}
