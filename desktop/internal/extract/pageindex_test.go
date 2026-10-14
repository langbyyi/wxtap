package extract

// page-index.json 的形状：主包/子包页面取自 app.json，插件页面取
// plugin-private 下实际恢复出的 wxml；索引页必须有 wxml 才算页面。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWritePageIndexGroupsMainSubAndPlugin(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app.json":                            `{"pages":["pages/index/index","pages/onlyconfig/config"],"subPackages":[{"root":"pkgA","pages":["pages/sub/page"]}]}`,
		"pages/index/index.wxml":              "<view/>",
		"pages/index/index.wxss":              "view{}",
		"pages/index/index.js":                "Page({})",
		"pages/index/index.json":              "{}",
		"pkgA/pages/sub/page.wxml":            "<view/>",
		"components/badge/badge.wxml":         "<view/>",
		"plugin-private/wxPLUGIN123/x.y.wxml": "<view/>",
	})
	if err := writePageIndex(root); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "page-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index pageIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	byPage := map[string]pageIndexEntry{}
	for _, entry := range index.Pages {
		byPage[entry.Page] = entry
	}
	main, ok := byPage["pages/index/index"]
	if !ok || main.Pkg != "main" || main.Files["wxml"] != "pages/index/index.wxml" ||
		main.Files["wxss"] == "" || main.Files["js"] == "" || main.Files["json"] == "" {
		t.Fatalf("主包页面应带全部文件映射: %+v", main)
	}
	if _, ok := byPage["pages/onlyconfig/config"]; ok {
		t.Fatal("app.json 列了但没有恢复出 wxml 的页面不该进索引")
	}
	if _, ok := byPage["components/badge/badge"]; ok {
		t.Fatal("组件不该混进页面索引")
	}
	sub, ok := byPage["pkgA/pages/sub/page"]
	if !ok || sub.Pkg != "sub:pkgA" {
		t.Fatalf("子包页面应标 sub:<root>: %+v", sub)
	}
	plugin, ok := byPage["plugin-private/wxPLUGIN123/x.y"]
	if !ok || !strings.HasPrefix(plugin.Pkg, "plugin:") {
		t.Fatalf("插件页面应标 plugin:<appid>: %+v", plugin)
	}
	if index.Packages["main"] != 1 || index.Packages["sub:pkgA"] != 1 || index.Packages["plugin:wxPLUGIN123"] != 1 {
		t.Fatalf("分组计数不对: %v", index.Packages)
	}
}

// app.json 缺失或坏了：索引降级为插件-only，绝不拦反编译。
func TestWritePageIndexSurvivesAMissingAppJSON(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"plugin-private/wxSOLO/p.wxml": "<view/>",
	})
	if err := writePageIndex(root); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "page-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index pageIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Pages) != 1 || index.Pages[0].Pkg != "plugin:wxSOLO" {
		t.Fatalf("应有且仅有插件页面: %+v", index.Pages)
	}
}
