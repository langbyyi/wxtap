package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/api"
	"github.com/langbyyi/wxtap/desktop/internal/api/ipc"
	"github.com/langbyyi/wxtap/desktop/internal/engine"
	"github.com/langbyyi/wxtap/desktop/internal/rpc"
)

func TestCoreCommandExplicitScriptWins(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cli.js")
	if err := os.WriteFile(script, []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WXTAP_CORE_SCRIPT", script)
	// Pinned rather than left to PATH: the Node gate would otherwise make this
	// test fail on any host whose Node is older than Core requires.
	t.Setenv("WXTAP_CORE_CMD", fakeNode(t, "24.19.0"))

	cmd, err := coreCommand()
	if err != nil {
		t.Fatalf("coreCommand: %v", err)
	}
	if cmd.Args[1] != script {
		t.Fatalf("script: %v", cmd.Args)
	}
	if cmd.Dir != dir {
		t.Fatalf("dir should default to the script directory: %q", cmd.Dir)
	}
}

func TestCoreCommandMissingNodeFailsClearly(t *testing.T) {
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")
	_, err := coreCommand()
	if err == nil {
		t.Fatal("expected a clear error for a missing node binary")
	}
	// A pinned override is the operator's explicit choice, so its own problem is
	// reported rather than advice to install a Node they may already have.
	for _, want := range []string{"无法运行", "wxtap-missing-node-xyz"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %q: %v", want, err)
		}
	}
}

// With nothing configured and nothing on PATH, the message has to tell the user
// what to install and where to point the app instead.
func TestCoreCommandWithoutAnyNodeExplainsWhatToDo(t *testing.T) {
	isolateFromInstalledNode(t)
	_, err := coreCommand()
	if err == nil {
		t.Fatal("expected a clear error when no Node is available")
	}
	for _, want := range []string{"nodejs.org", "设置 → Node 运行时"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %q: %v", want, err)
		}
	}
}

func TestCoreCommandRejectsAnOldNode(t *testing.T) {
	t.Setenv("WXTAP_CORE_CMD", fakeNode(t, "18.20.4"))
	_, err := coreCommand()
	if err == nil || !strings.Contains(err.Error(), "版本过低") {
		t.Fatalf("an old Node must be refused before Core starts: %v", err)
	}
}

func TestCoreCommandMissingScriptFailsClearly(t *testing.T) {
	t.Setenv("WXTAP_CORE_SCRIPT", filepath.Join(t.TempDir(), "nope", "cli.js"))
	t.Setenv("WXTAP_CORE_CMD", fakeNode(t, "24.19.0"))
	_, err := coreCommand()
	if err == nil {
		t.Fatal("expected a clear error for a missing core script")
	}
	if !strings.Contains(err.Error(), "npm run build") {
		t.Fatalf("error should tell the user how to build core: %v", err)
	}
}

func TestFetchMarkdownRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer server.Close()

	app := NewApp()
	app.ctx = context.Background()
	app.setupIPC()
	_, err := app.router.Call(context.Background(), "fetch.md", json.RawMessage(`{"url":"`+server.URL+`"}`))
	if err == nil {
		t.Fatal("fetch.md must reject non-2xx responses")
	}
}

