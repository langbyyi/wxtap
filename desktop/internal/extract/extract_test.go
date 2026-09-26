package extract

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildEncryptedWxapkg produces a V1MMWX-encrypted package whose decrypted
// payload is a valid wxapkg containing the given files, using the
// format: AES-256-CBC over file[6:1030] (PKCS7-padded 1023-byte header) and
// XOR(appID[-2]) over the tail.
func buildEncryptedWxapkg(t *testing.T, appID string, payload []byte) []byte {
	t.Helper()
	if len(payload) < 1030 {
		t.Fatalf("fixture payload too short: %d", len(payload))
	}

	plain := payload[:1023]
	padded := pkcs7Pad(plain, aes.BlockSize)
	key, err := deriveKey(appID)
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	iv := []byte(wxapkgIV)
	if len(iv) != aes.BlockSize {
		t.Fatalf("iv length %d", len(iv))
	}
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted, padded)

	xorKey := appID[len(appID)-2]
	tail := make([]byte, len(payload[1023:]))
	for i, b := range payload[1023:] {
		tail[i] = b ^ byte(xorKey)
	}

	out := append([]byte(wxapkgMagic), encrypted...)
	return append(out, tail...)
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padLen := blockSize - len(data)%blockSize
	pad := bytes.Repeat([]byte{byte(padLen)}, padLen)
	return append(append([]byte{}, data...), pad...)
}

// buildWxapkg assembles a plaintext wxapkg with the given virtual files.
func buildWxapkg(files map[string][]byte) []byte {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	// Deterministic order.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}

	var index, body bytes.Buffer
	// Offsets are absolute from the start of the package file.
	indexLen := 0
	for _, name := range names {
		indexLen += 12 + len(name)
	}
	baseOffset := 1 + 4 + 4 + 4 + 1 + 4 + indexLen // header through fileCount + index
	offset := 0
	for _, name := range names {
		content := files[name]
		var lenBuf [4]byte
		bePutUint32(lenBuf[:], uint32(len(name)))
		index.Write(lenBuf[:])
		index.WriteString(name)
		bePutUint32(lenBuf[:], uint32(baseOffset+offset))
		index.Write(lenBuf[:])
		bePutUint32(lenBuf[:], uint32(len(content)))
		index.Write(lenBuf[:])
		body.Write(content)
		offset += len(content)
	}

	var out bytes.Buffer
	out.WriteByte(0xBE)
	var hdr [4]byte
	bePutUint32(hdr[:], 0) // info1
	out.Write(hdr[:])
	bePutUint32(hdr[:], uint32(index.Len()))
	out.Write(hdr[:])
	bePutUint32(hdr[:], uint32(body.Len()))
	out.Write(hdr[:])
	out.WriteByte(0xED)
	bePutUint32(hdr[:], uint32(len(names)))
	out.Write(hdr[:])
	out.Write(index.Bytes())
	out.Write(body.Bytes())
	return out.Bytes()
}

func bePutUint32(dst []byte, v uint32) {
	dst[0] = byte(v >> 24)
	dst[1] = byte(v >> 16)
	dst[2] = byte(v >> 8)
	dst[3] = byte(v)
}

func TestDecryptRoundTrip(t *testing.T) {
	inner := buildWxapkg(map[string][]byte{
		"app.js":           []byte(strings.Repeat("console.log('hi');", 80)),
		"pages/index.json": []byte(`{"n":1}`),
	})
	enc := buildEncryptedWxapkg(t, "wx1234567890abcdef", inner)

	decrypted, err := Decrypt(enc, "wx1234567890abcdef")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(decrypted, inner) {
		t.Fatalf("decrypted payload mismatch: got %d bytes want %d", len(decrypted), len(inner))
	}
}

func TestDecryptPassesThroughUnencryptedPkg(t *testing.T) {
	plain := append(buildWxapkg(map[string][]byte{"a.js": []byte("x")}),
		bytes.Repeat([]byte{0x00}, 1100)...)

	decrypted, err := Decrypt(plain, "wx1234567890abcdef")
	if err != nil {
		t.Fatalf("decrypt plain: %v", err)
	}
	if !bytes.Equal(decrypted, plain) {
		t.Fatal("unencrypted package must pass through unchanged")
	}
}

