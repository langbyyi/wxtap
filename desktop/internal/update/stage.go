package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// updateDirName is the staging area under the data directory. It never lives
// inside the install directory: that may be read-only (Program Files), and a
// half-unpacked tree there would be a broken install rather than a retry.
const updateDirName = ".update"

// maxExtractedBytes bounds one unpacked release. The payload unpacks to
// roughly 250MB; the cap only stops a decompression bomb, not a real release.
const maxExtractedBytes = 2 << 30

// legacyRegType is the NUL typeflag POSIX allows for a regular file.
// staticcheck flags tar.TypeRegA as deprecated in favour of tar.TypeReg, but an
// archive is free to carry either spelling, so both are accepted.
const legacyRegType = 0

// assetTimeout bounds one archive transfer, so a dead peer cannot hold the
// download open forever. It is generous because the largest shard is tens of
// megabytes; a resumable transfer within a single shard is not implemented —
// a retry re-fetches the shard, while shards that already verified are reused.
const assetTimeout = 30 * time.Minute

// PendingState is the marker that tells the next launch to swap in a staged
// version. It is written only after every asset has been verified and
// unpacked, so its presence always means "complete enough to apply".
type PendingState struct {
	Version  string    `json:"version"`
	StagedAt time.Time `json:"stagedAt"`
}

// StagedVersion reports the version waiting to be applied, if any.
func StagedVersion(dataDir string) (PendingState, bool) {
	data, err := os.ReadFile(pendingPath(dataDir))
	if err != nil {
		return PendingState{}, false
	}
	var state PendingState
	if err := json.Unmarshal(data, &state); err != nil || state.Version == "" {
		return PendingState{}, false
	}
	return state, true
}

// StageVersion downloads and unpacks every asset for manifest.Version under
// the data directory. The pending marker is written last: an interrupted or
// corrupted download leaves archives in the cache for a retry but never a
// tree the next launch would swap in.
func StageVersion(ctx context.Context, manifest Manifest, dataDir string, progress func(done, total int64)) (string, error) {
	return stageVersionFor(ctx, manifest, dataDir, progress, layoutFor(runtime.GOOS))
}

// stageVersionFor is StageVersion with the install layout named by the caller.
// The staged names a payload must carry differ per platform, and naming the
// layout as data is what lets the tests check both shapes on either host —
// the same seam applyPendingFor and cleanupFor already give the swap side.
func stageVersionFor(ctx context.Context, manifest Manifest, dataDir string, progress func(done, total int64), lay layout) (string, error) {
	// Validated here as well as in FetchManifest: the version becomes a
	// directory name, and this entry point takes a document derived from remote
	// input, so it must not depend on a caller having normalized it first.
	if err := manifest.normalize(); err != nil {
		return "", err
	}
	downloads := filepath.Join(updateRoot(dataDir), "downloads")
	if err := os.MkdirAll(downloads, 0o750); err != nil {
		return "", err
	}
	archives := make([]string, 0, len(manifest.Assets))
	total := manifest.TotalSize()
	var completed int64
	for _, asset := range manifest.Assets {
		done := completed
		archive, err := DownloadAsset(ctx, asset, downloads, func(written, _ int64) {
			if progress != nil {
				progress(done+written, total)
			}
		})
		if err != nil {
			return "", fmt.Errorf("分片 %s 下载失败: %w", asset.Name, err)
		}
		archives = append(archives, archive)
		completed += asset.Size
	}

	// Rebuild the tree from scratch: a previous stage of the same version
	// could otherwise leave files that the manifest no longer lists.
	root := stagedDir(dataDir, manifest.Version)
	if err := os.RemoveAll(root); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", err
	}
	for _, archive := range archives {
		if err := ExtractTarGz(archive, root); err != nil {
			_ = os.RemoveAll(root)
			return "", fmt.Errorf("%s 解包失败: %w", filepath.Base(archive), err)
		}
	}
	// Fail here rather than at the next launch: a tree missing the Core script
	// would otherwise be swapped in and then rolled back in front of the user.
	if err := validateStagedTree(root, lay); err != nil {
		_ = os.RemoveAll(root)
		return "", err
	}
	state := PendingState{Version: manifest.Version, StagedAt: time.Now().UTC()}
	if err := writePending(dataDir, state); err != nil {
		return "", err
	}
	return root, nil
}

// DownloadAsset fetches one archive into destDir and returns its path. Sources
// are tried in order. The file only takes its final name once both the
// declared size and the sha256 digest match, so a truncated or tampered
// download never becomes a candidate for extraction.
func DownloadAsset(ctx context.Context, asset Asset, destDir string, progress func(done, total int64)) (string, error) {
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return "", err
	}
	final := filepath.Join(destDir, asset.Name)
	// An archive that already verifies is reused, so retrying after a later
	// shard fails does not re-download the megabytes that were fine.
	if matchesAsset(final, asset) {
		return final, nil
	}
	client := &http.Client{Timeout: assetTimeout}
	var failures []error
	for _, source := range asset.URLs {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		err := downloadTo(ctx, client, source, final, asset, progress)
		if err == nil {
			return final, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", source, err))
	}
	return "", fmt.Errorf("全部分片地址失败: %w", errors.Join(failures...))
}