// resolveCoreScript probes the deployment and development layouts before
// falling back to the cwd-relative default.
func TestResolveCoreScriptProbesCandidates(t *testing.T) {
	name := filepath.Join("dist", "cli.js")

	root := t.TempDir()
	deployed := filepath.Join(root, "core", name)
	if err := os.MkdirAll(filepath.Join(root, "core", "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deployed, []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveCoreScript(root); got != deployed {
		t.Fatalf("exe-sibling core/ probe: got %q want %q", got, deployed)
	}

	// With no exe-sibling core/, the cwd candidates are probed (dev layout:
	// cwd = desktop → ../core/dist/cli.js).
	empty := t.TempDir()
	dev := filepath.Join(empty, "core", name)
	if err := os.MkdirAll(filepath.Join(empty, "core", "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dev, []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(empty)
	if got := resolveCoreScript(filepath.Join(empty, "build", "bin")); got != dev {
		t.Fatalf("cwd ../core probe: got %q want %q", got, dev)
	}

	// Nothing anywhere: the first candidate is reported so the error names
	// a concrete path.
	missing := resolveCoreScript(filepath.Join(empty, "nowhere"))
	if !strings.HasSuffix(filepath.ToSlash(missing), "core/dist/cli.js") {
		t.Fatalf("fallback should be a core/dist/cli.js path: %q", missing)
	}
}

// resolveMigrationsDir probes the exe-sibling migrations/ (release layout)
// before the cwd-relative one (dev layout), like resolveCoreScript does.
func TestResolveMigrationsDirProbesCandidates(t *testing.T) {
	root := t.TempDir()
	deployed := filepath.Join(root, "migrations")
	if err := os.MkdirAll(deployed, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveMigrationsDir(root); got != deployed {
		t.Fatalf("exe-sibling migrations probe: got %q want %q", got, deployed)
	}

	// With no exe-sibling migrations/, the cwd candidate is probed (dev
	// layout: cwd = desktop → ./migrations).
	empty := t.TempDir()
	dev := filepath.Join(empty, "migrations")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(empty)
	if got := resolveMigrationsDir(filepath.Join(empty, "build", "bin")); got != dev {
		t.Fatalf("cwd migrations probe: got %q want %q", got, dev)
	}

	// Nothing anywhere: the first candidate is reported so the Open error
	// names a concrete path.
	t.Chdir(t.TempDir())
	missing := resolveMigrationsDir(filepath.Join(empty, "nowhere"))
	if !strings.HasSuffix(filepath.ToSlash(missing), "nowhere/migrations") {
		t.Fatalf("fallback should name the migrations dir: %q", missing)
	}
}

// engine.status: an unreachable Core is a false status,
// not an error the frontend renders as a failure banner on startup.
func TestEngineStatusOfflineYieldsFalseStatus(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.setupIPC()
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")

	status, err := app.router.Call(context.Background(), "engine.status", nil)
	if err != nil {
		t.Fatalf("engine.status must not fail offline: %v", err)
	}
	want := map[string]any{"frida": false, "miniapp": false, "devtools": false}
	got, ok := status.(map[string]any)
	if !ok || got["frida"] != false || got["miniapp"] != false || got["devtools"] != false {
		t.Fatalf("offline status: got %#v want %#v", status, want)
	}
}

// TestOfflineSmokeBasicMethods walks the calls the frontend makes on startup
// and on pages that do not require the engine: none of these may fail (or
// panic) just because WeChat is not connected.
func TestOfflineSmokeBasicMethods(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir() // keep hook.list out of the real AppData
	app.setupIPC()
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")

	ctx := context.Background()
	basic := map[string]string{
		"config.load":             `{}`,
		"config.save":             `{"cdp_port":62000}`,
		"settings.getPaths":       `{}`,
		"engine.status":           `{}`,
		"wechat.status":           `{}`,
		"extract.defaultDir":      `{}`,
		"extract.packages":        `{}`,
		"extract.builtinPatterns": `{}`,
		"wxapi.poll":              `{}`,
		"cloud.poll":              `{}`,
		"wxapi.clear":             `{}`,
		"cloud.clear":             `{}`,
		"hook.list":               `{}`,
		"hook.clear":              `{}`,
		"mcp.status":              `{}`,
		"cloudapi.status":         `{}`,
		"node.status":             `{}`,
		"node.detect":             `{}`,
		"electron.status":         `{}`,
		"electron.detect":         `{}`,
		"code.expandDir":          `{"path":"Z:/definitely-missing"}`,
		"code.readFile":           `{"path":"Z:/definitely-missing.js"}`,
		"code.search":             `{"root":"Z:/definitely-missing","query":"x"}`,
		"code.formatAll":          `{"root":"Z:/definitely-missing"}`,
	}
	for method, params := range basic {
		t.Run(method, func(t *testing.T) {
			result, err := app.router.Call(ctx, method, json.RawMessage(params))
			if err != nil {
				t.Fatalf("must degrade gracefully offline: %v", err)
			}
			if result == nil {
				t.Fatal("must return a payload")
			}
		})
	}
}

// startup must not leave the app unusable when storage fails (locked DB,
// wrong cwd for the migrations dir): the IPC router does not depend on
// storage and the traffic API must answer with a typed error, never panic.
func TestStartupStorageFailureKeepsRouterAndTypedErrors(t *testing.T) {
	app := NewApp()
	t.Setenv("WXTAP_MIGRATIONS_DIR", filepath.Join(t.TempDir(), "missing-migrations"))
	t.Setenv("WXTAP_TRAFFIC_DB", filepath.Join(t.TempDir(), "traffic.db"))
	// Keep the status poller from spawning a real Core: outside Wails the
	// resulting status emit would hit runtime.EventsEmit's fatal context check.
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")

	app.startup(context.Background())

	if _, err := app.TrafficList(api.TrafficListParams{}); err == nil {
		t.Fatal("traffic list must report a typed error when storage failed")
	}
	if _, err := app.TrafficGetBody(api.TrafficGetBodyParams{ID: "x"}); err == nil {
		t.Fatal("traffic getBody must report a typed error when storage failed")
	}
	if _, err := app.IngestTraffic(nil); err == nil {
		t.Fatal("ingest must report a typed error when storage failed")
	}

	status, err := app.router.Call(context.Background(), "engine.status", nil)
	if err != nil {
		t.Fatalf("IPC router must stay usable when storage failed: %v", err)
	}
	if status == nil {
		t.Fatal("IPC router must return a payload")
	}
}

// Concurrent cloudapi/mcp start-stop-status cycles: the server pointers are
// shared mutable state reached from independent Wails goroutines.
func TestCloudAPIMcpSSEStartStopConcurrency(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.setupIPC()
	// Keep the status poller from spawning a real Core: outside Wails the
	// resulting status emit would hit runtime.EventsEmit's fatal context check.
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		for _, call := range []struct {
			method string
			params string
		}{
			{"cloudapi.start", `{"port":0}`},
			{"cloudapi.status", `{}`},
			{"cloudapi.stop", `{}`},
			{"mcp.start", `{"port":0}`},
			{"mcp.status", `{}`},
			{"mcp.stop", `{}`},
		} {
			wg.Add(1)
			go func(method, params string) {
				defer wg.Done()
				_, _ = app.router.Call(ctx, method, json.RawMessage(params))
			}(call.method, call.params)
		}
	}
	wg.Wait()
}

// Wails v2's binder treats a context.Context parameter as a plain input, so
// any bound method taking one can never be called from the frontend with the
// right argument count. Only the lifecycle hooks (startup/shutdown) may take
// a context.
func TestBoundMethodsTakeNoContext(t *testing.T) {
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	appType := reflect.TypeOf(App{})
	lifecycle := map[string]bool{"startup": true, "shutdown": true, "beforeClose": true, "domReady": true}
	for i := 0; i < appType.NumMethod(); i++ {
		method := appType.Method(i)
		if lifecycle[method.Name] {
			continue
		}
		for j := 1; j < method.Type.NumIn(); j++ {
			if method.Type.In(j) == ctxType {
				t.Errorf("App.%s takes a context.Context parameter; drop it so the Wails binder sees the right argument count", method.Name)
			}
		}
	}
}

func TestAppInfoEventClearsTheTopBarWhenTheMiniappLeaves(t *testing.T) {
	info := &engine.AppInfo{AppID: "wxabc", Name: "示例"}
	payload, ok := appInfoEvent(nil, info)
	if !ok || payload["appid"] != "wxabc" || payload["name"] != "示例" {
		t.Fatalf("first identity = %#v, %v", payload, ok)
	}
	if _, ok := appInfoEvent(info, info); ok {
		t.Fatal("an unchanged identity must not emit again")
	}
	cleared, ok := appInfoEvent(info, nil)
	if !ok || cleared["appid"] != "" || cleared["name"] != "" {
		t.Fatalf("disconnect = %#v, %v", cleared, ok)
	}
	if _, ok := appInfoEvent(nil, nil); ok {
		t.Fatal("staying disconnected must not emit")
	}
}

// 版本号的唯一真源是 desktop/wails.json：前端构建期读它（vite.config.ts），
// 外壳运行期读它（version.go 的 embed），MCP 握手再由外壳注入。发版只改这一个
// 文件，所以这条用例守的是「它还能被解析出来」和「解析出来确实是版本号」。
func TestVersionComesFromTheEmbeddedWailsJSON(t *testing.T) {
	if !verifyEmbeddedVersion() {
		t.Fatalf("嵌入的 wails.json 未给出可用的版本号：appVersion=%q", appVersion)
	}
	product := versionFromWailsJSON(wailsJSON)
	if product == "" {
		t.Fatal("wails.json info.productVersion 为空或无法解析")
	}
	if appVersion != "v"+product {
		t.Fatalf("appVersion %q 不是由 wails.json 的 %q 推导而来", appVersion, product)
	}
	// 描述文件损坏时报空，而不是编一个看起来像发布号的版本。
	if got := versionFromWailsJSON([]byte("{ not json")); got != "" {
		t.Fatalf("损坏的描述文件应报空，实际 %q", got)
	}
}

func TestHookAutoInjectionTracksPageGeneration(t *testing.T) {
	last := engine.EngineStatus{Miniapp: true, Generation: 4}
	refreshed := engine.EngineStatus{Miniapp: true, Generation: 5}
	if !shouldAutoInjectHooks(last, refreshed) {
		t.Fatal("a setupContext generation change must reinstall hooks")
	}
	if shouldAutoInjectHooks(refreshed, refreshed) {
		t.Fatal("an unchanged generation must not replay old hook records")
	}
	if shouldAutoInjectHooks(refreshed, engine.EngineStatus{Miniapp: false, Generation: 5}) {
		t.Fatal("a disconnected miniapp cannot receive hooks")
	}
}

// The repository keeps resources/ at its root while `wails dev` runs
// desktop/build/bin/WxTap-dev.exe with desktop/ as the working directory.
// Without probing the parent of the working directory a dev build loses
// frida/config and skills/ — Core still finds them by
// deriving the checkout root from core/dist/cli.js, so the two would disagree
// on where the runtime assets live.
func TestResourceProbeRootsIncludeCheckoutRoot(t *testing.T) {
	root := t.TempDir()
	exeDir := filepath.Join(root, "desktop", "build", "bin")
	wd := filepath.Join(root, "desktop")
	roots := resourceProbeRootsFor(exeDir, wd)
	want := filepath.Join(root, "resources")
	found := false
	for _, candidate := range roots {
		if candidate == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("checkout resources root missing from probe roots: %#v", roots)
	}

	// The consumers have to agree with Core, which derives the same root from
	// core/dist/cli.js: the OSS sync target lives in the checkout resources/
	// directory.
	for _, dir := range []string{
		wd,
		filepath.Join(want, "frida"),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(wd)
	if got := resolveSyncBaseDir(); got != want {
		t.Fatalf("dev-layout sync root: got %q want %q", got, want)
	}
}

// skillsDirIn prefers the current name and falls back to the pre-rename
// mcp_skills/ so an installation that has not run the OSS sync since upgrading
// keeps serving its skills. Both paths are judged by content: the sync creates
// the directory before it lists the remote prefix, so a failed listing leaves an
// empty skills/ that must not shadow a populated legacy tree.
func TestSkillsDirPrefersCurrentNameWithLegacyFallback(t *testing.T) {
	base := t.TempDir()
	current := filepath.Join(base, "skills")
	legacy := filepath.Join(base, "mcp_skills")
	writeSkill := func(dir, name string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("# skill\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Nothing synced yet: return the current path so callers report one stable
	// location instead of an empty string.
	if got := skillsDirIn(base); got != current {
		t.Fatalf("nothing on disk: got %q want %q", got, current)
	}

	// Pre-rename installation: only the legacy directory holds skills.
	writeSkill(legacy, "session.md")
	if got := skillsDirIn(base); got != legacy {
		t.Fatalf("legacy only: got %q want %q", got, legacy)
	}

	// A sync that created skills/ but wrote nothing (its listing failed) and a
	// directory holding only the non-skill README are both empty of skills, so
	// neither may shadow the populated legacy tree.
	writeSkill(current, "README.md")
	if got := skillsDirIn(base); got != legacy {
		t.Fatalf("empty current (README only): got %q want %q", got, legacy)
	}
	if err := os.Remove(filepath.Join(current, "README.md")); err != nil {
		t.Fatal(err)
	}
	if got := skillsDirIn(base); got != legacy {
		t.Fatalf("empty current dir: got %q want %q", got, legacy)
	}

	// Once the current directory holds a skill it wins, legacy tree present or
	// not.
	writeSkill(current, "session.md")
	if got := skillsDirIn(base); got != current {
		t.Fatalf("both populated: got %q want %q", got, current)
	}
	if err := os.RemoveAll(legacy); err != nil {
		t.Fatal(err)
	}
	if got := skillsDirIn(base); got != current {
		t.Fatalf("current only: got %q want %q", got, current)
	}
}

// Production packaging stages the WxTap layout (core, frontend, resources).
func TestProductionBuildScriptStagesWxTapLayout(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "build-wails.ps1")
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read scripts/build-wails.ps1: %v", err)
	}
	lower := strings.ToLower(string(data))
	for _, required := range []string{"core", "frontend", "resources", "frida", "migrations", "wails"} {
		if !strings.Contains(lower, strings.ToLower(required)) {
			t.Fatalf("scripts/build-wails.ps1 should stage %q", required)
		}
	}
	// Node comes from the user's PATH; bundling one again would silently
	// re-add the ~33MB the release deliberately dropped.
	if strings.Contains(lower, "node.exe") {
		t.Fatal("scripts/build-wails.ps1 must not bundle node.exe")
	}
}

// The macOS release ships as WxTap.app: the executable lives in
// Contents/MacOS and every runtime asset in Contents/Resources. The probes
// must find core/ and migrations/ there, or a packaged mac app degrades to
// "engine not running" even though the bundle is complete. Node is not among
// them: it comes from the user's PATH.
func TestBundleLayoutProbesMacOSResources(t *testing.T) {
	t.Setenv("WXTAP_CORE_SCRIPT", "")
	t.Setenv("WXTAP_MIGRATIONS_DIR", "")

	root := t.TempDir()
	macOS := filepath.Join(root, "WxTap.app", "Contents", "MacOS")
	resources := filepath.Join(root, "WxTap.app", "Contents", "Resources")
	for _, dir := range []string{
		filepath.Join(resources, "core", "dist"),
		filepath.Join(resources, "migrations"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(resources, "core", "dist", "cli.js"), []byte("// core"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := resolveCoreScript(macOS); got != filepath.Join(resources, "core", "dist", "cli.js") {
		t.Fatalf("bundle core probe: got %q", got)
	}
	if got := resolveMigrationsDir(macOS); got != filepath.Join(resources, "migrations") {
		t.Fatalf("bundle migrations probe: got %q", got)
	}
}

// resourceProbeRoots must offer the bundle Resources directory so frida/ and
// skills/ resolve inside WxTap.app.
func TestResourceProbeRootsIncludeBundleResources(t *testing.T) {
	root := t.TempDir()
	macOS := filepath.Join(root, "WxTap.app", "Contents", "MacOS")
	roots := resourceProbeRootsFor(macOS, "")
	wantBundle := filepath.Join(root, "WxTap.app", "Contents", "Resources")
	found := false
	for _, candidate := range roots {
		if candidate == wantBundle {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("bundle Resources missing from probe roots: %#v", roots)
	}
}

// The macOS build script must produce a bundle the Go probes can consume:
// WxTap.app with core/, resources/ and migrations/ inside Contents/Resources,
// plus the same path guard the PowerShell script has.
func TestMacOSBuildScriptStagesBundleLayout(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "scripts", "build-wails.sh"))
	if err != nil {
		t.Fatalf("read scripts/build-wails.sh: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"wails_platform=\"darwin/",
		"WxTap.app",
		"Contents/Resources",
		"core/dist",
		"core/hooks",
		"core/node_modules",
		"resources",
		"migrations",
		"refusing to clean",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("scripts/build-wails.sh should stage %q", required)
		}
	}
	// Node comes from the user's PATH; bundling one again would silently
	// re-add the ~33MB the release deliberately dropped.
	if strings.Contains(text, "bundle_resources/runtime") {
		t.Fatal("scripts/build-wails.sh must not bundle a Node runtime")
	}
	// The mac bundle ships the same pruned dependency set as the Windows one.
	// Copying core/node_modules straight out of the working tree would ship the
	// ~100MB dev toolchain (typescript, esbuild, eslint) plus native binaries
	// built for whatever host ran the script, none of which the Core imports.
	for _, required := range []string{"build/core-stage", "--omit=dev"} {
		if !strings.Contains(text, required) {
			t.Fatalf("scripts/build-wails.sh should stage the pruned Core (%q)", required)
		}
	}
	if strings.Contains(text, `cp -R "$root/core/node_modules"`) {
		t.Fatal("scripts/build-wails.sh must copy the staged node_modules, not the dev tree")
	}
}

// Shutdown must drain tracked async work before closing the database: an
// export or scan that is mid-write would otherwise race repo.Close and lose
// its output (or hit a closed handle).
func TestShutdownWaitsForBackgroundWork(t *testing.T) {
	app := NewApp()
	release := make(chan struct{})
	finished := make(chan struct{})
	app.goBackground(func() {
		<-release
		close(finished)
	})

	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()

	app.shutdown(context.Background())
	select {
	case <-finished:
	default:
		t.Fatal("shutdown returned before background work completed")
	}
}

// The MCP status hint names this build's stdout launch so an operator can copy
// it verbatim; the binary name differs per platform.
func TestMCPCommandHintUsesRunningBinaryName(t *testing.T) {
	hint := mcpCommandHint()
	if !strings.HasSuffix(hint, " -mcp") {
		t.Fatalf("hint should end with -mcp: %q", hint)
	}
	name := filepath.Base(os.Args[0])
	if !strings.HasPrefix(hint, name) {
		t.Fatalf("hint %q should start with the running binary name %q", hint, name)
	}
}

// macOS ships Electron as an app bundle; exec'ing the bundle path fails.
// The launcher must go through `open -a` for bundles while keeping the plain
// binary path working on Windows/Linux.
func TestElectronLaunchResolvesMacBundles(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "Electron.app")
	inner := filepath.Join(bundle, "Contents", "MacOS")
	if err := os.MkdirAll(inner, 0o750); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(inner, "Electron")
	if err := os.WriteFile(binary, []byte("#!/bin/sh"), 0o700); err != nil {
		t.Fatal(err)
	}

	name, args, err := electronLaunch("darwin", bundle, "/s.js", "devtools://x")
	if err != nil {
		t.Fatalf("bundle launch: %v", err)
	}
	if name != "open" {
		t.Fatalf("bundle should launch through open, got %q", name)
	}
	want := []string{"-a", bundle, "--args", "/s.js", "devtools://x"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("bundle args: %#v want %#v", args, want)
	}

	name, args, err = electronLaunch("darwin", inner, "/s.js", "devtools://x")
	if err != nil {
		t.Fatalf("inner dir launch: %v", err)
	}
	if name != binary {
		t.Fatalf("inner dir should resolve the binary, got %q", name)
	}
	if !reflect.DeepEqual(args, []string{"/s.js", "devtools://x"}) {
		t.Fatalf("inner dir args: %#v", args)
	}
}

func TestDevtoolsInspectorURLPinsASafeTarget(t *testing.T) {
	if got := devtoolsInspectorURL(31415, ""); got != "devtools://devtools/bundled/inspector.html?ws=127.0.0.1:31415" {
		t.Fatalf("browser url: %s", got)
	}
	if got := devtoolsInspectorURL(31415, "page-1"); got != "devtools://devtools/bundled/inspector.html?ws=127.0.0.1:31415/devtools/page/page-1" {
		t.Fatalf("target url: %s", got)
	}
	if got := devtoolsInspectorURL(31415, "bad id"); !strings.HasSuffix(got, "ws=127.0.0.1:31415") || strings.Contains(got, "chii_app") {
		t.Fatalf("unsafe target should stay on the devtools page: %s", got)
	}
}

func TestElectronLaunchKeepsPlainBinaryPath(t *testing.T) {
	name, args, err := electronLaunch("windows", `C:\electron\electron.exe`, "/s.js", "devtools://x")
	if err != nil {
		t.Fatalf("windows launch: %v", err)
	}
	if name != `C:\electron\electron.exe` || !reflect.DeepEqual(args, []string{"/s.js", "devtools://x"}) {
		t.Fatalf("windows launch: %q %#v", name, args)
	}

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "electron.cmd")
	exePath := filepath.Join(dir, "dist", "electron.exe")
	if err := os.MkdirAll(filepath.Dir(exePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte("@echo off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, args, err = electronLaunch("windows", scriptPath, "/s.js", "http://127.0.0.1/chii_app.html?ws=127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if name != exePath || len(args) != 2 {
		t.Fatalf("cmd wrapper should run the exe: %q %#v", name, args)
	}
	if _, _, err := electronLaunch("windows", filepath.Join(t.TempDir(), "electron.cmd"), "/s.js", "u"); err == nil {
		t.Fatal("a command script with no electron.exe must fail")
	}

	standalone := filepath.Join(t.TempDir(), "electron")
	if err := os.WriteFile(standalone, []byte("#!/bin/sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	name, args, err = electronLaunch("darwin", standalone, "/s.js", "u")
	if err != nil {
		t.Fatalf("darwin binary launch: %v", err)
	}
	if name != standalone || !reflect.DeepEqual(args, []string{"/s.js", "u"}) {
		t.Fatalf("darwin binary launch: %q %#v", name, args)
	}
}

// Revealing a path in the system file manager must follow the platform:
// explorer on Windows, open on macOS, xdg-open elsewhere.
func TestFileManagerCommandPerPlatform(t *testing.T) {
	cases := []struct {
		goos    string
		command string
	}{
		{"windows", "explorer"},
		{"darwin", "open"},
		{"linux", "xdg-open"},
	}
	for _, tc := range cases {
		command, args := fileManagerCommand(tc.goos, "/tmp/out")
		if command != tc.command {
			t.Fatalf("%s: command %q want %q", tc.goos, command, tc.command)
		}
		if !reflect.DeepEqual(args, []string{"/tmp/out"}) {
			t.Fatalf("%s: args %#v", tc.goos, args)
		}
	}
}

// App.Call is the single Wails-bound entry the frontend uses; every view
// depends on its envelope shape, so the contract is pinned here.
func TestCallEnvelopeContract(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.setupIPC()

	// Success: {"result": ...} with no "error" key.
	ok := app.Call("config.load", `{}`)
	var okEnvelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(ok), &okEnvelope); err != nil {
		t.Fatalf("success envelope is not JSON: %v (%s)", err, ok)
	}
	if _, hasResult := okEnvelope["result"]; !hasResult {
		t.Fatalf("success envelope missing result: %s", ok)
	}
	if _, hasError := okEnvelope["error"]; hasError {
		t.Fatalf("success envelope carries an error: %s", ok)
	}

	// Empty params must default to {} instead of failing to parse.
	if got := app.Call("config.load", ""); got == "" || got[0] != '{' {
		t.Fatalf("empty params should still dispatch: %q", got)
	}

	// Failure: {"error": "..."} with no "result" key.
	bad := app.Call("definitely.not.a.method", `{}`)
	var badEnvelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(bad), &badEnvelope); err != nil {
		t.Fatalf("error envelope is not JSON: %v (%s)", err, bad)
	}
	if _, hasError := badEnvelope["error"]; !hasError {
		t.Fatalf("error envelope missing error: %s", bad)
	}
	if _, hasResult := badEnvelope["result"]; hasResult {
		t.Fatalf("error envelope carries a result: %s", bad)
	}
}

// retryable 是前端「重试」按钮的开关（stores/engine.ts 的 retryableOf）。Core
// 侧失败自带类型化的 Retryable，而它渲染出来的文案（"core error 1002: core call
// timed out: ..."）不匹配任何关键词规则 —— 拿文案去猜等于把这批错误的重试入口
// 一并关掉，而这恰恰是最该重试的一类（引擎没了、连接断了）。
func TestCallEnvelopeCarriesTheTypedRetryableFlag(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.setupIPC()
	app.router.Register("test.coreDown", func(context.Context, json.RawMessage) (any, error) {
		return nil, &rpc.CoreError{Code: 1002, Message: "core call timed out: engine.start", Retryable: true}
	})
	app.router.Register("test.coreRejected", func(context.Context, json.RawMessage) (any, error) {
		return nil, &rpc.CoreError{Code: 1000, Message: "no miniapp connected", Retryable: false}
	})
	app.router.Register("test.contextTimeout", func(context.Context, json.RawMessage) (any, error) {
		return nil, context.DeadlineExceeded
	})

	retryable := func(method string) bool {
		t.Helper()
		var envelope struct {
			Error struct {
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(app.Call(method, `{}`)), &envelope); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return envelope.Error.Retryable
	}

	if !retryable("test.coreDown") {
		t.Fatal("a typed retryable Core error must reach the UI as retryable")
	}
	if retryable("test.coreRejected") {
		t.Fatal("a typed non-retryable Core error must not be marked retryable")
	}
	// 没有类型信息的失败仍按文案判断 —— 引擎不可用时的 context 超时同样值得重试。
	if !retryable("test.contextTimeout") {
		t.Fatal("an untyped deadline failure must stay retryable")
	}
}

func TestEngineStatusUsesUnifiedServiceShapeWhenOffline(t *testing.T) {
	app := NewApp()
	app.setupIPC()
	result, err := app.router.Call(context.Background(), "engine.status", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	status := result.(map[string]any)
	if status["kind"] != "engine" || (status["state"] != "offline" && status["state"] != "stopped") || status["available"] != false {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestLocalServiceStatusesUseUnifiedShape(t *testing.T) {
	app := NewApp()
	app.setupIPC()
	for _, method := range []string{"cloudapi.status", "mcp.status"} {
		result, err := app.router.Call(context.Background(), method, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		status := result.(map[string]any)
		if status["kind"] == nil || status["state"] == nil || status["available"] == nil {
			t.Fatalf("%s lacks unified fields: %#v", method, status)
		}
	}
}

// Handlers that answer without a payload must still produce a valid envelope
// (the frontend reads result?.ok).
func TestCallEnvelopeForNilResult(t *testing.T) {
	app := NewApp()
	app.router = ipc.New()
	app.router.Register("test.nil", func(context.Context, json.RawMessage) (any, error) {
		return nil, nil
	})

	if got := app.Call("test.nil", `{}`); got != `{"result": {"ok": true}}` {
		t.Fatalf("nil result envelope: %s", got)
	}
}

// The frontend keeps its own allow-list of backend methods. Drift in either
// direction is a silent runtime break ("unsupported backend method" or a
// dead backend surface), so both directions are pinned against the router.
func TestFrontendMethodAllowListMatchesRouter(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(root, "desktop", "frontend", "src", "api", "bridge.ts"))
	if err != nil {
		t.Fatalf("read bridge.ts: %v", err)
	}
	frontend := parseSupportedMethods(string(source))
	if len(frontend) == 0 {
		t.Fatal("no supportedMethods entries parsed from bridge.ts")
	}

	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	registered := map[string]bool{}
	for _, method := range app.router.Methods() {
		registered[method] = true
	}
	for method := range frontend {
		if !registered[method] {
			t.Errorf("frontend calls %q but the backend does not register it", method)
		}
	}

	// Backend-only methods: surfaces the CLI/MCP entry points use without a
	// Vue caller, plus the frozen v0.1.0 surface (testdata/compat-surface.json).
	// Keep this list explicit and reviewed.
	backendOnly := map[string]bool{
		"engine.status": true,
		// Compat surface: registered without a Vue caller (CLI/MCP clients use them).
		// navigator.currentRoute / getBlockedRedirects are superseded by guardState,
		// and pageStack's Vue caller (the page-stack panel) is gone — the runtime
		// stack is read through the IPC method / MCP navigator_page_stack only.
		"hook.clear":                    true,
		"miniapp.getLock":               true,
		"miniapp.setLock":               true,
		"miniapp.switch":                true,
		"navigator.currentRoute":        true,
		"navigator.getBlockedRedirects": true,
		"navigator.getCurrentRoute":     true,
		"navigator.pageStack":           true,
		"targets.attach":                true,
	}
	for method := range registered {
		if frontend[method] || backendOnly[method] {
			continue
		}
		t.Errorf("backend registers %q but the frontend allow-list omits it", method)
	}
}

// parseSupportedMethods extracts the string literals of the
// `new Set([...])` allow-list in bridge.ts.
func parseSupportedMethods(source string) map[string]bool {
	start := strings.Index(source, "const supportedMethods = new Set([")
	if start < 0 {
		return nil
	}
	end := strings.Index(source[start:], "]);")
	if end < 0 {
		return nil
	}
	block := source[start : start+end]
	methods := map[string]bool{}
	for _, quote := range []string{"'", "`"} {
		for rest := block; ; {
			open := strings.Index(rest, quote)
			if open < 0 {
				break
			}
			closeIdx := strings.Index(rest[open+1:], quote)
			if closeIdx < 0 {
				break
			}
			name := rest[open+1 : open+1+closeIdx]
			if name != "" && !strings.ContainsAny(name, " \n\t") {
				methods[name] = true
			}
			rest = rest[open+1+closeIdx+1:]
		}
	}
	return methods
}

// Go is the only caller of the Core RPC surface, and the Core rejects unknown
// methods at runtime. Pin the client's method names against the Core's
// dispatch table so a rename on either side fails here instead of on a user's
// machine with "unknown method".
func TestCoreRPCMethodNamesAreHandledByCore(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	clientSource, err := os.ReadFile(filepath.Join(root, "desktop", "internal", "engine", "client.go"))
	if err != nil {
		t.Fatalf("read client.go: %v", err)
	}
	appSource, err := os.ReadFile(filepath.Join(root, "core", "src", "app.ts"))
	if err != nil {
		t.Fatalf("read app.ts: %v", err)
	}

	callers := parseRPCNames(string(clientSource))
	if len(callers) == 0 {
		t.Fatal("no Core RPC method names parsed from client.go")
	}
	handled := parseRPCCases(string(appSource))
	for method := range callers {
		if !handled[method] {
			t.Errorf("engine client calls %q but core/src/app.ts does not handle it", method)
		}
	}
}

// parseRPCNames collects `c.call(ctx, "domain.method"` style literals.
func parseRPCNames(source string) map[string]bool {
	names := map[string]bool{}
	for rest := source; ; {
		idx := strings.Index(rest, "c.call(ctx, \"")
		if idx < 0 {
			break
		}
		rest = rest[idx+len("c.call(ctx, \""):]
		end := strings.Index(rest, "\"")
		if end < 0 {
			break
		}
		if name := rest[:end]; strings.Contains(name, ".") {
			names[name] = true
		}
		rest = rest[end:]
	}
	return names
}

// parseRPCCases collects `case "domain.method":` labels.
func parseRPCCases(source string) map[string]bool {
	names := map[string]bool{}
	for rest := source; ; {
		idx := strings.Index(rest, "case \"")
		if idx < 0 {
			break
		}
		rest = rest[idx+len("case \""):]
		end := strings.Index(rest, "\"")
		if end < 0 {
			break
		}
		if name := rest[:end]; strings.Contains(name, ".") {
			names[name] = true
		}
		rest = rest[end:]
	}
	return names
}

// Shutdown must be idempotent and must stop the status poller before draining
// tracked work; a poller that keeps adding to the WaitGroup while shutdown
// waits would trip the runtime's WaitGroup misuse check.
func TestShutdownIdempotentWithPollerRunning(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC() // starts the status poller

	if app.pollerCancel == nil {
		t.Fatal("status poller should be cancellable after setup")
	}
	done := make(chan struct{})
	go func() {
		app.shutdown(context.Background())
		app.shutdown(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not complete")
	}
	if app.pollerCancel != nil {
		t.Fatal("shutdown should clear the poller cancel func")
	}
}

// config.save must reject malformed JSON instead of persisting a partially
// decoded patch over the user's settings.
func TestConfigSaveRejectsMalformedParams(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	store := ipc.NewConfigStore(t.TempDir())
	if err := store.Save(context.Background(), map[string]any{"cdp_port": float64(62000)}); err != nil {
		t.Fatal(err)
	}
	app.configStore = store
	before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := app.router.Call(context.Background(), "config.save", json.RawMessage(`{"cdp_port":`)); err == nil {
		t.Fatal("malformed config.save must fail")
	}
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("config changed despite a rejected save: before=%v after=%v", before, after)
	}

	// A valid patch still merges.
	if _, err := app.router.Call(context.Background(), "config.save", json.RawMessage(`{"theme":"dark"}`)); err != nil {
		t.Fatalf("valid save: %v", err)
	}
	merged, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if merged["theme"] != "dark" || merged["cdp_port"] != float64(62000) {
		t.Fatalf("merge lost keys: %v", merged)
	}
}

// Operators need to find the persisted logs from the UI (the E2E gate asks
// for Core stderr logs as evidence).
func TestSettingsExposeLogDir(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	result, err := app.router.Call(context.Background(), "settings.getPaths", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("settings.getPaths: %v", err)
	}
	paths, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected payload: %#v", result)
	}
	logDir, _ := paths["logDir"].(string)
	if logDir == "" {
		t.Fatalf("logDir missing from settings: %#v", paths)
	}
	if !strings.HasSuffix(filepath.ToSlash(logDir), "logs") {
		t.Fatalf("logDir should point at the logs folder: %q", logDir)
	}
}

// Startup must leave a trace on disk: the E2E gate collects these files as
// evidence, and a crash before the UI is up is exactly when they matter most.
func TestStartupWritesLogFile(t *testing.T) {
	base := t.TempDir()
	t.Setenv("WXTAP_DATA_DIR", base)
	t.Setenv("WXTAP_MIGRATIONS_DIR", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("WXTAP_TRAFFIC_DB", filepath.Join(base, "traffic.db"))
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")

	app := NewApp()
	app.startup(context.Background())
	defer app.shutdown(context.Background())

	if app.logger == nil {
		t.Fatal("startup should open the operation log")
	}
	path := newestLogFile(t, filepath.Join(base, "logs"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "WxTap 启动") {
		t.Fatalf("startup line missing from %s: %s", path, data)
	}
	if !strings.Contains(string(data), "存储初始化失败") {
		t.Fatalf("storage failure should be recorded: %s", data)
	}
}

// wantDefaultUserBaseDir mirrors userBaseDir's platform split, so these tests
// assert the contract of the platform they run on instead of the Windows one.
//
// User-created data staying with the running executable is the Windows answer:
// a portable release must not silently write configuration, logs, databases or
// extracted source into the roaming profile. macOS cannot use the executable's
// directory at all — that is Contents/MacOS inside the .app bundle, read-only
// for a run from a mounted .dmg and signature-breaking to write — so it uses
// Apple's per-user location instead. WXTAP_DATA_DIR overrides both.
func wantDefaultUserBaseDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return exeDir()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home directory: %v", err)
	}
	return filepath.Join(home, "Library", "Application Support", "WxTap")
}

func TestUserBaseDirDefaultsToExecutableDirectoryUnlessOverridden(t *testing.T) {
	base, err := userBaseDir()
	if err != nil {
		t.Fatalf("default user base dir: %v", err)
	}
	if want := wantDefaultUserBaseDir(t); base != want {
		t.Fatalf("default user base dir = %q, want %q", base, want)
	}

	override := t.TempDir()
	t.Setenv("WXTAP_DATA_DIR", override)
	base, err = userBaseDir()
	if err != nil {
		t.Fatalf("overridden user base dir: %v", err)
	}
	if base != override {
		t.Fatalf("overridden user base dir = %q, want %q", base, override)
	}
}

func TestResolveDBPathDefaultsToExecutableDirectoryUnlessOverridden(t *testing.T) {
	if got, want := resolveDBPath(), filepath.Join(wantDefaultUserBaseDir(t), "traffic.db"); got != want {
		t.Fatalf("default database path = %q, want %q", got, want)
	}

	override := filepath.Join(t.TempDir(), "custom.db")
	t.Setenv("WXTAP_TRAFFIC_DB", override)
	if got := resolveDBPath(); got != override {
		t.Fatalf("overridden database path = %q, want %q", got, override)
	}
}

func TestWebviewUserDataDirUsesExecutableDirectory(t *testing.T) {
	if got, want := webviewUserDataDir(), filepath.Join(wantDefaultUserBaseDir(t), "webview"); got != want {
		t.Fatalf("webview user-data directory = %q, want %q", got, want)
	}
}
