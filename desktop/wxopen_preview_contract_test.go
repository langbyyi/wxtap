package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updatePreviewSnapshot rewrites the catalog the browser preview serves.
var updatePreviewSnapshot = flag.Bool("update-preview-snapshot", false,
	"rewrite frontend/src/data/wxopen-endpoints.json from the live catalog")

// A browser has no Go backend, so api/bridge.ts answers wxopen.endpoints with a
// snapshot rather than the live table. That snapshot is generated from the real
// catalog instead of being written by hand: the hand-written one had shrunk to
// two mini-program endpoints while the catalog carried sixteen, which reads as
// "the tool only knows a handful of APIs".
func TestWxOpenPreviewSnapshotMatchesCatalog(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	result, err := app.router.Call(context.Background(), "wxopen.endpoints", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("wxopen.endpoints: %v", err)
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}

	path := filepath.Join("frontend", "src", "data", "wxopen-endpoints.json")
	if *updatePreviewSnapshot {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o640); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", path, buf.Len())
		return
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate: go test . -update-preview-snapshot)", path, err)
	}
	if !bytes.Equal(onDisk, buf.Bytes()) {
		t.Fatalf("%s no longer matches the catalog — regenerate: go test . -update-preview-snapshot", path)
	}
}
