package ipc

import (
	"context"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

func TestIsUserHash(t *testing.T) {
	for _, ok := range []string{"2ff6ef464aec84bdf6c3f39f0506ee68", "e11be83df7db556b157fbd25970002c8"} {
		if !isUserHash(ok) {
			t.Errorf("isUserHash(%q) = false, want true", ok)
		}
	}
	for _, no := range []string{"", "applet", "e11be83df7db556b157fbd25970002c", "E11BE83DF7DB556B157FBD25970002C8"} {
		if isUserHash(no) {
			t.Errorf("isUserHash(%q) = true, want false", no)
		}
	}
}

func TestScanUserAppIDs(t *testing.T) {
	dir := t.TempDir()
	appletLocal := filepath.Join(dir, "applet", "local", "wx1234567890abcdef")
	os.MkdirAll(appletLocal, 0o755)
	os.MkdirAll(filepath.Join(dir, "applet", "local", "not-an-appid"), 0o755)
	ids := scanUserAppIDs(dir)
	if len(ids) != 1 || ids[0] != "wx1234567890abcdef" {
		t.Fatalf("scanUserAppIDs = %v, want [wx1234567890abcdef]", ids)
	}
}

func TestUserPackagesDir(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, "2ff6ef464aec84bdf6c3f39f0506ee68")
	if got := userPackagesDir(userDir); got != "" {
		t.Fatalf("userPackagesDir without packages = %q, want empty", got)
	}
	want := filepath.Join(userDir, "Applet", "packages")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := userPackagesDir(userDir); got != want {
		t.Fatalf("userPackagesDir = %q, want %q", got, want)
	}
	users := DetectedUsers(base)
	if len(users) != 1 {
		t.Fatalf("DetectedUsers = %d users, want 1", len(users))
	}
	if users[0].PackagesDir != want {
		t.Fatalf("DetectedUsers PackagesDir = %q, want %q", users[0].PackagesDir, want)
	}
}

func TestScanUserMMKVSessionKey(t *testing.T) {
	dir := t.TempDir()
	appID := "wx1234567890abcdef"
	// Build a minimal MMKV file with one session_key entry.
	key := []byte("session_key")
	val := []byte("tiihtNczf5v6AKRyjwEUhQ==")
	var buf []byte
	buf = appendLE32(buf, uint32(len(key)))
	buf = append(buf, key...)
	buf = appendLE32(buf, uint32(len(val)))
	buf = append(buf, val...)

	mmkvDir := filepath.Join(dir, "applet", "local", appID, "usrmmkvstorage0")
	os.MkdirAll(mmkvDir, 0o755)
	os.WriteFile(filepath.Join(mmkvDir, appID), buf, 0o644)

	findings := ScanUser(dir)
	if len(findings) == 0 {
		t.Fatal("ScanUser returned 0 findings, want >=1")
	}
	f := findings[0]
	if f.AppID != appID {
		t.Errorf("AppID = %q, want %q", f.AppID, appID)
	}
	if f.Value != "tiihtNczf5v6AKRyjwEUhQ==" {
		t.Errorf("Value = %q", f.Value)
	}
	if f.Kind != "mmkv" {
		t.Errorf("Kind = %q, want mmkv", f.Kind)
	}
	// Decoded must be 16 bytes.
	decoded, err := base64.StdEncoding.DecodeString(f.Value)
	if err != nil || len(decoded) != 16 {
		t.Errorf("decoded %q: %d bytes, err=%v", f.Value, len(decoded), err)
	}
}

func TestScanUserJSONSessionKey(t *testing.T) {
	dir := t.TempDir()
	appID := "wx99abcdef01234567"
	logDir := filepath.Join(dir, "applet", "local", appID, "usr", "miniprogramLog")
	os.MkdirAll(logDir, 0o755)
	os.WriteFile(filepath.Join(logDir, "log1"), []byte(
		`{"openId":"oABC","session_key":"abc22defghi22jklmnopqr==","unionId":"uXY"}`,
	), 0o644)

	findings := ScanUser(dir)
	found := false
	for _, f := range findings {
		if f.Kind == "json" && f.AppID == appID {
			found = true
			if f.Value != "abc22defghi22jklmnopqr==" {
				t.Errorf("Value = %q", f.Value)
			}
		}
	}
	if !found {
		t.Fatalf("no json finding found; got %+v", findings)
	}
}