// A small unencrypted package (under the 1030-byte encrypted header) must
// still pass through: the 0xBE marker identifies it before any length rule.
func TestDecryptPassesThroughSmallUnencryptedPkg(t *testing.T) {
	plain := buildWxapkg(map[string][]byte{"app.js": []byte("var x = 1;")})

	decrypted, err := Decrypt(plain, "wx1234567890abcdef")
	if err != nil {
		t.Fatalf("decrypt small plain package: %v", err)
	}
	if !bytes.Equal(decrypted, plain) {
		t.Fatal("small unencrypted package must pass through unchanged")
	}
}

func TestDecryptRejectsWrongAppID(t *testing.T) {
	enc := buildEncryptedWxapkg(t, "wx1234567890abcdef", buildWxapkg(map[string][]byte{"a.js": []byte(strings.Repeat("y", 1200))}))
	if _, err := Decrypt(enc, "wx0000000000000000"); err == nil {
		t.Fatal("wrong appID must fail (invalid unpack header or garbage)")
	}
}

// TestUnpackRejectsTruncatedHeader feeds packages cut short inside the fixed
// header. A 14-byte file satisfied the old length check and then panicked while
// reading the file count out of bytes that were not there.
func TestUnpackRejectsTruncatedHeader(t *testing.T) {
	inner := buildWxapkg(map[string][]byte{"app.js": []byte("console.log(1)")})
	for length := 1; length < 18; length++ {
		if _, err := Unpack(inner[:length]); err == nil {
			t.Fatalf("Unpack accepted a %d-byte truncated package", length)
		}
	}
	if _, err := Unpack(inner); err != nil {
		t.Fatalf("unpack complete package: %v", err)
	}
}

func TestUnpackAndExtract(t *testing.T) {
	inner := buildWxapkg(map[string][]byte{
		"app.js":           []byte("console.log(1)"),
		"pages/index.html": []byte("<html></html>"),
	})
	entries, err := Unpack(inner)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if len(entries) != 2 || entries["app.js"] == nil {
		t.Fatalf("unexpected entries: %v", entryNames(entries))
	}

	outDir := t.TempDir()
	extracted, err := ExtractTo(outDir, entries)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(extracted) != 2 {
		t.Fatalf("extracted %d files", len(extracted))
	}
	data, err := os.ReadFile(filepath.Join(outDir, "pages", "index.html"))
	if err != nil || string(data) != "<html></html>" {
		t.Fatalf("extracted content wrong: %v %q", err, data)
	}
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	inner := buildWxapkg(map[string][]byte{
		"../../evil.js": []byte("pwned"),
		"ok.js":         []byte("fine"),
	})
	entries, err := Unpack(inner)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	outDir := t.TempDir()
	extracted, err := ExtractTo(outDir, entries)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(extracted) != 1 {
		t.Fatalf("traversal file must be skipped, extracted %d", len(extracted))
	}
	if _, err := os.Stat(filepath.Dir(outDir) + "/evil.js"); err == nil {
		t.Fatal("path traversal escaped output dir")
	}
}

const jwtToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.abc123def456ghi"

func TestAnalyzeKeepsOnlyValidIdentityAndTokenValues(t *testing.T) {
	validID := "11010519491231002X"
	invalidID := "110105194912310021"
	validJWT := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl"
	partialJWT := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0"
	result := Analyze(`var id = "` + validID + `"; var badId = "` + invalidID + `"; var token = "` + validJWT + `"; var partial = "` + partialJWT + `";`)

	if !contains(result["sfz"], validID) || contains(result["sfz"], invalidID) {
		t.Fatalf("identity validation mismatch: %#v", result["sfz"])
	}
	if !contains(result["jwt"], validJWT) || contains(result["jwt"], partialJWT) {
		t.Fatalf("JWT validation mismatch: %#v", result["jwt"])
	}
}

func TestAnalyzeRejectsPlaceholderCredentialsAndShortPaths(t *testing.T) {
	result := Analyze(`var api_key = "YOUR_API_KEY"; var route = "a.b"; var real = "LTAI5tABCDEFghijklmnop";`)
	if contains(result["secret"], `api_key = "YOUR_API_KEY"`) || contains(result["key"], `api_key = "YOUR_API_KEY"`) {
		t.Fatalf("placeholder credential must be excluded: %#v", result)
	}
	if len(result["path"]) != 0 {
		t.Fatalf("short dotted token must not be a path: %#v", result["path"])
	}
	if !contains(result["secret"], "LTAI5tABCDEFghijklmnop") {
		t.Fatalf("real credential must remain: %#v", result["secret"])
	}
}

