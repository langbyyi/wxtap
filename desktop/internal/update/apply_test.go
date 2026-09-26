package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// payloadMarker is the content every staged file carries, so a test can tell
// the new tree from the old one by reading any file.
const payloadMarker = "new-build"

// installDir builds a minimal Windows-shaped install directory: the entries a
// release replaces, plus the runtime data an update must never touch. The
// Windows shape is data (layoutFor("windows")), so the tests that use it name
// that layout explicitly rather than going through the runtime.GOOS dispatch —
// otherwise they would only hold on Windows. The darwin counterpart is
// bundleDir below. runtime/ is a leftover from a version that still bundled a
// Node — newer payloads do not carry one, and it must be neither swapped nor
// swept away.
func installDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"WxTap.exe":                                "old-exe",
		filepath.Join("core", "dist", "cli.js"):    "old-core",
		filepath.Join("runtime", "node.exe"):       "old-node",
		filepath.Join("resources", "hook", "a.js"): "old-hook",
		filepath.Join("migrations", "001.sql"):     "old-migration",
		filepath.Join("logs", "run.log"):           "user-log",
		"traffic.db":                               "user-history",
		"config.json":                              "user-config",
	}
	writeFiles(t, dir, files)
	return dir
}

// stageTree writes a staged payload directly and marks it pending, so the swap
// can be tested without a network round trip. The tree it writes is the
// Windows shape, matching installDir.
func stageTree(t *testing.T, dataDir, version string, extra map[string]string) string {
	t.Helper()
	root := stagedDir(dataDir, version)
	files := map[string]string{
		"WxTap.exe":                                payloadMarker,
		filepath.Join("core", "dist", "cli.js"):    payloadMarker,
		filepath.Join("resources", "hook", "a.js"): payloadMarker,
		filepath.Join("migrations", "002.sql"):     payloadMarker,
	}
	for name, body := range extra {
		files[name] = body
	}
	writeFiles(t, root, files)
	if err := writePending(dataDir, PendingState{Version: version}); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(body)
}

func TestApplyPendingWithoutMarkerLeavesInstallAlone(t *testing.T) {
	dir := installDir(t)
	record := applyPendingFor(dir, t.TempDir(), layoutFor("windows"))
	if record.Applied || record.Error != "" || record.Version != "" {
		t.Fatalf("record: %+v", record)
	}
	if got := readFile(t, filepath.Join(dir, "WxTap.exe")); got != "old-exe" {
		t.Fatalf("install was touched: %q", got)
	}
}

