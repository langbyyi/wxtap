package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/langbyyi/wxtap/desktop/internal/update"
)

// fakePublication points the update chain at a manifest served by the test and
// makes its shard transfer hang, which is what opens the staging window these
// tests observe.
func fakePublication(t *testing.T) {
	t.Helper()

	// The transfer is released through a channel rather than by the server
	// closing: Close does not cancel an in-flight handler's context, so a
	// handler waiting on it would deadlock teardown. Cleanups run last-in
	// first-out, so releasing is registered after Close and therefore runs
	// before it.
	release := make(chan struct{})
	shards := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(shards.Close)
	t.Cleanup(func() { close(release) })

	// Declared size and digest only matter once a transfer completes; these
	// tests stop at the transfer itself.
	manifest, err := json.Marshal(update.Manifest{
		Version: "v9.9.9",
		Assets: []update.Asset{{
			Name:   "WxTap-9.9.9-a.tar.gz",
			Size:   1 << 20,
			SHA256: strings.Repeat("0", 64),
			URLs:   []string{shards.URL + "/a.tar.gz"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	publication := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(manifest)
	}))
	t.Cleanup(publication.Close)

	original := update.ManifestURLs
	update.ManifestURLs = []string{publication.URL + "/latest.json"}
	t.Cleanup(func() { update.ManifestURLs = original })
}

func newUpdateContractApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("WXTAP_DATA_DIR", t.TempDir())
	app := NewApp()
	app.ctx = context.Background()
	app.setupIPC()
	return app
}

// A download outlives its request, so the IPC surface has to refuse a second
// one: two concurrent stages would rebuild each other's staging tree. The
// button disables itself, but the router is reachable from outside the UI too.
func TestUpdateDownloadRefusesASecondConcurrentStage(t *testing.T) {
	fakePublication(t)
	app := newUpdateContractApp(t)

	raw, err := app.router.Call(context.Background(), "update.downloadRelease", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("first download: %v", err)
	}
	first, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("first download result: %#v", raw)
	}
	if first["async"] != true || first["version"] != "v9.9.9" {
		t.Fatalf("first download result: %#v", first)
	}
	// The task id is the handle the settings page uses to follow progress; it
	// has to come back even though the work has not finished.
	if taskID, _ := first["task_id"].(string); taskID == "" {
		t.Fatalf("first download did not report a task id: %#v", first)
	}

	if _, err := app.router.Call(context.Background(), "update.downloadRelease", json.RawMessage(`{}`)); err == nil {
		t.Fatal("a second concurrent download must be refused")
	} else if !strings.Contains(err.Error(), "已有更新任务在进行中") {
		t.Fatalf("second download: %v", err)
	}
}

// A failed check is not an IPC failure: the silent startup check has to be able
// to tell "no update" from "could not ask", and this is the shape it reads.
func TestUpdateCheckReportsAReachabilityFailureInline(t *testing.T) {
	original := update.ManifestURLs
	// 127.0.0.1:1 refuses immediately, so the check does not wait on a timeout.
	update.ManifestURLs = []string{"http://127.0.0.1:1/latest.json"}
	t.Cleanup(func() { update.ManifestURLs = original })

	app := newUpdateContractApp(t)
	raw, err := app.router.Call(context.Background(), "update.checkVersion", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("checkVersion must not raise: %v", err)
	}
	result, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("checkVersion result: %#v", raw)
	}
	if result["current"] != appVersion || result["has_update"] != false {
		t.Fatalf("checkVersion result: %#v", result)
	}
	if message, _ := result["error"].(string); message == "" {
		t.Fatalf("checkVersion hid the failure: %#v", result)
	}
}

// After a download the settings page needs to know both that a version is
// waiting and which one, without having run a check first.
func TestUpdateStatusReportsNothingBeforeADownload(t *testing.T) {
	app := newUpdateContractApp(t)
	raw, err := app.router.Call(context.Background(), "update.status", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	result, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("status result: %#v", raw)
	}
	if result["staged"] != false {
		t.Fatalf("status result: %#v", result)
	}
}

// The release workflow publishes the manifest to the repository the client reads
// it from, and nothing else connects those two files — one is a Go constant, the
// other is YAML. A rename on either side would kill the update chain for every
// installed client without failing any build, so it is asserted here.
func TestReleaseWorkflowPublishesWhereTheClientLooks(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read the release workflow: %v", err)
	}
	want := "RELEASE_REPO: " + update.ReleaseRepo
	if !strings.Contains(string(raw), want) {
		t.Fatalf("release.yml must set %q — that is where clients look for latest.json", want)
	}
}
