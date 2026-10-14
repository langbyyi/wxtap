package extract

// page-index.json：页面路径 → 恢复出的 wxml/wxss/js/json + 包归属（主包 /
// 子包 / 插件）。反编译输出的页面与文件的对应原本只在恢复时刻隐式成立，agent
// 离线审计时只能整树翻——这份索引把对应关系显式化。

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const pageIndexName = "page-index.json"

// appJSON is the slice of a restored app.json the index needs.
type appJSON struct {
	Pages       []string `json:"pages"`
	SubPackages []struct {
		Root  string   `json:"root"`
		Pages []string `json:"pages"`
	} `json:"subPackages"`
	Subpackages []struct {
		Root  string   `json:"root"`
		Pages []string `json:"pages"`
	} `json:"subpackages"`
}

type pageIndexEntry struct {
	Page  string            `json:"page"`
	Pkg   string            `json:"pkg"`
	Files map[string]string `json:"files,omitempty"`
}

type pageIndex struct {
	Pages    []pageIndexEntry `json:"pages"`
	Packages map[string]int   `json:"packages"`
}

// writePageIndex generates page-index.json inside the staged tree so the
// atomic swap ships it with everything else. Main and subpackage pages come
// from the restored app.json; plugin pages are every wxml under
// plugin-private/ (plugin packages offer no reliable page list). Best-effort:
// a missing or malformed app.json yields a plugin-only index, never a failure.
func writePageIndex(root string) error {
	index := pageIndex{Packages: map[string]int{}}
	pages := map[string]string{} // page path → pkg label

	var app appJSON
	if raw, err := os.ReadFile(filepath.Join(root, "app.json")); err == nil {
		_ = json.Unmarshal(raw, &app)
	}
	for _, page := range app.Pages {
		pages[path.Clean(strings.TrimPrefix(page, "/"))] = "main"
	}
	for _, sub := range append(app.SubPackages, app.Subpackages...) {
		root_ := path.Clean(strings.TrimPrefix(sub.Root, "/"))
		for _, page := range sub.Pages {
			pages[path.Join(root_, page)] = "sub:" + root_
		}
	}
	for page, pkg := range pages {
		if entry, ok := pageIndexEntryFor(root, page, pkg); ok {
			index.Pages = append(index.Pages, entry)
		}
	}

	// 插件页面：plugin-private/<wxAPPID>/ 下实际恢复出的 wxml 全部列出。
	pluginRoot := filepath.Join(root, "plugin-private")
	if entries, err := os.ReadDir(pluginRoot); err == nil {
		for _, plugin := range entries {
			if !plugin.IsDir() {
				continue
			}
			_ = filepath.WalkDir(filepath.Join(pluginRoot, plugin.Name()), func(p string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".wxml") {
					return nil
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return nil
				}
				page := path.Clean(filepath.ToSlash(rel))
				page = strings.TrimSuffix(page, ".wxml")
				if entry, ok := pageIndexEntryFor(root, page, "plugin:"+plugin.Name()); ok {
					index.Pages = append(index.Pages, entry)
				}
				return nil
			})
		}
	}

	sort.Slice(index.Pages, func(i, j int) bool { return index.Pages[i].Page < index.Pages[j].Page })
	for _, entry := range index.Pages {
		index.Packages[entry.Pkg]++
	}
	raw, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, pageIndexName), raw, 0o600)
}

// pageIndexEntryFor lists the restored files of one page; a page without its
// wxml is not a page and is skipped.
func pageIndexEntryFor(root, page, pkg string) (pageIndexEntry, bool) {
	files := map[string]string{}
	for _, ext := range []string{".wxml", ".wxss", ".js", ".json"} {
		rel := page + ext
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			files[strings.TrimPrefix(ext, ".")] = rel
		}
	}
	if files["wxml"] == "" {
		return pageIndexEntry{}, false
	}
	return pageIndexEntry{Page: page, Pkg: pkg, Files: files}, true
}
