package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	body     string
}

func regular(name, body string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeReg, mode: 0o644, body: body}
}

func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressor)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Mode: entry.mode}
		if entry.typeflag == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := archive.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// payloadTree is a minimal archive that satisfies the staged-tree check.
// payloadTree is a Windows-shaped payload: the shell sits beside the Core
// script, which is the shape layoutFor("windows").required names. Tests that
// stage it must therefore name that layout explicitly — going through
// StageVersion would check a Windows-shaped tree against whatever layout the
// host happens to have, which only holds on Windows.
func payloadTree(t *testing.T, marker string) []byte {
	t.Helper()
	return buildTarGz(t, []tarEntry{
		regular("WxTap.exe", "exe-"+marker),
		regular("core/dist/cli.js", "core-"+marker),
	})
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// payloadServer serves parts at /part0, /part1, … and counts requests.
func payloadServer(t *testing.T, parts [][]byte) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/part"))
		if err != nil || index < 0 || index >= len(parts) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write(parts[index])
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func assetFor(server string, index int, name string, payload []byte) Asset {
	return Asset{
		Name:   name,
		Size:   int64(len(payload)),
		SHA256: digestOf(payload),
		URLs:   []string{server + "/part" + strconv.Itoa(index)},
	}
}

func TestDownloadAssetVerifiesAndStores(t *testing.T) {
	payload := payloadTree(t, "one")
	server, _ := payloadServer(t, [][]byte{payload})
	dest := t.TempDir()

	var lastDone, lastTotal int64
	path, err := DownloadAsset(context.Background(), assetFor(server.URL, 0, "a.tar.gz", payload), dest, func(done, total int64) {
		lastDone, lastTotal = done, total
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "a.tar.gz" {
		t.Fatalf("path: %s", path)
	}
	stored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("stored bytes differ: %v", err)
	}
	if lastDone != int64(len(payload)) || lastTotal != int64(len(payload)) {
		t.Fatalf("progress: %d/%d, want %d", lastDone, lastTotal, len(payload))
	}
	assertNoPartials(t, dest)
}

// The manifest is the only authority on what a shard should be, so a tampered
// or truncated transfer must never reach the extraction step.
func TestDownloadAssetRejectsMismatchedPayload(t *testing.T) {
	payload := payloadTree(t, "one")
	server, _ := payloadServer(t, [][]byte{payload})
	dest := t.TempDir()

	asset := assetFor(server.URL, 0, "a.tar.gz", payload)
	asset.Size++
	if _, err := DownloadAsset(context.Background(), asset, dest, nil); err == nil || !strings.Contains(err.Error(), "大小不符") {
		t.Fatalf("size mismatch: %v", err)
	}
	assertNoPartials(t, dest)

	asset = assetFor(server.URL, 0, "a.tar.gz", payload)
	asset.SHA256 = digestOf([]byte("something else"))
	if _, err := DownloadAsset(context.Background(), asset, dest, nil); err == nil || !strings.Contains(err.Error(), "sha256 不符") {
		t.Fatalf("digest mismatch: %v", err)
	}
	assertNoPartials(t, dest)
	if _, err := os.Stat(filepath.Join(dest, "a.tar.gz")); !os.IsNotExist(err) {
		t.Fatal("a rejected download must not take the final name")
	}
}

func TestDownloadAssetFallsBackToNextSource(t *testing.T) {
	payload := payloadTree(t, "one")
	server, _ := payloadServer(t, [][]byte{payload})
	dest := t.TempDir()

	asset := assetFor(server.URL, 0, "a.tar.gz", payload)
	asset.URLs = []string{server.URL + "/missing", server.URL + "/part0"}
	if _, err := DownloadAsset(context.Background(), asset, dest, nil); err != nil {
		t.Fatalf("fallback should have succeeded: %v", err)
	}
}

// A verified archive is reused, so retrying after a later shard fails does not
// re-download the megabytes that were already fine.
func TestDownloadAssetReusesVerifiedArchive(t *testing.T) {
	payload := payloadTree(t, "one")
	server, hits := payloadServer(t, [][]byte{payload})
	dest := t.TempDir()
	asset := assetFor(server.URL, 0, "a.tar.gz", payload)

	if _, err := DownloadAsset(context.Background(), asset, dest, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := DownloadAsset(context.Background(), asset, dest, nil); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("verified archive was fetched %d times, want 1", got)
	}
}

func TestDownloadAssetStopsOnCancelledContext(t *testing.T) {
	payload := payloadTree(t, "one")
	server, _ := payloadServer(t, [][]byte{payload})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dest := t.TempDir()
	if _, err := DownloadAsset(ctx, assetFor(server.URL, 0, "a.tar.gz", payload), dest, nil); err == nil {
		t.Fatal("a cancelled download must fail")
	}
	assertNoPartials(t, dest)
}

func TestExtractTarGzWritesTree(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	data := buildTarGz(t, []tarEntry{
		{name: "core/", typeflag: tar.TypeDir, mode: 0o755},
		regular("core/dist/cli.js", "console.log(1)"),
		regular("WxTap.exe", "exe"),
	})
	if err := os.WriteFile(archive, data, 0o600); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := ExtractTarGz(archive, dest); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "core", "dist", "cli.js"))
	if err != nil || string(body) != "console.log(1)" {
		t.Fatalf("cli.js: %q %v", body, err)
	}
}

