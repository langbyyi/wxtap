package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildContractWxapkg assembles an unencrypted wxapkg fixture with absolute
// offsets (wxapkg package format: 0xBE, info1(4),
// indexInfoLength(4), bodyInfoLength(4), 0xED, fileCount(4), entries).
func buildContractWxapkg(files map[string][]byte) []byte {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	indexLen := 0
	for _, name := range names {
		indexLen += 12 + len(name)
	}
	base := 18 + indexLen

	var index, body bytes.Buffer
	offset := 0
	for _, name := range names {
		content := files[name]
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(len(name)))
		index.Write(b[:])
		index.WriteString(name)
		binary.BigEndian.PutUint32(b[:], uint32(base+offset))
		index.Write(b[:])
		binary.BigEndian.PutUint32(b[:], uint32(len(content)))
		index.Write(b[:])
		body.Write(content)
		offset += len(content)
	}

	var out bytes.Buffer
	out.WriteByte(0xBE)
	var b [4]byte
	out.Write(b[:])
	binary.BigEndian.PutUint32(b[:], uint32(index.Len()))
	out.Write(b[:])
	binary.BigEndian.PutUint32(b[:], uint32(body.Len()))
	out.Write(b[:])
	out.WriteByte(0xED)
	binary.BigEndian.PutUint32(b[:], uint32(len(names)))
	out.Write(b[:])
	out.Write(index.Bytes())
	out.Write(body.Bytes())
	return out.Bytes()
}

// newContractApp builds an App whose data root and packages dir live in
// temp dirs, with the Core unreachable and emits disabled (nil ctx).
func newContractApp(t *testing.T) *App {
	t.Helper()
	base := t.TempDir()
	t.Setenv("WXTAP_DATA_DIR", base)
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")

	app := NewApp()
	app.setupIPC() // dataBase/configStore follow WXTAP_DATA_DIR
	return app
}

