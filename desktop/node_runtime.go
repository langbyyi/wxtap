// Node runtime resolution. WxTap does not ship a Node, so the shell has to find
// the user's own — from the operator's saved path, from PATH, or from a
// well-known install location. Everything that decides *which* Node runs Core
// lives here so the Settings page and the Core spawn cannot disagree.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/api/ipc"
)

// minNodeMajor is the oldest Node.js that can run the Core bundle. Core is
// compiled for ES2024 (core/tsconfig.json) and esbuild is invoked without
// --target, so it lowers nothing: an older Node starts the process and then
// dies on syntax. The build scripts refuse to package a Core built by anything
// older, which is what keeps this number honest.
const minNodeMajor = 22

// nodeProbeTimeout bounds the version probe so a hung binary cannot block
// engine startup. Node answers -p in tens of milliseconds.
const nodeProbeTimeout = 10 * time.Second

// Where a resolved Node came from. The Settings page renders the label, so an
// automatically chosen Node is never a silent substitution.
const (
	nodeSourceEnv      = "env"      // WXTAP_CORE_CMD
	nodeSourceConfig   = "config"   // chosen in Settings
	nodeSourcePath     = "path"     // the user's PATH
	nodeSourceDetected = "detected" // a well-known install location
)

// configNodePathKey is the config.json key the Settings page writes.
const configNodePathKey = "node_path"

// NodeRuntime is the resolved Node plus how it was found. Core refuses to start
// when Error is set, and the Settings page shows the same Error string, so the
// two surfaces never disagree about what is wrong.
type NodeRuntime struct {
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
	Error   string `json:"error,omitempty"`
	// Skipped lists candidates that were present but unusable. A Node taken
	// from a later source is thus explained rather than silently swapped in.
	Skipped []string `json:"skipped,omitempty"`
}

// nodeMajorVersion reads the major out of a version string ("v24.19.0" or
// "20.11.1"). It is split from the probe so the parsing is testable without
// spawning a process.
func nodeMajorVersion(version string) (int, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(version), "v")
	digits := trimmed
	if index := strings.IndexFunc(trimmed, func(r rune) bool { return r < '0' || r > '9' }); index >= 0 {
		digits = trimmed[:index]
	}
	if digits == "" {
		return 0, fmt.Errorf("no major version in %q", version)
	}
	return strconv.Atoi(digits)
}

// nodeVersionCommand builds the version probe. It is split out so the process
// configuration is testable without spawning anything: a probe not marked as a
// background process makes Windows allocate a console for it, and the user sees
// a black window flash for every candidate Node resolution tries.
func nodeVersionCommand(ctx context.Context, nodeBin string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, nodeBin, "-p", "process.versions.node")
	configureBackgroundProcess(cmd)
	return cmd
}

// nodeVersionOf runs `node -p process.versions.node` and checks it against the
// floor. The returned error is written for the user, not the caller: it ends up
// verbatim in the Settings panel.
func nodeVersionOf(nodeBin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nodeProbeTimeout)
	defer cancel()
	output, err := nodeVersionCommand(ctx, nodeBin).Output()
	if err != nil {
		return "", fmt.Errorf("无法运行 %s：请确认它是可用的 node 可执行文件", nodeBin)
	}
	reported := strings.TrimSpace(string(output))
	major, err := nodeMajorVersion(reported)
	if err != nil {
		return "", fmt.Errorf("无法识别 Node.js 版本（%s 报告 %q）：请改用官方发行版", nodeBin, reported)
	}
	if major < minNodeMajor {
		return "", fmt.Errorf("%s 的 Node.js 版本过低（%s），需要 %d 或更高", nodeBin, reported, minNodeMajor)
	}
	return reported, nil
}

// probeNode validates one candidate. Error is empty when the candidate works.
func probeNode(nodeBin, source string) NodeRuntime {
	probed := NodeRuntime{Path: nodeBin, Source: source}
	version, err := nodeVersionOf(nodeBin)
	if err != nil {
		probed.Error = err.Error()
		return probed
	}
	probed.Version = version
	return probed
}

// resolveNodeRuntime picks the Node that runs Core, in this order:
//
//  1. WXTAP_CORE_CMD — an explicit override, so it must win outright; falling
//     through to another Node would make it useless.
//  2. the path saved in Settings.
//  3. whatever `node` resolves to on PATH, which is what the user's own shell
//     would run.
//  4. well-known install locations, for a machine where Node is installed but
//     PATH was never updated (the usual reason a fresh install "isn't seen").
//
// A candidate that exists but is unusable does not silently hand over to the
// next one at steps 1 and 2 — the operator asked for that Node, so its problem
// is reported. At steps 3 and 4 it does continue, because there the goal is
// simply to find a working Node; the substitution is recorded in Skipped.
func resolveNodeRuntime() NodeRuntime {
	var skipped []string
	consider := func(nodeBin, source string) (NodeRuntime, bool) {
		if strings.TrimSpace(nodeBin) == "" {
			return NodeRuntime{}, false
		}
		probed := probeNode(nodeBin, source)
		if probed.Error == "" {
			probed.Skipped = skipped
			return probed, true
		}
		skipped = append(skipped, probed.Error)
		return probed, false
	}

	if override := strings.TrimSpace(os.Getenv("WXTAP_CORE_CMD")); override != "" {
		probed, _ := consider(override, nodeSourceEnv)
		probed.Skipped = skipped
		return probed
	}
	if configured := configNodePath(); configured != "" {
		probed, _ := consider(configured, nodeSourceConfig)
		probed.Skipped = skipped
		return probed
	}
	if fromPath, err := exec.LookPath("node"); err == nil {
		if probed, ok := consider(fromPath, nodeSourcePath); ok {
			return probed
		}
	} else if errors.Is(err, exec.ErrDot) {
		// A PATH entry that points at the working directory is refused by Go
		// rather than run (on Windows the working directory is searched before
		// PATH at all). Say so: the user's PATH Node is fine, and "not found"
		// would send them installing one they already have.
		skipped = append(skipped, fmt.Sprintf(
			"已忽略运行目录下的 %s（执行当前目录中的程序有被劫持的风险，请把它放进 PATH 或在设置里指定路径）",
			nodeBinaryName(),
		))
	}
	for _, candidate := range nodeRuntimeCandidates() {
		if probed, ok := consider(candidate, nodeSourceDetected); ok {
			return probed
		}
	}
	return NodeRuntime{Skipped: skipped, Error: nodeMissingMessage()}
}

