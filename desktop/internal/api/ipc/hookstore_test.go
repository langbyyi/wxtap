package ipc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestHookStore(t *testing.T) *HookStore {
	t.Helper()
	base := t.TempDir()
	return NewHookStore(base)
}

func TestHookListCreatesDirAndListsScripts(t *testing.T) {
	store := newTestHookStore(t)

	scripts, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(scripts) != 0 {
		t.Fatalf("expected empty list: %+v", scripts)
	}
	if info, err := os.Stat(filepath.Join(store.dir, "hook_scripts")); err != nil || !info.IsDir() {
		t.Fatalf("hook_scripts dir not created: %v", err)
	}
}

func TestHookListMarksGlobalScripts(t *testing.T) {
	store := newTestHookStore(t)
	if _, err := store.List(); err != nil { // creates the directory
		t.Fatalf("list: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, "hook_scripts", "b.js"), []byte("// b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, "hook_scripts", "a.js"), []byte("// a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGlobal("b.js", true); err != nil {
		t.Fatalf("setGlobal: %v", err)
	}

	scripts, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Sorted by filename like the filename glob.
	if len(scripts) != 2 || scripts[0].Filename != "a.js" || scripts[1].Filename != "b.js" {
		t.Fatalf("unexpected order: %+v", scripts)
	}
	if scripts[0].Global || !scripts[1].Global {
		t.Fatalf("global flags wrong: %+v", scripts)
	}
	if scripts[0].Injected {
		t.Fatalf("injected flag must stay false: %+v", scripts[0])
	}
}

func TestHookSetGlobalTogglesAndPersists(t *testing.T) {
	store := newTestHookStore(t)

	if err := store.SetGlobal("x.js", true); err != nil {
		t.Fatalf("set on: %v", err)
	}
	if err := store.SetGlobal("x.js", true); err != nil {
		t.Fatalf("set on again: %v", err)
	}
	scripts, _ := store.List()
	if len(scripts) != 0 {
		t.Fatalf("global flag for missing script leaks into list: %+v", scripts)
	}

	if err := os.WriteFile(filepath.Join(store.dir, "hook_scripts", "x.js"), []byte("// x"), 0o644); err != nil {
		t.Fatal(err)
	}
	scripts, _ = store.List()
	if !scripts[0].Global {
		t.Fatalf("global flag lost: %+v", scripts)
	}

	if err := store.SetGlobal("x.js", false); err != nil {
		t.Fatalf("set off: %v", err)
	}
	scripts, _ = store.List()
	if scripts[0].Global {
		t.Fatalf("global flag not cleared: %+v", scripts)
	}
}

// The global list is persisted user state and every caller (Vue view, MCP,
// external bridge) sends names it got from hook.list, so a value that could not
// name a listed script — a path, another extension, the empty string — must be
// refused instead of being stored as a script name forever.
func TestHookSetGlobalRejectsNamesThatCannotBeScripts(t *testing.T) {
	store := newTestHookStore(t)
	if err := store.SetGlobal("ok.js", true); err != nil {
		t.Fatalf("plain script name must stay accepted: %v", err)
	}
	// A backslash splits path elements on Windows but is an ordinary file name
	// character on POSIX, where hook.list can hand back a script named that way.
	rejected := []string{"", "../../evil.js", "sub/dir.js", "notes.txt", "evil.js.exe"}
	if strings.Contains(pathSeparators(), `\`) {
		rejected = append(rejected, `..\..\evil.js`)
	}
	for _, name := range rejected {
		if err := store.SetGlobal(name, true); err == nil {
			t.Fatalf("SetGlobal(%q) must be rejected", name)
		}
	}
	config, err := store.store.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if names := globalScriptsFrom(config); len(names) != 1 || names[0] != "ok.js" {
		t.Fatalf("global list polluted: %v", names)
	}
}

// The separator set is the whole platform difference, so both platforms' answer
// is asserted from either host instead of only the one the test happens to run
// on: the same value is a path on Windows and a script name on POSIX.
func TestIsScriptNameFollowsPlatformSeparators(t *testing.T) {
	for _, test := range []struct {
		name       string
		separators string
		value      string
		want       bool
	}{
		{"bare name", `/\`, "trace.js", true},
		{"bare name on POSIX", "/", "trace.js", true},
		{"backslash path on Windows", `/\`, `..\..\evil.js`, false},
		{"backslash is a name character on POSIX", "/", `..\..\evil.js`, true},
		{"slash path", `/\`, "../../evil.js", false},
		{"slash path on POSIX", "/", "../../evil.js", false},
		{"nested name", `/\`, "sub/dir.js", false},
		{"nested name on POSIX", "/", "sub/dir.js", false},
		{"empty", `/\`, "", false},
		{"empty on POSIX", "/", "", false},
		{"other extension", `/\`, "notes.txt", false},
	} {
		if got := isScriptNameFor(test.separators, test.value); got != test.want {
			t.Fatalf("%s: isScriptNameFor(%q, %q) = %v, want %v",
				test.name, test.separators, test.value, got, test.want)
		}
	}
}

// hook.list 要带上文件的修改时间：界面靠它判断「注入之后文件又被改过」。
func TestHookListReportsFileMtime(t *testing.T) {
	store := newTestHookStore(t)
	if _, err := store.List(); err != nil { // creates the directory
		t.Fatalf("list: %v", err)
	}
	path := filepath.Join(store.dir, "hook_scripts", "trace.js")
	if err := os.WriteFile(path, []byte("// trace"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	scripts, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(scripts) != 1 || scripts[0].Mtime != info.ModTime().Unix() {
		t.Fatalf("mtime = %+v, want %d", scripts, info.ModTime().Unix())
	}
}

func TestHookInjectReadsScriptSource(t *testing.T) {
	store := newTestHookStore(t)
	if _, err := store.List(); err != nil { // the list flow creates the directory
		t.Fatalf("list: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, "hook_scripts", "mine.js"), []byte("console.log('injected')"), 0o644); err != nil {
		t.Fatal(err)
	}

	source, err := store.Inject("mine.js")
	if err != nil || source != "console.log('injected')" {
		t.Fatalf("inject: %q %v", source, err)
	}

	_, err = store.Inject("missing.js")
	if err == nil || !strings.Contains(err.Error(), "script not found") {
		t.Fatalf("missing script error: %v", err)
	}
}
