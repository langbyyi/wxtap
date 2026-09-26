package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/langbyyi/wxtap/desktop/internal/api/ipc"
	"strings"
)

// safeRemoveAll is the guard for every destructive IPC path (extract.delete,
// extract.clearOutput). Callers hand us paths the frontend picked or CLI/MCP
// callers passed explicitly, so the guard refuses the catastrophic targets
// while keeping ordinary "delete this appid's output" flows working:
//
//   - empty/blank paths
//   - filesystem roots, the user's home, and the process working directory
//   - special files (devices, sockets)
//
// RemoveAll semantics are preserved: a missing path is a no-op, not an error.
func safeRemoveAll(path string) error {
	cleaned := strings.TrimSpace(path)
	if cleaned == "" {
		return fmt.Errorf("删除路径不能为空")
	}
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return fmt.Errorf("无效删除路径 %q: %w", path, err)
	}
	abs = filepath.Clean(abs)
	if protectedDeleteTarget(abs) {
		return fmt.Errorf("拒绝删除受保护的路径: %s", abs)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取删除目标失败 %s: %w", abs, err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("拒绝删除特殊文件: %s", abs)
	}
	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("删除失败 %s: %w", abs, err)
	}
	return nil
}

// protectedDeleteTarget reports whether abs is a path the app must never
// delete wholesale. It protects both the roots themselves and their
// ancestors: deleting C:\Users (the parent of the current user's home) is
// just as destructive as deleting the home directory itself.
func protectedDeleteTarget(abs string) bool {
	volume := filepath.VolumeName(abs)
	root := filepath.Clean(volume + string(filepath.Separator))
	if abs == root || filepath.Dir(abs) == abs {
		return true
	}

	protected := []string{}
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		absPath, err := filepath.Abs(path)
		if err == nil {
			protected = append(protected, filepath.Clean(absPath))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
		add(filepath.Dir(home))
	}
	if wd, err := os.Getwd(); err == nil {
		add(wd)
		add(filepath.Dir(wd))
	}
	add(os.TempDir())
	for _, key := range []string{
		"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramData",
		"APPDATA", "LOCALAPPDATA",
	} {
		add(os.Getenv(key))
	}
	if runtime.GOOS == "darwin" {
		// The Windows entries above are env-derived, so they cover that
		// platform's system directories for free. macOS has no equivalent
		// variables: every root has to be named, and a missing one is a
		// deletable one. /tmp and /var are listed alongside their /private/*
		// targets because add() cleans without resolving symlinks.
		for _, path := range []string{
			"/Applications", "/Library", "/System", "/Users", "/Volumes",
			"/bin", "/cores", "/dev", "/etc", "/opt", "/private", "/sbin",
			"/tmp", "/usr", "/var",
		} {
			add(path)
		}
	}
	for _, path := range protected {
		if pathContains(abs, path) {
			return true
		}
	}
	return false
}

// pathContains reports whether child is parent itself or a descendant of
// parent. It is used only for protection checks, where a conservative false
// is safer than a false positive that would block ordinary target deletion.
func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// validateAppID rejects identifiers that would escape their parent directory
// when joined (`..`, separators, absolute paths). The rule lives in the ipc
// package so the MCP tool surface enforces exactly the same thing.
func validateAppID(appID string) error {
	return ipc.ValidateAppID(appID)
}

// requireDeletableDir validates a directory whose children are about to be
// removed: it must exist, be a directory, and not be a protected target.
func requireDeletableDir(dir string) error {
	cleaned := strings.TrimSpace(dir)
	if cleaned == "" {
		return fmt.Errorf("目录不能为空")
	}
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return fmt.Errorf("无效目录 %q: %w", dir, err)
	}
	abs = filepath.Clean(abs)
	if protectedDeleteTarget(abs) {
		return fmt.Errorf("拒绝清空受保护的路径: %s", abs)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("目录不存在: %s", abs)
	}
	if !info.IsDir() {
		return fmt.Errorf("不是目录: %s", abs)
	}
	return nil
}
