package extract

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// maxFileRead bounds how much of each file is read into memory. It matches
// the 2 MB analysis window (Analyze applies the same limit), so huge minified
// bundles cannot balloon memory while the reported scan result stays unchanged
// (read fully, analyze the first 2 MB).
const maxFileRead = 2_000_000

// ScanResult is the outcome of scanning a directory for sensitive info.
type ScanResult struct {
	FilesScanned []string  `json:"files_scanned"`
	TotalSize    int64     `json:"total_size"`
	Analysis     Analysis  `json:"result"`
	Findings     []Finding `json:"findings,omitempty"`
}

// ScanFiles walks a directory, analyzes every supported text source and
// merges the results. It is the compatibility entry point for callers that
// do not need custom patterns.
func ScanFiles(dir string) ScanResult {
	result, _ := ScanFilesWithPatterns(dir, nil)
	return result
}

// ScanFilesWithPatterns scans like ScanFiles and additionally applies user
// patterns (name -> regex) as extra result categories. A malformed user
// pattern surfaces as an error instead of silently skipping.
func ScanFilesWithPatterns(dir string, patterns map[string]string) (ScanResult, error) {
	return ScanFilesWithPatternsProgress(dir, patterns, nil)
}

// ScanFilesWithPatternsProgress is ScanFilesWithPatterns with a callback
// after each supported text file, used by the desktop progress event.
func ScanFilesWithPatternsProgress(dir string, patterns map[string]string, progress func(done, total int)) (ScanResult, error) {
	compiled := map[string]*regexp.Regexp{}
	for name, expr := range patterns {
		re, err := regexp.Compile(expr)
		if err != nil {
			return ScanResult{}, fmt.Errorf("自定义正则 %s 语法错误", name)
		}
		compiled[name] = re
	}
	var results []Analysis
	result := ScanResult{Analysis: Analysis{}}
	files := []string{}
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !isScannableFile(info.Name()) {
			return nil
		}
		files = append(files, path)
		return nil
	})
	for index, path := range files {
		// Bounded read: the scanner only analyzes the first maxFileRead bytes
		// (Analyze applies the same cap), so huge bundles cannot balloon
		// memory. total_size still reports the on-disk size.
		data, err := readFileHead(path, maxFileRead)
		if err != nil {
			if progress != nil {
				progress(index+1, len(files))
			}
			continue
		}
		result.FilesScanned = append(result.FilesScanned, path)
		if info, statErr := os.Stat(path); statErr == nil {
			result.TotalSize += info.Size()
		} else {
			result.TotalSize += int64(len(data))
		}
		content := string(data)
		analysis := Analyze(content)
		for name, re := range compiled {
			seen := map[string]bool{}
			for _, m := range re.FindAllStringSubmatch(content, -1) {
				value := m[0]
				if len(m) > 1 && m[1] != "" {
					value = m[1]
				}
				if value == "" || seen[value] {
					continue
				}
				seen[value] = true
				analysis[name] = append(analysis[name], value)
			}
		}
		result.Findings = append(result.Findings, FindingsFromAnalysis(analysis, content, relativeScanPath(dir, path))...)
		results = append(results, analysis)
		if progress != nil {
			progress(index+1, len(files))
		}
	}
	if progress != nil && len(files) == 0 {
		progress(0, 0)
	}
	// Merge only folds the builtin categories; user categories are appended
	// here so custom pattern keys survive the merge.
	merged := Merge(results)
	seenCustom := map[string]map[string]bool{}
	for _, analysis := range results {
		for key, values := range analysis {
			if isBuiltinCategory(key) {
				continue
			}
			if seenCustom[key] == nil {
				seenCustom[key] = map[string]bool{}
			}
			for _, v := range values {
				if !seenCustom[key][v] {
					seenCustom[key][v] = true
					merged[key] = append(merged[key], v)
				}
			}
		}
	}
	for key := range seenCustom {
		sort.Strings(merged[key])
	}
	result.Analysis = merged
	sortFindings(result.Findings)
	return result, nil
}

func isScannableFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".js", ".ts", ".json", ".wxml", ".wxss", ".html", ".htm", ".map", ".txt", ".xml":
		return true
	default:
		return false
	}
}

func relativeScanPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return filepath.Base(path)
	}
	return filepath.ToSlash(relative)
}

func sortFindings(findings []Finding) {
	severityRank := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3, "info": 4}
	sort.SliceStable(findings, func(i, j int) bool {
		left, right := severityRank[findings[i].Severity], severityRank[findings[j].Severity]
		if left != right {
			return left < right
		}
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].ID < findings[j].ID
	})
}

func isBuiltinCategory(key string) bool {
	for _, c := range Categories {
		if c == key {
			return true
		}
	}
	return false
}

// readFileHead reads at most limit bytes of path. Callers analyze only the
// prefix, so a bounded read keeps memory flat for oversized files.
func readFileHead(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, int64(limit)))
}

// ValidateAppID rejects identifiers that would escape the reports directory
// (or any other directory) when joined into a path: empty values, "..",
// separators and path-like input. It is the single source of the rule for the
// IPC handlers, the MCP tools and the report writer.
func ValidateAppID(appID string) error {
	trimmed := strings.TrimSpace(appID)
	if trimmed == "" {
		return fmt.Errorf("appid is required")
	}
	if trimmed != appID {
		return fmt.Errorf("appid 不能包含首尾空白: %q", appID)
	}
	if trimmed == "." || trimmed == ".." {
		return fmt.Errorf("appid 非法: %q", appID)
	}
	if strings.ContainsAny(trimmed, `/\`) {
		return fmt.Errorf("appid 不能包含路径分隔符: %q", appID)
	}
	if filepath.Base(trimmed) != trimmed {
		return fmt.Errorf("appid 非法: %q", appID)
	}
	return nil
}
