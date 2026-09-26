package update

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRepo serves a contents-API directory listing and the files it names,
// standing in for the repository the runtime assets are committed to.
//
// Every directory answers with the same four entries: one whose content the
// caller may already have, one to download, one zero-length entry, and a
// subdirectory. That covers the three branches SyncDir takes without a fake
// per test.
func fakeRepo(t *testing.T, failDownloads bool) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/contents/", func(w http.ResponseWriter, r *http.Request) {
		dir := strings.TrimPrefix(r.URL.Path, "/contents/")
		file := func(name, content string) string {
			return fmt.Sprintf(
				`{"name":%q,"sha":%q,"size":%d,"type":"file","download_url":"%s/raw/%s/%s"}`,
				name, blobSHA([]byte(content)), len(content), server.URL, dir, name)
		}
		_, _ = fmt.Fprintf(w, "[%s,%s,%s,%s]",
			file("one.txt", "AAA"),
			file("two.bin", "BBBB"),
			file("empty.txt", ""),
			// A subdirectory: size 0 and no download URL, exactly as the API
			// reports one.
			`{"name":"nested","sha":"deadbeef","size":0,"type":"dir","download_url":null}`,
		)
	})
	mux.HandleFunc("/raw/", func(w http.ResponseWriter, r *http.Request) {
		if failDownloads {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/one.txt"):
			_, _ = w.Write([]byte("AAA"))
		case strings.HasSuffix(r.URL.Path, "/two.bin"):
			_, _ = w.Write([]byte("BBBB"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// contentsRoot is the URL a test server serves the contents API under.
//
// The production base ends in /contents (AssetBase), so tests build the same
// shape instead of a bare host: a bare host would make every request take a
// path the real API never answers, and the tests would pass against a fake
// that agrees with the wrong layout.
func contentsRoot(server *httptest.Server) string {
	return server.URL + "/contents"
}

// endlessReader never returns EOF, standing in for a sender that streams until
// someone on this side stops reading.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// readCappedBody must stop at the cap instead of draining an endless response.
func TestReadCappedBodyStopsAtLimit(t *testing.T) {
	data, err := readCappedBody(strings.NewReader("0123456789"), 10)
	if err != nil || string(data) != "0123456789" {
		t.Fatalf("exact-size body: %q %v", data, err)
	}
	if _, err := readCappedBody(strings.NewReader("0123456789a"), 10); err == nil {
		t.Fatal("oversized body must be rejected")
	}
	if _, err := readCappedBody(endlessReader{}, 10); err == nil {
		t.Fatal("endless body must be rejected without reading it to EOF")
	}
}

// The sync skips a download when the local file already has the content the
// listing describes, and it recognizes that content by the git object name the
// contents API reports as `sha`. If blobSHA disagreed with git's own answer,
// every file would look changed on every sync — which is silent, because the
// result is still "some files updated".
//
// The expected values below come from `git hash-object --stdin`, not from this
// function: comparing it against itself would prove nothing.
func TestBlobSHAMatchesGitObjectName(t *testing.T) {
	for _, tc := range []struct {
		content string
		want    string
	}{
		{"AAA", "43d88b658623a3b06c40d318392d6c67f1e0b2f9"},
		{"BBBB", "c669c18b9ce69c2eab0cf6e2ece5bf56b7f2c925"},
		{"", "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
		{"hello\n", "ce013625030ba8dba906f756967f9e9ca394464a"},
	} {
		if got := blobSHA([]byte(tc.content)); got != tc.want {
			t.Fatalf("blobSHA(%q) = %s, want git's %s", tc.content, got, tc.want)
		}
	}
}

func TestSyncDirDownloadsNewAndSkipsUnchanged(t *testing.T) {
	server := fakeRepo(t, false)
	c := New(contentsRoot(server))
	local := t.TempDir()
	// one.txt already present with matching content → unchanged.
	if err := os.WriteFile(filepath.Join(local, "one.txt"), []byte("AAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := c.SyncDir("p", local)
	if result.Updated != 1 || result.Unchanged != 1 || result.Error != "" {
		t.Fatalf("result: %+v", result)
	}
	got, err := os.ReadFile(filepath.Join(local, "two.bin"))
	if err != nil || string(got) != "BBBB" {
		t.Fatalf("two.bin: %q %v", got, err)
	}
	// Zero-length entries are skipped entirely (the same rule the bucket sync
	// had: there is nothing to write and nothing to compare).
	if _, err := os.Stat(filepath.Join(local, "empty.txt")); !os.IsNotExist(err) {
		t.Fatal("zero-length entry should not be downloaded")
	}
}

// A directory entry is not a file: following it is out of scope for a sync
// whose two asset trees are flat, and creating it would leave an empty
// directory that the next listing then reports as unchanged forever.
func TestSyncDirSkipsDirectoryEntries(t *testing.T) {
	server := fakeRepo(t, false)
	local := t.TempDir()
	if result := New(contentsRoot(server)).SyncDir("p", local); result.Error != "" {
		t.Fatalf("result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(local, "nested")); !os.IsNotExist(err) {
		t.Fatal("a subdirectory entry must not be created")
	}
}

func TestSyncDirNetworkErrorSurfaces(t *testing.T) {
	c := New("http://127.0.0.1:1")
	result := c.SyncDir("p", t.TempDir())
	if result.Updated != 0 || result.Unchanged != 0 || !strings.Contains(result.Error, "网络错误") {
		t.Fatalf("result: %+v", result)
	}
}

func TestSyncDirFailedDownloadCountsNeither(t *testing.T) {
	server := fakeRepo(t, true)
	c := New(contentsRoot(server))
	result := c.SyncDir("p", t.TempDir())
	if result.Updated != 0 || result.Unchanged != 0 {
		t.Fatalf("failed downloads should count nothing: %+v", result)
	}
}

// The sync must read a named branch rather than whatever the repository's
// default happens to be: the client's asset locations are compiled in, so a
// default-branch switch would silently move the source of truth.
func TestSyncDirPinsTheAssetBranch(t *testing.T) {
	var requested []string
	mux := http.NewServeMux()
	mux.HandleFunc("/contents/", func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path+"?ref="+r.URL.Query().Get("ref"))
		_, _ = w.Write([]byte(`[]`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	New(contentsRoot(server)).SyncSkills(t.TempDir())
	want := "/contents/" + assetSkillDocs + "?ref=" + AssetRef
	if len(requested) != 1 || requested[0] != want {
		t.Fatalf("requested %v, want [%s]", requested, want)
	}
}

// The live constant has to name the repository the assets are actually in, or
// both syncs 404 for every user while every test above still passes.
func TestAssetBaseNamesThisRepository(t *testing.T) {
	want := "https://api.github.com/repos/langbyyi/wxtap/contents"
	if AssetBase != want {
		t.Fatalf("AssetBase = %q, want %q", AssetBase, want)
	}
}

func TestSyncWMPFLayout(t *testing.T) {
	server := fakeRepo(t, false)
	c := New(contentsRoot(server))
	base := t.TempDir()
	result := c.SyncWMPF(base)
	if result["error"] != nil {
		t.Fatalf("error: %v", result["error"])
	}
	// mac + win, and neither starts with anything on disk: one.txt and two.bin
	// are downloaded in each.
	if result["updated"] != 4 {
		t.Fatalf("updated: %v (mac+win, 2 files each)", result["updated"])
	}
	for _, rel := range []string{"frida/config/mac/one.txt", "frida/config/win/two.bin"} {
		if _, err := os.Stat(filepath.Join(base, rel)); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
	}
}

func TestSyncSkillsWritesSkills(t *testing.T) {
	server := fakeRepo(t, false)
	c := New(contentsRoot(server))
	base := t.TempDir()
	result := c.SyncSkills(base)
	if result.Updated != 2 || result.Error != "" {
		t.Fatalf("result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(base, "skills", "one.txt")); err != nil {
		t.Fatalf("skills file: %v", err)
	}
}

// A hostile or misconfigured listing must not escape the sync target: the
// listing drives local writes, so the entry name is untrusted input.
func TestSyncDirRejectsTraversalNames(t *testing.T) {
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/contents/", func(w http.ResponseWriter, r *http.Request) {
		entry := func(name string) string {
			return fmt.Sprintf(
				`{"name":%q,"sha":"deadbeef","size":4,"type":"file","download_url":"%s/raw/%s"}`,
				name, server.URL, name)
		}
		_, _ = fmt.Fprintf(w, "[%s,%s,%s,%s]", entry(".."), entry("../escaped.txt"),
			entry("a/b.txt"), entry("ok.txt"))
	})
	mux.HandleFunc("/raw/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "ok.txt") {
			_, _ = w.Write([]byte("ok"))
			return
		}
		_, _ = w.Write([]byte("evil"))
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)

	base := t.TempDir()
	target := filepath.Join(base, "a", "b")
	result := New(contentsRoot(server)).SyncDir("skill", target)

	if result.Updated != 1 {
		t.Fatalf("updated %d, want only the safe name", result.Updated)
	}
	for _, escaped := range []string{filepath.Join(base, "a", "escaped.txt"), filepath.Join(base, "escaped.txt")} {
		if _, err := os.Stat(escaped); err == nil {
			t.Fatalf("traversal name wrote outside the target: %s", escaped)
		}
	}
	// A name carrying a separator would make SyncDir create a subdirectory in
	// a tree the sync treats as flat.
	if _, err := os.Stat(filepath.Join(target, "a")); err == nil {
		t.Fatal("a name with a separator created a subdirectory")
	}
	if data, err := os.ReadFile(filepath.Join(target, "ok.txt")); err != nil || string(data) != "ok" {
		t.Fatalf("safe name: %q (%v)", data, err)
	}
}

// The contents API answers a file path with a JSON object where a directory
// path gets an array. Unmarshalling the object into a slice has to fail rather
// than yield a listing that looks empty — an empty listing would report a
// successful sync that wrote nothing.
func TestRemoteFileWhereADirectoryWasExpectedIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"win","type":"file","size":3}`))
	}))
	t.Cleanup(server.Close)

	sync := New(contentsRoot(server)).SyncDir("p", t.TempDir())
	if sync.Error == "" {
		t.Fatalf("a file-shaped answer was treated as an empty listing: %+v", sync)
	}
}

func TestRemoteHTTPErrorsAreRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	sync := New(contentsRoot(server)).SyncDir("p", t.TempDir())
	if sync.Error == "" {
		t.Fatalf("HTTP listing error was treated as success: %+v", sync)
	}
}
