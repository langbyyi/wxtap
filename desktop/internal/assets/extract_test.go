package assets

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// findAsset returns the asset with the given display URL or fails the test.
func findAsset(t *testing.T, items []Asset, url string) Asset {
	t.Helper()
	for _, it := range items {
		if it.URL == url {
			return it
		}
	}
	t.Fatalf("asset %q missing; have %+v", url, items)
	return Asset{}
}

func TestExtractFromCodeClassifiesAndMergesPerFile(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "app.js"), `
var login = "https://api.example.com/v1/login";
var ws = "wss://ws.example.com/conn";
var doc = "see https://docs.example.com/guide. and https://docs.example.com/guide#top";
var img = "https://cdn.example.com/logo.png?a=1&b=2";
`)
	writeFixture(t, filepath.Join(dir, "config.json"), `{"push":"wss://ws.example.com/conn"}`)
	writeFixture(t, filepath.Join(dir, "pages", "index.wxml"), `
<image src="https://cdn.example.com/logo.png?a=1&b=2" />
<web-view src="http://api.example.com/v1/login"></web-view>
`)

	items, files, err := ExtractFromCode(dir)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if files != 3 {
		t.Fatalf("files = %d, want 3", files)
	}

	login := findAsset(t, items, "https://api.example.com/v1/login")
	if login.Kind != KindAPI || login.Method != "*" || login.Hits != 1 {
		t.Fatalf("login asset mismatch: %+v", login)
	}
	if !reflect.DeepEqual(login.Sources, []Source{{Type: "code", Ref: "app.js"}}) {
		t.Fatalf("login sources = %+v", login.Sources)
	}

	ws := findAsset(t, items, "wss://ws.example.com/conn")
	if ws.Kind != KindWS || ws.Hits != 2 || len(ws.Sources) != 2 {
		t.Fatalf("ws asset mismatch: %+v", ws)
	}
	if ws.Sources[0].Ref != "app.js" || ws.Sources[1].Ref != "config.json" {
		t.Fatalf("ws sources = %+v (want walk order app.js, config.json)", ws.Sources)
	}

	doc := findAsset(t, items, "https://docs.example.com/guide")
	if doc.Hits != 2 || len(doc.Sources) != 1 {
		t.Fatalf("doc asset mismatch (hits and sources must fold): %+v", doc)
	}

	img := findAsset(t, items, "https://cdn.example.com/logo.png?a&b")
	if img.Kind != KindStatic {
		t.Fatalf("img kind = %s, want static", img.Kind)
	}
	if !reflect.DeepEqual(img.Tags, []string{"q:a", "q:b"}) {
		t.Fatalf("img tags = %+v", img.Tags)
	}
	if img.Sources[0].Ref != "app.js" || img.Sources[1].Ref != "pages/index.wxml" {
		t.Fatalf("img sources = %+v (want slash-separated relative refs)", img.Sources)
	}

	webview := findAsset(t, items, "http://api.example.com/v1/login")
	if webview.Kind != KindAPI || webview.Hits != 1 {
		t.Fatalf("web-view asset mismatch (http and https stay separate): %+v", webview)
	}
}

func TestExtractFromCodeNormalizesURLs(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "inline.js"), `
u1 https://tail.example.com/path]]}...,
u2 https://q.example.com/p?x=1,,;
u3 https://frag.example.com/home#section.
u4 wss://bare.example.com
u5 https://port.example.com:8443/x
`)

	items, files, err := ExtractFromCode(dir)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if files != 1 {
		t.Fatalf("files = %d, want 1", files)
	}

	cases := []struct{ url, host, path, kind string }{
		{"https://tail.example.com/path", "tail.example.com", "/path", KindAPI},
		{"https://q.example.com/p?x", "q.example.com", "/p", KindAPI},
		{"https://frag.example.com/home", "frag.example.com", "/home", KindAPI},
		{"wss://bare.example.com/", "bare.example.com", "/", KindWS},
		{"https://port.example.com:8443/x", "port.example.com:8443", "/x", KindAPI},
	}
	if len(items) != len(cases) {
		t.Fatalf("items = %d, want %d: %+v", len(items), len(cases), items)
	}
	for _, c := range cases {
		got := findAsset(t, items, c.url)
		if got.Host != c.host || got.Path != c.path || got.Kind != c.kind {
			t.Fatalf("%s: got host=%s path=%s kind=%s", c.url, got.Host, got.Path, got.Kind)
		}
	}
	if tags := findAsset(t, items, "https://q.example.com/p?x").Tags; !reflect.DeepEqual(tags, []string{"q:x"}) {
		t.Fatalf("query tags = %+v, want [q:x]", tags)
	}
}

