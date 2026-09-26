package assets

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

func TestBuildMergesCodeAndTraffic(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "app.js"), `fetch("https://api.example.com/v1/user")`)
	t1 := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)
	records := []traffic.Record{
		// r2 differs only in host case and query value; both must fold into
		// the same asset as the code hit.
		{ID: "r1", CapturedAt: t1, Method: "GET", URL: "https://api.example.com/v1/user?id=1"},
		{ID: "r2", CapturedAt: t2, Method: "GET", URL: "https://API.example.com/v1/user?id=2"},
	}

	inv, err := Build(dir, records, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if inv.Total != 1 {
		t.Fatalf("total = %d, want 1: %+v", inv.Total, inv.Items)
	}

	got := inv.Items[0]
	if got.Method != "*" || got.Kind != KindAPI || got.Hits != 3 {
		t.Fatalf("asset: %+v", got)
	}
	wantSources := []Source{
		{Type: "code", Ref: "app.js"},
		{Type: "traffic", Ref: "r1"},
		{Type: "traffic", Ref: "r2"},
	}
	if !reflect.DeepEqual(got.Sources, wantSources) {
		t.Fatalf("sources = %+v", got.Sources)
	}
	if !got.FirstSeen.Equal(t1) || !got.LastSeen.Equal(t2) {
		t.Fatalf("window = %v..%v, want %v..%v (code carries no time)", got.FirstSeen, got.LastSeen, t1, t2)
	}
	if !reflect.DeepEqual(got.Tags, []string{"q:id"}) {
		t.Fatalf("tags = %+v", got.Tags)
	}
}

func TestBuildWithTrafficOnly(t *testing.T) {
	records := []traffic.Record{
		{ID: "r1", CapturedAt: time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC), Method: "GET", URL: "https://api.example.com/v1/x"},
	}
	inv, err := Build("", records, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if inv.Total != 1 {
		t.Fatalf("total = %d, want 1", inv.Total)
	}
}

func TestBuildCloudFunctions(t *testing.T) {
	inv, err := Build("", nil, []string{"login", "getUser", "  ", "login"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if inv.Total != 2 || inv.ByKind[KindCloud] != 2 {
		t.Fatalf("inventory: total=%d byKind=%+v", inv.Total, inv.ByKind)
	}
	login := findAsset(t, inv.Items, "cloudfunction://login")
	if login.Kind != KindCloud || login.Host != "cloud" || login.Path != "login" || login.Method != "*" {
		t.Fatalf("login asset: %+v", login)
	}
	if login.Hits != 2 || !reflect.DeepEqual(login.Sources, []Source{{Type: "code", Ref: "cloud"}}) {
		t.Fatalf("login hits = %d, sources = %+v", login.Hits, login.Sources)
	}
	if got := findAsset(t, inv.Items, "cloudfunction://getUser"); got.Hits != 1 {
		t.Fatalf("getUser asset: %+v", got)
	}
}

func TestBuildWithoutSourcesFails(t *testing.T) {
	if _, err := Build("", nil, nil); err == nil || !strings.Contains(err.Error(), "没有可用来源") {
		t.Fatalf("err = %v, want 没有可用来源", err)
	}
	if _, err := Build("", nil, []string{" ", ""}); err == nil || !strings.Contains(err.Error(), "没有可用来源") {
		t.Fatalf("err = %v, want 没有可用来源 when every cloud name is blank", err)
	}
}

func TestBuildSortsAndAggregates(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "app.js"), `
a "https://bbb.example.com/a";
b "https://aaa.example.com/z";
c "https://aaa.example.com/m";
d "wss://aaa.example.com/ws";
e "https://cdn.example.com/x.png";
`)

	inv, err := Build(dir, nil, []string{"login"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	wantOrder := []string{
		"https://aaa.example.com/m",
		"wss://aaa.example.com/ws",
		"https://aaa.example.com/z",
		"https://bbb.example.com/a",
		"https://cdn.example.com/x.png",
		"cloudfunction://login",
	}
	if len(inv.Items) != len(wantOrder) {
		t.Fatalf("items = %d, want %d: %+v", len(inv.Items), len(wantOrder), inv.Items)
	}
	for i, u := range wantOrder {
		if inv.Items[i].URL != u {
			t.Fatalf("items[%d] = %s, want %s", i, inv.Items[i].URL, u)
		}
	}

	wantByKind := map[string]int{KindAPI: 3, KindWS: 1, KindStatic: 1, KindCloud: 1}
	if !reflect.DeepEqual(inv.ByKind, wantByKind) {
		t.Fatalf("byKind = %+v, want %+v", inv.ByKind, wantByKind)
	}
	wantHosts := []HostStat{
		{Host: "aaa.example.com", Count: 3},
		{Host: "bbb.example.com", Count: 1},
		{Host: "cdn.example.com", Count: 1},
		{Host: "cloud", Count: 1},
	}
	if !reflect.DeepEqual(inv.Hosts, wantHosts) {
		t.Fatalf("hosts = %+v, want %+v", inv.Hosts, wantHosts)
	}
	if inv.Total != 6 {
		t.Fatalf("total = %d, want 6", inv.Total)
	}
}

func TestBuildCapsSourcesAtEight(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "app.js"), `fetch("https://api.example.com/v1/heavy")`)
	records := make([]traffic.Record, 0, 10)
	for i := 0; i < 10; i++ {
		records = append(records, traffic.Record{
			ID:     fmt.Sprintf("r%02d", i+1),
			Method: "GET",
			URL:    "https://api.example.com/v1/heavy",
		})
	}

	inv, err := Build(dir, records, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got := findAsset(t, inv.Items, "https://api.example.com/v1/heavy")
	if got.Hits != 11 {
		t.Fatalf("hits = %d, want 11", got.Hits)
	}
	if len(got.Sources) != 9 {
		t.Fatalf("sources = %d entries, want 9 (8 kept + more marker): %+v", len(got.Sources), got.Sources)
	}
	if got.Sources[0] != (Source{Type: "code", Ref: "app.js"}) {
		t.Fatalf("first source = %+v", got.Sources[0])
	}
	for i, s := range got.Sources[1:8] {
		if want := (Source{Type: "traffic", Ref: fmt.Sprintf("r%02d", i+1)}); s != want {
			t.Fatalf("sources[%d] = %+v, want %+v", i+1, s, want)
		}
	}
	if got.Sources[8] != (Source{Type: "more", Ref: "+3"}) {
		t.Fatalf("last source = %+v, want more +3", got.Sources[8])
	}
}

func TestAssetIDIsFnv1a64Hex(t *testing.T) {
	ref := func(s string) string {
		h := uint64(14695981039346656037)
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= 1099511628211
		}
		return fmt.Sprintf("%016x", h)
	}
	got := assetID("api", "*", "https", "api.example.com", "/v1/login")
	if want := ref("api|*|https|api.example.com|/v1/login"); got != want {
		t.Fatalf("assetID = %s, want %s", got, want)
	}
	if again := assetID("api", "*", "https", "api.example.com", "/v1/login"); again != got {
		t.Fatal("assetID is not deterministic")
	}
	if assetID("api", "GET", "https", "h", "/p") == assetID("api", "POST", "https", "h", "/p") {
		t.Fatal("different methods must produce different IDs")
	}
}