func downloadTo(ctx context.Context, client *http.Client, source, final string, asset Asset, progress func(done, total int64)) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "WxTap")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}

	partial := final + ".part"
	// A partial file from an abandoned attempt is always discarded: only a
	// complete shard is ever verified, so there is nothing half-done to keep.
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	counter := &progressWriter{total: asset.Size, report: progress}
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(io.TeeReader(response.Body, counter), maxAssetBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(partial)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(partial)
		return closeErr
	}
	if written > maxAssetBytes {
		_ = os.Remove(partial)
		return fmt.Errorf("分片超过 %d 字节上限", maxAssetBytes)
	}
	// The manifest is the only authority on what this shard should be: a
	// short read is a truncated transfer, not a smaller release.
	if written != asset.Size {
		_ = os.Remove(partial)
		return fmt.Errorf("大小不符: 收到 %d 字节, 清单声明 %d", written, asset.Size)
	}
	if digest := hex.EncodeToString(hasher.Sum(nil)); digest != asset.SHA256 {
		_ = os.Remove(partial)
		return fmt.Errorf("sha256 不符: 收到 %s, 清单声明 %s", digest, asset.SHA256)
	}
	if err := os.Rename(partial, final); err != nil {
		_ = os.Remove(partial)
		return err
	}
	return nil
}

// matchesAsset reports whether path already holds exactly this asset.
func matchesAsset(path string, asset Asset) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() != asset.Size {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, io.LimitReader(file, asset.Size+1)); err != nil {
		return false
	}
	return hex.EncodeToString(hasher.Sum(nil)) == asset.SHA256
}

// ExtractTarGz unpacks an archive into destRoot. Only directories and regular
// files are accepted: a release has no reason to carry links, and honouring
// them would let a crafted archive write outside destRoot. The same reasoning
// rejects absolute entries and any path containing "..".
func ExtractTarGz(archive, destRoot string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	uncompressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { _ = uncompressed.Close() }()

	reader := tar.NewReader(uncompressed)
	var extracted int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(destRoot, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg, legacyRegType:
			extracted += header.Size
			if extracted > maxExtractedBytes {
				return fmt.Errorf("解包后超过 %d 字节上限", maxExtractedBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			if err := writeEntry(target, reader, header); err != nil {
				return err
			}
		default:
			return fmt.Errorf("归档含不支持的条目 %q (类型 %d)", header.Name, header.Typeflag)
		}
	}
}

// writeEntry copies one regular file. Archives produced by tar on Windows
// carry no executable bit, so the mode is normalised rather than trusted: the
// bundled runtime and Core never need setuid bits, and gosec's G110 does not
// apply because the output is bounded by the caller's running total.
func writeEntry(target string, reader io.Reader, header *tar.Header) error {
	mode := os.FileMode(0o600)
	if header.Mode&0o111 != 0 {
		mode = 0o700
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(reader, header.Size+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	// The header size is remote input; a short entry means a truncated archive.
	if written != header.Size {
		return fmt.Errorf("条目 %q 长度不符: 收到 %d, 声明 %d", header.Name, written, header.Size)
	}
	return nil
}

// safeJoin resolves a tar entry name under root, rejecting anything that
// would land outside it.
func safeJoin(root, name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("归档含空条目名")
	}
	slashed := filepath.ToSlash(trimmed)
	// A colon makes the entry drive-relative on Windows ("C:core"): joining it
	// does not escape, but it cannot name a real file there either, and a
	// release never contains one. Refusing it keeps the rule simple.
	if strings.HasPrefix(slashed, "/") || strings.Contains(slashed, "../") || slashed == ".." || strings.Contains(slashed, ":") {
		return "", fmt.Errorf("归档条目越出目标目录: %q", name)
	}
	target := filepath.Join(root, filepath.FromSlash(slashed))
	resolvedRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolvedTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if resolvedTarget != resolvedRoot && !strings.HasPrefix(resolvedTarget, resolvedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("归档条目越出目标目录: %q", name)
	}
	return resolvedTarget, nil
}

// progressWriter reports cumulative bytes to the caller's progress hook.
type progressWriter struct {
	written int64
	total   int64
	report  func(done, total int64)
}

func (w *progressWriter) Write(chunk []byte) (int, error) {
	w.written += int64(len(chunk))
	if w.report != nil {
		w.report(w.written, w.total)
	}
	return len(chunk), nil
}

func updateRoot(dataDir string) string {
	return filepath.Join(dataDir, updateDirName)
}

func stagedDir(dataDir, version string) string {
	return filepath.Join(updateRoot(dataDir), "staged", version)
}

func pendingPath(dataDir string) string {
	return filepath.Join(updateRoot(dataDir), "pending.json")
}

func writePending(dataDir string, state PendingState) error {
	if err := os.MkdirAll(updateRoot(dataDir), 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(pendingPath(dataDir), data, 0o600)
}
