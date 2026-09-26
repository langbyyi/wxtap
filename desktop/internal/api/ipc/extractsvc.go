package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/extract"
)

// PkgFile describes one discovered .wxapkg file. Decompiled is filled by
// the caller (it knows the output root). Unsupported marks a package whose page
// templates this tool cannot restore, so the app is never offered for
// decompilation; its zero value keeps every other producer's packages visible.
type PkgFile struct {
	AppID       string `json:"appid"`
	Path        string `json:"path"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Mtime       int64  `json:"mtime"`
	Decompiled  bool   `json:"decompiled"`
	OutputDir   string `json:"output_dir"`
	Unsupported bool   `json:"unsupported"`
}

// InventoryItem describes one AppID from the account index, package cache, or
// existing decompile output. Status reflects what can safely be done locally.
type InventoryItem struct {
	AppID          string   `json:"appid"`
	Name           string   `json:"name"`
	Subject        string   `json:"subject"`
	IconPath       string   `json:"icon_path"`
	IconDataURL    string   `json:"icon_data_url"`
	IconConfidence string   `json:"icon_confidence"`
	MetadataSource string   `json:"metadata_source"`
	Indexed        bool     `json:"indexed"`
	PackagePaths   []string `json:"package_paths"`
	Decompiled     bool     `json:"decompiled"`
	OutputDir      string   `json:"output_dir"`
	Mtime          int64    `json:"mtime"`
	Status         string   `json:"status"`
	Unsupported    bool     `json:"unsupported"`
}

type InventorySummary struct {
	IndexedCount int `json:"indexed_count"`
	PackageCount int `json:"package_count"`
	OutputCount  int `json:"output_count"`
}

// BuildInventory makes the three distinct local sources explicit instead of
// treating account history as a cache of executable wxapkg packages.
func BuildInventory(userDir string, packages []PkgFile, projects []CodeProject) ([]InventoryItem, InventorySummary) {
	byID := map[string]*InventoryItem{}
	ensure := func(id string) *InventoryItem {
		if byID[id] == nil {
			byID[id] = &InventoryItem{AppID: id, PackagePaths: []string{}}
		}
		return byID[id]
	}
	summary := InventorySummary{}
	for _, id := range scanUserAppIDs(userDir) {
		ensure(id).Indexed = true
		summary.IndexedCount++
	}
	packageIDs := make(map[string]bool, len(packages))
	for _, pkg := range packages {
		packageIDs[pkg.AppID] = true
		item := ensure(pkg.AppID)
		item.PackagePaths = append(item.PackagePaths, pkg.Path)
		item.Unsupported = item.Unsupported || pkg.Unsupported
		if pkg.Mtime > item.Mtime {
			item.Mtime = pkg.Mtime
		}
		if item.IconDataURL == "" {
			cached := ReadMiniAppCachedIcon(userDir, pkg.AppID)
			item.IconPath = cached.IconPath
			item.IconDataURL = cached.IconDataURL
			item.IconConfidence = cached.IconConfidence
			item.MetadataSource = cached.MetadataSource
		}
	}
	// The count is what can be decompiled on this machine, so apps whose
	// templates this tool cannot restore are left out of it entirely.
	for _, item := range byID {
		if len(item.PackagePaths) > 0 && !item.Unsupported {
			summary.PackageCount++
		}
	}
	for _, project := range projects {
		if !packageIDs[project.AppID] {
			continue
		}
		item := ensure(project.AppID)
		item.Decompiled = true
		item.OutputDir = project.Path
		if project.Subject != "" {
			item.Subject = project.Subject
		}
		if project.IconDataURL != "" {
			item.IconPath = project.IconPath
			item.IconDataURL = project.IconDataURL
			item.IconConfidence = project.IconConfidence
			item.MetadataSource = project.MetadataSource
		}
		if project.Mtime > item.Mtime {
			item.Mtime = project.Mtime
		}
		if item.Name == "" {
			item.Name = project.Name
		}
		summary.OutputCount++
	}
	items := make([]InventoryItem, 0, len(byID))
	for _, item := range byID {
		if item.Decompiled {
			item.Status = "decompiled"
		} else if len(item.PackagePaths) > 0 {
			item.Status = "ready"
		} else {
			item.Status = "indexed_only"
		}
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].AppID < items[j].AppID })
	return items, summary
}

// UnsupportedAppIDs returns the appids in this set that cannot be offered for
// decompilation. The verdict is per app, not per package: restoration works on
// the merged tree, so one unrestorable package fails the whole app.
func UnsupportedAppIDs(packages []PkgFile) map[string]bool {
	unsupported := map[string]bool{}
	for _, pkg := range packages {
		if pkg.Unsupported {
			unsupported[pkg.AppID] = true
		}
	}
	return unsupported
}

// UnsupportedAppReason refuses a decompile request that reaches the IPC layer
// anyway — the page leaves such an app out of the list, the count and the batch,
// so this only answers compat and direct callers. MCP carries its own English
// wording for agents.
const UnsupportedAppReason = "该小程序的页面模板由微信新版编译模板运行时（__wxCodeSpace__）生成，本工具还原不了，不在可反编译范围内"

// FindPackages walks a packages directory ({root}/{appid}/.../*.wxapkg) and
// groups files by appid (port of find_wxapkg_files).
//
// It is also the seam that decides which apps are decompilable at all: every
// caller — the package listing, the inventory, single and batch decompiles —
// enumerates through here, so classifying once keeps them from disagreeing about
// which apps exist. Classification costs a decryption scan of every package
// (~90 ms over 43 MB) plus one goja probe per app that mentions compiled
// templates, and the verdict is memoized per package set.
func FindPackages(root string) []PkgFile {
	var packages []PkgFile
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		appID := entry.Name()
		var appPackages []PkgFile
		_ = filepath.Walk(filepath.Join(root, appID), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if strings.HasSuffix(strings.ToLower(info.Name()), ".wxapkg") {
				appPackages = append(appPackages, PkgFile{
					AppID: appID,
					Path:  path,
					Name:  info.Name(),
					Size:  info.Size(),
					Mtime: info.ModTime().Unix(),
				})
			}
			return nil
		})
		if len(appPackages) == 0 {
			continue
		}
		// Judged per app, not per package: restoration restores the merged tree,
		// so one unrestorable package is enough to fail the whole app.
		unsupported := !appRestorable(appID, appPackages)
		for i := range appPackages {
			appPackages[i].Unsupported = unsupported
		}
		packages = append(packages, appPackages...)
	}
	return packages
}

// Template-restorability verdicts keyed by the app's package set. A directory
// refresh and a batch decompile both walk the same packages, and a verdict costs
// an unpack plus a goja run, so the second caller reuses the first one's.
var (
	restorableMu    sync.Mutex
	restorableCache = map[string]bool{}
)

// appRestorable reports whether every page template of this app can be restored.
// Only apps carrying WeChat's compiled templates are probed — the marker scan
// alone is cheap enough to run over every package, and a marker is not proof of
// failure (apps shipping both formats still restore). Anything the checks cannot
// read counts as restorable: the list must never hide an app quietly.
func appRestorable(appID string, packages []PkgFile) bool {
	paths := make([]string, 0, len(packages))
	key := appID
	for _, pkg := range packages {
		paths = append(paths, pkg.Path)
		key += fmt.Sprintf("|%s|%d|%d", pkg.Path, pkg.Size, pkg.Mtime)
	}

	restorableMu.Lock()
	cached, ok := restorableCache[key]
	restorableMu.Unlock()
	if ok {
		return cached
	}

	restorable := true
	if extract.UsesCompiledTemplates(paths, appID) {
		restorable = extract.ProbeWebviewWXML(paths, appID)
	}

	restorableMu.Lock()
	restorableCache[key] = restorable
	restorableMu.Unlock()
	return restorable
}

// Decompile decrypts (if needed) and unpacks one .wxapkg into outDir.
func Decompile(wxapkgPath, outDir, appID string) ([]string, error) {
	return extract.ExtractWxapkg(wxapkgPath, outDir, appID)
}

// DecompilePackages restores all main/sub packages for one app atomically.
func DecompilePackages(wxapkgPaths []string, outDir, appID string) ([]string, error) {
	return extract.DecompilePackages(wxapkgPaths, outDir, appID)
}

// ScanReport is the saved report plus a compact summary for the UI.
type ScanReport struct {
	AppID     string            `json:"appid"`
	Name      string            `json:"name"`
	Time      string            `json:"time"`
	JSCount   int               `json:"js_count"`
	TotalSize int64             `json:"total_size"`
	Result    extract.Analysis  `json:"result"`
	Summary   map[string]int    `json:"summary"`
	Findings  []extract.Finding `json:"findings,omitempty"`
}

// Scan scans an unpacked applet directory for sensitive information. Results
// stay in memory; the UI renders them without persisting report files.
func Scan(dir, appID, name string, patterns map[string]string) (ScanReport, error) {
	return ScanProgress(dir, appID, name, patterns, nil)
}

// ScanProgress is Scan with a per-file progress callback used by the desktop
// progress event.
func ScanProgress(dir, appID, name string, patterns map[string]string, progress func(done, total int)) (ScanReport, error) {
	if err := ValidateAppID(appID); err != nil {
		return ScanReport{}, err
	}
	result, err := extract.ScanFilesWithPatternsProgress(dir, patterns, progress)
	if err != nil {
		return ScanReport{}, err
	}
	report := ScanReport{
		AppID:     appID,
		Name:      name,
		Time:      time.Now().Format("2006-01-02 15:04:05"),
		JSCount:   len(result.FilesScanned),
		TotalSize: result.TotalSize,
		Result:    result.Analysis,
		Findings:  result.Findings,
		Summary:   map[string]int{},
	}
	for key, values := range result.Analysis {
		report.Summary[key] = len(values)
	}
	return report, nil
}

// ReadMiniAppName reads only display names declared in local package metadata.
// Runtime bundles are executable code and may contain template, cached, or
// unrelated appName literals, so they are deliberately not treated as identity
// metadata. An empty result lets callers show the verified AppID instead.
func ReadMiniAppName(outDir string) string {
	for _, filename := range []string{"app-config.json", "app.json"} {
		data, err := os.ReadFile(filepath.Join(outDir, filename))
		if err != nil {
			continue
		}
		var cfg struct {
			AppName string `json:"appname"`
			Name    string `json:"name"`
			Title   string `json:"title"`
		}
		if json.Unmarshal(data, &cfg) != nil {
			continue
		}
		for _, value := range []string{cfg.AppName, cfg.Name, cfg.Title} {
			if name := usableMiniAppName(value); name != "" {
				return name
			}
		}
	}
	return ""
}

func usableMiniAppName(value string) string {
	name := strings.TrimSpace(value)
	if len([]rune(name)) < 2 || len([]rune(name)) > 80 {
		return ""
	}
	switch strings.ToLower(name) {
	case "appname", "undefined", "null", "unknown":
		return ""
	}
	return name
}

// ReadMiniAppSubject returns publisher metadata only when the unpacked
// app-config.json declares it. Most wxapkg files do not contain a verified
// operator identity, so absence deliberately stays empty instead of guessed.
func ReadMiniAppSubject(outDir string) string {
	for _, filename := range []string{"app-config.json", "app.json", "project.config.json"} {
		data, err := os.ReadFile(filepath.Join(outDir, filename))
		if err != nil {
			continue
		}
		var cfg struct {
			Subject     string `json:"subject"`
			Enterprise  string `json:"enterprise"`
			Company     string `json:"company"`
			CompanyName string `json:"companyName"`
			LegalName   string `json:"legalName"`
			Publisher   string `json:"publisher"`
			Owner       string `json:"owner"`
		}
		if json.Unmarshal(data, &cfg) != nil {
			continue
		}
		for _, value := range []string{cfg.Subject, cfg.Enterprise, cfg.Company, cfg.CompanyName, cfg.LegalName, cfg.Publisher, cfg.Owner} {
			if subject := strings.TrimSpace(value); subject != "" {
				return subject
			}
		}
	}
	return ""
}

// DefaultPackagesDir returns the WeChat miniapp package directory, honoring
// WXTAP_PACKAGES_DIR, then the platform defaults (port of
// get_default_packages_dir).
func DefaultPackagesDir() string {
	if override := strings.TrimSpace(os.Getenv("WXTAP_PACKAGES_DIR")); override != "" {
		return override
	}
	// Newest logged-in account wins: WeChat 4.x keeps packages per user.
	if dir := newestUserPackagesDir(userDirectories()); dir != "" {
		return dir
	}
	// Shared WeChat 4.x location used before per-user storage.
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		if dir := filepath.Join(appdata, "Tencent", "xwechat", "radium", "Applet", "packages"); dirExists(dir) {
			return dir
		}
	}
	return ""
}

// newestUserPackagesDir picks the most recently modified package directory
// among the given WeChat account directories.
func newestUserPackagesDir(userDirs []string) string {
	var newest string
	var newestMod int64
	for _, userDir := range userDirs {
		pkgDir := userPackagesDir(userDir)
		if pkgDir == "" {
			continue
		}
		info, err := os.Stat(pkgDir)
		if err != nil || !info.IsDir() {
			continue
		}
		if info.ModTime().Unix() > newestMod {
			newestMod = info.ModTime().Unix()
			newest = pkgDir
		}
	}
	return newest
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// PatternInfo is one builtin analyzer category exposed to the frontend.
type PatternInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// BuiltinPatterns lists the analyzer categories with their Chinese labels
// (port of CATEGORY_INFO).
func BuiltinPatterns() []PatternInfo {
	return []PatternInfo{
		{"sfz", "身份证号"},
		{"mobile", "手机号码"},
		{"mail", "邮箱地址"},
		{"ip", "IP 地址"},
		{"ip_port", "IP:端口"},
		{"domain", "域名"},
		{"path", "路径"},
		{"url", "URL 链接"},
		{"jwt", "JWT Token"},
		{"algorithm", "加密算法"},
		{"secret", "凭证泄露"},
		{"key", "key"},
		{"jdbc", "JDBC"},
		{"static", "静态资源"},
		{"oss", "OSS 云存储"},
	}
}

// ConfigStore persists config.load / config.save as a JSON file.
type ConfigStore struct {
	path string
	mu   sync.Mutex
}

// NewConfigStore keeps settings at base/config.json.
func NewConfigStore(base string) *ConfigStore {
	return &ConfigStore{path: filepath.Join(base, "config.json")}
}

// Dir is the base directory the config file lives in (the shell's data dir);
// services that persist per-target files next to the config derive their
// storage location from it.
func (s *ConfigStore) Dir() string {
	return filepath.Dir(s.path)
}

// lastAssetTargetAppID reads the asset target saved by the last build
// (assetTargetAppID key); empty when unset or of another shape.
func (s *ConfigStore) lastAssetTargetAppID() string {
	config, err := s.Load()
	if err != nil {
		return ""
	}
	appid, _ := config[assetTargetConfigKey].(string)
	return strings.TrimSpace(appid)
}

// appNamesConfigKey holds the appid → display-name registry in config.json.
// It is learned from connected sessions: the decompiled output and the traffic
// store only know the appid, so the name a mini program reports while being
// debugged is the one durable place to get a readable label from.
const appNamesConfigKey = "appNames"

// AppNames returns the remembered appid → name map (empty when absent or of
// another shape). Nil-receiver safe so callers without a config store can
// pipe through unchanged.
func (s *ConfigStore) AppNames() map[string]string {
	if s == nil {
		return nil
	}
	config, err := s.Load()
	if err != nil {
		return nil
	}
	raw, _ := config[appNamesConfigKey].(map[string]any)
	names := make(map[string]string, len(raw))
	for appid, value := range raw {
		if name, ok := value.(string); ok && strings.TrimSpace(name) != "" {
			names[strings.TrimSpace(appid)] = name
		}
	}
	return names
}

// RememberAppName merges one learned name into the registry. Empty halves are
// ignored; a non-empty name replaces the previous one (mini programs can be
// renamed). Unchanged entries skip the disk write.
func (s *ConfigStore) RememberAppName(appid, name string) error {
	appid = strings.TrimSpace(appid)
	name = strings.TrimSpace(name)
	if appid == "" || name == "" || s == nil {
		return nil
	}
	config, err := s.Load()
	if err != nil {
		return err
	}
	merged := map[string]any{}
	if raw, ok := config[appNamesConfigKey].(map[string]any); ok {
		for key, value := range raw {
			if str, ok := value.(string); ok {
				merged[key] = str
			}
		}
	}
	if existing, _ := merged[appid].(string); existing == name {
		return nil
	}
	merged[appid] = name
	config[appNamesConfigKey] = merged
	return s.Save(context.Background(), config)
}

// Load returns the stored config (empty map when missing or corrupt).
func (s *ConfigStore) Load() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *ConfigStore) loadLocked() (map[string]any, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	config := map[string]any{}
	if err := json.Unmarshal(data, &config); err != nil {
		return map[string]any{}, nil
	}
	return config, nil
}

// Save merges and persists the config. The mutex serializes read-modify-write
// cycles, and the final rename keeps a crash from leaving a half-written
// config behind.
func (s *ConfigStore) Save(ctx context.Context, patch map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	config, _ := s.loadLocked()
	for key, value := range patch {
		config[key] = value
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	return writeFileAtomic(s.path, data, 0o600)
}

// writeFileAtomic writes data beside path and renames it into place. The
// rename is same-directory, so it does not cross filesystems.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".wxtap-config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// PackageDirCandidate is one discoverable WeChat Applet package directory.
type PackageDirCandidate struct {
	OS          string `json:"os"`
	Version     string `json:"version"`
	Path        string `json:"path"`
	Exists      bool   `json:"exists"`
	Selected    bool   `json:"selected"`
	Description string `json:"description"`
}

// PackageDirCandidates lists the platform-specific Applet package locations
// shown in the extraction UI. Existing paths are marked but missing defaults
// remain visible so users can understand where WeChat stores packages.
func PackageDirCandidates() []PackageDirCandidate {
	selected := filepath.Clean(DefaultPackagesDir())
	var candidates []PackageDirCandidate
	seen := map[string]bool{}
	add := func(version, path, description string) {
		if strings.TrimSpace(path) == "" {
			return
		}
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		seen[path] = true
		candidates = append(candidates, PackageDirCandidate{
			OS:          runtime.GOOS,
			Version:     version,
			Path:        path,
			Exists:      dirExists(path),
			Selected:    selected != "" && samePath(path, selected),
			Description: description,
		})
	}
	if selected != "" {
		if override := strings.TrimSpace(os.Getenv("WXTAP_PACKAGES_DIR")); override != "" {
			add("custom", selected, "自定义目录（WXTAP_PACKAGES_DIR）")
		}
	}

	home, _ := os.UserHomeDir()
	appdata := os.Getenv("APPDATA")
	switch runtime.GOOS {
	case "windows":
		add("Windows v3", filepath.Join(home, "Documents", "WeChat Files", "Applet"), "Windows 微信 3.x：文件管理中的 Applet 目录")
		add("Windows v4", filepath.Join(appdata, "Tencent", "xwechat", "radium", "Applet", "packages"), "Windows 微信 4.x：radium 下的 Applet 包目录")
		for _, userDir := range userDirectories() {
			if pkgDir := userPackagesDir(userDir); pkgDir != "" {
				add("Windows v4 最新版", pkgDir, "Windows 微信 4.x 最新版：按登录用户自动发现")
			}
		}
	case "darwin":
		container := filepath.Join(home, "Library", "Containers", "com.tencent.xinWeChat", "Data")
		add("macOS v3", filepath.Join(container, ".wxapplet", "packages"), "macOS 微信 3.x：.wxapplet/packages")
		add("macOS v4", filepath.Join(container, "Documents", "app_data", "radium", "Applet", "packages"), "macOS 微信 4.x：radium/Applet/packages")
		for _, userDir := range userDirectories() {
			if pkgDir := userPackagesDir(userDir); pkgDir != "" {
				add("macOS v4 最新版", pkgDir, "macOS 微信 4.x 最新版：按登录用户自动发现")
			}
		}
	default:
		if selected != "" {
			add("custom", selected, "当前平台的包目录")
		}
	}

	if selected != "" && !seen[selected] {
		add("current", selected, "当前选中的包目录")
	}
	if selected == "" {
		for i := range candidates {
			if candidates[i].Exists {
				candidates[i].Selected = true
				break
			}
		}
	}
	return candidates
}

func childDirectories(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			dirs = append(dirs, filepath.Join(root, entry.Name()))
		}
	}
	return dirs
}

func samePath(left, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}
