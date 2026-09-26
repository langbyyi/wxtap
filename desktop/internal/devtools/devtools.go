// Package devtools locates the devtools_electron.js launcher used to open a
// standalone DevTools window. The frontend itself comes from the Electron
// binary: devtools://devtools/bundled/... resolves inside Electron, so no
// frontend files ship with WxTap.
package devtools

import (
	"fmt"
	"os"
	"path/filepath"
)

// ElectronScript returns the path of the devtools_electron.js launcher from
// the first candidate directory that has one.
func ElectronScript(candidates []string) (string, error) {
	for _, dir := range candidates {
		script := filepath.Join(dir, "devtools_electron.js")
		if info, err := os.Stat(script); err == nil && !info.IsDir() {
			return script, nil
		}
	}
	return "", fmt.Errorf("devtools_electron.js 未找到")
}