func TestAnalyzeRejectsSourceFileEmailLookalikes(t *testing.T) {
	result := Analyze(`var component = "widget@page.vue"; var contact = "support@example.com";`)
	if contains(result["mail"], "widget@page.vue") {
		t.Fatalf("source file lookalike must not be an email: %#v", result["mail"])
	}
	if !contains(result["mail"], "support@example.com") {
		t.Fatalf("real email must remain: %#v", result["mail"])
	}
}

func TestCategoriesExcludeUnrequestedNetworkAndAlgorithmRules(t *testing.T) {
	for _, unwanted := range []string{"domain", "ip", "ip_port", "algorithm"} {
		if contains(Categories, unwanted) {
			t.Fatalf("%s must not be an extraction category: %v", unwanted, Categories)
		}
	}

	result := Analyze(`var host = "127.0.0.1:8080"; var hash = md5(value);`)
	for _, unwanted := range []string{"domain", "ip", "ip_port", "algorithm"} {
		if _, ok := result[unwanted]; ok {
			t.Fatalf("%s must not be scanned: %#v", unwanted, result)
		}
	}
}

func TestAnalyzeRejectsURLWithoutHost(t *testing.T) {
	result := Analyze(`var plain = "https://"; var escaped = "https:\/\/";`)
	if len(result["url"]) != 0 {
		t.Fatalf("incomplete URL must not be extracted: %#v", result["url"])
	}
}

func TestAnalyzeDetectsCategories(t *testing.T) {
	content := `
var phone = '13812345678';
var idcard = "11010519491231002X";
var mail = "admin@example.com";
var image = "logo.png";
var token = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.abc123def456ghi";
var ak = "LTAI5tABCDEFghijklmnop";
var bucket = "my-bucket.oss-cn-hangzhou.aliyuncs.com/data/file.png";
var api = "https://api.example.com/v1/users";
`
	result := Analyze(content)

	if !contains(result["mobile"], "13812345678") {
		t.Fatalf("mobile missing: %v", result["mobile"])
	}
	if !contains(result["sfz"], "11010519491231002X") {
		t.Fatalf("sfz missing: %v", result["sfz"])
	}
	if !contains(result["mail"], "admin@example.com") {
		t.Fatalf("mail missing: %v", result["mail"])
	}
	if contains(result["mail"], "logo.png") {
		t.Fatalf("image extension must be excluded from mail: %v", result["mail"])
	}
	if !contains(result["jwt"], jwtToken) {
		t.Fatalf("jwt missing: %v", result["jwt"])
	}
	if !contains(result["secret"], "LTAI5tABCDEFghijklmnop") {
		t.Fatalf("aliyun AK secret missing: %v", result["secret"])
	}
	if !contains(result["oss"], "my-bucket.oss-cn-hangzhou.aliyuncs.com") {
		t.Fatalf("oss missing: %v", result["oss"])
	}
	if !contains(result["url"], "https://api.example.com/v1/users") {
		t.Fatalf("url missing: %v", result["url"])
	}
}

func TestAnalyzeNucleiKeySecret(t *testing.T) {
	content := `config = { "wechat_appsecret": "a1b2c3d4e5f6g7h8", "other": 1 };`
	result := Analyze(content)
	found := false
	for _, s := range result["secret"] {
		if strings.Contains(s, "wechat_appsecret") {
			found = true
		}
	}
	if !found {
		t.Fatalf("credential key-name secret not detected: %v", result["secret"])
	}
	if !contains(result["key"], `"wechat_appsecret": "a1b2c3d4e5f6g7h8"`) {
		t.Fatalf("key category missing: %v", result["key"])
	}
}

func TestAnalyzeDetectsUnquotedMobileAndJDBC(t *testing.T) {
	result := Analyze(`phone=13800138000; db="jdbc:mysql://db.example.com:3306/app"`)
	if !contains(result["mobile"], "13800138000") {
		t.Fatalf("unquoted mobile missing: %v", result["mobile"])
	}
	if !contains(result["jdbc"], "jdbc:mysql://db.example.com:3306/app") {
		t.Fatalf("jdbc missing: %v", result["jdbc"])
	}
}

