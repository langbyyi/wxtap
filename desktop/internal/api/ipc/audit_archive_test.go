package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// appidAuditRecord builds one traffic record for the given mini program; the
// URL's host distinguishes the programs in the built inventory.
func appidAuditRecord(id, appid, rawURL string, seq int64, at time.Time) traffic.Record {
	return traffic.Record{
		ID: id, Seq: seq, CapturedAt: at,
		APIType: "wxapi", Name: "request",
		AppID: appid, Method: "GET", URL: rawURL, Status: traffic.StatusSuccess,
	}
}

// includeTrafficPtr is the AssetsScanParams.IncludeTraffic helper (JSON true).
func includeTrafficPtr() *bool {
	on := true
	return &on
}

// 清单是单个小程序的档案：appid 把流量收窄到该程序，构建结果带目标与构建
// 时间，并存档；新 service（模拟重启）从存档恢复同一份清单。
func TestAssetsScanScopesByAppIDAndArchives(t *testing.T) {
	repo := openAuditFixture(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	records := []traffic.Record{
		appidAuditRecord("wxapi-a-1", "wx-app-a", "https://a.example.com/api/users", 1, base),
		appidAuditRecord("wxapi-b-1", "wx-app-b", "https://b.example.com/api/orders", 2, base),
	}
	if _, err := repo.InsertBatch(ctx, records); err != nil {
		t.Fatalf("insert batch: %v", err)
	}

	dataDir := t.TempDir()
	config := NewConfigStore(dataDir)
	svc := NewAuditService(repo, nil, config, nil, nil, nil)

	svc.runAssetsScan(ctx, AssetsScanParams{AppID: "wx-app-a", IncludeTraffic: includeTrafficPtr()}, "")

	list := svc.AssetsList(AssetsListParams{})
	if list["appid"] != "wx-app-a" {
		t.Fatalf("list appid = %v, want wx-app-a", list["appid"])
	}
	if _, ok := list["builtAt"].(string); !ok || list["builtAt"] == "" {
		t.Fatalf("list builtAt missing: %v", list["builtAt"])
	}
	raw, _ := json.Marshal(list["items"])
	if !bytes.Contains(raw, []byte("a.example.com")) || bytes.Contains(raw, []byte("b.example.com")) {
		t.Fatalf("scoped build leaked other program: %s", raw)
	}

	// 存档落盘 + 目标写进 config。
	if _, err := os.Stat(filepath.Join(dataDir, "assets", "wx-app-a.json")); err != nil {
		t.Fatalf("archive not written: %v", err)
	}
	if config.lastAssetTargetAppID() != "wx-app-a" {
		t.Fatalf("config target = %q, want wx-app-a", config.lastAssetTargetAppID())
	}

	// 重启（新 service，同一个 config/存档）：清单自动恢复，仍是该程序口径。
	restored := NewAuditService(repo, nil, config, nil, nil, nil)
	relist := restored.AssetsList(AssetsListParams{})
	if relist["appid"] != "wx-app-a" {
		t.Fatalf("restored appid = %v, want wx-app-a", relist["appid"])
	}
	if relist["total"] != list["total"] {
		t.Fatalf("restored total = %v, want %v", relist["total"], list["total"])
	}
}

// 目录不再手填：一个 appid 的产物目录固定在 output/<appid>（与设置页的
// 反编译输出目录同源）。缺省 dir 的构建按 appid 自动解析；未反编译则解析为
// 空，构建退回流量等其余来源。
func TestAssetsScanResolvesDecompiledDirFromAppID(t *testing.T) {
	repo := openAuditFixture(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	if _, err := repo.InsertBatch(ctx, []traffic.Record{
		appidAuditRecord("wxapi-a-1", "wx-app-a", "https://a.example.com/api/users", 1, base),
	}); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	dataDir := t.TempDir()
	config := NewConfigStore(dataDir)
	svc := NewAuditService(repo, nil, config, nil, nil, nil)

	if got := svc.decompiledDir("wx-app-a"); got != "" {
		t.Fatalf("decompiledDir without output = %q, want empty", got)
	}
	outDir := filepath.Join(dataDir, "output", "wx-app-a")
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		t.Fatal(err)
	}
	js := `var api = "https://code.example.com/api/from-code";` + "\n"
	if err := os.WriteFile(filepath.Join(outDir, "app-service.js"), []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := svc.decompiledDir("wx-app-a"); got != outDir {
		t.Fatalf("decompiledDir = %q, want %q", got, outDir)
	}

	// 走公开入口：不带 dir 的受理会带上自动解析出的代码来源。
	done := make(chan map[string]any, 4)
	emit := func(name string, payload any) {
		if name == "assets_progress" {
			if frame, ok := payload.(map[string]any); ok {
				done <- frame
			}
		}
	}
	runner := NewAuditService(repo, nil, config, emit, nil, func(fn func()) { go fn() })
	accepted, err := runner.AssetsScan(AssetsScanParams{AppID: "wx-app-a", IncludeTraffic: includeTrafficPtr()})
	if err != nil || accepted["ok"] != true {
		t.Fatalf("scan not accepted: %v %v", accepted, err)
	}
	select {
	case frame := <-done:
		if frame["status"] != "done" {
			t.Fatalf("progress frame = %v, want done", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}
	raw, _ := json.Marshal(runner.AssetsList(AssetsListParams{})["items"])
	if !bytes.Contains(raw, []byte("code.example.com")) || !bytes.Contains(raw, []byte("a.example.com")) {
		t.Fatalf("auto-resolved build missing code or traffic source: %s", raw)
	}
}
