// Package ipc provides the sessionkey.* IPC surface: locate session_key, iv
// and encryptedData material in captured packets, decompiled mini-program
// sources and (as a fallback) local WeChat account storage.
package ipc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

var appidRe = regexp.MustCompile(`^wx[0-9a-f]{16}$`)

var sessionKeyJSONRe = regexp.MustCompile(`"(?:session_key|sessionKey|sessionkey)"\s*:\s*"([A-Za-z0-9+/=]{24})"`)

var b64SixteenRe = regexp.MustCompile(`[A-Za-z0-9+/]{22}==`)

// SessionKeyUser describes one detected WeChat user account.
type SessionKeyUser struct {
	ID          string   `json:"id"`           // 32-char hex directory name
	Dir         string   `json:"dir"`          // full path
	AppIDs      []string `json:"appids"`       // applets found under applet/local
	PackagesDir string   `json:"packages_dir"` // <dir>/Applet|applet/packages when present
}

// SessionKeyFinding is one extracted session_key candidate with context.
type SessionKeyFinding struct {
	AppID   string `json:"appid"`   // owning mini-program (empty for user-level)
	Source  string `json:"source"`  // relative file path
	Value   string `json:"value"`   // the base64 session_key
	Kind    string `json:"kind"`    // mmkv | log | json
	Context string `json:"context"` // surrounding text (trimmed, 200 chars)
	Masked  string `json:"masked"`  // masked display value
	FoundAt string `json:"foundAt"` // file mtime RFC3339
}

// DetectedUsers scans the standard xwechat users directories and returns one
// SessionKeyUser per user hash directory. baseDir is the radium/users path.
func DetectedUsers(baseDir string) []SessionKeyUser {
	var users []SessionKeyUser
	for _, userDir := range childDirectories(baseDir) {
		if !isUserHash(filepath.Base(userDir)) {
			continue
		}
		u := SessionKeyUser{
			ID: filepath.Base(userDir), Dir: userDir, AppIDs: []string{},
			PackagesDir: userPackagesDir(userDir),
		}
		if ids := scanUserAppIDs(userDir); len(ids) > 0 {
			u.AppIDs = ids
		}
		users = append(users, u)
	}
	return users
}