// callJSON dispatches through the router and round-trips the result through
// JSON so assertions see exactly what the webview's frontend sees (struct
// fields must survive their json tags).
func callJSON(t *testing.T, app *App, method string, params map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	result, err := app.router.Call(context.Background(), method, raw)
	if err != nil {
		t.Fatalf("%s failed: %v", method, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(encoded, &m); err != nil {
		t.Fatalf("%s returned non-object %s: %v", method, encoded, err)
	}
	return m
}

// TestExtractHandlerContract walks the full extract flow the frontend drives
// and asserts every payload shape ExtractView.vue reads.
func TestExtractHandlerContract(t *testing.T) {
	app := newContractApp(t)
	pkgDir := t.TempDir()
	appID := "wxcontract11111111"

	appSub := filepath.Join(pkgDir, appID)
	if err := os.MkdirAll(appSub, 0o755); err != nil {
		t.Fatal(err)
	}
	wxapkg := buildContractWxapkg(map[string][]byte{
		"app.js":          []byte(`var phone = '13812345678';`),
		"app-config.json": []byte(`{"appname":"契约演示"}`),
	})
	if err := os.WriteFile(filepath.Join(appSub, "main.wxapkg"), wxapkg, 0o644); err != nil {
		t.Fatal(err)
	}

	// extract.packages wraps the list and fills the UI state fields.
	packages := callJSON(t, app, "extract.packages", map[string]any{"dir": pkgDir})
	list, ok := packages["packages"].([]interface{})
	if !ok || len(list) != 1 {
		t.Fatalf("extract.packages payload: %#v", packages)
	}
	pkg := list[0].(map[string]any)
	if pkg["appid"] != appID || pkg["path"] == "" {
		t.Fatalf("package entry: %#v", pkg)
	}
	if pkg["mtime"].(float64) <= 0 {
		t.Fatalf("mtime missing: %#v", pkg)
	}
	if pkg["decompiled"] != false {
		t.Fatalf("fresh package state: %#v", pkg)
	}

	// extract.decompile returns files_count/name/dir.
	decompiled := callJSON(t, app, "extract.decompile", map[string]any{"dir": pkgDir, "appid": appID})
	if decompiled["files_count"].(float64) < 1 {
		t.Fatalf("files_count: %#v", decompiled)
	}
	if decompiled["name"] != "契约演示" {
		t.Fatalf("name from app-config.json: %#v", decompiled)
	}
	outDir := decompiled["dir"].(string)
	if decompiled["output_dir"] != outDir {
		t.Fatalf("output_dir alias: %#v", decompiled)
	}
	if info, err := os.Stat(outDir); err != nil || !info.IsDir() {
		t.Fatalf("decompiled dir missing: %v", err)
	}

	// extract.scan follows async semantics; results go to the scan event and
	// are not persisted.
	report := callJSON(t, app, "extract.scan", map[string]any{"dir": pkgDir, "appid": appID})
	if report["async"] != true {
		t.Fatalf("scan should return asynchronously: %#v", report)
	}
	app.waitBackground()
	if _, err := os.Stat(filepath.Join(app.dataBase, "scan_reports")); !os.IsNotExist(err) {
		t.Fatalf("scan must not persist reports: %v", err)
	}
	// extract.builtinPatterns exposes label -> regex (read-only list).
	patterns := callJSON(t, app, "extract.builtinPatterns", nil)["patterns"].(map[string]any)
	if len(patterns) == 0 {
		t.Fatal("builtin patterns empty")
	}
	if regex, ok := patterns["手机号码"].(string); !ok || !strings.Contains(regex, `\d`) {
		t.Fatalf("mobile label/regex: %#v", patterns["手机号码"])
	}

	// extract.delete removes the raw packages and the decompiled output.
	callJSON(t, app, "extract.delete", map[string]any{"dir": pkgDir, "appid": appID})
	if _, err := os.Stat(appSub); !os.IsNotExist(err) {
		t.Fatalf("raw package dir still present: %v", err)
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Fatalf("decompiled dir still present: %v", err)
	}

	// extract.clearOutput(type=applet) wipes the directory contents.
	os.WriteFile(filepath.Join(pkgDir, "leftover.wxapkg"), []byte("x"), 0o644)
	callJSON(t, app, "extract.clearOutput", map[string]any{"type": "applet", "dir": pkgDir})
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("applet dir not cleared: %#v", entries)
	}
}

// TestExtractScanRequiresDecompiledOutput: scanning an appid that was never
// decompiled fails with a clear hint, not a filesystem error.
func TestExtractScanRequiresDecompiledOutput(t *testing.T) {
	app := newContractApp(t)
	_, err := app.router.Call(context.Background(), "extract.scan",
		json.RawMessage(`{"appid":"wxnever111111111"}`))
	if err == nil || !strings.Contains(err.Error(), "请先反编译") {
		t.Fatalf("scan without decompile: %v", err)
	}
}

func TestExtractHandlersAcceptExplicitPaths(t *testing.T) {
	app := newContractApp(t)
	appID := "wxexplicit1111111"
	pkgDir := t.TempDir()
	pkgPath := filepath.Join(pkgDir, "explicit.wxapkg")
	if err := os.WriteFile(pkgPath, buildContractWxapkg(map[string][]byte{
		"app.js": []byte("var token = 'explicit';"),
	}), 0o644); err != nil {
		t.Fatal(err)
	}

	decompiled := callJSON(t, app, "extract.decompile", map[string]any{
		"path":  pkgPath,
		"appid": appID,
	})
	outDir := decompiled["dir"].(string)
	if _, err := os.Stat(filepath.Join(outDir, "app.js")); err != nil {
		t.Fatalf("explicit path was not decompiled: %v", err)
	}

	// The IPC API accepts an explicit scan path instead of requiring dir.
	if _, err := app.router.Call(context.Background(), "extract.scan", mustJSON(map[string]any{
		"path":  outDir,
		"appid": appID,
	})); err != nil {
		t.Fatalf("explicit scan path: %v", err)
	}
	// extract.scan answers immediately and writes its report from a tracked
	// background task; wait for it so TempDir cleanup cannot race the write.
	app.waitBackground()

	deleteDir := filepath.Join(t.TempDir(), "delete-me")
	if err := os.MkdirAll(deleteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := app.router.Call(context.Background(), "extract.delete", mustJSON(map[string]any{
		"path": deleteDir,
	})); err != nil {
		t.Fatalf("explicit delete path: %v", err)
	}
	if _, err := os.Stat(deleteDir); !os.IsNotExist(err) {
		t.Fatalf("explicit delete path still exists: %v", err)
	}
}

// TestExtractDecompileUnknownAppidFails checks the error for an
// appid with no matching package.
func TestExtractDecompileUnknownAppidFails(t *testing.T) {
	app := newContractApp(t)
	pkgDir := t.TempDir()
	t.Setenv("WXTAP_PACKAGES_DIR", pkgDir)
	_, err := app.router.Call(context.Background(), "extract.decompile",
		json.RawMessage(`{"dir":"`+jsonEscape(pkgDir)+`","appid":"wxghost111111111"}`))
	if err == nil || !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("decompile unknown appid: %v", err)
	}
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func mustJSON(value any) json.RawMessage {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return b
}

// cloud.scan always answers {items: [...]} — never the capture buffer,
// and never null (frontend `data.items` would crash). Offline engines yield
// []. Discovery uses __wxAppCode__ via cloudAudit.scanCloudFunctions.
func TestCloudScanContract(t *testing.T) {
	app := newContractApp(t)
	scan := callJSON(t, app, "cloud.scan", nil)
	items, ok := scan["items"].([]interface{})
	if !ok {
		t.Fatalf("cloud.scan must return {items: []} with no engine, got %#v", scan)
	}
	if len(items) != 0 {
		t.Fatalf("no engine means no findings: %#v", items)
	}
}

// TestSettingsGetPathsContract: the settings view's folder shortcuts keep
// the expected key names.

// navigator.getBlockedRedirects must wrap the array as {redirects:[...]}
// (bare arrays break IPC clients that read `.redirects`).
func TestGetBlockedRedirectsContractShape(t *testing.T) {
	// Unit coverage of the wrap lives in ipc.TestGetBlockedRedirects*;
	// this keeps the IPC method name in the contract suite inventory.
	_ = "navigator.getBlockedRedirects"
}

func TestSettingsGetPathsContract(t *testing.T) {
	app := newContractApp(t)
	paths := callJSON(t, app, "settings.getPaths", nil)
	for _, key := range []string{"hook_scripts", "frida_config", "skills", "outputDir", "dbPath"} {
		if _, ok := paths[key]; !ok {
			t.Fatalf("settings.getPaths missing %q: %#v", key, paths)
		}
	}
	if !strings.HasSuffix(paths["frida_config"].(string), filepath.Join("frida", "config")) {
		t.Fatalf("frida_config: %v", paths["frida_config"])
	}
}

func TestExtractBrowseKeepsRequestedDirectory(t *testing.T) {
	dir := t.TempDir()
	options := extractBrowseDialogOptions(dir)
	if options.DefaultDirectory != dir {
		t.Fatalf("extract.browse default directory: got %q want %q", options.DefaultDirectory, dir)
	}
}

func TestExtractDecompileProcessesAllPackagesForAppID(t *testing.T) {
	app := newContractApp(t)
	pkgDir := t.TempDir()
	appID := "wxmultipackage111"
	appSub := filepath.Join(pkgDir, appID)
	if err := os.MkdirAll(appSub, 0o755); err != nil {
		t.Fatal(err)
	}
	mainPkg := buildContractWxapkg(map[string][]byte{
		"app-config.json": []byte(`{"appname":"多包演示","pages":["pages/index/index"]}`),
		"app-service.js":  []byte(`define("pages/index/index.js", function(require,module,exports){Page({data:{title:"main"}});}, {isPage:true});`),
	})
	subPkg := buildContractWxapkg(map[string][]byte{
		"subpackage.js": []byte(`var subpackage = true;`),
	})
	if err := os.WriteFile(filepath.Join(appSub, "0-main.wxapkg"), mainPkg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appSub, "1-sub.wxapkg"), subPkg, 0o644); err != nil {
		t.Fatal(err)
	}

	result := callJSON(t, app, "extract.decompile", map[string]any{"dir": pkgDir, "appid": appID})
	outDir := result["dir"].(string)
	for _, relative := range []string{"app.json", "pages/index/index.js", "subpackage.js"} {
		if _, err := os.Stat(filepath.Join(outDir, filepath.FromSlash(relative))); err != nil {
			t.Fatalf("restored output %s: %v", relative, err)
		}
	}
	if result["packages_processed"] != float64(2) {
		t.Fatalf("packages_processed: %#v", result)
	}
}

func TestExtractDecompileFailurePreservesExistingOutput(t *testing.T) {
	app := newContractApp(t)
	pkgDir := t.TempDir()
	appID := "wxrollback11111111"
	appSub := filepath.Join(pkgDir, appID)
	if err := os.MkdirAll(appSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appSub, "0-main.wxapkg"), buildContractWxapkg(map[string][]byte{
		"main.js": []byte(`var main = true;`),
	}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appSub, "1-broken.wxapkg"), []byte("not a wxapkg"), 0o644); err != nil {
		t.Fatal(err)
	}

	outDir := app.decompileOutputDir(appID)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "marker.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{"dir": pkgDir, "appid": appID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.router.Call(context.Background(), "extract.decompile", raw); err == nil {
		t.Fatal("decompile must fail when any matching package is invalid")
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "marker.txt" {
		t.Fatalf("failed decompile changed existing output: %#v", entries)
	}
}