func TestScanFilesEndToEnd(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte(`var a = "13998887777";`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pages", "index.js"), []byte(`var u = "https://cdn.test.com/x/y.png";`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("not js"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := ScanFiles(dir)
	if len(result.FilesScanned) != 3 {
		t.Fatalf("expected JS and text sources scanned, got %v", result.FilesScanned)
	}
	if !contains(result.FilesScanned, filepath.Join(dir, "readme.txt")) {
		t.Fatalf("supported text source was skipped: %v", result.FilesScanned)
	}
	if !contains(result.Analysis["mobile"], "13998887777") {
		t.Fatalf("mobile not found in multi-file scan: %v", result.Analysis["mobile"])
	}
	// .png static asset from the URL must land in "static", not "url".
	if !contains(result.Analysis["static"], "https://cdn.test.com/x/y.png") {
		t.Fatalf("static asset missing: %v", result.Analysis["static"])
	}
}

func TestScanFilesWithPatternsReportsProgress(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.js"), []byte("var a=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.js"), []byte("var b=2"), 0o644); err != nil {
		t.Fatal(err)
	}
	var lastDone, lastTotal int
	result, err := ScanFilesWithPatternsProgress(dir, nil, func(done, total int) {
		lastDone, lastTotal = done, total
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(result.FilesScanned) != 2 || lastDone != 2 || lastTotal != 2 {
		t.Fatalf("progress=%d/%d files=%d", lastDone, lastTotal, len(result.FilesScanned))
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func entryNames(entries map[string][]byte) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	return names
}

// Report values come from scanned sources; the HTML rendition must mask
// credentials and escape remaining text so crafted values cannot become markup.
func TestScanFilesBoundsOversizedRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.js")
	head := "var early = '13800138000';\n"
	padding := strings.Repeat("// padding\n", (maxFileRead/11)+16)
	tail := "\nvar late = '13900139000';\n"
	if err := os.WriteFile(path, []byte(head+padding+tail), 0o644); err != nil {
		t.Fatal(err)
	}

	result := ScanFiles(dir)
	if len(result.FilesScanned) != 1 {
		t.Fatalf("files scanned: %#v", result.FilesScanned)
	}
	if result.TotalSize != int64(len(head)+len(padding)+len(tail)) {
		t.Fatalf("total size should report the on-disk size, got %d", result.TotalSize)
	}
	if !containsString(result.Analysis["mobile"], "13800138000") {
		t.Fatalf("mobile number inside the analyzed prefix was missed: %#v", result.Analysis["mobile"])
	}
	if containsString(result.Analysis["mobile"], "13900139000") {
		t.Fatalf("content past the 2 MB window must stay out of the result: %#v", result.Analysis["mobile"])
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Report files are named <appid>_<ts>.json; a path-like appid would write (or
// glob) outside the reports directory.
func TestValidateAppIDRules(t *testing.T) {
	for _, bad := range []string{"", " ", "..", ".", "a/b", `a\b`, " wx", "wx "} {
		if err := ValidateAppID(bad); err == nil {
			t.Fatalf("appid %q must be rejected", bad)
		}
	}
	if err := ValidateAppID("wxone"); err != nil {
		t.Fatalf("valid appid rejected: %v", err)
	}
}

// The pattern scanner is the path the app's extract.scan uses; it must apply
// the same bounded read and truncation window as the plain scanner. Content
// past 2 MB stays out of the result, while total_size reports the real file.
func TestScanFilesWithPatternsBoundsOversizedRead(t *testing.T) {
	dir := t.TempDir()
	head := "var early = '13800138000';\n"
	padding := strings.Repeat("// padding\n", (maxFileRead/11)+16)
	tail := "\nvar late = '13900139000';\n"
	if err := os.WriteFile(filepath.Join(dir, "huge.js"), []byte(head+padding+tail), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ScanFilesWithPatternsProgress(dir, map[string]string{"phone": `1[38]\d{9}`}, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if result.TotalSize != int64(len(head)+len(padding)+len(tail)) {
		t.Fatalf("total size should be the on-disk size, got %d", result.TotalSize)
	}
	custom := result.Analysis["phone"]
	if !containsString(custom, "13800138000") {
		t.Fatalf("prefix match missing: %#v", custom)
	}
	if containsString(custom, "13900139000") {
		t.Fatalf("content past the read window must not be scanned: %#v", custom)
	}
}

// TestUnpackBoundsPreallocation covers a crafted package header: fileCount is
// read straight out of the file, so trusting it for make([]entry, 0, fileCount)
// let a 49-byte package ask the runtime for gigabytes. The declared count must
// never drive the preallocation.
func TestUnpackBoundsPreallocation(t *testing.T) {
	inner := buildWxapkg(map[string][]byte{"app.js": []byte("console.log(1)")})
	// Bytes 14:18 hold fileCount; claim far more entries than the package can
	// physically contain (each index entry needs at least 12 bytes).
	if len(inner) < 18 {
		t.Fatalf("fixture too short: %d", len(inner))
	}
	bePutUint32(inner[14:18], 10_000_000)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	entries, err := Unpack(inner)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if len(entries) != 1 || string(entries["app.js"]) != "console.log(1)" {
		t.Fatalf("unexpected entries: %v", entryNames(entries))
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("Unpack allocated %d bytes for a %d-byte package; the declared file count must not drive preallocation", allocated, len(inner))
	}
}

// ExtractWxapkg is the primitive behind extract.decompile / miniapp_decompile:
// it has to decrypt, unpack and write in one step, and a wrong appID must fail
// without leaving half-written files behind.
func TestExtractWxapkgDecryptsUnpacksAndWrites(t *testing.T) {
	const appID = "wx1234567890abcdef"
	inner := buildWxapkg(map[string][]byte{
		"app.js":     []byte(strings.Repeat("/** pad */", 120)),
		"pages/a.js": []byte("Page({})"),
	})
	if len(inner) < 1030 {
		t.Fatalf("fixture payload too short for the encrypted format: %d", len(inner))
	}
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildEncryptedWxapkg(t, appID, inner), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	written, err := ExtractWxapkg(pkgPath, outDir, appID)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("wrote %d files, want 2: %v", len(written), written)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "pages", "a.js"))
	if err != nil || string(data) != "Page({})" {
		t.Fatalf("extracted content wrong: %v %q", err, data)
	}

	wrongOut := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, wrongOut, "wx0000000000000000"); err == nil {
		t.Fatal("a wrong appID must fail decryption")
	}
	if entries, err := os.ReadDir(wrongOut); err != nil || len(entries) != 0 {
		t.Fatalf("failed decryption must not write files: %v %v", entries, err)
	}
}

func TestExtractWxapkgRestoresAppConfigAndSplitsModules(t *testing.T) {
	const appID = "wx1234567890abcdef"
	inner := buildWxapkg(map[string][]byte{
		"app-config.json": []byte(`{"appname":"Demo","pages":["pages/index/index"],"entryPagePath":"pages/index/index.html","global":{"window":{"navigationBarTitleText":"Demo"}}}`),
		"app-service.js":  []byte(`define("pages/index/index.js", function(require,module,exports){Page({data:{title:"Hello"}});}, {isPage:true});`),
		"assets/pad.txt":  bytes.Repeat([]byte("x"), 1200),
	})
	if len(inner) < 1030 {
		t.Fatalf("fixture payload too short for the encrypted format: %d", len(inner))
	}
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildEncryptedWxapkg(t, appID, inner), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, appID); err != nil {
		t.Fatalf("extract: %v", err)
	}

	appJSON, err := os.ReadFile(filepath.Join(outDir, "app.json"))
	if err != nil {
		t.Fatalf("restored app.json: %v", err)
	}
	var app map[string]any
	if err := json.Unmarshal(appJSON, &app); err != nil {
		t.Fatalf("decode app.json: %v", err)
	}
	if app["appname"] != "Demo" {
		t.Fatalf("app.json did not preserve app-config data: %#v", app)
	}

	pageJS, err := os.ReadFile(filepath.Join(outDir, "pages", "index", "index.js"))
	if err != nil {
		t.Fatalf("restored page module: %v", err)
	}
	if !strings.Contains(string(pageJS), "Page({data:{title:\"Hello\"}})") {
		t.Fatalf("page module content mismatch: %q", pageJS)
	}
}

// A ${...} substitution holding another template literal used to end the outer
// template at the inner backtick. The scanner then read template text as code,
// lost the module's closing brace and failed the whole app with
// `unterminated module common.js in app-service.js`.
//
// The trigger is a nested template whose text holds a brace: the outer template
// is cut short at the inner backtick, so the inner template's text is counted as
// code and a later real brace no longer closes the module. minified chunks with
// ${cond?`"${x}"`:y} inside an 800 KB one-line bundle hit this.
func TestSplitAppServiceKeepsModulesAfterNestedTemplate(t *testing.T) {
	// Raw strings, so the placeholder stands in for a backtick.
	const body = `var s=@BT@${a?@BT@{@BT@:b}@BT@;`
	const fixture = `define("common.js", function(require,module,exports){
` + body + `
});
define("runtime.js", function(require,module,exports){exports.v=1;});`
	readable := strings.ReplaceAll(body, "@BT@", "`")

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app-service.js"), []byte(strings.ReplaceAll(fixture, "@BT@", "`")), 0o600); err != nil {
		t.Fatal(err)
	}
	written, err := splitAppService(root)
	if err != nil {
		t.Fatalf("split app-service: %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("wrote %d modules, want 2: %v", len(written), written)
	}
	restored, err := os.ReadFile(filepath.Join(root, "common.js"))
	if err != nil {
		t.Fatalf("common.js: %v", err)
	}
	// The module must be cut at its own closing brace, not inside the template.
	if !strings.Contains(string(restored), readable) {
		t.Fatalf("module body truncated: %q", restored)
	}
	if _, err := os.ReadFile(filepath.Join(root, "runtime.js")); err != nil {
		t.Fatalf("module after the nested template was not restored: %v", err)
	}
}

// A chunk compiled by WeChat's newer template runtime resolves to a chain of
// functions instead of a node tree, so the export cannot be marshalled and the
// step reports encoding/json's "unsupported type: func(...)". That message says
// nothing about what the user can do, and this step's failure aborts the whole
// decompilation, so it has to name the real cause.
func TestRestoreWebviewWXMLExplainsCompiledTemplates(t *testing.T) {
	root := t.TempDir()
	chunk := `var __wxCodeSpace__ = {batchAddCompiledTemplate: function(){}};
__wxCodeSpace__.batchAddCompiledTemplate(function(G,R){return {};});
__wxAppCode__['pages/a/index.wxml'] = [function(){ return function(){} }];`
	if err := os.WriteFile(filepath.Join(root, "chunk_0.webview.js"), []byte(chunk), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := restoreWebviewWXML(root)
	if err == nil {
		t.Fatal("a compiled-template chunk must fail restoration")
	}
	if !strings.Contains(err.Error(), "__wxCodeSpace__") {
		t.Fatalf("error does not name the compiled-template runtime: %v", err)
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("original cause dropped: %v", err)
	}
}

func TestExtractWxapkgRestoresWXMLFromPageFrame(t *testing.T) {
	frame := `<script>
var $gwx = function(path) {
  return function() {
    return {tag:"wx-view", attr:{class:"page"}, children:[
      {tag:"wx-text", attr:{}, children:["hello"]}
    ]};
  };
};
else __wxAppCode__['pages/index/index.wxml'] = $gwx('pages/index/index.wxml');
</script>`
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{
		"page-frame.html": []byte(frame),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx1234567890abcdef"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	wxml, err := os.ReadFile(filepath.Join(outDir, "pages", "index", "index.wxml"))
	if err != nil {
		t.Fatalf("restored wxml: %v", err)
	}
	if !strings.Contains(string(wxml), `<view class="page">`) || !strings.Contains(string(wxml), `<text>hello</text>`) {
		t.Fatalf("wxml content mismatch: %q", wxml)
	}
}

func TestExtractWxapkgRestoresWXMLWhenTemplateUsesConsole(t *testing.T) {
	frame := `<script>
var $gwx = function(path) {
  return function() {
    console.log(path);
    return {tag:"wx-view", attr:{}, children:["ok"]};
  };
};
__wxAppCode__['pages/index/comp.wxml'] = $gwx('pages/index/comp.wxml');
</script>`
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{"page-frame.html": []byte(frame)}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx1234567890abcdef"); err != nil {
		t.Fatalf("extract WXML with console: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "pages", "index", "comp.wxml")); err != nil {
		t.Fatalf("restored template missing: %v", err)
	}
}

func TestSafeChildPathMapsPluginPrivateWXMLToPortablePath(t *testing.T) {
	root := t.TempDir()
	target, err := safeChildPath(root, "plugin-private://wxc6e2bf2f6fc2e53a/components/mantisChat.wxml")
	if err != nil {
		t.Fatalf("plugin path: %v", err)
	}
	want := filepath.Join(root, "plugin-private", "wxc6e2bf2f6fc2e53a", "components", "mantisChat.wxml")
	if target != want {
		t.Fatalf("target = %q, want %q", target, want)
	}
}

func TestExtractWxapkgRestoresWXSSFromAppWxss(t *testing.T) {
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{
		"app-wxss.js": []byte(`setCssToHead([".page {", [0, 16], "}"], "pages/index/index.wxss");`),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx1234567890abcdef"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	wxss, err := os.ReadFile(filepath.Join(outDir, "pages", "index", "index.wxss"))
	if err != nil {
		t.Fatalf("restored wxss: %v", err)
	}
	if !strings.Contains(string(wxss), ".page {16rpx}") {
		t.Fatalf("wxss content mismatch: %q", wxss)
	}
}

func TestExtractWxapkgRestoresDeveloperAppConfig(t *testing.T) {
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{
		"app-config.json": []byte(`{"appname":"Demo","pages":["pages/index/index","pkg/a/a"],"entryPagePath":"pages/index/index.html","global":{"window":{"navigationBarTitleText":"Demo"}},"subPackages":[{"root":"pkg/a","pages":["a"]}]}`),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx1234567890abcdef"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "app.json"))
	if err != nil {
		t.Fatal(err)
	}
	var app map[string]any
	if err := json.Unmarshal(data, &app); err != nil {
		t.Fatal(err)
	}
	if app["entryPagePath"] != "pages/index/index" {
		t.Fatalf("entryPagePath mismatch: %#v", app)
	}
	pages := app["pages"].([]any)
	if len(pages) != 1 || pages[0] != "pages/index/index" {
		t.Fatalf("pages mismatch: %#v", pages)
	}
	window := app["window"].(map[string]any)
	if window["navigationBarTitleText"] != "Demo" {
		t.Fatalf("window mismatch: %#v", window)
	}
	subPackages := app["subPackages"].([]any)
	sub := subPackages[0].(map[string]any)
	if sub["root"] != "pkg/a/" {
		t.Fatalf("subpackage mismatch: %#v", sub)
	}
}

func TestExtractWxapkgRestoresPageJSONFromAppService(t *testing.T) {
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{
		"app-service.js": []byte(`__wxAppCode__['pages/index/index.json'] = {"navigationBarTitleText":"Home","usingComponents":{"card":"/components/card/card"}};`),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx1234567890abcdef"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "pages", "index", "index.json"))
	if err != nil {
		t.Fatalf("restored page json: %v", err)
	}
	var page map[string]any
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if page["navigationBarTitleText"] != "Home" {
		t.Fatalf("page json mismatch: %#v", page)
	}
}

func TestExtractWxapkgRestoresPageConfigs(t *testing.T) {
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{
		"app-config.json": []byte(`{"pages":["pages/index/index"],"page":{"pages/index/index.html":{"window":{"enablePullDownRefresh":true}}}}`),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx1234567890abcdef"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "pages", "index", "index.json"))
	if err != nil {
		t.Fatalf("restored page json: %v", err)
	}
	var page map[string]any
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if page["enablePullDownRefresh"] != true {
		t.Fatalf("page json mismatch: %#v", page)
	}
}

func TestExtractWxapkgSplitsModuleWithRegexBrace(t *testing.T) {
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{
		"app-service.js": []byte(`define("common/vendor.js", function(require,module,exports){var open = /{/g;}, {isPage:false});`),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx1234567890abcdef"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "common", "vendor.js"))
	if err != nil {
		t.Fatalf("restored regex module: %v", err)
	}
	if !strings.Contains(string(data), `/{/g`) {
		t.Fatalf("regex module content mismatch: %q", data)
	}
}

func TestExtractWxapkgRestoresWXMLFromWebviewChunk(t *testing.T) {
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{
		"chunk_0.webview.js": []byte(`
$gwx_XC_0 = function() {
  return function(path) {
    return function(env) {
      return {tag:"wx-page", children:[{tag:"wx-view", attr:{class:"chunk"}, children:["chunk"]}]};
    };
  };
};
__wxAppCode__ = __wxAppCode__ || {};
__wxAppCode__['pages/chunk/index.wxml'] = $gwx_XC_0('./pages/chunk/index.wxml');
`),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx1234567890abcdef"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	wxml, err := os.ReadFile(filepath.Join(outDir, "pages", "chunk", "index.wxml"))
	if err != nil {
		t.Fatalf("restored chunk wxml: %v", err)
	}
	if strings.TrimSpace(string(wxml)) != `<view class="chunk">chunk</view>` {
		t.Fatalf("chunk wxml mismatch: %q", wxml)
	}
}

func TestExtractWxapkgRestoresWXMLWhenTemplateFactoryIsDefinedAfterAssignment(t *testing.T) {
	frame := `<script>
__wxAppCode__['pages/404/404.wxml'] = $gwx_XC_0('./pages/404/404.wxml');
var $gwx_XC_0 = function(path) {
  return function() {
    return {tag:"wx-view", attr:{class:"not-found"}, children:["404"]};
  };
};
</script>`
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{
		"page-frame.html": []byte(frame),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx30a3a79e182c606f"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	wxml, err := os.ReadFile(filepath.Join(outDir, "pages", "404", "404.wxml"))
	if err != nil {
		t.Fatalf("restored WXML: %v", err)
	}
	if strings.TrimSpace(string(wxml)) != `<view class="not-found">404</view>` {
		t.Fatalf("WXML content mismatch: %q", wxml)
	}
}

func TestExtractWxapkgKeepsPackageWhenOneWXMLTemplateCannotRender(t *testing.T) {
	frame := `<script>
__wxAppCode__['pages/404/404.wxml'] = $gwx_XC_0('./pages/404/404.wxml');
</script>`
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{
		"app.js":          []byte(`App({})`),
		"page-frame.html": []byte(frame),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx30a3a79e182c606f"); err != nil {
		t.Fatalf("a non-renderable WXML template must not fail extraction: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "app.js")); err != nil {
		t.Fatalf("raw package files must remain available: %v", err)
	}
}

func TestExtractWxapkgRestoresWXSSFromWebviewChunk(t *testing.T) {
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{
		"chunk_0.webview.js": []byte(`setCssToHead([".chunk{",[0,12],"}"],undefined,{path:"./pages/chunk/index.wxss"});`),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx1234567890abcdef"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	wxss, err := os.ReadFile(filepath.Join(outDir, "pages", "chunk", "index.wxss"))
	if err != nil {
		t.Fatalf("restored chunk wxss: %v", err)
	}
	if strings.TrimSpace(string(wxss)) != ".chunk{12rpx}" {
		t.Fatalf("chunk wxss mismatch: %q", wxss)
	}
}

func TestExtractWxapkgRestoresWXSSWithWarningArgument(t *testing.T) {
	pkgPath := filepath.Join(t.TempDir(), "0.wxapkg")
	if err := os.WriteFile(pkgPath, buildWxapkg(map[string][]byte{
		"chunk_0.webview.js": []byte(`setCssToHead([".chunk{",[0,12],"}"],"warning text",{path:"./pages/chunk/index.wxss"});`),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	if _, err := ExtractWxapkg(pkgPath, outDir, "wx1234567890abcdef"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	wxss, err := os.ReadFile(filepath.Join(outDir, "pages", "chunk", "index.wxss"))
	if err != nil {
		t.Fatalf("restored chunk wxss: %v", err)
	}
	if strings.TrimSpace(string(wxss)) != ".chunk{12rpx}" {
		t.Fatalf("chunk wxss mismatch: %q", wxss)
	}
}

func TestAnalyzeFindsOSSAfterLongLine(t *testing.T) {
	oss := "bucket.oss-cn-beijing.aliyuncs.com"
	content := `var config={prefix:"` + strings.Repeat("a", 5000) + `",bucket:"` + oss + `"};`
	analysis := Analyze(content)
	if !containsString(analysis["oss"], oss) {
		t.Fatalf("OSS domain after a long line was missed: %#v", analysis["oss"])
	}
}

// TestDecompilePackagesReplacesPreviousOutput pins the re-decompile contract:
// the output tree must be replaced wholesale, otherwise the code browser keeps
// showing files from the previous decompile.
func TestDecompilePackagesReplacesPreviousOutput(t *testing.T) {
	const appID = "wx1234567890abcdef"
	outDir := filepath.Join(t.TempDir(), appID)

	write := func(name string, files map[string][]byte) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, buildWxapkg(files), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	first := write("first.wxapkg", map[string][]byte{"pages/old.js": []byte("var oldVersion = 1;")})
	if _, err := DecompilePackages([]string{first}, outDir, appID); err != nil {
		t.Fatalf("first decompile: %v", err)
	}
	stale := filepath.Join(outDir, "pages", "old.js")
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("first decompile output missing: %v", err)
	}

	second := write("second.wxapkg", map[string][]byte{"pages/new.js": []byte("var newVersion = 2;")})
	if _, err := DecompilePackages([]string{second}, outDir, appID); err != nil {
		t.Fatalf("second decompile: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file survived re-decompile: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "pages", "new.js")); err != nil {
		t.Fatalf("second decompile output missing: %v", err)
	}
}
