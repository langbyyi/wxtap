// Package update carries two surfaces under one IPC domain.
//
// The asset sync (Checker) refreshes runtime assets — WMPF Frida address
// tables and MCP skill documents — from the source repository. The files are
// committed under resources/, and the sync reads the newest revision on the
// default branch through GitHub's contents API: add/overwrite only, unchanged
// files detected by content address and skipped, per-file download failures
// silent.
//
// The self-update chain (FetchManifest, StageVersion, ApplyPending) reads a
// release manifest, verifies and unpacks the staged build into the data
// directory, then swaps it into the install directory on the next launch.
package update

import (
	"crypto/sha1" //nolint:gosec // G505: git's object name is sha1 by definition; this is a content address, not a signature.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxListingBytes caps one directory listing. The contents API returns a few
// hundred bytes per entry; the cap only stops a hostile server from streaming
// forever.
const maxListingBytes = 8 << 20

// maxObjectBytes caps one synced asset. Address tables and skill documents are
// kilobytes; the cap only stops a hostile server from streaming forever.
const maxObjectBytes = 32 << 20

// readCappedBody reads a remote body up to limit bytes. A server (or anything
// impersonating one) must not decide how much memory this process allocates,
// so an oversized response is an error rather than a truncated value.
func readCappedBody(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	return data, nil
}

// AssetBase is the root the runtime assets are read from: this repository's
// contents API, anonymously — a public repository needs no token, and a token
// shipped inside the client is a token every user can read out of it.
//
// There is no object storage behind this project. The address tables and the
// skill documents are committed under resources/, so the same repository that
// serves the code, the releases and the update manifest serves them too —
// "sync" means "read the newest revision", not "read a bucket".
//
// Anonymous reads of this API are limited to 60 requests per hour per address.
// A full sync spends one listing per directory (three), and it only runs when
// the user asks for it, so the limit is not a constraint the sync has to work
// around.
const AssetBase = "https://api.github.com/repos/langbyyi/wxtap/contents"

// AssetRef pins the sync to a branch. Naming the default branch explicitly
// keeps which revision the sync reads independent of a repository setting.
const AssetRef = "main"

// The committed directories the two syncs mirror.
const (
	assetWMPFMac   = "resources/frida/config/mac"
	assetWMPFWin   = "resources/frida/config/win"
	assetSkillDocs = "resources/skills"
)

// Checker reads asset directories from one contents-API root; tests inject a
// fake base URL.
type Checker struct {
	Base string
	Ref  string
	http *http.Client
}

// New creates a checker against the given contents-API root.
func New(base string) *Checker {
	return &Checker{Base: base, Ref: AssetRef, http: &http.Client{Timeout: 15 * time.Second}}
}

// contentsEntry is one element of a directory listing, in the shape the
// contents API returns it. Only file entries carry a download URL.
type contentsEntry struct {
	Name        string `json:"name"`
	SHA         string `json:"sha"`
	Size        int64  `json:"size"`
	Type        string `json:"type"`
	DownloadURL string `json:"download_url"`
}

// SyncResult is the sync report. Error is "" on success and the
// 网络错误 message when the listing itself failed.
type SyncResult struct {
	Updated   int    `json:"updated"`
	Unchanged int    `json:"unchanged"`
	Error     string `json:"error"`
}

// SyncDir copies the repository directory dir into localDir: new or changed
// files are downloaded, files whose content address already matches are
// skipped, and failed downloads are counted as neither. Only the listing error
// is reported.
func (c *Checker) SyncDir(dir string, localDir string) SyncResult {
	if err := os.MkdirAll(localDir, 0o750); err != nil {
		return SyncResult{Error: fmt.Sprintf("网络错误: %v", err)}
	}
	entries, err := c.list(dir)
	if err != nil {
		return SyncResult{Error: fmt.Sprintf("网络错误: %v", err)}
	}
	var result SyncResult
	for _, entry := range entries {
		// Subdirectories are skipped: both asset trees are flat, and walking
		// them would need its own depth and cycle guards for no extra files.
		if entry.Type != "file" || entry.Size <= 0 {
			continue
		}
		// The listing is remote input, and it drives local writes: an entry
		// name must be a plain basename or it escapes localDir (same guard as
		// extract.ExtractTo).
		if entry.Name == "" || entry.Name == "." || strings.Contains(entry.Name, "..") ||
			strings.ContainsAny(entry.Name, `/\`) {
			continue
		}
		localPath := filepath.Join(localDir, entry.Name)
		if existing, err := os.ReadFile(localPath); err == nil && blobSHA(existing) == entry.SHA {
			result.Unchanged++
			continue
		}
		data, err := c.download(entry.DownloadURL)
		if err != nil {
			continue
		}
		if err := os.WriteFile(localPath, data, 0o600); err == nil {
			result.Updated++
		}
	}
	return result
}

// SyncWMPF syncs the two WMPF address-table directories into frida/config/mac
// and frida/config/win under baseDir, aggregating both reports.
func (c *Checker) SyncWMPF(baseDir string) map[string]any {
	fridaConfig := filepath.Join(baseDir, "frida", "config")
	mac := c.SyncDir(assetWMPFMac, filepath.Join(fridaConfig, "mac"))
	win := c.SyncDir(assetWMPFWin, filepath.Join(fridaConfig, "win"))
	errorMessage := ""
	if mac.Error != "" {
		errorMessage = mac.Error
	} else if win.Error != "" {
		errorMessage = win.Error
	}
	return map[string]any{
		"updated":   mac.Updated + win.Updated,
		"unchanged": mac.Unchanged + win.Unchanged,
		"error":     nilIfEmpty(errorMessage),
	}
}

// SyncSkills syncs the skill documents into baseDir/skills.
func (c *Checker) SyncSkills(baseDir string) SyncResult {
	return c.SyncDir(assetSkillDocs, filepath.Join(baseDir, "skills"))
}

// list reads one directory. The contents API answers a directory with an array
// and a file with an object; unmarshalling the latter into a slice fails, which
// is the wanted outcome for a path that names a file rather than a directory.
//
// The API returns a directory in one response (up to 1000 entries, no
// continuation token). Both asset trees are far below that and change only
// when an address table is added, so a silent truncation is not a case this
// sync is built to survive — the count is small enough to keep flat.
func (c *Checker) list(dir string) ([]contentsEntry, error) {
	requestURL := fmt.Sprintf("%s/%s?ref=%s", c.Base, escapePathSegments(dir), url.QueryEscape(c.Ref))
	resp, err := c.http.Get(requestURL)
	if err != nil {
		return nil, err
	}
	body, err := readCappedBody(resp.Body, maxListingBytes)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var entries []contentsEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// download fetches one entry by the URL the listing gave it: raw content, not
// the API, so the per-file reads do not spend the API's rate limit.
func (c *Checker) download(downloadURL string) ([]byte, error) {
	if downloadURL == "" {
		return nil, fmt.Errorf("listing entry carries no download URL")
	}
	resp, err := c.http.Get(downloadURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, downloadURL)
	}
	return readCappedBody(resp.Body, maxObjectBytes)
}

// blobSHA is the git object name of a file's contents: sha1 over the
// "blob <len>\0" header plus the bytes. The contents API reports exactly this
// as an entry's sha, so the comparison needs no download — the role the
// bucket's MD5 ETag played before the assets moved into the repository.
func blobSHA(data []byte) string {
	h := sha1.New() //nolint:gosec // G401: git's object name is sha1 by definition.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// escapePathSegments escapes each path segment but keeps the separators, so a
// directory path stays a path instead of becoming a single escaped token.
func escapePathSegments(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