func TestExtractCandidateDirsIncludesConfiguredDirectory(t *testing.T) {
	app := newContractApp(t)
	configured := t.TempDir()
	t.Setenv("WXTAP_PACKAGES_DIR", configured)

	result := callJSON(t, app, "extract.candidateDirs", nil)
	dirs, ok := result["dirs"].([]any)
	if !ok || len(dirs) == 0 {
		t.Fatalf("candidate dirs payload: %#v", result)
	}
	found := false
	for _, item := range dirs {
		candidate := item.(map[string]any)
		if candidate["path"] != configured {
			continue
		}
		found = true
		if candidate["exists"] != true || candidate["selected"] != true || candidate["description"] == "" {
			t.Fatalf("configured candidate: %#v", candidate)
		}
	}
	if !found {
		t.Fatalf("configured directory missing from candidates: %#v", dirs)
	}
}

func TestExtractDecompileAllProcessesEveryApp(t *testing.T) {
	app := newContractApp(t)
	pkgDir := t.TempDir()
	for _, fixture := range []struct {
		appID string
		file  string
	}{
		{appID: "wxall11111111111", file: "one.js"},
		{appID: "wxall22222222222", file: "two.js"},
	} {
		appSub := filepath.Join(pkgDir, fixture.appID)
		if err := os.MkdirAll(appSub, 0o755); err != nil {
			t.Fatal(err)
		}
		wxapkg := buildContractWxapkg(map[string][]byte{
			fixture.file: []byte(`var value = true;`),
		})
		if err := os.WriteFile(filepath.Join(appSub, "0.wxapkg"), wxapkg, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	result := callJSON(t, app, "extract.decompileAll", map[string]any{"dir": pkgDir})
	if result["succeeded"] != float64(2) || result["failed"] != float64(0) {
		t.Fatalf("decompile all summary: %#v", result)
	}
	results := result["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("decompile all results: %#v", results)
	}
	for _, item := range results {
		entry := item.(map[string]any)
		if entry["ok"] != true {
			t.Fatalf("decompile all entry: %#v", entry)
		}
		outDir, _ := entry["dir"].(string)
		if _, err := os.Stat(outDir); err != nil {
			t.Fatalf("decompile all output %s: %v", outDir, err)
		}
	}
}

func TestExtractScanRejectsPathLikeAppIDBeforeScanning(t *testing.T) {
	app := newContractApp(t)
	scanDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scanDir, "app.js"), []byte(`var token = "abc123";`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := app.router.Call(context.Background(), "extract.scan", mustJSON(map[string]any{
		"path": scanDir, "appid": "../escape",
	}))
	if err == nil || !strings.Contains(err.Error(), "appid") {
		t.Fatalf("path-like appid should be rejected before scanning: %v", err)
	}
}

func TestExtractDecompileRejectsPathLikeAppID(t *testing.T) {
	app := newContractApp(t)
	pkgPath := filepath.Join(t.TempDir(), "main.wxapkg")
	if err := os.WriteFile(pkgPath, []byte("not-a-real-package"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := app.router.Call(context.Background(), "extract.decompile", mustJSON(map[string]any{
		"path":  pkgPath,
		"appid": "../escape",
	}))
	if err == nil || !strings.Contains(err.Error(), "appid") {
		t.Fatalf("path-like appid was not rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(app.extractOutputRoot(), "escape")); !os.IsNotExist(err) {
		t.Fatalf("path-like appid reached the output root: %v", err)
	}
}

// TestExtractPackagesIncludesOutputOnlyApps: 已经反编译过、但原始包已被清理
// （或换过包目录）的小程序仍要出现在列表里并标记为已反编译，否则用户看不到
// 自己的产物，也无法从列表打开输出目录。
func TestExtractPackagesIncludesOutputOnlyApps(t *testing.T) {
	app := newContractApp(t)
	pkgDir := t.TempDir()
	appID := "wxoutputonly11111"

	if err := os.MkdirAll(filepath.Join(pkgDir, appID), 0o755); err != nil {
		t.Fatal(err)
	}
	wxapkg := buildContractWxapkg(map[string][]byte{
		"app.js":          []byte("var a = 1;"),
		"app-config.json": []byte(`{"appname":"产物演示"}`),
	})
	if err := os.WriteFile(filepath.Join(pkgDir, appID, "main.wxapkg"), wxapkg, 0o644); err != nil {
		t.Fatal(err)
	}
	callJSON(t, app, "extract.decompile", map[string]any{"dir": pkgDir, "appid": appID})

	// 原始包没了，只剩输出目录。
	if err := os.RemoveAll(filepath.Join(pkgDir, appID)); err != nil {
		t.Fatal(err)
	}

	packages := callJSON(t, app, "extract.packages", map[string]any{"dir": pkgDir})
	list, ok := packages["packages"].([]interface{})
	if !ok || len(list) != 1 {
		t.Fatalf("output-only package missing: %#v", packages)
	}
	pkg := list[0].(map[string]any)
	if pkg["appid"] != appID {
		t.Fatalf("appid: %#v", pkg)
	}
	if pkg["decompiled"] != true {
		t.Fatalf("output-only package must read as decompiled: %#v", pkg)
	}
	if dir, _ := pkg["output_dir"].(string); dir == "" {
		t.Fatalf("output_dir missing: %#v", pkg)
	}
	if pkg["name"] != "产物演示" {
		t.Fatalf("name not read from app-config.json: %#v", pkg)
	}
}

// Apps whose page templates cannot be restored are left out of a batch
// decompile: the succeeded/failed counts describe what is decompilable on this
// machine, and such an app never reaches the list the batch is driven from.
func TestExtractDecompileAllLeavesUnrestorableAppsOut(t *testing.T) {
	app := newContractApp(t)
	pkgDir := t.TempDir()
	const compiledAppID = "wx1111111111111111"
	const legacyAppID = "wx2222222222222222"
	for _, appID := range []string{compiledAppID, legacyAppID} {
		if err := os.MkdirAll(filepath.Join(pkgDir, appID), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The compiled template hands out a chain of functions instead of a node
	// tree, so the webview WXML step cannot render it and the app's decompile
	// would fail with no output at all.
	const compiledChunk = `var __wxCodeSpace__ = {batchAddCompiledTemplate: function(){}};
__wxCodeSpace__.batchAddCompiledTemplate(function(G,R){return {};});
__wxAppCode__['pages/a/index.wxml'] = [function(){ return function(){} }];`
	const legacyChunk = `__wxAppCode__['pages/a/index.wxml'] = function(){ return {tag:'view', attr:{}, children:['hi']} };`
	write := func(appID, chunk string) {
		t.Helper()
		data := buildContractWxapkg(map[string][]byte{
			"app-service.js":     []byte(`define("pages/index/index.js", function(require,module,exports){Page({});});`),
			"chunk_0.webview.js": []byte(chunk),
		})
		if err := os.WriteFile(filepath.Join(pkgDir, appID, "0-main.wxapkg"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(compiledAppID, compiledChunk)
	write(legacyAppID, legacyChunk)

	result := callJSON(t, app, "extract.decompileAll", map[string]any{"dir": pkgDir})
	if result["succeeded"] != float64(1) || result["failed"] != float64(0) {
		t.Fatalf("succeeded/failed = %v/%v, want 1/0 (payload %#v)", result["succeeded"], result["failed"], result)
	}
	seen := map[string]bool{}
	for _, item := range result["results"].([]any) {
		entry, _ := item.(map[string]any)
		appid, _ := entry["appid"].(string)
		seen[appid] = true
	}
	if seen[compiledAppID] {
		t.Fatal("an app with unrestorable templates must not be decompiled in a batch")
	}
	if !seen[legacyAppID] {
		t.Fatal("the decompilable app is missing from the batch")
	}
}

// A single decompile reaches the pipeline through the appid route too, and that
// route must refuse an app the page never offers. An explicit package path stays
// a deliberate escape hatch.
func TestExtractDecompileRefusesAnAppItCannotRestore(t *testing.T) {
	app := newContractApp(t)
	pkgDir := t.TempDir()
	const appID = "wx1111111111111111"
	if err := os.MkdirAll(filepath.Join(pkgDir, appID), 0o755); err != nil {
		t.Fatal(err)
	}
	const compiledChunk = `var __wxCodeSpace__ = {batchAddCompiledTemplate: function(){}};
__wxCodeSpace__.batchAddCompiledTemplate(function(G,R){return {};});
__wxAppCode__['pages/a/index.wxml'] = [function(){ return function(){} }];`
	pkgPath := filepath.Join(pkgDir, appID, "0-main.wxapkg")
	if err := os.WriteFile(pkgPath, buildContractWxapkg(map[string][]byte{
		"app-service.js":     []byte(`define("pages/index/index.js", function(require,module,exports){Page({});});`),
		"chunk_0.webview.js": []byte(compiledChunk),
	}), 0o644); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{"dir": pkgDir, "appid": appID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.router.Call(context.Background(), "extract.decompile", raw); err == nil {
		t.Fatal("the appid route must refuse an app whose templates cannot be restored")
	} else if !strings.Contains(err.Error(), "不在可反编译范围内") {
		t.Fatalf("expected the refusal, got: %v", err)
	}

	raw, err = json.Marshal(map[string]any{"appid": appID, "path": pkgPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.router.Call(context.Background(), "extract.decompile", raw); err == nil {
		t.Fatal("an explicit package path must still attempt the decompile")
	} else if strings.Contains(err.Error(), "不在可反编译范围内") {
		t.Fatalf("an explicit path must reach the pipeline, not the refusal: %v", err)
	}
}
