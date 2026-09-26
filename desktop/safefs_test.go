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

// Destructive IPC paths come from the frontend (and from CLI/MCP callers that
// pass explicit paths). The guard must reject the catastrophic cases while
// keeping ordinary "delete this appid's packages/output" flows working.
func TestSafeRemoveAllRejectsProtectedTargets(t *testing.T) {
	if err := safeRemoveAll(""); err == nil {
		t.Fatal("empty path must be rejected")
	}
	if err := safeRemoveAll("   "); err == nil {
		t.Fatal("blank path must be rejected")
	}

	root := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	if err := safeRemoveAll(root); err == nil {
		t.Fatalf("filesystem root %q must be rejected", root)
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if err := safeRemoveAll(home); err == nil {
			t.Fatalf("home directory %q must be rejected", home)
		}
	}
	wd, err := os.Getwd()
	if err == nil {
		if err := safeRemoveAll(wd); err == nil {
			t.Fatalf("working directory %q must be rejected", wd)
		}
	}
}

func TestSafeRemoveAllDeletesOrdinaryTargetAndToleratesMissing(t *testing.T) {
	target := filepath.Join(t.TempDir(), "output", "wxapp")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "app.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := safeRemoveAll(target); err != nil {
		t.Fatalf("ordinary target: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target should be gone: %v", err)
	}

	// RemoveAll semantics: a missing path is not an error.
	if err := safeRemoveAll(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("missing path should be a no-op, got %v", err)
	}
}

// appid is joined into a directory path: a value like ".." or a nested path
// would escape the packages directory and delete unrelated trees.
func TestSafeAppIDRejectsPathEscapes(t *testing.T) {
	for _, bad := range []string{"", " ", "..", ".", "../evil", "a/b", `a\b`, "wx..id/../.."} {
		if err := validateAppID(bad); err == nil {
			t.Fatalf("appid %q must be rejected", bad)
		}
	}
	for _, ok := range []string{"wxfake123", "wx1234567890abcdef"} {
		if err := validateAppID(ok); err != nil {
			t.Fatalf("appid %q should be accepted: %v", ok, err)
		}
	}
}

// The handlers must surface malformed input instead of silently deleting with
// zero values.
func TestExtractDeleteRejectsUnsafeInput(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	// Malformed JSON: destructive handler must not guess.
	if _, err := app.router.Call(context.Background(), "extract.delete", json.RawMessage(`{bad`)); err == nil {
		t.Fatal("malformed params must fail")
	}

	// appid path escape must be refused.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.MkdirAll(filepath.Join(victim, "child"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := app.router.Call(context.Background(), "extract.delete", mustJSON(map[string]any{
		"dir":   filepath.Join(victim, "child"),
		"appid": "..",
	})); err == nil {
		t.Fatal("appid escaping the packages dir must fail")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim directory was deleted despite the guard: %v", err)
	}

	// Explicit protected path must be refused.
	root := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = strings.TrimRight(root, `\`) + `\`
	}
	if _, err := app.router.Call(context.Background(), "extract.delete", mustJSON(map[string]any{
		"path": root,
	})); err == nil {
		t.Fatalf("deleting %q must be refused", root)
	}
}

func TestExtractClearOutputRejectsUnsafeInput(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	keep := filepath.Join(t.TempDir(), "keep")
	if err := os.MkdirAll(filepath.Join(keep, "child"), 0o750); err != nil {
		t.Fatal(err)
	}

	// "applet" wipes the given directory's children: refusing a protected
	// path keeps the tree intact.
	root := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	if _, err := app.router.Call(context.Background(), "extract.clearOutput", mustJSON(map[string]any{
		"type": "applet",
		"dir":  root,
	})); err == nil {
		t.Fatalf("wiping %q must be refused", root)
	}
	if _, err := os.Stat(filepath.Join(keep, "child")); err != nil {
		t.Fatalf("unrelated tree was touched: %v", err)
	}

	// Explicit protected path in the passthrough branch must be refused.
	if _, err := app.router.Call(context.Background(), "extract.clearOutput", mustJSON(map[string]any{
		"type": "unknown",
		"path": root,
	})); err == nil {
		t.Fatalf("passthrough delete of %q must be refused", root)
	}
}

func TestSafeRemoveAllRejectsProtectedAncestors(t *testing.T) {
	if wd, err := os.Getwd(); err == nil {
		parent := filepath.Dir(wd)
		if parent != wd {
			if err := safeRemoveAll(parent); err == nil {
				t.Fatalf("work-directory ancestor %q must be rejected", parent)
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		parent := filepath.Dir(home)
		if parent != home {
			if err := safeRemoveAll(parent); err == nil {
				t.Fatalf("home ancestor %q must be rejected", parent)
			}
		}
	}
}