// DefaultUsersDir returns the first existing platform-specific WeChat users
// directory, or the preferred path when none exists yet.
func DefaultUsersDir() string {
	candidates := usersDirCandidates()
	for _, dir := range candidates {
		if dirExists(dir) {
			return dir
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

// UsersDirCandidates exposes the platform-specific WeChat user roots so IPC
// callers can validate every legitimate installation layout, not just the
// first candidate returned by DefaultUsersDir.
func UsersDirCandidates() []string {
	return usersDirCandidates()
}

// IsUsersDir reports whether dir is a WeChat users root or a descendant of
// one. filepath.Rel gives the containment decision without permitting "..".
func IsUsersDir(dir string) bool {
	target, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return false
	}
	for _, candidate := range UsersDirCandidates() {
		base, err := filepath.Abs(filepath.Clean(candidate))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(base, target)
		if err == nil && rel == "." {
			return true
		}
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func usersDirCandidates() []string {
	if override := strings.TrimSpace(os.Getenv("WXTAP_USERS_DIR")); override != "" {
		return []string{override}
	}
	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return nil
		}
		return []string{filepath.Join(appData, "Tencent", "xwechat", "radium", "users")}
	}
	home, _ := os.UserHomeDir()
	container := filepath.Join(home, "Library", "Containers", "com.tencent.xinWeChat", "Data")
	return []string{
		filepath.Join(container, "Documents", "app_data", "radium", "users"),
		filepath.Join(container, "xwechat", "radium", "users"),
	}
}

// userDirectories returns every WeChat account directory across the
// platform-specific users roots. Package discovery and session_key scanning
// both derive from this single enumeration; callers that need the 32-hex
// account guarantee apply isUserHash themselves.
func userDirectories() []string {
	var dirs []string
	for _, root := range usersDirCandidates() {
		dirs = append(dirs, childDirectories(root)...)
	}
	return dirs
}

// ScanUser scans one user's directory for session_key material.
func ScanUser(userDir string) []SessionKeyFinding {
	var findings []SessionKeyFinding
	appletDir := filepath.Join(userDir, "applet", "local")
	appIDs := scanUserAppIDs(userDir)

	// 1. Scan per-app MMKV storage files.
	for _, appID := range appIDs {
		appDir := filepath.Join(appletDir, appID)
		for _, pattern := range []string{"usrmmkvstorage*", "mmkvadapterstorage"} {
			matches, _ := filepath.Glob(filepath.Join(appDir, pattern))
			for _, dir := range matches {
				if info, err := os.Stat(dir); err == nil && !info.IsDir() {
					findings = append(findings, scanFile(dir, appID, userDir)...)
					continue
				}
				entries, _ := os.ReadDir(dir)
				for _, e := range entries {
					if e.IsDir() || strings.HasSuffix(e.Name(), ".crc") {
						continue
					}
					findings = append(findings, scanFile(filepath.Join(dir, e.Name()), appID, userDir)...)
				}
			}
		}
	}

	// 2. Scan user-level mmkv files.
	mmkvDir := filepath.Join(userDir, "mmkv")
	_ = filepath.Walk(mmkvDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasSuffix(path, ".crc") {
			return nil
		}
		findings = append(findings, scanFile(path, "", userDir)...)
		return nil
	})

	// 3. Scan global applet MMKV.
	globalDir := filepath.Join(appletDir, "globalmmkvstorage")
	if entries, err := os.ReadDir(globalDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.HasSuffix(e.Name(), ".crc") {
				continue
			}
			findings = append(findings, scanFile(filepath.Join(globalDir, e.Name()), "", userDir)...)
		}
	}

	// 4. Scan mini-program logs for session_key patterns.
	for _, appID := range appIDs {
		logDir := filepath.Join(appletDir, appID, "usr", "miniprogramLog")
		if entries, err := os.ReadDir(logDir); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				findings = append(findings, scanLogFile(filepath.Join(logDir, e.Name()), appID, userDir)...)
			}
		}
	}

	return dedupFindings(findings)
}

// userPackagesDir returns the mini-program package directory of one WeChat
// user account. WeChat 4.x keeps packages inside the per-user applet tree and
// the casing differs between Windows and macOS installs, so the child is
// matched case-insensitively and returned with its real spelling. An account
// without packages yields "".
func userPackagesDir(userDir string) string {
	entries, err := os.ReadDir(userDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.EqualFold(entry.Name(), "applet") {
			continue
		}
		pkgDir := filepath.Join(userDir, entry.Name(), "packages")
		if dirExists(pkgDir) {
			return pkgDir
		}
	}
	return ""
}

