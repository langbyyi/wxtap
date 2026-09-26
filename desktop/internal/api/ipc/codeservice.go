// Code-browser IPC surface: file tree, paged file reading, grep-style
// search, and the extension bookkeeping shared by the format handlers.
package ipc

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// FileNode is one entry in the code browser tree (directories first,
// case-insensitive names).
type FileNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	IsDir    bool       `json:"isDir"`
	Children []FileNode `json:"-"`
}

// MarshalJSON keeps expanded-but-empty directories as "children": [] — the
// default omitempty would drop the empty slice the frontend relies on to
// mark a node as loaded.
func (n FileNode) MarshalJSON() ([]byte, error) {
	out := map[string]any{"name": n.Name, "path": n.Path, "isDir": n.IsDir}
	if n.Children != nil {
		out["children"] = n.Children
	}
	return json.Marshal(out)
}

// CodeProject is one decompiled mini program available for auditing.
type CodeProject struct {
	AppID          string `json:"appid"`
	Name           string `json:"name"`
	Subject        string `json:"subject"`
	IconPath       string `json:"icon_path"`
	IconDataURL    string `json:"icon_data_url"`
	IconConfidence string `json:"icon_confidence"`
	MetadataSource string `json:"metadata_source"`
	Path           string `json:"path"`
	Mtime          int64  `json:"mtime"`
}

// DecompiledProjects lists the per-appid directories under the extraction
// output root, newest first, so the code browser can audit one mini program
// instead of an arbitrary folder.
func DecompiledProjects(root string) []CodeProject {
	entries, err := os.ReadDir(root)
	if err != nil {
		return []CodeProject{}
	}
	projects := make([]CodeProject, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		metadata := ReadMiniAppMetadata(path)
		projects = append(projects, CodeProject{
			AppID:          entry.Name(),
			Name:           ReadMiniAppName(path),
			Subject:        ReadMiniAppSubject(path),
			IconPath:       metadata.IconPath,
			IconDataURL:    metadata.IconDataURL,
			IconConfidence: metadata.IconConfidence,
			MetadataSource: metadata.MetadataSource,
			Path:           path,
			Mtime:          info.ModTime().Unix(),
		})
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Mtime != projects[j].Mtime {
			return projects[i].Mtime > projects[j].Mtime
		}
		return projects[i].AppID < projects[j].AppID
	})
	return projects
}

// BuildFileTree lists dir down to depth levels of expansion. A nil result
// means the directory could not be read; an expanded-but-empty directory
// marshals as [] (empty array, not null).
func BuildFileTree(dir string, depth int) []FileNode {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	items := []FileNode{}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		full := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			var children []FileNode
			if depth > 0 {
				children = BuildFileTree(full, depth-1)
			}
			items = append(items, FileNode{Name: entry.Name(), Path: full, IsDir: true, Children: children})
		} else {
			items = append(items, FileNode{Name: entry.Name(), Path: full, IsDir: false})
		}
	}
	return items
}

// ExpandDir lists one directory level for lazy tree expansion.
func ExpandDir(dir string) []FileNode {
	return BuildFileTree(dir, 1)
}

// CodeFile is the readFile payload: content (possibly truncated), the real
// file size, and a highlighter hint language.
type CodeFile struct {
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	Language  string `json:"language"`
	Truncated bool   `json:"truncated"`
	// Kind picks the editor pane: "text" is source, "image" is inlined in
	// DataURL, "binary" is anything else the viewer can only describe.
	// Without it a PNG's bytes reach the highlighter and render as mojibake.
	Kind string `json:"kind"`
	// DataURL carries the inlined image bytes; empty when the file is over
	// the inline cap, in which case Content says why.
	DataURL string `json:"dataUrl"`
}

// codeReadLimit keeps the frontend payload bounded to a 1MB cap.
const codeReadLimit = 1024 * 1024

// inlineImageLimit bounds an inlined image. Base64 costs a third on top, and
// an asset past a few megabytes has nothing left to show in a code audit.
const inlineImageLimit = 4 << 20

// codeImageMimes is the extension whitelist rendered as a picture instead of
// source. .svg is deliberately absent: it is markup an audit wants to read,
// and it stays in the source pane like any other text file.
var codeImageMimes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".ico":  "image/x-icon",
	".avif": "image/avif",
}

