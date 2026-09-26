package ipc

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeTestPNG(t *testing.T, path string, size int) {
	writeTestPNGRect(t, path, size, size)
}

func writeTestPNGRect(t *testing.T, path string, width, height int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 120, G: 80, B: 220, A: 255})
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		t.Fatal(err)
	}
}

// buildPlainWxapkg assembles an unencrypted wxapkg fixture with absolute offsets.
func buildPlainWxapkg(files map[string][]byte) []byte {
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

func TestFindPackagesGroupsByAppID(t *testing.T) {
	root := t.TempDir()
	appA := filepath.Join(root, "wxaaaa111111111111")
	appB := filepath.Join(root, "wxbbbb222222222222")
	if err := os.MkdirAll(filepath.Join(appA, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appA, "main.wxapkg"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appA, "sub", "page.wxapkg"), []byte("AA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appB, "main.wxapkg"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray.wxapkg"), []byte("X"), 0o644); err != nil {
		t.Fatal(err)
	}

	packages := FindPackages(root)
	if len(packages) != 3 {
		t.Fatalf("expected 3 packages, got %d: %+v", len(packages), packages)
	}
	byApp := map[string]int{}
	for _, pkg := range packages {
		byApp[pkg.AppID]++
		if pkg.Size <= 0 || pkg.Name == "" {
			t.Fatalf("package metadata missing: %+v", pkg)
		}
	}
	if byApp["wxaaaa111111111111"] != 2 || byApp["wxbbbb222222222222"] != 1 {
		t.Fatalf("grouping wrong: %v", byApp)
	}
}

func TestBuildInventorySeparatesIndexPackagesAndOutputs(t *testing.T) {
	items, summary := BuildInventory("", []PkgFile{{AppID: "wxpackage00000001", Path: "main.wxapkg", Mtime: 41}}, []CodeProject{{AppID: "wxpackage00000001", Name: "Output", Subject: "演示主体", Path: "out", Mtime: 55}})
	if summary.IndexedCount != 0 || summary.PackageCount != 1 || summary.OutputCount != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(items) != 1 || items[0].Status != "decompiled" {
		t.Fatalf("items = %#v", items)
	}
	if items[0].Mtime != 55 || items[0].Subject != "演示主体" {
		t.Fatalf("display metadata missing: %#v", items)
	}
}

func TestBuildInventoryOnlyIncludesOutputForCurrentPackageSet(t *testing.T) {
	items, summary := BuildInventory("", []PkgFile{{AppID: "wxcurrent000000001", Path: "main.wxapkg"}}, []CodeProject{
		{AppID: "wxcurrent000000001", Path: "current-output"},
		{AppID: "wxother00000000001", Path: "other-user-output"},
	})

	if summary.PackageCount != 1 || summary.OutputCount != 1 {
		t.Fatalf("summary = %#v, want one current package and output", summary)
	}
	if len(items) != 1 || items[0].AppID != "wxcurrent000000001" || items[0].OutputDir != "current-output" {
		t.Fatalf("items = %#v, want only the selected user's package", items)
	}
}

func TestBuildInventoryIncludesLocalIconMetadata(t *testing.T) {
	items, _ := BuildInventory("", []PkgFile{{AppID: "wxmeta00000000001", Path: "main.wxapkg"}}, []CodeProject{{
		AppID: "wxmeta00000000001", Path: "out", IconPath: "static/appicon.png", IconDataURL: "data:image/png;base64,abc", IconConfidence: "candidate", MetadataSource: "local_package",
	}})
	if len(items) != 1 || items[0].IconPath != "static/appicon.png" || items[0].IconConfidence != "candidate" || items[0].IconDataURL == "" {
		t.Fatalf("inventory metadata = %#v", items)
	}
}

func TestDecompileAndScanEndToEnd(t *testing.T) {
	base := t.TempDir()
	pkgDir := filepath.Join(base, "wxtest1111111111111")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`var phone = '13812345678'; var api = "https://api.test.com/v1";` +
		strings.Repeat(" var filler = 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx';", 40))
	wxapkg := buildPlainWxapkg(map[string][]byte{"app.js": payload})
	if err := os.WriteFile(filepath.Join(pkgDir, "main.wxapkg"), wxapkg, 0o644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(base, "out", "wxtest1111111111111")
	packages := FindPackages(base)
	if len(packages) != 1 {
		t.Fatalf("fixture packages: %d", len(packages))
	}
	extracted, err := Decompile(packages[0].Path, outDir, packages[0].AppID)
	if err != nil {
		t.Fatalf("decompile: %v", err)
	}
	if len(extracted) != 1 {
		t.Fatalf("extracted %d files", len(extracted))
	}

	report, err := Scan(outDir, packages[0].AppID, "", nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.Summary["mobile"] != 1 || report.Summary["url"] == 0 {
		t.Fatalf("scan summary wrong: %+v", report.Summary)
	}
}

func TestDefaultPackagesDirRespectsEnvOverride(t *testing.T) {
	t.Setenv("WXTAP_PACKAGES_DIR", filepath.Join("custom", "packages"))
	dir := DefaultPackagesDir()
	if !strings.HasSuffix(dir, filepath.Join("custom", "packages")) {
		t.Fatalf("env override ignored: %s", dir)
	}
}

func TestBuiltinPatternsExposed(t *testing.T) {
	patterns := BuiltinPatterns()
	if len(patterns) < 13 {
		t.Fatalf("builtin patterns missing: %d", len(patterns))
	}
	found := map[string]bool{}
	for _, p := range patterns {
		found[p.Key] = true
		if p.Label == "" {
			t.Fatalf("label missing for %s", p.Key)
		}
	}
	for _, want := range []string{"sfz", "mobile", "secret", "oss", "url"} {
		if !found[want] {
			t.Fatalf("pattern %s missing", want)
		}
	}
}

func TestFindPackagesFillsMtime(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "wxmtime11111111111")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "main.wxapkg"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}

	packages := FindPackages(root)
	if len(packages) != 1 {
		t.Fatalf("packages: %d", len(packages))
	}
	if packages[0].Mtime <= 0 {
		t.Fatalf("mtime not filled: %+v", packages[0])
	}
}

