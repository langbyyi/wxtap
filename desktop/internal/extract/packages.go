package extract

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DecompilePackages restores every package for one app into a staging tree and
// only replaces outDir after all packages have succeeded.
func DecompilePackages(packagePaths []string, outDir, appID string) ([]string, error) {
	if len(packagePaths) == 0 {
		return nil, fmt.Errorf("no wxapkg packages supplied")
	}
	if outDir == "" {
		return nil, fmt.Errorf("output directory is required")
	}
	parent := filepath.Dir(outDir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return nil, err
	}
	workDir, err := os.MkdirTemp(parent, ".wxtap-decompile-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	stagedRoot := filepath.Join(workDir, "staged")
	if err := os.MkdirAll(stagedRoot, 0o750); err != nil {
		return nil, err
	}
	for i, packagePath := range packagePaths {
		packageRoot := filepath.Join(workDir, fmt.Sprintf("package-%04d", i))
		if _, err := extractWxapkgRaw(packagePath, packageRoot, appID); err != nil {
			return nil, fmt.Errorf("decompile %s: %w", filepath.Base(packagePath), err)
		}
		if err := mergeTree(packageRoot, stagedRoot); err != nil {
			return nil, fmt.Errorf("merge %s: %w", filepath.Base(packagePath), err)
		}
	}
	if _, err := RestoreSourceTree(stagedRoot); err != nil {
		return nil, err
	}
	files, err := treeFiles(stagedRoot, outDir)
	if err != nil {
		return nil, err
	}
	if err := replaceDirectory(stagedRoot, outDir); err != nil {
		return nil, err
	}
	return files, nil
}

func mergeTree(sourceRoot, targetRoot string) error {
	return filepath.WalkDir(sourceRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		target, err := safeChildPath(targetRoot, rel)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}

func treeFiles(root, finalRoot string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.Join(finalRoot, rel))
		return nil
	})
	return files, err
}

func replaceDirectory(staged, target string) error {
	info, err := os.Stat(target)
	if os.IsNotExist(err) {
		return os.Rename(staged, target)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("output path is not a directory: %s", target)
	}

	backup, err := os.MkdirTemp(filepath.Dir(target), ".wxtap-backup-*")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(backup, target)
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
}