// hasBinaryHead reports whether data looks like a binary file. Decompiled
// packages are full of fonts, archives and wasm; rendering their bytes as
// source shows replacement characters and nothing an audit can use.
func hasBinaryHead(data []byte) bool {
	if len(data) > binarySniffSize {
		data = data[:binarySniffSize]
	}
	return bytes.IndexByte(data, 0) >= 0
}

// codeLanguages maps extensions to display languages; it doubles as the
// format whitelist for formatFile.
var codeLanguages = map[string]string{
	".js":   "javascript",
	".json": "json",
	".html": "html",
	".wxml": "xml",
	".wxss": "css",
	".css":  "css",
	".ts":   "typescript",
	".xml":  "xml",
	".md":   "markdown",
}

// ReadCodeFile loads a file with the 1MB truncation marker. Images come back
// as an inline data URL, other binaries as a one-line description.
func ReadCodeFile(path string) CodeFile {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return CodeFile{Language: "text", Kind: "text"}
	}
	if mimeType, ok := codeImageMimes[strings.ToLower(filepath.Ext(path))]; ok {
		return readCodeImage(path, info, mimeType)
	}
	file, err := os.Open(path)
	if err != nil {
		return CodeFile{Content: fmt.Sprintf("读取失败: %v", err), Size: info.Size(), Language: "text", Kind: "text"}
	}
	defer func() { _ = file.Close() }()

	data, readErr := io.ReadAll(io.LimitReader(file, codeReadLimit))
	if readErr != nil && len(data) == 0 {
		return CodeFile{Content: fmt.Sprintf("读取失败: %v", readErr), Size: info.Size(), Language: "text", Kind: "text"}
	}
	if hasBinaryHead(data) {
		return CodeFile{
			Content:  fmt.Sprintf("二进制文件（%s），无法按源码显示。", formatByteSize(info.Size())),
			Size:     info.Size(),
			Language: "text",
			Kind:     "binary",
		}
	}
	content := string(data)
	truncated := info.Size() > codeReadLimit
	if truncated {
		content += fmt.Sprintf("\n\n... 文件过大，已截断显示（%d bytes）", info.Size())
	}
	language := "text"
	if mapped, ok := codeLanguages[strings.ToLower(filepath.Ext(path))]; ok {
		language = mapped
	}
	return CodeFile{Content: content, Size: info.Size(), Language: language, Truncated: truncated, Kind: "text"}
}

// readCodeImage inlines an image as a data URL, which is all the viewer's
// image pane needs. An image past the cap keeps the kind so the viewer still
// says "图片" instead of falling back to a source pane full of mojibake, with
// Content carrying the reason no picture is shown.
func readCodeImage(path string, info os.FileInfo, mimeType string) CodeFile {
	out := CodeFile{Size: info.Size(), Language: mimeType, Kind: "image"}
	if info.Size() > inlineImageLimit {
		out.Content = fmt.Sprintf("图片 %s，超过 %s 内联上限，未显示。", formatByteSize(info.Size()), formatByteSize(inlineImageLimit))
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		out.Content = fmt.Sprintf("读取失败: %v", err)
		return out
	}
	if len(data) == 0 {
		out.Content = "图片内容为空。"
		return out
	}
	out.DataURL = fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data))
	return out
}

// formatByteSize renders a byte count for the reader (KB/MB, one decimal).
func formatByteSize(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(size)/float64(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(size)/float64(1<<10))
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}

// SearchHit is one matched line in a code search.
type SearchHit struct {
	File string `json:"file"`
	Line int    `json:"line"`
	// Column is the 1-based rune offset of the match inside Text (0 when
	// unknown), so a caller can locate the hit without re-scanning the line.
	Column int    `json:"column,omitempty"`
	Text   string `json:"text"`
}

// searchHitCap is the 500-result ceiling for code.search.
const searchHitCap = 500

// searchLineCap keeps each returned line bounded to 200 characters.
const searchLineCap = 200

// searchContextBefore is how much of the line the excerpt keeps before the
// match; the rest of the line cap continues after it. Decompiled bundles are
// routinely one minified line tens of thousands of columns long; excerpting
// from the line start shows the caller the file's boilerplate instead of the
// hit it asked about.
const searchContextBefore = 60