func TestReadMiniAppNamePrefersAppname(t *testing.T) {
	dir := t.TempDir()
	if name := ReadMiniAppName(dir); name != "" {
		t.Fatalf("missing config: %q", name)
	}
	if err := os.WriteFile(filepath.Join(dir, "app-config.json"), []byte(`{"appname":"演示小程序","name":"fallback"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if name := ReadMiniAppName(dir); name != "演示小程序" {
		t.Fatalf("appname not preferred: %q", name)
	}
	if err := os.WriteFile(filepath.Join(dir, "app-config.json"), []byte(`{"name":"fallback"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if name := ReadMiniAppName(dir); name != "fallback" {
		t.Fatalf("name fallback: %q", name)
	}
	if err := os.WriteFile(filepath.Join(dir, "app-config.json"), []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if name := ReadMiniAppName(dir); name != "" {
		t.Fatalf("corrupt config: %q", name)
	}
}

func TestReadMiniAppNameRejectsUnverifiedRuntimeAppName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app-config.json"), []byte(`{"pages":["pages/index"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app-service.js"), []byte(`const appInfo = { appName: "智慧能源管家" };`), 0o644); err != nil {
		t.Fatal(err)
	}
	if name := ReadMiniAppName(dir); name != "" {
		t.Fatalf("unverified runtime appName must be ignored, got %q", name)
	}
}

func TestReadMiniAppSubjectUsesOnlyDeclaredMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app-config.json"), []byte(`{"appname":"演示小程序","subject":"演示主体"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if subject := ReadMiniAppSubject(dir); subject != "演示主体" {
		t.Fatalf("subject = %q", subject)
	}
	if err := os.WriteFile(filepath.Join(dir, "app-config.json"), []byte(`{"appname":"演示小程序"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if subject := ReadMiniAppSubject(dir); subject != "" {
		t.Fatalf("missing subject must stay empty, got %q", subject)
	}
}

func TestReadMiniAppSubjectReadsDeclaredCompanyNameFromAppJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"companyName":"示例科技有限公司"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if subject := ReadMiniAppSubject(dir); subject != "示例科技有限公司" {
		t.Fatalf("subject = %q, want 示例科技有限公司", subject)
	}
}

func TestReadMiniAppMetadataPrefersDeclaredIcon(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestPNG(t, filepath.Join(dir, "assets", "app.png"), 32)
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"iconPath":"assets/app.png"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ReadMiniAppMetadata(dir)
	if got.IconPath != "assets/app.png" || got.IconConfidence != "declared" || got.IconDataURL == "" {
		t.Fatalf("metadata = %#v", got)
	}
}

func TestReadMiniAppCachedIconUsesExactAppIDCachePath(t *testing.T) {
	user := t.TempDir()
	appID := "wxdeadbeefdeadbeef"
	path := filepath.Join(user, "applet", "local", appID, "store", "images")
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestPNG(t, filepath.Join(path, appID), 48)
	got := ReadMiniAppCachedIcon(user, appID)
	if got.IconConfidence != "cached" || got.MetadataSource != "wechat_cache" || got.IconDataURL == "" {
		t.Fatalf("cached metadata = %#v", got)
	}
}

func TestReadMiniAppMetadataRejectsEscapingDeclaredIcon(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"iconPath":"../../secret.png"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadMiniAppMetadata(dir); got.IconPath != "" {
		t.Fatalf("metadata = %#v", got)
	}
}

func TestReadMiniAppMetadataDoesNotGuessFromImageCandidates(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestPNG(t, filepath.Join(dir, "static", "appicon.png"), 64)
	got := ReadMiniAppMetadata(dir)
	if got.IconPath != "" || got.IconConfidence != "" || got.IconDataURL != "" {
		t.Fatalf("metadata = %#v", got)
	}
}

func TestReadMiniAppMetadataRejectsTiedCandidates(t *testing.T) {
	dir := t.TempDir()
	writeTestPNG(t, filepath.Join(dir, "logo-a.png"), 64)
	writeTestPNG(t, filepath.Join(dir, "logo-b.png"), 64)
	if got := ReadMiniAppMetadata(dir); got.IconPath != "" || got.IconDataURL != "" {
		t.Fatalf("metadata = %#v", got)
	}
}

func TestReadMiniAppMetadataDoesNotGuessFromRuntimeIconReference(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestPNGRect(t, filepath.Join(dir, "brand.png"), 48, 64)
	if err := os.WriteFile(filepath.Join(dir, "app-service.js"), []byte(`const icon = "brand.png";`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ReadMiniAppMetadata(dir)
	if got.IconPath != "" || got.IconConfidence != "" {
		t.Fatalf("metadata = %#v", got)
	}
}

func TestReadMiniAppMetadataDoesNotGuessWebPIconCandidate(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 30)
	copy(data[0:4], "RIFF")
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8X")
	data[20] = 63
	data[23] = 63
	data[24], data[25], data[26] = 63, 0, 0
	data[27], data[28], data[29] = 63, 0, 0
	if err := os.WriteFile(filepath.Join(dir, "appicon.webp"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	got := ReadMiniAppMetadata(dir)
	if got.IconPath != "" || got.IconDataURL != "" {
		t.Fatalf("metadata = %#v", got)
	}
}

// The default packages directory drives "extract without an explicit dir":
// picking the wrong user directory unpacks the wrong miniapp, so the
// selection rules are pinned here.
func TestNewestUserPackagesDirPicksNewestEligibleUser(t *testing.T) {
	users := t.TempDir()
	makeUser := func(name string, mod time.Time) string {
		pkg := filepath.Join(users, name, "applet", "packages")
		if err := os.MkdirAll(pkg, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(pkg, mod, mod); err != nil {
			t.Fatal(err)
		}
		return pkg
	}
	old := makeUser("user-a", time.Now().Add(-2*time.Hour))
	newer := makeUser("user-b", time.Now().Add(-1*time.Hour))

	// A package-less account must never win.
	packageLess := filepath.Join(users, "user-c")
	if err := os.MkdirAll(packageLess, 0o750); err != nil {
		t.Fatal(err)
	}

	dirs := []string{filepath.Join(users, "user-a"), filepath.Join(users, "user-b"), packageLess}
	if got := newestUserPackagesDir(dirs); got != newer {
		t.Fatalf("newest user packages: got %q want %q (old=%q)", got, newer, old)
	}
	if got := newestUserPackagesDir([]string{filepath.Join(users, "missing")}); got != "" {
		t.Fatalf("missing users dir should yield empty, got %q", got)
	}
	if got := newestUserPackagesDir([]string{packageLess}); got != "" {
		t.Fatalf("package-less account should yield empty, got %q", got)
	}
}

func TestDirExists(t *testing.T) {
	dir := t.TempDir()
	if !dirExists(dir) {
		t.Fatalf("temp dir should exist")
	}
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if dirExists(file) {
		t.Fatalf("a file is not a directory")
	}
	if dirExists(filepath.Join(dir, "nope")) {
		t.Fatalf("missing path should not exist")
	}
}

// DefaultPackagesDir: the env override wins outright, and the Windows
// layout (per-user radium dir, then the flat fallback) is probed in order.
func TestDefaultPackagesDirHonorsOverrideAndWindowsLayout(t *testing.T) {
	override := t.TempDir()
	t.Setenv("WXTAP_PACKAGES_DIR", override)
	if got := DefaultPackagesDir(); got != override {
		t.Fatalf("override: got %q want %q", got, override)
	}

	t.Setenv("WXTAP_PACKAGES_DIR", "")
	appdata := t.TempDir()
	t.Setenv("APPDATA", appdata)

	radium := filepath.Join(appdata, "Tencent", "xwechat", "radium")
	usersRoot := filepath.Join(radium, "users")
	perUser := filepath.Join(usersRoot, "u1", "Applet", "packages")
	if err := os.MkdirAll(perUser, 0o750); err != nil {
		t.Fatal(err)
	}
	// The per-user tree is discovered under the platform's WeChat users root,
	// which is radium/users under APPDATA on Windows and the WeChat container
	// elsewhere; WXTAP_USERS_DIR overrides both. Naming the root with the
	// override keeps the probe asserted where production would not read
	// APPDATA at all, and Windows keeps the APPDATA root — the one it really
	// reads — on top of that.
	t.Setenv("WXTAP_USERS_DIR", usersRoot)
	if got := DefaultPackagesDir(); got != perUser {
		t.Fatalf("per-user layout: got %q want %q", got, perUser)
	}
	if runtime.GOOS == "windows" {
		t.Setenv("WXTAP_USERS_DIR", "")
		if got := DefaultPackagesDir(); got != perUser {
			t.Fatalf("per-user layout under APPDATA: got %q want %q", got, perUser)
		}
	}

	// Without a users/ tree the flat Applet/packages fallback is used.
	t.Setenv("WXTAP_USERS_DIR", "")
	flatData := t.TempDir()
	t.Setenv("APPDATA", flatData)
	flat := filepath.Join(flatData, "Tencent", "xwechat", "radium", "Applet", "packages")
	if err := os.MkdirAll(flat, 0o750); err != nil {
		t.Fatal(err)
	}
	if got := DefaultPackagesDir(); got != flat {
		t.Fatalf("flat layout: got %q want %q", got, flat)
	}
}

// An app whose page templates this tool cannot restore must be flagged, so the
// page leaves it out of the decompilable list and the count. A normal app must
// stay unflagged: hiding an app that works is worse than showing one that fails.
func TestFindPackagesFlagsAppsWithUnrestorableTemplates(t *testing.T) {
	const compiledAppID = "wx1111111111111111"
	const legacyAppID = "wx2222222222222222"

	root := t.TempDir()
	for _, appID := range []string{compiledAppID, legacyAppID} {
		if err := os.MkdirAll(filepath.Join(root, appID), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	compiledChunk := `var __wxCodeSpace__ = {batchAddCompiledTemplate: function(){}};
__wxCodeSpace__.batchAddCompiledTemplate(function(G,R){return {};});
__wxAppCode__['pages/a/index.wxml'] = [function(){ return function(){} }];`
	legacyChunk := `__wxAppCode__['pages/a/index.wxml'] = function(){ return {tag:'view', attr:{}, children:['hi']} };`
	write := func(appID, chunk string) {
		t.Helper()
		data := buildPlainWxapkg(map[string][]byte{"chunk_0.webview.js": []byte(chunk)})
		if err := os.WriteFile(filepath.Join(root, appID, "main.wxapkg"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(compiledAppID, compiledChunk)
	write(legacyAppID, legacyChunk)

	flags := map[string]bool{}
	for _, pkg := range FindPackages(root) {
		flags[pkg.AppID] = pkg.Unsupported
	}
	if !flags[compiledAppID] {
		t.Fatal("an app with compiled templates must be flagged unsupported")
	}
	if flags[legacyAppID] {
		t.Fatal("an app with legacy templates must not be flagged")
	}
}

// The count is what can be decompiled on this machine, so an app whose templates
// cannot be restored is left out of it — while still being described for callers
// that show it.
func TestBuildInventoryKeepsUnsupportedAppsOutOfTheCount(t *testing.T) {
	const compiledAppID = "wx1111111111111111"
	const readyAppID = "wx3333333333333333"

	items, summary := BuildInventory("", []PkgFile{
		{AppID: readyAppID, Path: "ready.wxapkg"},
		{AppID: compiledAppID, Path: "compiled.wxapkg", Unsupported: true},
	}, nil)
	if summary.PackageCount != 1 {
		t.Fatalf("package_count = %d, want 1: an app that cannot be decompiled is not part of the count", summary.PackageCount)
	}
	found := false
	for _, item := range items {
		if item.AppID != compiledAppID {
			continue
		}
		found = true
		if !item.Unsupported {
			t.Fatal("the item must keep the flag")
		}
	}
	if !found {
		t.Fatal("the unsupported app must still be described")
	}
}

func TestUnsupportedAppIDsGroupsByApp(t *testing.T) {
	ids := UnsupportedAppIDs([]PkgFile{
		{AppID: "wx1111111111111111", Path: "a.wxapkg"},
		{AppID: "wx1111111111111111", Path: "b.wxapkg", Unsupported: true},
		{AppID: "wx2222222222222222", Path: "c.wxapkg"},
	})
	if !ids["wx1111111111111111"] {
		t.Fatal("one unrestorable package has to mark the whole app")
	}
	if ids["wx2222222222222222"] {
		t.Fatal("a clean app must not be in the set")
	}
}
