package update

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// digestOfABC is sha256("abc"), the fixture payload used across these tests.
const digestOfABC = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

func manifestDoc(version string, assets string) string {
	return fmt.Sprintf(`{"version":%q,"notes":"说明","assets":[%s]}`, version, assets)
}

func assetDoc(name string, size int64, digest string, urls ...string) string {
	quoted := make([]string, 0, len(urls))
	for _, url := range urls {
		quoted = append(quoted, fmt.Sprintf("%q", url))
	}
	return fmt.Sprintf(`{"name":%q,"size":%d,"sha256":%q,"urls":[%s]}`, name, size, digest, strings.Join(quoted, ","))
}

// bodyServer serves one body for every path.
func bodyServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// manifestServer serves a fixed body per path.
func manifestServer(t *testing.T, bodies map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFetchManifestUsesFirstSourceThatResolves(t *testing.T) {
	good := manifestDoc("v2.1.0", assetDoc("a.tar.gz", 3, digestOfABC, "https://example.com/a.tar.gz"))
	server := manifestServer(t, map[string]string{"/mirror.json": good})

	manifest, err := FetchManifest(context.Background(), []string{server.URL + "/missing.json", server.URL + "/mirror.json"})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "v2.1.0" || manifest.Notes != "说明" {
		t.Fatalf("manifest: %+v", manifest)
	}
	if len(manifest.Assets) != 1 || manifest.Assets[0].Name != "a.tar.gz" {
		t.Fatalf("assets: %+v", manifest.Assets)
	}
	if manifest.TotalSize() != 3 {
		t.Fatalf("total size: %d", manifest.TotalSize())
	}
}

func TestFetchManifestReportsEverySource(t *testing.T) {
	server := manifestServer(t, map[string]string{"/bad.json": "{not json"})
	_, err := FetchManifest(context.Background(), []string{server.URL + "/missing.json", server.URL + "/bad.json"})
	if err == nil {
		t.Fatal("all sources failing must be an error")
	}
	message := err.Error()
	if !strings.Contains(message, "HTTP 404") || !strings.Contains(message, "清单不是合法 JSON") {
		t.Fatalf("error should name each source's failure: %s", message)
	}
}

func TestFetchManifestRejectsDocuments(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty body", "", "清单不是合法 JSON"},
		{"not an object", `[]`, "清单不是合法 JSON"},
		{"missing version", manifestDoc("", assetDoc("a.tar.gz", 3, digestOfABC, "https://example.com/a")), "清单缺少版本号"},
		{"version with separator", manifestDoc("v2.1.0/../x", assetDoc("a.tar.gz", 3, digestOfABC, "https://example.com/a")), "不能作目录名"},
		{"no assets", manifestDoc("v2.1.0", ""), "没有可下载的分片"},
		{"empty asset name", manifestDoc("v2.1.0", assetDoc("", 3, digestOfABC, "https://example.com/a")), "分片名不安全"},
		{"asset name with slash", manifestDoc("v2.1.0", assetDoc("sub/a.tar.gz", 3, digestOfABC, "https://example.com/a")), "分片名不安全"},
		{"asset name traversal", manifestDoc("v2.1.0", assetDoc("..", 3, digestOfABC, "https://example.com/a")), "分片名不安全"},
		{"duplicate asset name", manifestDoc("v2.1.0", assetDoc("a.tar.gz", 3, digestOfABC, "https://example.com/a")+","+assetDoc("a.tar.gz", 3, digestOfABC, "https://example.com/a")), "分片名重复"},
		{"zero size", manifestDoc("v2.1.0", assetDoc("a.tar.gz", 0, digestOfABC, "https://example.com/a")), "超出范围"},
		{"oversized asset", manifestDoc("v2.1.0", assetDoc("a.tar.gz", maxAssetBytes+1, digestOfABC, "https://example.com/a")), "超出范围"},
		{"short digest", manifestDoc("v2.1.0", assetDoc("a.tar.gz", 3, "abc", "https://example.com/a")), "sha256 长度"},
		{"non-hex digest", manifestDoc("v2.1.0", assetDoc("a.tar.gz", 3, strings.Repeat("z", 64), "https://example.com/a")), "不是合法十六进制"},
		{"no urls", manifestDoc("v2.1.0", assetDoc("a.tar.gz", 3, digestOfABC)), "没有下载地址"},
		{"blank urls", manifestDoc("v2.1.0", assetDoc("a.tar.gz", 3, digestOfABC, "  ", "")), "没有下载地址"},
	}
	for _, testCase := range cases {
		server := bodyServer(t, testCase.body)
		_, err := FetchManifest(context.Background(), []string{server.URL + "/latest.json"})
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: err = %v, want it to mention %q", testCase.name, err, testCase.want)
		}
	}
}

func TestFetchManifestCanonicalisesTheDocument(t *testing.T) {
	body := manifestDoc("v2.1.0", assetDoc("a.tar.gz", 3, strings.ToUpper(digestOfABC), " https://example.com/a ", "", "https://mirror.example.com/a"))
	server := manifestServer(t, map[string]string{"/latest.json": body})

	manifest, err := FetchManifest(context.Background(), []string{server.URL + "/latest.json"})
	if err != nil {
		t.Fatal(err)
	}
	asset := manifest.Assets[0]
	if asset.SHA256 != digestOfABC {
		t.Fatalf("digest not lower-cased: %q", asset.SHA256)
	}
	if len(asset.URLs) != 2 || asset.URLs[0] != "https://example.com/a" || asset.URLs[1] != "https://mirror.example.com/a" {
		t.Fatalf("urls not trimmed of blanks: %#v", asset.URLs)
	}
}

