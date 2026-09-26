package update

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// layoutEntry says where one payload item belongs in an install. Keeping "what
// the payload calls it" separate from "where it goes" is what lets a single
// payload shape serve both platforms: Windows keeps the items flat beside
// WxTap.exe, macOS maps the same items into the .app bundle.
type layoutEntry struct {
	staged string
	target string
}

// layout is where a release's payload lives inside an install and what an
// update may replace. The two platforms genuinely differ, so the difference is
// expressed as data — which also means both layouts can be exercised by tests
// on either platform, rather than only on the one they are for.
type layout struct {
	// root is the install root, relative to the running executable's directory:
	// "." on Windows (payload beside the exe) and ".." + ".." on macOS
	// (…/WxTap.app/Contents/MacOS/WxTap → the bundle root).
	root    string
	entries []layoutEntry
	// required lists staged names that must exist for a tree to be worth
	// swapping in; a payload missing one would be swapped in and then rolled
	// back in front of the user.
	required []string
	// needsSecondProcess reports whether the swap must be performed by a second
	// process after this one exits. Windows cannot overwrite a running
	// executable but can rename it, so it relaunches into a helper that waits
	// for the old process. macOS unlinks freely — the running process keeps its
	// own inode — so it swaps in place.
	needsSecondProcess bool
}

// layoutFor returns the install layout for goos. Swappable entries are an
// allowlist rather than "whatever the archive happens to contain": the
// directory the build stages from also holds runtime data (logs/, webview/,
// traffic.db, config.json), so a packaging slip must not overwrite a user's
// history. A payload that has stopped carrying runtime/ is why that directory
// is absent here — installs upgraded from a bundled-Node release keep it as
// dead weight, and swapping over it would only re-add a runtime nothing reads.
func layoutFor(goos string) layout {
	if goos == "darwin" {
		return layout{
			root: filepath.Join("..", ".."),
			entries: []layoutEntry{
				{staged: "WxTap", target: filepath.Join("Contents", "MacOS", "WxTap")},
				{staged: "core", target: filepath.Join("Contents", "Resources", "core")},
				{staged: "resources", target: filepath.Join("Contents", "Resources", "resources")},
				{staged: "migrations", target: filepath.Join("Contents", "Resources", "migrations")},
			},
			required: []string{"WxTap", filepath.Join("core", "dist", "cli.js")},
			// Replacing Contents/MacOS/WxTap while it runs is allowed: unlink
			// leaves the old inode mapped for the running process.
			needsSecondProcess: false,
		}
	}
	return layout{
		root: ".",
		entries: []layoutEntry{
			{staged: "WxTap.exe", target: "WxTap.exe"},
			{staged: "core", target: "core"},
			{staged: "resources", target: "resources"},
			{staged: "migrations", target: "migrations"},
		},
		required:           []string{"WxTap.exe", filepath.Join("core", "dist", "cli.js")},
		needsSecondProcess: true,
	}
}

// ApplyRecord is the outcome of the most recent swap attempt. main() performs
// the swap before any logging exists, so it records the result here and
// startup() folds it into the operation log afterwards.
type ApplyRecord struct {
	Version string    `json:"version"`
	Applied bool      `json:"applied"`
	Error   string    `json:"error,omitempty"`
	Skipped []string  `json:"skipped,omitempty"`
	At      time.Time `json:"at"`
}

// Summary renders the record as a log level and one line, so the shell can
// report an update without knowing the record's shape.
//
// An empty reason on a failed record means the process died between writing the
// record and finishing the swap: nothing reported a failure, so say that rather
// than logging a blank reason.
func (r ApplyRecord) Summary() (string, string) {
	if !r.Applied {
		reason := r.Error
		if reason == "" {
			reason = "换入过程被中断"
		}
		return "error", "更新未生效: " + reason
	}
	message := "已更新到 " + r.Version
	if len(r.Skipped) != 0 {
		message += "（已跳过 " + strings.Join(r.Skipped, ", ") + "）"
	}
	return "info", message
}

