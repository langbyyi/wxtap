package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// 审计面契约测试：与 traffic_contract_test.go 同一方法论——契约的唯一权威是
// contracts/audit.ts 的 `export type` 块，这里用真实 router.Call（临时 SQLite
// 仓库 + httptest 假后端）构造实际 payload，逐字段比对。前端按契约编译，IPC
// 载荷漂移会在这里、而不是在用户屏幕上现形。

// auditContractFields parses one `export type X = {...}` block out of
// contracts/audit.ts into {field: optional}; the generalized reader
// (contractFieldsIn) lives beside the traffic contract test.
func auditContractFields(t *testing.T, name string) map[string]bool {
	t.Helper()
	return contractFieldsIn(t, "audit.ts", name)
}

// seedAuditRecords stores two replayable wx.request records whose payloads
// exercise every field: auth headers (no_auth variants), identity params in
// query and body with two distinct values (id_swap variants).
func seedAuditRecords(t *testing.T, app *App, baseURL string) []traffic.Record {
	t.Helper()
	hookPayload := func(url, uid, order string) []byte {
		body := map[string]any{
			"url":    url,
			"method": "POST",
			"data":   `{"uid":"` + uid + `","orderid":"` + order + `"}`,
			"header": map[string]string{
				"Authorization": "Bearer seed-token",
				"Content-Type":  "application/json",
			},
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal hook payload: %v", err)
		}
		return encoded
	}
	base := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	records := []traffic.Record{
		{
			ID: "wx1-wxapp11111111-1790000000000-1", Seq: 1, CapturedAt: base,
			APIType: "wx.request", Name: "request", AppID: "wxapp11111111",
			Method: "POST", URL: baseURL + "/api/user?uid=1001", Status: traffic.StatusSuccess,
			RequestBody: hookPayload(baseURL+"/api/user?uid=1001", "1001", "A1001"),
		},
		{
			ID: "wx1-wxapp11111111-1790000000001-2", Seq: 2, CapturedAt: base.Add(time.Second),
			APIType: "wx.request", Name: "request", AppID: "wxapp11111111",
			Method: "POST", URL: baseURL + "/api/user?uid=1002", Status: traffic.StatusSuccess,
			RequestBody: hookPayload(baseURL+"/api/user?uid=1002", "1002", "A1002"),
		},
	}
	if _, err := app.repo.InsertBatch(context.Background(), records); err != nil {
		t.Fatalf("seed records: %v", err)
	}
	return records
}

// ③ assets.scan 全链路 + assets.list 的页字段、host 汇总与 item 字段集。
func TestAssetsListContract(t *testing.T) {
	app := newAuditApp(t)
	seedAuditRecords(t, app, "https://api.example.com")

	none := callJSON(t, app, "assets.list", map[string]any{})
	if none["ok"] != true || none["total"].(float64) != 0 {
		t.Fatalf("未构建时 assets.list 必须回答空页而不是报错: %#v", none)
	}

	accepted := callJSON(t, app, "assets.scan", map[string]any{})
	if accepted["ok"] != true || accepted["async"] != true {
		t.Fatalf("assets.scan 受理 = %#v", accepted)
	}
	var page map[string]any
	waitFor(t, settleWaitBudget, "assets.list total>0", func() bool {
		page = callJSON(t, app, "assets.list", map[string]any{})
		return page["total"].(float64) > 0
	})

	assertFieldsWithin(t, page, auditContractFields(t, "AssetsPage"), "assets.list")
	hosts, ok := page["hosts"].([]any)
	if !ok || len(hosts) == 0 {
		t.Fatalf("hosts 汇总缺失: %#v", page)
	}
	assertExactFields(t, hosts[0].(map[string]any), auditContractFields(t, "AssetHostStat"), "assets.list host")
	items, ok := page["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("items 缺失: %#v", page)
	}
	itemFields := auditContractFields(t, "AssetItem")
	sourceFields := auditContractFields(t, "AssetSource")
	for _, raw := range items {
		item := raw.(map[string]any)
		assertFieldsWithin(t, item, itemFields, "assets.list item")
		sources, ok := item["sources"].([]any)
		if !ok || len(sources) == 0 {
			t.Fatalf("asset 缺 sources: %#v", item)
		}
		for _, rawSource := range sources {
			assertExactFields(t, rawSource.(map[string]any), sourceFields, "assets.list item.sources[]")
		}
	}

	// 无来源与非法格式的拒绝形状。
	refused := callJSON(t, app, "assets.scan", map[string]any{"includeTraffic": false})
	if refused["ok"] != false || refused["error"] != "没有可用来源" {
		t.Fatalf("无来源受理 = %#v", refused)
	}
	badFormat := callJSON(t, app, "assets.export", map[string]any{"format": "nessus"})
	if badFormat["ok"] != false || badFormat["error"] == "" {
		t.Fatalf("非法格式 = %#v", badFormat)
	}
	inline := callJSON(t, app, "assets.export", map[string]any{"format": "nuclei"})
	if inline["ok"] != true {
		t.Fatalf("内联导出 = %#v", inline)
	}
	if _, has := inline["path"]; has {
		t.Fatalf("内联导出不得带 path: %#v", inline)
	}
	if !strings.Contains(inline["content"].(string), "https://api.example.com") {
		t.Fatalf("nuclei 导出应含流量里的端点: %#v", inline["content"])
	}
}

// newAuditApp is newTrafficContractApp's audit twin: a real temp store wired
// to the router, so the assertions see exactly what the webview receives.
func newAuditApp(t *testing.T) *App {
	t.Helper()
	base := t.TempDir()
	t.Setenv("WXTAP_DATA_DIR", base)
	t.Setenv("WXTAP_TRAFFIC_DB", filepath.Join(t.TempDir(), "traffic.db"))
	return newTrafficContractApp(t)
}
