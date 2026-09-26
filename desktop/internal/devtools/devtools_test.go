package devtools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestElectronScriptFindsLauncher(t *testing.T) {
	present := t.TempDir()
	if err := os.WriteFile(filepath.Join(present, "devtools_electron.js"), []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := ElectronScript([]string{t.TempDir(), present})
	if err != nil {
		t.Fatalf("electron script: %v", err)
	}
	if script != filepath.Join(present, "devtools_electron.js") {
		t.Fatalf("got %s", script)
	}
}
