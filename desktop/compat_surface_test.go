package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// compatSurface mirrors testdata/compat-surface.json: the frozen v0.1.0 IPC
// surface that stays registered. Removing an entry is a feature regression,
// not a cleanup. Sole exception: shell.openDevtools left the surface in
// 2026-09 by decision — Chromium drops devtools:// launch arguments, so the
// browser path could not open DevTools at all; shell.openDevtoolsWindow is
// the only remaining way to open a DevTools window.
type compatSurface struct {
	IPCMethods []string `json:"ipcMethods"`
	MCPTools   []string `json:"mcpTools"`
}

func loadCompatSurface(t *testing.T) compatSurface {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "compat-surface.json"))
	if err != nil {
		t.Fatalf("read compat surface fixture: %v", err)
	}
	var surface compatSurface
	if err := json.Unmarshal(data, &surface); err != nil {
		t.Fatalf("parse compat surface fixture: %v", err)
	}
	if len(surface.IPCMethods) == 0 || len(surface.MCPTools) == 0 {
		t.Fatal("compat surface fixture is empty")
	}
	return surface
}

// TestCompatIPCMethodsStayRegistered keeps the frozen entry points callable.
// The frontend allow-list test only covers what the new UI calls, so a
// surface-only method (fetch.md, code.expandDir, ...) could disappear without
// any other test noticing.
func TestCompatIPCMethodsStayRegistered(t *testing.T) {
	surface := loadCompatSurface(t)

	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	registered := map[string]bool{}
	for _, method := range app.router.Methods() {
		registered[method] = true
	}
	for _, method := range surface.IPCMethods {
		if !registered[method] {
			t.Errorf("compat IPC method %q is no longer registered", method)
		}
	}
}