// NeedsSecondProcess reports whether this platform has to hand the swap to a
// helper process, and therefore whether an applied update necessarily ends in a
// relaunch. Windows does; macOS does not.
func NeedsSecondProcess() bool {
	return layoutFor(runtime.GOOS).needsSecondProcess
}

// InstallRoot returns the directory an update writes into, given the directory
// holding the running executable. On Windows that is the executable's own
// directory; on macOS it is the enclosing .app bundle.
func InstallRoot(exeDir string) string {
	return filepath.Join(exeDir, layoutFor(runtime.GOOS).root)
}

// ApplyPending swaps a staged version into the install directory. It runs
// before the UI exists, so every failure is reported through the returned
// record rather than raised: a broken update must leave a launchable app.
//
// The running executable cannot be overwritten on Windows but it can be
// renamed, which is what makes the swap possible at all — the same technique
// the Chrome and Firefox updaters use. Each entry is moved aside before its
// replacement takes its place, so a failure part-way through can be undone
// entry by entry instead of leaving a mixed install. macOS replaces in place;
// see layout.needsSecondProcess.
func ApplyPending(exeDir, dataDir string) ApplyRecord {
	return applyPendingFor(exeDir, dataDir, layoutFor(runtime.GOOS))
}

// applyPendingFor is ApplyPending for an explicit layout. It is split out so
// the platform this build is not running on still gets exercised by tests —
// the layout is data, so neither branch needs the other OS to be testable.
func applyPendingFor(exeDir, dataDir string, lay layout) ApplyRecord {
	record := ApplyRecord{At: time.Now().UTC()}
	state, ok := StagedVersion(dataDir)
	if !ok {
		return record
	}
	record.Version = state.Version
	// Written before the swap starts as well as after it finishes: a crash
	// part-way through then still leaves evidence of what was being attempted,
	// which is the only record that survives the process.
	record.write(dataDir)

	staged := stagedDir(dataDir, state.Version)
	if err := validateStagedTree(staged, lay); err != nil {
		// Re-downloading is the only fix, so drop the tree instead of letting
		// it fail again on every launch.
		_ = os.RemoveAll(staged)
		_ = os.RemoveAll(pendingPath(dataDir))
		record.Error = err.Error()
		record.write(dataDir)
		return record
	}
	record.Skipped = unexpectedEntries(staged, lay)

	if err := swapInto(staged, filepath.Join(exeDir, lay.root), lay); err != nil {
		record.Error = err.Error()
		record.write(dataDir)
		return record
	}
	_ = os.RemoveAll(pendingPath(dataDir))
	record.Applied = true
	record.write(dataDir)
	return record
}

// CleanupAfterLaunch removes the leftovers of a completed swap: the renamed
// previous entries and the staging area. It is only safe once a launch has
// found no pending version, which means the swap it follows already succeeded.
//
// Removing an entry can fail while the process that was replaced is still
// exiting — Windows keeps a mapped image. That is not an error worth reporting:
// the launch after next sweeps up whatever survived.
func CleanupAfterLaunch(exeDir, dataDir string) {
	cleanupFor(exeDir, dataDir, layoutFor(runtime.GOOS))
}

// cleanupFor is CleanupAfterLaunch for an explicit layout, for the same reason
// applyPendingFor exists.
func cleanupFor(exeDir, dataDir string, lay layout) {
	root := filepath.Join(exeDir, lay.root)
	for _, entry := range lay.entries {
		_ = os.RemoveAll(filepath.Join(root, entry.target) + ".old")
	}
	_ = os.RemoveAll(filepath.Join(updateRoot(dataDir), "staged"))
	_ = os.RemoveAll(filepath.Join(updateRoot(dataDir), "downloads"))
}

// LastApply returns the most recent swap record. It does not consume it:
// ClearLastApply does, once the caller has logged it.
func LastApply(dataDir string) (ApplyRecord, bool) {
	data, err := os.ReadFile(applyRecordPath(dataDir))
	if err != nil {
		return ApplyRecord{}, false
	}
	var record ApplyRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return ApplyRecord{}, false
	}
	return record, true
}