func TestScanUserNoFalsePositives(t *testing.T) {
	dir := t.TempDir()
	// Write a random 32-byte file with no session_key.
	os.WriteFile(filepath.Join(dir, "random"), []byte("hello world, no session keys here"), 0o644)
	findings := ScanUser(dir)
	// The raw b64 scan may pick up random 24-char base64 that decodes to 16 bytes
	// but the above string is too short and has spaces so should yield 0.
	if len(findings) > 0 {
		for _, f := range findings {
			if !strings.Contains(f.Context, "session") && f.Kind == "mmkv" {
				t.Errorf("unexpected finding: %+v", f)
			}
		}
	}
}

func TestDedupFindings(t *testing.T) {
	input := []SessionKeyFinding{
		{Source: "a", Value: "x"},
		{Source: "a", Value: "x"},
		{Source: "b", Value: "x"},
		{Source: "b", Value: "y"},
	}
	got := dedupFindings(input)
	if len(got) != 3 {
		t.Fatalf("dedupFindings got %d, want 3", len(got))
	}
}

func TestDefaultUsersDirNotEmpty(t *testing.T) {
	dir := DefaultUsersDir()
	if dir == "" {
		t.Skip("DefaultUsersDir returned empty; skip on this platform")
	}
	t.Logf("DefaultUsersDir: %s", dir)
}

func appendLE32(buf []byte, v uint32) []byte {
	var b [4]byte
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	return append(buf, b[:]...)
}

var _ = reflect.TypeOf

