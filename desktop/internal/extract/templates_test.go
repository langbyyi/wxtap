package extract

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const templateProbeAppID = "wx1234567890abcdef"

// A chunk compiled by WeChat's newer runtime registers its template through
// __wxCodeSpace__ and hands out a chain of functions instead of a node tree.
// Nothing about it can be rendered, and the decompilation as a whole fails.
//
// The code space is stubbed rather than missing: each chunk runs inside a
// try/catch, so an undefined __wxCodeSpace__ would throw before the template is
// registered and hide the very failure this tests.
const compiledTemplateChunk = `var __wxCodeSpace__ = {batchAddCompiledTemplate: function(){}};
__wxCodeSpace__.batchAddCompiledTemplate(function(G,R){return {};});
__wxAppCode__['pages/a/index.wxml'] = [function(){ return function(){} }];`

const legacyTemplateChunk = `__wxAppCode__['pages/a/index.wxml'] = function(){ return {tag:'view', attr:{class:'a'}, children:['hi']} };`

func buildProbePackage(t *testing.T, chunk string) string {
	t.Helper()
	inner := buildWxapkg(map[string][]byte{
		"app-service.js":     []byte(`define("app.js", function(require,module,exports){App({});});`),
		"chunk_0.webview.js": []byte(chunk),
		"assets/pad.txt":     bytes.Repeat([]byte("x"), 1200),
	})
	if len(inner) < 1030 {
		t.Fatalf("fixture payload too short for the encrypted format: %d", len(inner))
	}
	path := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(path, buildEncryptedWxapkg(t, templateProbeAppID, inner), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The probe hides an app from the list, so it may only act on a confirmed
// verdict. The step also fails on a timeout — a heavy legacy app can exceed
// wxmlRestoreTimeout — and on I/O errors, and hiding those would drop a working
// app from the list without saying anything.
func TestProbeKeepsAppsWhoseTemplatesItCouldNotJudge(t *testing.T) {
	for _, err := range []error{
		errors.New("WXML chunk restoration timed out"),
		errors.New("open page-frame.html: permission denied"),
		errors.New("json: unsupported type: func(goja.FunctionCall) goja.Value"),
	} {
		if templatesUnrenderable(err) {
			t.Fatalf("%v must not count as a verdict on its own", err)
		}
	}
	if !templatesUnrenderable(&unrenderableTemplatesError{cause: errors.New("some cause")}) {
		t.Fatal("the confirmed unrenderable error must be recognised")
	}
	if !templatesUnrenderable(fmt.Errorf("wrapped: %w", &unrenderableTemplatesError{cause: errors.New("some cause")})) {
		t.Fatal("the verdict must survive wrapping")
	}
}

// The verdict has to come from the step itself: a fixture that fails the way
// real compiled-template chunks do must be recognisable, or the probe would
// silently stop excluding anything.
func TestRestoreWebviewWXMLMarksTheUnrenderableVerdict(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "chunk_0.webview.js"), []byte(compiledTemplateChunk), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := restoreWebviewWXML(root)
	if err == nil {
		t.Fatal("a compiled-template chunk must fail restoration")
	}
	if !templatesUnrenderable(err) {
		t.Fatalf("the failure must be the verdict the probe acts on: %v", err)
	}
}

func TestUsesCompiledTemplatesFindsTheRuntimeMarker(t *testing.T) {
	compiled := buildProbePackage(t, compiledTemplateChunk)
	legacy := buildProbePackage(t, legacyTemplateChunk)
	if !UsesCompiledTemplates([]string{compiled}, templateProbeAppID) {
		t.Fatal("a compiled-template chunk must be detected")
	}
	// The marker is only a pre-filter, so a miss here must not come from the
	// scan failing to read the package at all.
	if UsesCompiledTemplates([]string{legacy}, templateProbeAppID) {
		t.Fatal("a legacy chunk must not be flagged")
	}
}

// The probe is only worth its cost if it agrees with what decompiling does.
func TestProbeWebviewWXMLPredictsTheDecompileOutcome(t *testing.T) {
	compiled := buildProbePackage(t, compiledTemplateChunk)
	legacy := buildProbePackage(t, legacyTemplateChunk)

	if ProbeWebviewWXML([]string{compiled}, templateProbeAppID) {
		t.Fatal("a compiled-template app must not probe as restorable")
	}
	if _, err := DecompilePackages([]string{compiled}, filepath.Join(t.TempDir(), "out"), templateProbeAppID); err == nil {
		t.Fatal("compiled-template app: expected the decompile to fail")
	}

	if !ProbeWebviewWXML([]string{legacy}, templateProbeAppID) {
		t.Fatal("a legacy app must probe as restorable")
	}
	if _, err := DecompilePackages([]string{legacy}, filepath.Join(t.TempDir(), "out"), templateProbeAppID); err != nil {
		t.Fatalf("legacy app: decompile must succeed, got %v", err)
	}
}

// The verdict has to be the compiled-template signature itself, not "the step
// failed and nothing was written". A render that throws fails identically while
// saying nothing about the templates, and hiding an app on it drops a working
// one from the list.
func TestRestoreWebviewWXMLOnlyBlamesTheTemplateSignatures(t *testing.T) {
	root := t.TempDir()
	chunk := `__wxAppCode__['pages/a/index.wxml'] = function(){ throw new Error('document is not defined'); };`
	if err := os.WriteFile(filepath.Join(root, "chunk_0.webview.js"), []byte(chunk), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := restoreWebviewWXML(root)
	if err == nil {
		t.Fatal("nothing was written, so the step must report the failure")
	}
	if templatesUnrenderable(err) {
		t.Fatalf("a throwing render must not be a verdict on the templates: %v", err)
	}
}

// The same rule under the failure that actually happens in the field: a heavy
// app whose chunk cannot finish inside wxmlRestoreTimeout. Classifying that as
// the verdict hides the app, which is the very thing the probe must not do.
func TestRestoreWebviewWXMLKeepsASlowChunkRestorable(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out wxmlRestoreTimeout")
	}
	root := t.TempDir()
	chunk := `__wxAppCode__['pages/a/index.wxml'] = function(){ while(true){} };`
	if err := os.WriteFile(filepath.Join(root, "chunk_0.webview.js"), []byte(chunk), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := restoreWebviewWXML(root)
	if err == nil {
		t.Fatal("the render never returns, so the step must report the failure")
	}
	if templatesUnrenderable(err) {
		t.Fatalf("a timeout must not be a verdict on the templates: %v", err)
	}
}

func TestProbeKeepsAnAppWhoseRenderOnlyThrows(t *testing.T) {
	throwing := `__wxAppCode__['pages/a/index.wxml'] = function(){ throw new Error('document is not defined'); };`
	path := buildProbePackage(t, throwing)
	if !ProbeWebviewWXML([]string{path}, templateProbeAppID) {
		t.Fatal("a render failure that says nothing about the templates must not hide the app")
	}
}