// ClearLastApply drops the record once startup() has logged it.
func ClearLastApply(dataDir string) {
	_ = os.Remove(applyRecordPath(dataDir))
}

func validateStagedTree(root string, lay layout) error {
	for _, relative := range lay.required {
		info, err := os.Stat(filepath.Join(root, relative))
		if err != nil || info.IsDir() {
			return fmt.Errorf("更新包不完整: 缺少 %s", relative)
		}
	}
	return nil
}

// claims reports whether a name at the staging root is one this layout knows
// how to place.
func (l layout) claims(stagedName string) bool {
	for _, entry := range l.entries {
		if strings.EqualFold(entry.staged, stagedName) {
			return true
		}
	}
	return false
}

// unexpectedEntries reports staged items no layout entry claims. They are left
// alone rather than rejected: an unrecognised extra file is not a reason to
// strand the whole update. os.ReadDir already returns names sorted, so the
// record is deterministic.
func unexpectedEntries(staged string, lay layout) []string {
	items, err := os.ReadDir(staged)
	if err != nil {
		return nil
	}
	var unexpected []string
	for _, item := range items {
		if !lay.claims(item.Name()) {
			unexpected = append(unexpected, item.Name())
		}
	}
	return unexpected
}

// swapInto moves each payload entry into place, undoing the entries it already
// swapped if a later one fails.
func swapInto(staged, installRoot string, lay layout) error {
	swapped := make([]layoutEntry, 0, len(lay.entries))
	rollback := func() {
		for index := len(swapped) - 1; index >= 0; index-- {
			_ = restoreEntry(installRoot, swapped[index].target)
		}
	}
	for _, entry := range lay.entries {
		source := filepath.Join(staged, entry.staged)
		if _, err := os.Stat(source); err != nil {
			// A release may legitimately omit an item; only lay.required is
			// mandatory, and that was checked before the swap started.
			continue
		}
		target := filepath.Join(installRoot, entry.target)
		aside := target + ".old"
		hadOld := false
		if _, err := os.Lstat(target); err == nil {
			// A stale .old from an interrupted swap would block the rename.
			_ = os.RemoveAll(aside)
			if err := os.Rename(target, aside); err != nil {
				rollback()
				return fmt.Errorf("无法替换 %s: %w", entry.target, err)
			}
			hadOld = true
		}
		if err := movePath(source, target); err != nil {
			if hadOld {
				_ = os.Rename(aside, target)
			}
			rollback()
			return fmt.Errorf("无法写入 %s: %w", entry.target, err)
		}
		swapped = append(swapped, entry)
	}
	return nil
}

// restoreEntry puts an entry's previous version back after a later entry in
// the same swap failed. An entry that did not exist before the swap is left as
// it is: there is no previous version to restore, and removing the new one
// would only widen the damage.
func restoreEntry(installRoot, target string) error {
	path := filepath.Join(installRoot, target)
	aside := path + ".old"
	if _, err := os.Stat(aside); err != nil {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return os.Rename(aside, path)
}

// movePath moves src to dst, falling back to a copy when the two sit on
// different volumes — the data directory may be redirected to another drive
// via WXTAP_DATA_DIR, and os.Rename cannot cross volumes.
func movePath(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst)
	}
	return copyTree(src, dst)
}

func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info.Mode()&0o111 != 0 {
		mode = 0o700
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	target, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func (r ApplyRecord) write(dataDir string) {
	if err := os.MkdirAll(updateRoot(dataDir), 0o750); err != nil {
		return
	}
	data, err := json.Marshal(r)
	if err != nil {
		return
	}
	_ = os.WriteFile(applyRecordPath(dataDir), data, 0o600)
}

func applyRecordPath(dataDir string) string {
	return filepath.Join(updateRoot(dataDir), "last-apply.json")
}