func TestScanDecompiledFindsCryptoFields(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "wx1234567890abcdef", "pages", "login")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `{"session_key":"tiihtNczf5v6AKRyjwEUhQ==","iv":"Xsdni3/wBgoPUlvmCMljyA==","encryptedData":"kOM74Dk6JNXG6Dc7dwyrpmdalmoEyVhCqNGPmQf2n1yQL/z6bwHQ81eUtWBppkjvA4ZfXyqUgmqX+uyT5InF1w6TPgwfcgEOy/vDMCk3koVTzcZVhfbnHCRu7EcWby30dgZUdRyqTTnf/3X3H5/esmHsFvKdwblZajwJz/TqJg4="}`
	if err := os.WriteFile(filepath.Join(appDir, "login.js"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	findings := ScanDecompiled(root)
	kinds := map[string]bool{}
	for _, finding := range findings {
		if finding.AppID != "wx1234567890abcdef" {
			t.Fatalf("appid = %q, want wx1234567890abcdef", finding.AppID)
		}
		kinds[finding.Kind] = true
	}
	for _, kind := range []string{"session_key", "iv", "encryptedData"} {
		if !kinds[kind] {
			t.Fatalf("missing %s finding: %+v", kind, findings)
		}
	}
}

func TestIsUsersDir(t *testing.T) {
	base := DefaultUsersDir()
	if base == "" {
		t.Skip("no platform users dir candidate")
	}
	if !IsUsersDir(base) {
		t.Fatalf("base %q should be valid", base)
	}
	if !IsUsersDir(filepath.Join(base, "0123456789abcdef0123456789abcdef")) {
		t.Fatalf("child of %q should be valid", base)
	}
	if IsUsersDir(filepath.Join(filepath.Dir(base), "outside")) {
		t.Fatal("sibling of the users root must be rejected")
	}
	if IsUsersDir("../outside") {
		t.Fatal("relative traversal must be rejected")
	}
}
func TestScanTrafficRecordsFindsFieldsInPackets(t *testing.T) {
	captured := time.Unix(1700000000, 0).UTC()
	payload := `{"errcode":0,"result":{"session_key":"tiihtNczf5v6AKRyjwEUhQ==","iv":"Xsdni3/wBgoPUlvmCMljyA==","encryptedData":"kOM74Dk6JNXG6Dc7dwyrpmdalmoEyVhCqNGPmQf2n1yQL/z6bwHQ81eUtWBppkjvA4ZfXyqUgmqX+uyT5InF1w6TPgwfcgEOy/vDMCk3koVTzcZVhfbnHCRu7EcWby30dgZUdRyqTTnf/3X3H5/esmHsFvKdwblZajwJz/TqJg4="}}`
	encoded := url.QueryEscape(`{"session_key":"tiihtNczf5v6AKRyjwEUhQ=="}`)

	records := []traffic.Record{
		{
			ID: "traffic-1", Seq: 42, APIType: "wx.request", Method: "POST",
			URL: "https://api.example.com/login", AppID: "wx1234567890abcdef",
			CapturedAt: captured, ResponseBody: []byte(payload),
		},
		{
			ID: "traffic-2", Seq: 43, APIType: "wx.request", Method: "POST",
			URL: "https://api.example.com/phone", AppID: "wx1234567890abcdef",
			CapturedAt: captured, RequestBody: []byte(encoded),
		},
	}

	findings := ScanTrafficRecords(context.Background(), records)
	kinds := map[string]int{}
	for _, finding := range findings {
		if finding.AppID != "wx1234567890abcdef" {
			t.Fatalf("appid = %q, want wx1234567890abcdef", finding.AppID)
		}
		if finding.Masked == finding.Value {
			t.Fatalf("value must be masked for display: %+v", finding)
		}
		if !strings.HasPrefix(finding.Source, "traffic#") {
			t.Fatalf("source = %q, want traffic# prefix", finding.Source)
		}
		if finding.FoundAt == "" {
			t.Fatalf("foundAt must carry the capture time: %+v", finding)
		}
		kinds[finding.Kind]++
	}
	for _, kind := range []string{"session_key", "iv", "encryptedData"} {
		if kinds[kind] == 0 {
			t.Fatalf("missing %s finding: %+v", kind, findings)
		}
	}
	// The URL-encoded packet is decoded before matching, so session_key shows
	// up twice: once from the response and once from the form-encoded body.
	if kinds["session_key"] < 2 {
		t.Fatalf("url-encoded body was not decoded: %+v", findings)
	}
}

func TestScanTrafficRecordsFindsQueryAndFormFieldValues(t *testing.T) {
	captured := time.Unix(1700000000, 0).UTC()
	key := "tiihtNczf5v6AKRyjwEUhQ=="
	iv := "Xsdni3/wBgoPUlvmCMljyA=="
	encrypted := "kOM74Dk6JNXG6Dc7dwyrpmdalmoEyVhCqNGPmQf2n1yQL/z6bwHQ81eUtWBppkjvA4ZfXyqUgmqX+uyT5InF1w6TPgwfcgEOy/vDMCk3koVTzcZVhfbnHCRu7EcWby30dgZUdRyqTTnf/3X3H5/esmHsFvKdwblZajwJz/TqJg4="
	records := []traffic.Record{
		{
			ID: "traffic-query", Seq: 1, CapturedAt: captured,
			APIType: "wx.request", Method: "GET", AppID: "wx1234567890abcdef",
			URL: "https://api.example.com/session?session_key=" + key + "&iv=" + iv,
		},
		{
			ID: "traffic-form", Seq: 2, CapturedAt: captured,
			APIType: "wx.request", Method: "POST", AppID: "wx1234567890abcdef",
			URL:         "https://api.example.com/phone",
			RequestBody: []byte("session_key=" + key + "&iv=" + iv + "&encryptedData=" + encrypted),
		},
	}

	findings := ScanTrafficRecords(context.Background(), records)
	kinds := map[string]int{}
	for _, finding := range findings {
		if finding.AppID != "wx1234567890abcdef" {
			t.Fatalf("appid = %q, want wx1234567890abcdef", finding.AppID)
		}
		kinds[finding.Kind]++
	}
	for _, kind := range []string{"session_key", "iv", "encryptedData"} {
		if kinds[kind] == 0 {
			t.Fatalf("missing %s in query/form payload: %+v", kind, findings)
		}
	}
}

func TestScanTrafficRecordsIgnoresCleanPackets(t *testing.T) {
	records := []traffic.Record{{
		ID: "traffic-1", Seq: 1, APIType: "wx.request", Method: "GET",
		URL: "https://api.example.com/health", AppID: "wx1234567890abcdef",
		RequestBody:  []byte(`{"page":1}`),
		ResponseBody: []byte(`{"ok":true}`),
	}}
	if findings := ScanTrafficRecords(context.Background(), records); len(findings) != 0 {
		t.Fatalf("unexpected findings: %+v", findings)
	}
}

func TestUsersDirOverrideIsAccepted(t *testing.T) {
	base := t.TempDir()
	t.Setenv("WXTAP_USERS_DIR", base)
	if DefaultUsersDir() != base {
		t.Fatalf("DefaultUsersDir = %q, want override %q", DefaultUsersDir(), base)
	}
	if !IsUsersDir(base) || !IsUsersDir(filepath.Join(base, "user")) {
		t.Fatal("override root and child should be accepted")
	}
	if IsUsersDir(filepath.Join(filepath.Dir(base), "outside")) {
		t.Fatal("sibling of override root must be rejected")
	}
}