func TestApplyPendingSwapsPayloadAndKeepsUserData(t *testing.T) {
	dir := installDir(t)
	dataDir := t.TempDir()
	stageTree(t, dataDir, "v2.1.0", nil)

	record := applyPendingFor(dir, dataDir, layoutFor("windows"))
	if !record.Applied || record.Error != "" || record.Version != "v2.1.0" {
		t.Fatalf("record: %+v", record)
	}
	for _, name := range []string{
		"WxTap.exe",
		filepath.Join("core", "dist", "cli.js"),
		filepath.Join("resources", "hook", "a.js"),
		filepath.Join("migrations", "002.sql"),
	} {
		if got := readFile(t, filepath.Join(dir, name)); got != payloadMarker {
			t.Errorf("%s = %q, want the staged build", name, got)
		}
	}
	// The user's own files survive an update, and so does the runtime/ a
	// previous bundled-Node release left behind.
	for name, want := range map[string]string{
		filepath.Join("logs", "run.log"):     "user-log",
		filepath.Join("runtime", "node.exe"): "old-node",
		"traffic.db":                         "user-history",
		"config.json":                        "user-config",
	} {
		if got := readFile(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, ok := StagedVersion(dataDir); ok {
		t.Fatal("the pending marker must be cleared once applied")
	}
}

// The release directory the build script stages from also holds runtime data,
// so a packaging slip must not be able to overwrite a user's history.
func TestApplyPendingSkipsEntriesOutsideTheAllowlist(t *testing.T) {
	dir := installDir(t)
	dataDir := t.TempDir()
	stageTree(t, dataDir, "v2.1.0", map[string]string{
		filepath.Join("logs", "run.log"): "attacker-log",
		"traffic.db":                     "attacker-history",
	})

	record := applyPendingFor(dir, dataDir, layoutFor("windows"))
	if !record.Applied {
		t.Fatalf("record: %+v", record)
	}
	if len(record.Skipped) != 2 {
		t.Fatalf("skipped entries: %v", record.Skipped)
	}
	if got := readFile(t, filepath.Join(dir, "logs", "run.log")); got != "user-log" {
		t.Fatalf("logs were overwritten: %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "traffic.db")); got != "user-history" {
		t.Fatalf("traffic.db was overwritten: %q", got)
	}
}

// A payload from a release that still bundled Node carries runtime/. It is no
// longer on the allowlist, so it must be reported as skipped rather than
// swapped: the Node that runs Core is the user's, and putting a runtime nothing
// resolves back on disk helps no one.
func TestApplyPendingSkipsABundledRuntime(t *testing.T) {
	dir := installDir(t)
	dataDir := t.TempDir()
	stageTree(t, dataDir, "v2.1.0", map[string]string{
		filepath.Join("runtime", "node.exe"): payloadMarker,
	})

	record := applyPendingFor(dir, dataDir, layoutFor("windows"))
	if !record.Applied {
		t.Fatalf("record: %+v", record)
	}
	if len(record.Skipped) != 1 || record.Skipped[0] != "runtime" {
		t.Fatalf("skipped entries: %v", record.Skipped)
	}
	if got := readFile(t, filepath.Join(dir, "runtime", "node.exe")); got != "old-node" {
		t.Fatalf("the leftover runtime was overwritten: %q", got)
	}
}

// Re-downloading is the only fix for an incomplete payload, so it is dropped
// instead of failing again on every launch.
func TestApplyPendingDiscardsIncompletePayload(t *testing.T) {
	dir := installDir(t)
	dataDir := t.TempDir()
	stageTree(t, dataDir, "v2.1.0", nil)
	if err := os.Remove(filepath.Join(stagedDir(dataDir, "v2.1.0"), "core", "dist", "cli.js")); err != nil {
		t.Fatal(err)
	}

	record := applyPendingFor(dir, dataDir, layoutFor("windows"))
	if record.Applied || !strings.Contains(record.Error, "更新包不完整") {
		t.Fatalf("record: %+v", record)
	}
	if got := readFile(t, filepath.Join(dir, "WxTap.exe")); got != "old-exe" {
		t.Fatalf("install was touched: %q", got)
	}
	if _, err := os.Stat(stagedDir(dataDir, "v2.1.0")); !os.IsNotExist(err) {
		t.Fatal("an unusable payload must be discarded")
	}
	if _, ok := StagedVersion(dataDir); ok {
		t.Fatal("an unusable payload must not stay pending")
	}
}

// A failure part-way through a multi-entry swap has to leave the previous
// install intact, or the app becomes unlaunchable.
func TestSwapIntoRollsBackEarlierEntries(t *testing.T) {
	dir := installDir(t)
	staged := t.TempDir()
	writeFiles(t, staged, map[string]string{"WxTap.exe": payloadMarker, "core": payloadMarker})
	// The swap clears each target before moving onto it, so a failure cannot be
	// arranged by making the target the wrong kind of thing. Instead the second
	// entry's *parent* is a regular file, which no rename can write under.
	writeFiles(t, dir, map[string]string{"blocked": "not-a-directory"})
	lay := layoutFor("windows")
	lay.entries = []layoutEntry{
		{staged: "WxTap.exe", target: "WxTap.exe"},
		{staged: "core", target: filepath.Join("blocked", "core")},
	}

	err := swapInto(staged, dir, lay)
	if err == nil || !strings.Contains(err.Error(), "无法写入") {
		t.Fatalf("err = %v, want a write failure", err)
	}
	if got := readFile(t, filepath.Join(dir, "WxTap.exe")); got != "old-exe" {
		t.Fatalf("WxTap.exe was not rolled back: %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "core", "dist", "cli.js")); got != "old-core" {
		t.Fatalf("core was disturbed: %q", got)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, "*.old"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("rollback left renamed entries behind: %v", leftovers)
	}
}

// bundleDir builds a macOS-shaped install: the payload lives under
// Contents/Resources and the executable under Contents/MacOS. It returns the
// Contents directory, which is what the assertions below name paths against.
func bundleDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		filepath.Join("WxTap.app", "Contents", "Info.plist"):                             "old-plist",
		filepath.Join("WxTap.app", "Contents", "MacOS", "WxTap"):                         "old-exe",
		filepath.Join("WxTap.app", "Contents", "Resources", "core", "dist", "cli.js"):    "old-core",
		filepath.Join("WxTap.app", "Contents", "Resources", "resources", "hook", "a.js"): "old-hook",
		filepath.Join("WxTap.app", "Contents", "Resources", "migrations", "001.sql"):     "old-migration",
		filepath.Join("WxTap.app", "Contents", "Resources", "logs", "run.log"):           "user-log",
	})
	return filepath.Join(root, "WxTap.app", "Contents")
}

// stageMacPayload writes the flat payload shape the darwin layout maps into the
// bundle: the same items as Windows, with the shell named WxTap.
func stageMacPayload(t *testing.T, dataDir, version string, extra map[string]string) {
	t.Helper()
	files := map[string]string{
		"WxTap":                                 payloadMarker,
		filepath.Join("core", "dist", "cli.js"): payloadMarker,
		filepath.Join("resources", "hook", "a.js"): payloadMarker,
		filepath.Join("migrations", "002.sql"):     payloadMarker,
	}
	for name, body := range extra {
		files[name] = body
	}
	writeFiles(t, stagedDir(dataDir, version), files)
	if err := writePending(dataDir, PendingState{Version: version}); err != nil {
		t.Fatal(err)
	}
}

// The macOS layout is data, so it is exercised here rather than only on a Mac.
func TestApplyPendingMapsAPayloadIntoTheAppBundle(t *testing.T) {
	contents := bundleDir(t)
	dataDir := t.TempDir()
	stageMacPayload(t, dataDir, "v2.1.0", nil)

	record := applyPendingFor(filepath.Join(contents, "MacOS"), dataDir, layoutFor("darwin"))
	if !record.Applied || record.Error != "" {
		t.Fatalf("record: %+v", record)
	}
	for name, want := range map[string]string{
		filepath.Join("MacOS", "WxTap"):                         payloadMarker,
		filepath.Join("Resources", "core", "dist", "cli.js"):    payloadMarker,
		filepath.Join("Resources", "resources", "hook", "a.js"): payloadMarker,
		filepath.Join("Resources", "migrations", "002.sql"):     payloadMarker,
	} {
		if got := readFile(t, filepath.Join(contents, name)); got != want {
			t.Errorf("%s = %q, want the staged build", name, got)
		}
	}
	// Info.plist belongs to the bundle, not the payload, so it must survive.
	if got := readFile(t, filepath.Join(contents, "Info.plist")); got != "old-plist" {
		t.Fatalf("Info.plist was disturbed: %q", got)
	}
	// So must the user's own data, which lives under Resources on macOS.
	if got := readFile(t, filepath.Join(contents, "Resources", "logs", "run.log")); got != "user-log" {
		t.Fatalf("user data was disturbed: %q", got)
	}
}

// A darwin payload without the shell is not worth swapping in, and the check
// has to name the staged name the payload actually uses.
func TestDarwinLayoutRequiresTheShellAndCore(t *testing.T) {
	contents := bundleDir(t)
	macOS := filepath.Join(contents, "MacOS")
	dataDir := t.TempDir()
	stageMacPayload(t, dataDir, "v2.1.0", nil)
	if err := os.Remove(filepath.Join(stagedDir(dataDir, "v2.1.0"), "WxTap")); err != nil {
		t.Fatal(err)
	}

	record := applyPendingFor(macOS, dataDir, layoutFor("darwin"))
	if record.Applied || !strings.Contains(record.Error, "更新包不完整") {
		t.Fatalf("record: %+v", record)
	}
	if got := readFile(t, filepath.Join(macOS, "WxTap")); got != "old-exe" {
		t.Fatalf("the bundle was touched: %q", got)
	}
}

// Only Windows needs a helper process to swap a running executable.
func TestLayoutNeedsSecondProcessOnlyOnWindows(t *testing.T) {
	if !layoutFor("windows").needsSecondProcess {
		t.Fatal("Windows cannot overwrite a running executable, so it needs the helper")
	}
	if layoutFor("darwin").needsSecondProcess {
		t.Fatal("macOS unlinks a running executable freely, so it needs no helper")
	}
	// The root is expressed relative to the executable's own directory, so it
	// has to climb out of Contents/MacOS to reach the bundle.
	insideExe := filepath.Join("x", "WxTap.app", "Contents", "MacOS")
	if got := filepath.Join(insideExe, layoutFor("darwin").root); got != filepath.Join("x", "WxTap.app") {
		t.Fatalf("the darwin root should reach the bundle: %q", got)
	}
	if got := filepath.Join(insideExe, layoutFor("windows").root); got != insideExe {
		t.Fatalf("the windows root should stay beside the executable: %q", got)
	}
}

func TestCleanupAfterLaunchRemovesLeftovers(t *testing.T) {
	dir := installDir(t)
	dataDir := t.TempDir()
	writeFiles(t, dir, map[string]string{"WxTap.exe.old": "previous", filepath.Join("core.old", "x.js"): "previous"})
	stageTree(t, dataDir, "v2.1.0", nil)

	cleanupFor(dir, dataDir, layoutFor("windows"))

	leftovers, err := filepath.Glob(filepath.Join(dir, "*.old"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("leftovers: %v (%v)", leftovers, err)
	}
	if got := readFile(t, filepath.Join(dir, "WxTap.exe")); got != "old-exe" {
		t.Fatalf("cleanup disturbed the install: %q", got)
	}
	for _, dirName := range []string{"staged", "downloads"} {
		if _, err := os.Stat(filepath.Join(updateRoot(dataDir), dirName)); !os.IsNotExist(err) {
			t.Fatalf("%s was not cleaned up", dirName)
		}
	}
}

func TestApplyRecordSummary(t *testing.T) {
	cases := []struct {
		name      string
		record    ApplyRecord
		wantLevel string
		wantText  string
	}{
		{"applied", ApplyRecord{Version: "v2.1.0", Applied: true}, "info", "已更新到 v2.1.0"},
		{"applied with skipped entries", ApplyRecord{Version: "v2.1.0", Applied: true, Skipped: []string{"logs", "traffic.db"}}, "info", "已跳过 logs, traffic.db"},
		{"failed", ApplyRecord{Version: "v2.1.0", Error: "更新包不完整: 缺少 core/dist/cli.js"}, "error", "更新未生效: 更新包不完整"},
		// Nothing reported a failure, which means the process died mid-swap.
		{"interrupted", ApplyRecord{Version: "v2.1.0"}, "error", "换入过程被中断"},
	}
	for _, testCase := range cases {
		level, message := testCase.record.Summary()
		if level != testCase.wantLevel || !strings.Contains(message, testCase.wantText) {
			t.Errorf("%s: (%s, %q), want %s containing %q", testCase.name, level, message, testCase.wantLevel, testCase.wantText)
		}
	}
}

func TestLastApplyIsReportedOnce(t *testing.T) {
	dataDir := t.TempDir()
	record := ApplyRecord{Version: "v2.1.0", Applied: true, Error: "部分失败"}
	record.write(dataDir)

	read, ok := LastApply(dataDir)
	if !ok || read.Version != "v2.1.0" || !read.Applied || read.Error != "部分失败" {
		t.Fatalf("record: %+v ok=%v", read, ok)
	}
	ClearLastApply(dataDir)
	if _, ok := LastApply(dataDir); ok {
		t.Fatal("the record should be consumed once")
	}
}
