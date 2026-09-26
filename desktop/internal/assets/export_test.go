package assets

import (
	"encoding/csv"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// buildFixtureInventory produces an inventory with one asset of each shape:
//
//   - api  *   https://api.example.com/v1/login      (code + traffic GET, hits 2, 2 sources)
//   - api  GET https://api.example.com/v1/token      (traffic only)
//   - api  POST https://api.example.com/v1/token     (traffic only, same URL as above)
//   - api  POST https://api.example.com/v2/upload    (traffic only)
//   - static * https://cdn.example.com/logo.png     (code only)
//   - ws   *   wss://ws.example.com/conn            (code only)
//   - cloud *  cloudfunction://login                (cloud)
func buildFixtureInventory(t *testing.T) *Inventory {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "app.js"), `
login "https://api.example.com/v1/login";
logo "https://cdn.example.com/logo.png";
conn "wss://ws.example.com/conn";
`)
	t1 := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)
	records := []traffic.Record{
		{ID: "r1", CapturedAt: t1, Method: "GET", URL: "https://api.example.com/v1/login"},
		{ID: "r2", CapturedAt: t1, Method: "POST", URL: "https://api.example.com/v2/upload"},
		{ID: "r3", CapturedAt: t1, Method: "GET", URL: "https://api.example.com/v1/token"},
		{ID: "r4", CapturedAt: t2, Method: "POST", URL: "https://api.example.com/v1/token"},
	}
	inv, err := Build(dir, records, []string{"login"})
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	if inv.Total != 7 {
		t.Fatalf("fixture total = %d, want 7: %+v", inv.Total, inv.Items)
	}
	return inv
}

func TestExportNuclei(t *testing.T) {
	inv := buildFixtureInventory(t)
	got, err := inv.Export("nuclei")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	want := strings.Join([]string{
		"https://api.example.com/v1/login",
		"https://api.example.com/v1/token", // once, although GET and POST assets share it
		"https://api.example.com/v2/upload",
		"wss://ws.example.com/conn",
	}, "\n")
	if got != want {
		t.Fatalf("nuclei =\n%s\nwant\n%s", got, want)
	}
}

func TestExportHTTPX(t *testing.T) {
	inv := buildFixtureInventory(t)
	got, err := inv.Export("httpx")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	want := "https://api.example.com\nhttps://cdn.example.com"
	if got != want {
		t.Fatalf("httpx =\n%s\nwant\n%s", got, want)
	}
}

func TestExportTXT(t *testing.T) {
	inv := buildFixtureInventory(t)
	got, err := inv.Export("txt")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	want := strings.Join([]string{
		"https://api.example.com/v1/login",
		"https://api.example.com/v1/token",
		"https://api.example.com/v2/upload",
		"https://cdn.example.com/logo.png",
		"cloudfunction://login",
		"wss://ws.example.com/conn",
	}, "\n")
	if got != want {
		t.Fatalf("txt =\n%s\nwant\n%s", got, want)
	}
}

func TestExportCSV(t *testing.T) {
	inv := buildFixtureInventory(t)
	got, err := inv.Export("csv")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	wantRows := [][]string{
		{"kind", "method", "host", "path", "url", "hits", "source_count"},
		{"api", "*", "api.example.com", "/v1/login", "https://api.example.com/v1/login", "2", "2"},
		{"api", "GET", "api.example.com", "/v1/token", "https://api.example.com/v1/token", "1", "1"},
		{"api", "POST", "api.example.com", "/v1/token", "https://api.example.com/v1/token", "1", "1"},
		{"api", "POST", "api.example.com", "/v2/upload", "https://api.example.com/v2/upload", "1", "1"},
		{"static", "*", "cdn.example.com", "/logo.png", "https://cdn.example.com/logo.png", "1", "1"},
		{"cloud", "*", "cloud", "login", "cloudfunction://login", "1", "1"},
		{"ws", "*", "ws.example.com", "/conn", "wss://ws.example.com/conn", "1", "1"},
	}
	rows, err := csv.NewReader(strings.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("csv roundtrip: %v", err)
	}
	if !reflect.DeepEqual(rows, wantRows) {
		t.Fatalf("csv rows =\n%#v\nwant\n%#v", rows, wantRows)
	}
}

func TestExportCSVEscapesRFC4180(t *testing.T) {
	inv, err := Build("", nil, []string{`he said "hi", ok`, "multi\nline"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, err := inv.Export("csv")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	want := "kind,method,host,path,url,hits,source_count\n" +
		`cloud,*,cloud,"he said ""hi"", ok","cloudfunction://he said ""hi"", ok",1,1` + "\n" +
		"cloud,*,cloud,\"multi\nline\",\"cloudfunction://multi\nline\",1,1\n"
	if got != want {
		t.Fatalf("csv =\n%#v\nwant\n%#v", got, want)
	}
	rows, err := csv.NewReader(strings.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("csv roundtrip: %v", err)
	}
	if len(rows) != 3 || rows[1][3] != `he said "hi", ok` || rows[2][4] != "cloudfunction://multi\nline" {
		t.Fatalf("csv roundtrip mismatch: %#v", rows)
	}
}

func TestExportJSON(t *testing.T) {
	inv := buildFixtureInventory(t)
	raw, err := inv.Export("json")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if strings.Contains(raw, "\n") {
		t.Fatal("json export must be compact")
	}
	var back Inventory
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(inv, &back) {
		t.Fatal("json roundtrip mismatch")
	}
}

func TestExportRejectsUnknownFormat(t *testing.T) {
	inv := buildFixtureInventory(t)
	if _, err := inv.Export("yaml"); err == nil || !strings.Contains(err.Error(), "不支持的导出格式") {
		t.Fatalf("err = %v, want 不支持的导出格式", err)
	}
}
