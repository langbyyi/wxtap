package ipc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// HookScript is one user hook script in the hook.list payload.
type HookScript struct {
	Filename string `json:"filename"`
	Global   bool   `json:"global"`
	Injected bool   `json:"injected"`
	// Mtime is the script's last modification time (Unix seconds). The GUI
	// compares it with the last injection to tell "文件在注入之后又被改过" —
	// 没有这个比对，用户改完文件再点注入会以为改动没生效。
	Mtime int64 `json:"mtime"`
}

// HookStore manages the user hook script directory and the global injection
// list persisted in the GUI config (hook.* domain).
type HookStore struct {
	dir   string
	store *ConfigStore
}

// NewHookStore keeps scripts at base/hook_scripts and the global list under
// the config's global_hook_scripts key.
func NewHookStore(base string) *HookStore {
	return &HookStore{dir: base, store: NewConfigStore(base)}
}

// ScriptsDir is the writable directory the user drops scripts into. The shell
// exposes it to the UI (settings.getPaths / the hook page's folder shortcut)
// and a contract test pins the two to the same path.
func (h *HookStore) ScriptsDir() string {
	return filepath.Join(h.dir, "hook_scripts")
}

// List creates the script directory if needed and returns its scripts, sorted,
// with their global flag.
func (h *HookStore) List() ([]HookScript, error) {
	scriptsDir := h.ScriptsDir()
	if err := os.MkdirAll(scriptsDir, 0o750); err != nil {
		return nil, err
	}
	globals := h.globalScripts()
	scripts, err := readScripts(scriptsDir)
	if err != nil {
		return nil, err
	}
	for i := range scripts {
		scripts[i].Global = contains(globals, scripts[i].Filename)
	}
	return scripts, nil
}

// readScripts lists the *.js files of the script directory, in ReadDir's order
// (by filename).
func readScripts(dir string) ([]HookScript, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	scripts := make([]HookScript, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		script := HookScript{Filename: entry.Name()}
		// 读不到 mtime 不是错误：文件仍可注入，只是界面少一条「文件已更新」的提示。
		if info, err := entry.Info(); err == nil {
			script.Mtime = info.ModTime().Unix()
		}
		scripts = append(scripts, script)
	}
	return scripts, nil
}

// Inject returns the source of a hook script for runtime evaluation.
func (h *HookStore) Inject(filename string) (string, error) {
	if !isScriptName(filename) {
		return "", errScriptNotFound
	}
	data, err := os.ReadFile(filepath.Join(h.ScriptsDir(), filename))
	if err != nil {
		return "", errScriptNotFound
	}
	return string(data), nil
}

// SetGlobal toggles a script's membership in the global injection list.
// The flag may be set before the file exists (the GUI allows
// pre-arming a script), but the name still has to be one hook.list could
// return: a bare *.js file name. Anything else would be persisted as a script
// name that never resolves to a file.
func (h *HookStore) SetGlobal(filename string, global bool) error {
	if !isScriptName(filename) {
		return fmt.Errorf("invalid hook script name %q", filename)
	}
	config, err := h.store.Load()
	if err != nil {
		return err
	}
	scripts := globalScriptsFrom(config)
	if global {
		if !contains(scripts, filename) {
			scripts = append(scripts, filename)
		}
	} else {
		filtered := scripts[:0]
		for _, name := range scripts {
			if name != filename {
				filtered = append(filtered, name)
			}
		}
		scripts = filtered
	}
	config["global_hook_scripts"] = scripts
	// The store writes the whole config atomically; this path has no request
	// context of its own, so a background context is the honest choice.
	return h.store.Save(context.Background(), config)
}

// isScriptName reports whether filename is the shape hook.list can return: a
// bare *.js name with no directory component. A separator of this platform makes
// the value a path, and on Windows so does a volume form like "C:x.js", which
// names a stream rather than a script.
func isScriptName(filename string) bool {
	return isScriptNameFor(pathSeparators(), filename)
}

// isScriptNameFor is isScriptName under an explicit separator set, so a test can
// exercise the Windows and the POSIX answer from either host. Volume names are
// not parameterised: filepath.VolumeName only recognises them on Windows, which
// is also the only platform where pathSeparators names '\'.
func isScriptNameFor(separators, filename string) bool {
	if filename == "" || !strings.HasSuffix(filename, ".js") {
		return false
	}
	return !strings.ContainsAny(filename, separators) && filepath.VolumeName(filename) == ""
}

// pathSeparators is the set of characters this platform's filepath splits paths
// on: Windows takes both '\' and '/', POSIX only '/'.
func pathSeparators() string {
	if runtime.GOOS == "windows" {
		return `/\`
	}
	return "/"
}

func (h *HookStore) globalScripts() []string {
	config, err := h.store.Load()
	if err != nil {
		return nil
	}
	return globalScriptsFrom(config)
}

func globalScriptsFrom(config map[string]any) []string {
	raw, ok := config["global_hook_scripts"].([]any)
	if !ok {
		return nil
	}
	scripts := make([]string, 0, len(raw))
	for _, item := range raw {
		if name, ok := item.(string); ok {
			scripts = append(scripts, name)
		}
	}
	return scripts
}

func contains(scripts []string, filename string) bool {
	for _, name := range scripts {
		if name == filename {
			return true
		}
	}
	return false
}

var errScriptNotFound = errorString("script not found")

type errorString string

func (e errorString) Error() string { return string(e) }