// nodeBinaryName is how this file's messages name the interpreter. Saying
// "node.exe" to a mac user is a Windows-ism that reads as a different program.
func nodeBinaryName() string {
	if runtime.GOOS == "windows" {
		return "node.exe"
	}
	return "node"
}

// nodeMissingMessage is the one wording for "there is no usable Node", used by
// the Settings panel and by the Core spawn failure alike.
func nodeMissingMessage() string {
	return fmt.Sprintf(
		"未找到可用的 Node.js：WxTap 不自带运行时，需要系统中已安装 Node.js %d 或更高版本。"+
			"请从 https://nodejs.org/ 安装（国内可用 https://npmmirror.com/mirrors/node/），"+
			"或在「设置 → Node 运行时」里指定 node 可执行文件路径。"+
			// Not Windows-specific: a new PATH is only visible to a new logon
			// session on either platform, which is why a Dock-launched mac app
			// does not see a just-installed Node either.
			"注意：刚装完需重新登录系统，新的 PATH 才会生效",
		minNodeMajor,
	)
}

// configNodePath returns the Node the operator picked in Settings, or "".
func configNodePath() string {
	base, err := userBaseDir()
	if err != nil {
		return ""
	}
	config, err := ipc.NewConfigStore(base).Load()
	if err != nil {
		return ""
	}
	value, _ := config[configNodePathKey].(string)
	return strings.TrimSpace(value)
}

// nodeRuntimeCandidates returns the well-known install locations detection
// probes. It is a variable so tests can pin the list: the real answer depends on
// where this host installed Node, which no test can control. It must not be
// reassigned from a parallel test.
var nodeRuntimeCandidates = detectNodeCandidates

// detectNodeCandidates lists well-known install locations that actually hold a
// file. Existence is all it checks: which of them works is decided by the
// version probe, so a stale entry costs one spawn and nothing else.
func detectNodeCandidates() []string {
	var candidates []string
	add := func(parts ...string) {
		for _, part := range parts {
			if part == "" {
				return
			}
		}
		candidates = append(candidates, filepath.Join(parts...))
	}
	// Version managers keep the interpreter under a versioned directory, and
	// Glob sorts ascending, so the newest tends to come last. Reverse the
	// versioned groups so the likely-newest is tried first.
	addGlob := func(pattern string) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return
		}
		for index := len(matches) - 1; index >= 0; index-- {
			candidates = append(candidates, matches[index])
		}
	}

	if runtime.GOOS == "windows" {
		// Everything is derived from the environment rather than hardcoded, so a
		// relocated Windows and a test that pins these variables both work.
		// The official installer, machine-wide and per-user; this is also where
		// nvm-windows points its symlink.
		add(os.Getenv("ProgramFiles"), "nodejs", "node.exe")
		add(os.Getenv("ProgramFiles(x86)"), "nodejs", "node.exe")
		add(os.Getenv("LOCALAPPDATA"), "Programs", "nodejs", "node.exe")
		// nvm-windows exposes both, depending on how it was configured.
		add(os.Getenv("NVM_SYMLINK"), "node.exe")
		add(os.Getenv("NVM_HOME"), "node.exe")
		add(os.Getenv("USERPROFILE"), "scoop", "apps", "nodejs", "current", "node.exe")
		add(os.Getenv("ProgramData"), "chocolatey", "bin", "node.exe")
		addGlob(filepath.Join(os.Getenv("LOCALAPPDATA"), "Volta", "tools", "image", "node", "*", "node.exe"))
		addGlob(filepath.Join(os.Getenv("LOCALAPPDATA"), "fnm_multishells", "*", "node.exe"))
		addGlob(filepath.Join(os.Getenv("APPDATA"), "nvm", "*", "node.exe"))
	} else {
		add("/usr/local/bin/node")
		add("/opt/homebrew/bin/node")
		add("/usr/bin/node")
		add(os.Getenv("HOME"), ".volta", "bin", "node")
		addGlob(filepath.Join(os.Getenv("HOME"), ".nvm", "versions", "node", "*", "bin", "node"))
		// The other version managers macOS users reach for: asdf and mise put a
		// shim in their own root, fnm keeps real installs under Application
		// Support, and pnpm's standalone install lands in ~/Library. None of them
		// are on launchd's PATH, which is the whole reason this list exists.
		add(os.Getenv("HOME"), ".asdf", "shims", "node")
		add(os.Getenv("HOME"), ".local", "share", "mise", "shims", "node")
		addGlob(filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "fnm", "node-versions", "*", "installation", "bin", "node"))
		add(os.Getenv("HOME"), "Library", "pnpm", "node")
	}

	// Insertion order is deliberate and deterministic: the plain install
	// locations come first (that is the install the user's PATH most likely
	// meant), then the versioned groups newest-first. Sorting here would throw
	// that ordering away, and the version probe is the real gate anyway.
	found := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		key := strings.ToLower(candidate)
		if seen[key] {
			continue
		}
		seen[key] = true
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			found = append(found, candidate)
		}
	}
	return found
}