// A release has no reason to carry links or absolute paths; honouring them
// would let a crafted archive write outside the staging root.
func TestExtractTarGzRejectsUnsafeEntries(t *testing.T) {
	cases := []struct {
		name   string
		entry  tarEntry
		expect string
	}{
		{"absolute path", regular("/etc/passwd", "x"), "越出目标目录"},
		{"parent traversal", regular("../escaped.txt", "x"), "越出目标目录"},
		{"nested traversal", regular("core/../../escaped.txt", "x"), "越出目标目录"},
		{"symlink", tarEntry{name: "link", typeflag: tar.TypeSymlink, mode: 0o777, body: "/etc/passwd"}, "不支持的条目"},
		{"hard link", tarEntry{name: "hard", typeflag: tar.TypeLink, mode: 0o644, body: "WxTap.exe"}, "不支持的条目"},
		{"windows drive-relative", regular("core:C:/evil", "x"), "越出目标目录"},
	}
	for _, testCase := range cases {
		archive := filepath.Join(t.TempDir(), "a.tar.gz")
		if err := os.WriteFile(archive, buildTarGz(t, []tarEntry{testCase.entry}), 0o600); err != nil {
			t.Fatal(err)
		}
		dest := t.TempDir()
		err := ExtractTarGz(archive, dest)
		if err == nil || !strings.Contains(err.Error(), testCase.expect) {
			t.Errorf("%s: err = %v, want it to mention %q", testCase.name, err, testCase.expect)
		}
		if entries, readErr := os.ReadDir(dest); readErr == nil && len(entries) != 0 {
			t.Errorf("%s: refused archive still wrote %v", testCase.name, entries)
		}
	}
}

func TestExtractTarGzRejectsTruncatedEntry(t *testing.T) {
	// A header that claims more bytes than the archive carries.
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressor)
	if err := archive.WriteHeader(&tar.Header{Name: "WxTap.exe", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4096}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("short")); err != nil {
		t.Fatal(err)
	}
	_ = archive.Close()
	_ = compressor.Close()

	path := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTarGz(path, t.TempDir()); err == nil {
		t.Fatal("a truncated entry must be an error")
	}
}

func TestStageVersionMarksPendingOnlyWhenComplete(t *testing.T) {
	partA := payloadTree(t, "a")
	partB := payloadTree(t, "b")
	server, _ := payloadServer(t, [][]byte{partA, partB})
	dataDir := t.TempDir()

	manifest := Manifest{Version: "v2.1.0", Assets: []Asset{
		assetFor(server.URL, 0, "WxTap-2.1.0-a.tar.gz", partA),
		assetFor(server.URL, 1, "WxTap-2.1.0-b.tar.gz", partB),
	}}
	if _, err := stageVersionFor(context.Background(), manifest, dataDir, nil, layoutFor("windows")); err != nil {
		t.Fatal(err)
	}

	state, ok := StagedVersion(dataDir)
	if !ok || state.Version != "v2.1.0" {
		t.Fatalf("pending marker: %+v ok=%v", state, ok)
	}
	if _, err := os.Stat(filepath.Join(stagedDir(dataDir, "v2.1.0"), "core", "dist", "cli.js")); err != nil {
		t.Fatalf("staged tree: %v", err)
	}
	assertNoPartials(t, filepath.Join(updateRoot(dataDir), "downloads"))
}