func TestExtractFromCodeSkipsNodeModulesGitAndOversize(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), `"https://skipped.example.com/a"`)
	writeFixture(t, filepath.Join(dir, ".git", "hooks", "pre.js"), `"https://skipped.example.com/b"`)
	writeFixture(t, filepath.Join(dir, "ok.js"), `"https://kept.example.com/a"`)
	big := "// pad\n" + strings.Repeat("x", 2_000_000) + `"https://skipped.example.com/c";`
	writeFixture(t, filepath.Join(dir, "big.js"), big)

	items, files, err := ExtractFromCode(dir)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if files != 1 {
		t.Fatalf("files = %d, want 1 (node_modules, .git and the oversize file are not scanned)", files)
	}
	if len(items) != 1 || items[0].URL != "https://kept.example.com/a" {
		t.Fatalf("items = %+v, want only kept.example.com", items)
	}
}

func TestExtractFromCodeCountsHitsPerOccurrenceButOneSourcePerFile(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "multi.js"), `
fetch("https://api.example.com/v1/ping");
fetch("https://api.example.com/v1/ping");
retry("https://api.example.com/v1/ping");
`)

	items, files, err := ExtractFromCode(dir)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if files != 1 {
		t.Fatalf("files = %d, want 1", files)
	}
	got := findAsset(t, items, "https://api.example.com/v1/ping")
	if got.Hits != 3 || len(got.Sources) != 1 {
		t.Fatalf("hits = %d, sources = %d, want 3 and 1", got.Hits, len(got.Sources))
	}
}

func TestExtractFromCodeRejectsMissingDir(t *testing.T) {
	if _, _, err := ExtractFromCode(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("want an error for a missing directory")
	}
}

func TestExtractFromRecords(t *testing.T) {
	t1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	records := []traffic.Record{
		{ID: "r1", CapturedAt: t1, Method: "POST", URL: "https://api.example.com/v1/upload?a=1"},
		{ID: "r2", CapturedAt: t2, Method: "post", URL: "https://api.example.com/v1/upload?b=2"},
		{ID: "r3", CapturedAt: t2, Method: "GET", URL: "https://api.example.com/v1/upload"},
		{ID: "r4", CapturedAt: t2, Method: "", URL: "https://api.example.com/v2/info"},
		{ID: "r5", CapturedAt: t2, Method: "GET", URL: "not a url"},
		{ID: "r6", CapturedAt: t2, Method: "GET", URL: ""},
	}

	items := ExtractFromRecords(records)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3: %+v", len(items), items)
	}

	upload := findAsset(t, items, "https://api.example.com/v1/upload?a")
	if upload.Method != "POST" || upload.Hits != 2 || len(upload.Sources) != 2 {
		t.Fatalf("upload asset: %+v", upload)
	}
	if !upload.FirstSeen.Equal(t1) || !upload.LastSeen.Equal(t2) {
		t.Fatalf("upload window = %v..%v, want %v..%v", upload.FirstSeen, upload.LastSeen, t1, t2)
	}
	if !reflect.DeepEqual(upload.Tags, []string{"q:a", "q:b"}) {
		t.Fatalf("upload tags = %+v", upload.Tags)
	}
	if upload.Sources[0] != (Source{Type: "traffic", Ref: "r1"}) || upload.Sources[1] != (Source{Type: "traffic", Ref: "r2"}) {
		t.Fatalf("upload sources = %+v", upload.Sources)
	}

	direct := findAsset(t, items, "https://api.example.com/v1/upload")
	if direct.Method != "GET" || direct.Hits != 1 {
		t.Fatalf("GET asset: %+v", direct)
	}
	info := findAsset(t, items, "https://api.example.com/v2/info")
	if info.Method != "GET" { // empty method falls back to GET
		t.Fatalf("info method = %s, want GET", info.Method)
	}
}