// A remote host must not decide how much memory the shell allocates for a
// document that is a few hundred bytes in practice.
func TestFetchManifestRejectsOversizedDocument(t *testing.T) {
	server := bodyServer(t, `{"version":"v2.1.0","assets":[],"pad":"`+strings.Repeat("a", maxManifestBytes)+`"}`)
	if _, err := FetchManifest(context.Background(), []string{server.URL + "/latest.json"}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized manifest: %v", err)
	}
}

func TestFetchManifestWithoutSources(t *testing.T) {
	if _, err := FetchManifest(context.Background(), nil); err == nil {
		t.Fatal("no configured source must be an error")
	}
}

// otherPlatformKey is a platform this build is never running on, for fixtures
// that need two entries.
func otherPlatformKey() string {
	if PlatformKey() == "windows-amd64" {
		return "darwin-arm64"
	}
	return "windows-amd64"
}

// platformsDoc builds a multi-platform document from one entry per key.
func platformsDoc(version string, entries map[string]string) string {
	parts := make([]string, 0, len(entries))
	for key, assets := range entries {
		parts = append(parts, fmt.Sprintf("%q:{\"assets\":[%s]}", key, assets))
	}
	slices.Sort(parts)
	return fmt.Sprintf(`{"version":%q,"notes":"说明","platforms":{%s}}`, version, strings.Join(parts, ","))
}

func TestFetchManifestResolvesTheRunningPlatform(t *testing.T) {
	mine := assetDoc("mine-a.tar.gz", 1024, digestOfABC, "https://example.invalid/mine")
	theirs := assetDoc("theirs-a.tar.gz", 2048, digestOfABC, "https://example.invalid/theirs")
	server := bodyServer(t, platformsDoc("v2.1.0", map[string]string{
		PlatformKey():      mine,
		otherPlatformKey(): theirs,
	}))

	manifest, err := FetchManifest(context.Background(), []string{server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Assets) != 1 || manifest.Assets[0].Name != "mine-a.tar.gz" {
		t.Fatalf("resolved the wrong platform's payload: %+v", manifest.Assets)
	}
	if manifest.TotalSize() != 1024 {
		t.Fatalf("TotalSize = %d, want the running platform's 1024", manifest.TotalSize())
	}
	// The platform dimension is resolved away, so nothing downstream sees it.
	if manifest.Platforms != nil {
		t.Fatalf("Platforms should be cleared after normalize: %+v", manifest.Platforms)
	}
}

// A release that has not been built for this platform must say so, naming both
// what was wanted and what exists.
func TestResolveAssetsReportsAMissingPlatform(t *testing.T) {
	manifest := Manifest{
		Version:   "v2.1.0",
		Platforms: map[string]Payload{otherPlatformKey(): {Assets: []Asset{{Name: "a.tar.gz", Size: 1, SHA256: digestOfABC, URLs: []string{"https://example.invalid/a"}}}}},
	}
	_, err := manifest.resolveAssets(PlatformKey())
	if err == nil || !strings.Contains(err.Error(), PlatformKey()) || !strings.Contains(err.Error(), otherPlatformKey()) {
		t.Fatalf("err = %v, want it to name both platforms", err)
	}
}

// One document is generated by one script, so a mistake in another platform's
// entry means the document is wrong — better reported than shipped, where it
// would be invisible until that platform's users could not update.
func TestResolveAssetsValidatesEveryPlatform(t *testing.T) {
	manifest := Manifest{
		Version: "v2.1.0",
		Platforms: map[string]Payload{
			PlatformKey():      {Assets: []Asset{{Name: "mine.tar.gz", Size: 1024, SHA256: digestOfABC, URLs: []string{"https://example.invalid/mine"}}}},
			otherPlatformKey(): {Assets: []Asset{{Name: "theirs.tar.gz", Size: 1024, SHA256: "not-a-digest", URLs: []string{"https://example.invalid/theirs"}}}},
		},
	}
	_, err := manifest.resolveAssets(PlatformKey())
	if err == nil || !strings.Contains(err.Error(), otherPlatformKey()) {
		t.Fatalf("err = %v, want it to name the broken platform", err)
	}
}

func TestResolveAssetsRejectsBothForms(t *testing.T) {
	manifest := Manifest{
		Version:   "v2.1.0",
		Assets:    []Asset{{Name: "a.tar.gz", Size: 1, SHA256: digestOfABC, URLs: []string{"https://example.invalid/a"}}},
		Platforms: map[string]Payload{PlatformKey(): {Assets: []Asset{{Name: "b.tar.gz", Size: 1, SHA256: digestOfABC, URLs: []string{"https://example.invalid/b"}}}}},
	}
	if _, err := manifest.resolveAssets(PlatformKey()); err == nil || !strings.Contains(err.Error(), "只能取其一") {
		t.Fatalf("err = %v, want an ambiguous-document rejection", err)
	}
}

// The single-payload form still works: a Windows-only release keeps publishing
// without inventing a macOS entry.
func TestResolveAssetsKeepsTheSinglePayloadForm(t *testing.T) {
	manifest := Manifest{
		Version: "v2.1.0",
		Assets:  []Asset{{Name: "a.tar.gz", Size: 1024, SHA256: strings.ToUpper(digestOfABC), URLs: []string{"https://example.invalid/a"}}},
	}
	assets, err := manifest.resolveAssets(PlatformKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].SHA256 != digestOfABC {
		t.Fatalf("assets = %+v, want the digest canonicalised", assets)
	}
}
