package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCodeProjectContract(t *testing.T) {
	app := newContractApp(t)
	appID := "wxcode123"
	root := app.decompileOutputDir(appID)
	if err := os.MkdirAll(filepath.Join(root, "pages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pages", "index.js"), []byte("Page({})"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := callJSON(t, app, "code.project", map[string]any{"appid": appID})
	if result["root"] != root {
		t.Fatalf("root = %v, want %s", result["root"], root)
	}
	tree, ok := result["tree"].([]any)
	if !ok || len(tree) != 1 {
		t.Fatalf("tree = %#v", result["tree"])
	}
	first, ok := tree[0].(map[string]any)
	if !ok || first["name"] != "pages" || first["isDir"] != true {
		t.Fatalf("first node = %#v", tree[0])
	}
}

func TestCodeProjectRejectsUnsafeOrMissingAppID(t *testing.T) {
	app := newContractApp(t)
	for _, appID := range []string{"../outside", "missing-app"} {
		_, err := app.router.Call(context.Background(), "code.project", mustJSON(map[string]any{"appid": appID}))
		if err == nil {
			t.Fatalf("code.project(%q) should fail", appID)
		}
	}

	if _, err := app.router.Call(context.Background(), "code.openDir", mustJSON(map[string]any{})); err == nil {
		t.Fatal("code.openDir without an explicit path should not open an arbitrary picker")
	}
}
