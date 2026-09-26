package extract

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// compiledTemplateMarker is how WeChat's newer page-template runtime registers a
// compiled template inside a *.webview.js chunk. This restorer has no
// implementation of that runtime, so a chunk that relies on it cannot be turned
// back into a .wxml.
const compiledTemplateMarker = "batchAddCompiledTemplate"

// UsesCompiledTemplates reports whether any package mentions WeChat's compiled
// templates at all. It is a pre-filter, not a verdict: the bare marker also
// matches the runtime's own definition and its error string — one local app
// carries four such mentions and not a single call site, and restores all of its
// templates — so this only decides which apps the probe is worth running on.
// Over-selecting is deliberate: a missed call site would hide a broken app from
// the list, while an extra probe costs one run.
//
// It is cheap enough to run over every package: decrypting and scanning 43 MB of
// local packages takes ~90 ms.
func UsesCompiledTemplates(packagePaths []string, appID string) bool {
	for _, path := range packagePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		decrypted, err := Decrypt(data, appID)
		if err != nil {
			continue
		}
		if bytes.Contains(decrypted, []byte(compiledTemplateMarker)) {
			return true
		}
	}
	return false
}

// ProbeWebviewWXML answers whether restoring this app's page templates will
// succeed, by unpacking the packages and running the real webview WXML step on
// them. The step itself is the judge: a compiled-template chunk resolves to a
// chain of functions instead of a node tree, the export cannot be marshalled,
// and the step reports the error that aborts the whole decompilation.
//
// Only call it for apps that UsesCompiledTemplates selected. It fails open: an
// app is hidden only on a confirmed verdict, never because the probe timed out
// or could not run — a heavy legacy app can exceed the step's own timeout, and
// silently dropping it from the list would be the very thing this guards
// against.
func ProbeWebviewWXML(packagePaths []string, appID string) bool {
	workDir, err := os.MkdirTemp("", ".wxtap-template-probe-*")
	if err != nil {
		return true
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	staged := filepath.Join(workDir, "staged")
	if err := os.MkdirAll(staged, 0o750); err != nil {
		return true
	}
	for i, path := range packagePaths {
		packageRoot := filepath.Join(workDir, fmt.Sprintf("package-%04d", i))
		if _, err := extractWxapkgRaw(path, packageRoot, appID); err != nil {
			return true
		}
		if err := mergeTree(packageRoot, staged); err != nil {
			return true
		}
	}
	if _, err := restoreWebviewWXML(staged); err != nil {
		return !templatesUnrenderable(err)
	}
	return true
}