func TestStageVersionLeavesNoPendingOnFailure(t *testing.T) {
	partA := payloadTree(t, "a")
	partB := payloadTree(t, "b")
	server, _ := payloadServer(t, [][]byte{partA, partB})
	dataDir := t.TempDir()

	broken := assetFor(server.URL, 1, "WxTap-2.1.0-b.tar.gz", partB)
	broken.SHA256 = digestOf([]byte("not the payload"))
	manifest := Manifest{Version: "v2.1.0", Assets: []Asset{
		assetFor(server.URL, 0, "WxTap-2.1.0-a.tar.gz", partA),
		broken,
	}}
	if _, err := stageVersionFor(context.Background(), manifest, dataDir, nil, layoutFor("windows")); err == nil {
		t.Fatal("a failing shard must fail the stage")
	}
	if _, ok := StagedVersion(dataDir); ok {
		t.Fatal("an incomplete stage must not be marked pending")
	}
	if _, err := os.Stat(stagedDir(dataDir, "v2.1.0")); !os.IsNotExist(err) {
		t.Fatal("an incomplete stage must not leave a tree behind")
	}
	assertNoPartials(t, filepath.Join(updateRoot(dataDir), "downloads"))
}

// A tree missing the Core script would be swapped in and then rolled back in
// front of the user, so it is refused at stage time instead.
func TestStageVersionRejectsIncompletePayload(t *testing.T) {
	part := buildTarGz(t, []tarEntry{regular("WxTap.exe", "exe")})
	server, _ := payloadServer(t, [][]byte{part})
	dataDir := t.TempDir()

	manifest := Manifest{Version: "v2.1.0", Assets: []Asset{assetFor(server.URL, 0, "a.tar.gz", part)}}
	if _, err := stageVersionFor(context.Background(), manifest, dataDir, nil, layoutFor("windows")); err == nil || !strings.Contains(err.Error(), "更新包不完整") {
		t.Fatalf("incomplete payload: %v", err)
	}
	if _, ok := StagedVersion(dataDir); ok {
		t.Fatal("an incomplete payload must not be marked pending")
	}
}

// The required entries are per-platform, and the darwin one names the shell
// without the .exe suffix. Staging that shape has to be covered here rather
// than only on a Mac: a payload that omits it would otherwise be staged and
// then rolled back in front of the user.
func TestStageVersionAcceptsTheDarwinPayloadShape(t *testing.T) {
	part := buildTarGz(t, []tarEntry{
		regular("WxTap", "exe"),
		regular("core/dist/cli.js", "core"),
	})
	server, _ := payloadServer(t, [][]byte{part})
	dataDir := t.TempDir()

	manifest := Manifest{Version: "v2.1.0", Assets: []Asset{assetFor(server.URL, 0, "a.tar.gz", part)}}
	if _, err := stageVersionFor(context.Background(), manifest, dataDir, nil, layoutFor("darwin")); err != nil {
		t.Fatalf("the darwin payload shape must stage: %v", err)
	}
	if _, ok := StagedVersion(dataDir); !ok {
		t.Fatal("a complete darwin payload must be marked pending")
	}
}

func TestStageVersionRejectsAnUnsafeVersion(t *testing.T) {
	part := payloadTree(t, "a")
	server, _ := payloadServer(t, [][]byte{part})
	dataDir := t.TempDir()

	manifest := Manifest{Version: "../escape", Assets: []Asset{assetFor(server.URL, 0, "a.tar.gz", part)}}
	if _, err := StageVersion(context.Background(), manifest, dataDir, nil); err == nil || !strings.Contains(err.Error(), "不能作目录名") {
		t.Fatalf("unsafe version: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "escape")); !os.IsNotExist(err) {
		t.Fatal("a version with a separator must not become a path")
	}
}

func TestStagedVersionIgnoresUnreadableMarker(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(updateRoot(dataDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath(dataDir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := StagedVersion(dataDir); ok {
		t.Fatal("a corrupt marker must not report a staged version")
	}
}

func assertNoPartials(t *testing.T, dir string) {
	t.Helper()
	partials, err := filepath.Glob(filepath.Join(dir, "*.part"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("partial downloads left behind: %v", partials)
	}
}