// binarySniffSize bounds the NUL-byte probe used to skip binary files.
const binarySniffSize = 8000

// looksBinary reports whether file starts with binary content. A decompiled
// package is full of images, fonts and archives; grepping their bytes returns
// matches no one asked for and buries the real ones. The read position is
// rewound so the caller can keep scanning the file when it is text.
func looksBinary(file *os.File) bool {
	head := make([]byte, binarySniffSize)
	// A short file ends with ErrUnexpectedEOF/EOF; whatever was read still
	// answers the question.
	n, _ := io.ReadFull(file, head)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		// Without a rewind the scan would start past the head; skipping the
		// file beats reporting matches from a partial read.
		return true
	}
	return bytes.IndexByte(head[:n], 0) >= 0
}

// SearchCode greps root for query (substring, or regex when useRegex). A
// malformed pattern surfaces as an error so the caller can render it.
// truncated reports that the 500-hit cap ended the walk early, so callers can
// tell a partial result set from a complete one instead of presenting it as
// complete.
func SearchCode(root, query string, useRegex bool) ([]SearchHit, bool, error) {
	if root == "" || query == "" {
		return nil, false, nil
	}
	var pattern *regexp.Regexp
	if useRegex {
		compiled, err := regexp.Compile(query)
		if err != nil {
			return nil, false, fmt.Errorf("正则语法错误")
		}
		pattern = compiled
	}

	var hits []SearchHit
	truncated := false
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory (ACL, reparse point) must not truncate
			// the whole search; skip its contents and keep the siblings.
			if entry != nil && entry.IsDir() && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if len(hits) >= searchHitCap {
			truncated = true
			return filepath.SkipAll
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer func() { _ = file.Close() }()
		if looksBinary(file) {
			return nil
		}

		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for lineNo := 1; scanner.Scan(); lineNo++ {
			if len(hits) >= searchHitCap {
				truncated = true
				return filepath.SkipAll
			}
			line := strings.TrimRight(scanner.Text(), " \t\r\n")
			if (pattern != nil && pattern.MatchString(line)) || (pattern == nil && strings.Contains(line, query)) {
				runes := []rune(line)
				matchStart := 0
				if pattern != nil {
					if loc := pattern.FindStringIndex(line); loc != nil {
						matchStart = utf8.RuneCountInString(line[:loc[0]])
					}
				} else if idx := strings.Index(line, query); idx >= 0 {
					matchStart = utf8.RuneCountInString(line[:idx])
				}
				text, column := excerptAround(runes, matchStart)
				hits = append(hits, SearchHit{File: path, Line: lineNo, Column: column, Text: text})
			}
		}
		return nil
	})
	return hits, truncated, nil
}

// excerptAround windows runes to searchLineCap around the match at matchStart
// (0-based rune offset of the match's first rune in the original line) and
// returns the excerpt plus the 1-based rune offset of the match inside the
// returned Text — with a leading ellipsis that offset shifts by one, since the
// marker occupies Text's first rune. Short lines pass through whole.
func excerptAround(runes []rune, matchStart int) (string, int) {
	column := matchStart + 1
	if len(runes) <= searchLineCap {
		return string(runes), column
	}
	from := matchStart - searchContextBefore
	if from < 0 {
		from = 0
	}
	to := from + searchLineCap
	if to > len(runes) {
		to = len(runes)
		from = to - searchLineCap
	}
	text := string(runes[from:to])
	adjusted := matchStart - from + 1
	if from > 0 {
		text = "…" + text
		adjusted++
	}
	if to < len(runes) {
		text += "…"
	}
	return text, adjusted
}

// FormatLanguage maps a file extension to the beautifier language used by
// the Core's code.format; ok is false for extensions outside the whitelist.
func FormatLanguage(ext string) (string, bool) {
	language, ok := codeLanguages[strings.ToLower(ext)]
	return language, ok
}

// FormatAllExtension reports whether formatAll covers this extension
// (.ts/.xml are intentionally excluded from single-file format).
func FormatAllExtension(ext string) bool {
	return formatAllExtensions[strings.ToLower(ext)]
}

// formatAllExtensions is the formatAll whitelist (.ts/.xml excluded).
var formatAllExtensions = map[string]bool{
	".js": true, ".json": true, ".html": true, ".wxml": true, ".css": true, ".wxss": true,
}