// scanUserAppIDs returns the mini-program appids found under applet/local.
func scanUserAppIDs(userDir string) []string {
	localDir := filepath.Join(userDir, "applet", "local")
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && appidRe.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

// isUserHash checks for the 32-char hex directory naming used by xwechat.
func isUserHash(name string) bool {
	if len(name) != 32 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// scanFile reads a file (with shared read to handle locked files) and
// extracts session_key candidates from MMKV and JSON formats.
func scanFile(path, appID, userDir string) []SessionKeyFinding {
	data, err := readFileShared(path)
	if err != nil || len(data) < 24 {
		return nil
	}
	rel := relativePath(userDir, path)
	foundAt := fileModTime(path)

	var findings []SessionKeyFinding

	findings = append(findings, scanMMKV(data, appID, rel, foundAt)...)

	findings = append(findings, scanJSONKeys(data, appID, rel, foundAt)...)

	if len(findings) == 0 {
		findings = append(findings, scanRawB64(data, appID, rel, foundAt)...)
	}

	return findings
}

// scanMMKV walks the MMKV binary format looking for session_key keys.
func scanMMKV(data []byte, appID, rel, foundAt string) []SessionKeyFinding {
	var findings []SessionKeyFinding
	pos := 0
	for pos+8 <= len(data) {
		keyLen := int(binary.LittleEndian.Uint32(data[pos:]))
		pos += 4
		if keyLen <= 0 || keyLen > 1024 || pos+keyLen > len(data) {
			break
		}
		key := string(data[pos : pos+keyLen])
		pos += keyLen
		if pos+4 > len(data) {
			break
		}
		valLen := int(binary.LittleEndian.Uint32(data[pos:]))
		pos += 4
		if valLen < 0 || valLen > 1024*1024 || pos+valLen > len(data) {
			break
		}
		val := data[pos : pos+valLen]
		pos += valLen

		if isSessionKeyName(key) {
			value := extractB64Value(val)
			if value != "" {
				findings = append(findings, SessionKeyFinding{
					AppID: appID, Source: rel, Value: value, Kind: "mmkv",
					Context: fmt.Sprintf("MMKV key=%q value_len=%d", key, valLen),
					Masked:  maskValue(value), FoundAt: foundAt,
				})
			}
		}
		if len(val) > 0 && (val[0] == '{' || val[0] == '[') {
			findings = append(findings, scanJSONBytes(val, appID, rel, foundAt)...)
		}
	}
	return findings
}

// scanJSONKeys searches raw bytes for session_key/sessionKey JSON fields.
func scanJSONKeys(data []byte, appID, rel, foundAt string) []SessionKeyFinding {
	return scanJSONBytes(data, appID, rel, foundAt)
}

// scanJSONBytes extracts session_key values from JSON-like content.
func scanJSONBytes(data []byte, appID, rel, foundAt string) []SessionKeyFinding {
	var findings []SessionKeyFinding
	for _, m := range sessionKeyJSONRe.FindAllSubmatch(data, -1) {
		value := string(m[1])
		if decoded, err := base64.StdEncoding.DecodeString(value); err == nil && len(decoded) == 16 {
			ctx := contextAround(data, m[1])
			findings = append(findings, SessionKeyFinding{
				AppID: appID, Source: rel, Value: value, Kind: "json",
				Context: ctx, Masked: maskValue(value), FoundAt: foundAt,
			})
		}
	}
	return findings
}

// scanRawB64 scans for standalone base64 16-byte values (last resort).
func scanRawB64(data []byte, appID, rel, foundAt string) []SessionKeyFinding {
	var findings []SessionKeyFinding
	if len(data) > 256*1024 {
		return findings
	}
	seen := map[string]bool{}
	for _, m := range b64SixteenRe.FindAllSubmatch(data, -1) {
		value := string(m[0])
		if seen[value] {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(decoded) != 16 {
			continue
		}
		seen[value] = true
		findings = append(findings, SessionKeyFinding{
			AppID: appID, Source: rel, Value: value, Kind: "mmkv",
			Context: "standalone base64 16-byte value", Masked: maskValue(value), FoundAt: foundAt,
		})
	}
	return findings
}

// scanLogFile scans mini-program log files for session_key in logged JSON.
func scanLogFile(path, appID, userDir string) []SessionKeyFinding {
	data, err := readFileShared(path)
	if err != nil {
		return nil
	}
	rel := relativePath(userDir, path)
	foundAt := fileModTime(path)
	return scanJSONBytes(data, appID, rel, foundAt)
}

// isSessionKeyName checks if a MMKV key refers to session key material.
func isSessionKeyName(key string) bool {
	lower := strings.ToLower(key)
	return strings.Contains(lower, "session_key") ||
		strings.Contains(lower, "sessionkey") ||
		strings.Contains(lower, "session-key")
}

// extractB64Value pulls a base64 16-byte string from raw value bytes.
func extractB64Value(val []byte) string {
	if s := strings.TrimSpace(string(val)); s != "" {
		if decoded, err := base64.StdEncoding.DecodeString(s); err == nil && len(decoded) == 16 {
			return s
		}
	}
	if m := b64SixteenRe.Find(val); m != nil {
		if decoded, err := base64.StdEncoding.DecodeString(string(m)); err == nil && len(decoded) == 16 {
			return string(m)
		}
	}
	return ""
}

// readFileShared opens a file for reading. os.Open uses read access with
// FILE_SHARE_READ|FILE_SHARE_WRITE on Windows, so it can read files locked
// by a running WeChat process.
func readFileShared(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > 10*1024*1024 {
		return nil, fmt.Errorf("file too large: %d bytes", st.Size())
	}
	data := make([]byte, st.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, err
	}
	return data, nil
}

// relativePath returns path relative to base, or the full path if not inside.
func relativePath(base, path string) string {
	if rel, err := filepath.Rel(base, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// fileModTime returns the file's modification time as RFC3339 or "".
func fileModTime(path string) string {
	if st, err := os.Stat(path); err == nil {
		return st.ModTime().Format(time.RFC3339)
	}
	return ""
}

// contextAround extracts ~200 chars of surrounding text around a match.
func contextAround(data []byte, match []byte) string {
	idx := strings.Index(string(data), string(match))
	if idx < 0 {
		return ""
	}
	start := idx - 80
	if start < 0 {
		start = 0
	}
	end := idx + len(match) + 80
	if end > len(data) {
		end = len(data)
	}
	ctx := string(data[start:end])
	ctx = strings.Map(func(r rune) rune {
		if r >= 32 && r < 127 {
			return r
		}
		return -1
	}, ctx)
	if len(ctx) > 200 {
		ctx = ctx[:200]
	}
	return ctx
}

// maskValue shows the first 6 and last 4 chars with ellipsis in between.
func maskValue(v string) string {
	if len(v) <= 12 {
		return strings.Repeat("*", len(v))
	}
	return v[:6] + "..." + v[len(v)-4:]
}

// dedupFindings removes duplicate findings by (source, value).
func dedupFindings(findings []SessionKeyFinding) []SessionKeyFinding {
	seen := map[string]bool{}
	var out []SessionKeyFinding
	for _, f := range findings {
		key := f.Source + "\x00" + f.Value
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}

var (
	decompiledSessionKeyRe = regexp.MustCompile(`"(?:session_key|sessionKey|sessionkey)"\s*:\s*"([A-Za-z0-9+/]{22}==)"`)
	decompiledIVRe         = regexp.MustCompile(`"\s*(?:iv|IV|ivBase64)\s*"\s*:\s*"([A-Za-z0-9+/]{22}==)"`)
	decompiledEncryptedRe  = regexp.MustCompile(`"(?:encryptedData|encrypted_data)"\s*:\s*"([A-Za-z0-9+/=]{32,})"`)

	// Packets often carry the same fields as query parameters or form bodies
	// instead of JSON. Percent decoding happens before matching; these rules
	// then cover both raw and decoded key/value payloads.
	sessionKeyParamRe = regexp.MustCompile(`(?i)(?:^|[?&])(?:session_key|sessionKey|sessionkey|session-key)=([A-Za-z0-9+/]{22}==)(?:&|$)`)
	ivParamRe         = regexp.MustCompile(`(?i)(?:^|[?&])(?:ivBase64|iv)=([A-Za-z0-9+/]{22}==)(?:&|$)`)
	encryptedParamRe  = regexp.MustCompile(`(?i)(?:^|[?&])(?:encryptedData|encrypted_data)=([A-Za-z0-9+/=]{32,})(?:&|$)`)
)

// ScanDecompiled searches restored mini-program sources for crypto material
// that can be fed back into the SessionKey tool. It only returns masked
// values to callers; the original value is retained for explicit user action.
func ScanDecompiled(root string) []SessionKeyFinding {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	var findings []SessionKeyFinding
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".js", ".json", ".html", ".wxml", ".wxss", ".txt", ".log", ".map":
		default:
			return nil
		}
		data, readErr := readFileShared(path)
		if readErr != nil || len(data) == 0 {
			return nil
		}
		rel := relativePath(root, path)
		appID := ""
		if parts := strings.Split(rel, string(filepath.Separator)); len(parts) > 1 && appidRe.MatchString(parts[0]) {
			appID = parts[0]
		}
		foundAt := fileModTime(path)
		findings = append(findings, findDecompiledMatches(data, decompiledSessionKeyRe, "session_key", appID, rel, foundAt)...)
		findings = append(findings, findDecompiledMatches(data, decompiledIVRe, "iv", appID, rel, foundAt)...)
		findings = append(findings, findDecompiledMatches(data, decompiledEncryptedRe, "encryptedData", appID, rel, foundAt)...)
		return nil
	})
	return dedupFindings(findings)
}

func findDecompiledMatches(data []byte, pattern *regexp.Regexp, kind, appID, source, foundAt string) []SessionKeyFinding {
	matches := pattern.FindAllSubmatch(data, -1)
	out := make([]SessionKeyFinding, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		value := strings.TrimSpace(string(match[1]))
		if value == "" {
			continue
		}
		out = append(out, SessionKeyFinding{
			AppID: appID, Source: source, Value: value, Kind: kind,
			Context: contextAround(data, match[1]), Masked: maskValue(value), FoundAt: foundAt,
		})
	}
	return out
}

// ScanTrafficRecords extracts session_key / iv / encryptedData material from
// already captured packets (hook-drained wxapi and cloud records). Bodies are
// matched verbatim and again after percent-decoding, because mini programs
// routinely send these fields URL-encoded.
//
// ctx bounds the scan itself, not just the SQL read: worst case this walks
// gigabytes of bodies through six regexes each, and the caller's timeout must
// be able to stop it. A cancelled scan returns the findings collected so far.
func ScanTrafficRecords(ctx context.Context, records []traffic.Record) []SessionKeyFinding {
	var findings []SessionKeyFinding
	for _, record := range records {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		source := trafficSource(record)
		foundAt := ""
		if !record.CapturedAt.IsZero() {
			foundAt = record.CapturedAt.Format(time.RFC3339)
		}
		if len(record.RequestBody) > 0 {
			findings = append(findings, scanTrafficBody(record.RequestBody, record.AppID, source+" (request)", foundAt)...)
		}
		if len(record.ResponseBody) > 0 {
			findings = append(findings, scanTrafficBody(record.ResponseBody, record.AppID, source+" (response)", foundAt)...)
		}
		if record.URL != "" {
			findings = append(findings, scanTrafficBody([]byte(record.URL), record.AppID, source, foundAt)...)
		}
	}
	return dedupFindings(findings)
}

// trafficSource labels a finding with the packet it came from so the UI can
// show which request carried the material.
func trafficSource(record traffic.Record) string {
	label := strings.TrimSpace(strings.TrimSpace(record.Method) + " " + strings.TrimSpace(record.URL))
	if label == "" {
		label = strings.TrimSpace(record.Name)
	}
	if label == "" {
		label = strings.TrimSpace(record.APIType)
	}
	if label == "" {
		label = "capture"
	}
	// The full record ID is stable across Core restarts; page-local seq alone
	// can repeat in another app or another page generation.
	identity := record.ID
	if identity == "" {
		identity = strconv.Itoa(int(record.Seq))
	}
	return fmt.Sprintf("traffic#%s %s", identity, label)
}

// scanTrafficBody matches one body (or URL) and then its percent-decoded form.
func scanTrafficBody(body []byte, appID, source, foundAt string) []SessionKeyFinding {
	findings := findTrafficMatches(body, appID, source, foundAt)
	if decoded, ok := percentDecode(body); ok {
		findings = append(findings, findTrafficMatches(decoded, appID, source, foundAt)...)
	}
	return findings
}

func findTrafficMatches(body []byte, appID, source, foundAt string) []SessionKeyFinding {
	var findings []SessionKeyFinding
	findings = append(findings, findDecompiledMatches(body, decompiledSessionKeyRe, "session_key", appID, source, foundAt)...)
	findings = append(findings, findDecompiledMatches(body, decompiledIVRe, "iv", appID, source, foundAt)...)
	findings = append(findings, findDecompiledMatches(body, decompiledEncryptedRe, "encryptedData", appID, source, foundAt)...)
	findings = append(findings, findDecompiledMatches(body, sessionKeyParamRe, "session_key", appID, source, foundAt)...)
	findings = append(findings, findDecompiledMatches(body, ivParamRe, "iv", appID, source, foundAt)...)
	findings = append(findings, findDecompiledMatches(body, encryptedParamRe, "encryptedData", appID, source, foundAt)...)
	return findings
}

// percentDecode decodes a URL-encoded payload. PathUnescape is used instead of
// QueryUnescape so base64 '+' stays a plus sign.
func percentDecode(body []byte) ([]byte, bool) {
	if !bytes.ContainsRune(body, '%') {
		return nil, false
	}
	decoded, err := url.PathUnescape(string(body))
	if err != nil {
		return nil, false
	}
	return []byte(decoded), true
}
