package mcp

// agent 调用体验回归：本轮全量核查发现的四类误导性输出，各自固化为断言 ——
// 零目标时的自相矛盾文案、扫描报告的上下文爆炸、脚本清单被空 url 淹没、
// 会话状态查询在无目标时的报错形态。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 零连接时 session_start 曾回答 "multiple mini programs connected" 而
// available 是空数组——文案把调用方引去找一个不存在的 target:<id>。
func TestSessionStartNoTargetNamesTheRealCause(t *testing.T) {
	bridge := &sessionBridge{empty: true}
	messages := exchange(t, Deps{Core: &fakeCore{}, AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start","arguments":{}}}`+"\n")
	var payload struct {
		OK      bool   `json:"ok"`
		Step    string `json:"step"`
		Error   string `json:"error"`
		Have    []any  `json:"available"`
		Capture any    `json:"capture"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OK || payload.Step != "target" {
		t.Fatalf("应停在 target 步: %s", toolText(t, messages, 1))
	}
	if len(payload.Have) != 0 {
		t.Fatalf("available 应为空: %s", toolText(t, messages, 1))
	}
	if payload.Capture != nil {
		t.Fatalf("没选到目标不得开采集: %s", toolText(t, messages, 1))
	}
	if !strings.Contains(payload.Error, "no miniapp connected") || strings.Contains(payload.Error, "multiple") {
		t.Fatalf("零连接的文案必须指向「未打开小程序」: %q", payload.Error)
	}
}

// 压缩单行 bundle 的原始扫描报告曾把单次回复撑到 600KB+：同一规则在同一
// 文件里命中几十次、每条 snippet 都是一整行压缩代码。整形后必须去重、
// 窗口化、封顶，并以 total/truncated 如实上报被收敛掉的部分。
func TestShapeScanFindingsDedupsWindowsAndCaps(t *testing.T) {
	longLine := strings.Repeat("x", 900) + "AKIA1234" + strings.Repeat("x", 900)
	raw := []any{}
	for i := 0; i < 80; i++ {
		raw = append(raw, map[string]any{
			"rule_id": "secret:cloud_ak", "value": "AKIA1234", "file": "app-service.js",
			// 数值字段与真实报告一致：json.Unmarshal 进 map 后是 float64。
			"column": float64(908), "snippet": longLine,
		})
	}
	for i := 0; i < 70; i++ {
		raw = append(raw, map[string]any{
			"rule_id": "builtin:url", "value": "https://probe.example.net/" + string(rune('a'+i)),
			"file": "lib/sdk.js", "column": 10, "snippet": "short",
		})
	}

	findings, total, truncated := shapeScanFindings(raw)
	if total != 71 {
		t.Fatalf("去重后 total = %d, want 71", total)
	}
	if !truncated {
		t.Fatal("超过封顶必须如实标注 truncated")
	}
	if len(findings) != scanFindingsCap {
		t.Fatalf("findings = %d, want cap %d", len(findings), scanFindingsCap)
	}
	first := findings[0].(map[string]any)
	snippet := first["snippet"].(string)
	if len(snippet) >= len(longLine) {
		t.Fatalf("snippet 应被窗口化: %d bytes", len(snippet))
	}
	if !strings.Contains(snippet, "AKIA1234") || !strings.Contains(snippet, "…") {
		t.Fatalf("snippet 应围绕匹配列并标注截断: %q", snippet)
	}
}

// 空输入与缺字段的报告走安全路径。
func TestShapeScanFindingsHandlesEmptyInput(t *testing.T) {
	findings, total, truncated := shapeScanFindings(nil)
	if len(findings) != 0 || total != 0 || truncated {
		t.Fatalf("nil findings 应得空结果: total=%d truncated=%v findings=%d", total, truncated, len(findings))
	}
	findings, total, truncated = shapeScanFindings([]any{
		map[string]any{"rule_id": "r", "value": "v", "file": "f", "snippet": "short"},
	})
	if total != 1 || truncated || len(findings) != 1 {
		t.Fatalf("单条发现不应标记截断: total=%d truncated=%v findings=%d", total, truncated, len(findings))
	}
}

// listingScriptsCore 的脚本清单模拟真实环形缓冲：绝大多数是 eval 出来的
// 空 url 脚本，命名的脚本只有几个且排在后段。
type listingScriptsCore struct {
	fakeCore
	scripts []any
}

func (c *listingScriptsCore) DebugState(context.Context, bool) (map[string]any, error) {
	return map[string]any{
		"enabled": true, "paused": false, "scripts": c.scripts,
		"scriptsTruncated": float64(0),
	}, nil
}

// 断点与 get_source 只能作用于有 url 的脚本；默认清单若被 500 个空 url 条目
// 占满，命名的脚本等于不可见。
func TestListScriptsPutsNamedUrlsFirst(t *testing.T) {
	scripts := []any{}
	for i := 0; i < 300; i++ {
		scripts = append(scripts, map[string]any{"scriptId": string(rune('a'+i%26)) + string(rune(i)), "url": ""})
	}
	scripts = append(scripts,
		map[string]any{"scriptId": "900", "url": "https://usr/chunk_1.appservice.js"},
		map[string]any{"scriptId": "901", "url": "https://usr/chunk_0.common.js"},
	)
	messages := exchange(t, Deps{Core: &listingScriptsCore{scripts: scripts}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_list_scripts","arguments":{"limit":2}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("list_scripts 不该失败: %s", toolText(t, messages, 1))
	}
	var payload struct {
		Scripts []struct {
			ScriptID string `json:"scriptId"`
			URL      string `json:"url"`
		} `json:"scripts"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Total != 302 {
		t.Fatalf("total = %d, want 302", payload.Total)
	}
	for _, script := range payload.Scripts {
		if script.URL == "" {
			t.Fatalf("limit 窗口内不得混入空 url 条目: %+v", payload.Scripts)
		}
	}
}

// 页面侧返回值没有天然上限：evaluate 拉回整个数据集、storage 里躺着大 blob，
// 原样进 MCP 结果就是一次调用烧掉一屏 token。超限必须截断并如实标注总量。
func TestOversizedResultsAreCappedWithHonestTotals(t *testing.T) {
	small := map[string]any{"ok": true}
	if capped, over := oversizedResult(small, evaluateResultCap); over {
		t.Fatalf("小结果不该被截断: %#v", capped)
	}

	big := map[string]any{"value": strings.Repeat("x", evaluateResultCap+100)}
	capped, over := oversizedResult(big, evaluateResultCap)
	if !over {
		t.Fatal("超限结果必须被截断")
	}
	if capped["truncated"] != true || capped["total_bytes"].(int) <= evaluateResultCap {
		t.Fatalf("截断标注不完整: %#v", capped)
	}
	if !strings.Contains(capped["note"].(string), "截断") {
		t.Fatalf("截断说明缺失: %#v", capped["note"])
	}
}

// ---- 第二轮核查回归：合并名与旧名必须同口径，快照读必须有页宽 ----

// storageKeyCore 让 getStorageSync 返回一个超过 32KB 的值，其余求值
// （执行上下文探针）交回 fakeCore 作答。
type storageKeyCore struct {
	fakeCore
}

func (c *storageKeyCore) CDPCommand(ctx context.Context, method string, params map[string]any, timeoutMs int) (map[string]any, error) {
	if method == "Runtime.evaluate" && strings.Contains(stringArg(params, "expression", ""), "getStorageSync") {
		value := `{"key":"probe","value":"` + strings.Repeat("x", storageResultCap) + `","type":"string"}`
		return map[string]any{"result": map[string]any{"result": map[string]any{"type": "string", "value": value}}}, nil
	}
	return c.fakeCore.CDPCommand(ctx, method, params, timeoutMs)
}

// 旧名 miniapp_get_storage_key 与合并形态 miniapp_get_storage 读同一个
// 存储：合并形态有 32KB 帽，旧名曾是无帽旁路。
func TestLegacyStorageKeyNameIsCappedToo(t *testing.T) {
	messages := exchange(t, Deps{Core: &storageKeyCore{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_get_storage_key","arguments":{"key":"probe"}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("不该失败: %s", toolText(t, messages, 1))
	}
	var payload struct {
		Truncated  bool `json:"truncated"`
		TotalBytes int  `json:"total_bytes"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Truncated || payload.TotalBytes <= storageResultCap {
		t.Fatalf("旧名单键读必须同受 32KB 帽: %+v", payload)
	}
}

// cloudPageCore 的云捕获流返回超过页帽的记录数。
type cloudPageCore struct {
	fakeCore
	count int
}

func (c *cloudPageCore) HookDrain(context.Context, string, int64, int, int64, int) (DrainPage, error) {
	records := make([]DrainedRecord, 0, c.count)
	for i := 0; i < c.count; i++ {
		records = append(records, DrainedRecord{Seq: int64(i + 1), Record: map[string]any{"name": "callFunction"}})
	}
	return DrainPage{Records: records, NextSeq: int64(c.count)}, nil
}

// miniapp_cloud_captures 是无游标的快照读：曾一次拉 1000 条全量 body。
// 现在封顶并如实标注，余量指给 hook_drain 的游标翻页。
func TestCloudCapturesCapsThePage(t *testing.T) {
	messages := exchange(t, Deps{Core: &cloudPageCore{count: cloudCapturesCap + 50}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_cloud_captures","arguments":{}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("不该失败: %s", toolText(t, messages, 1))
	}
	var payload struct {
		Count     int    `json:"count"`
		Truncated bool   `json:"truncated"`
		Note      string `json:"note"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Count != cloudCapturesCap || !payload.Truncated || payload.Note == "" {
		t.Fatalf("快照读必须封顶并指路: %+v", payload)
	}
}

// hugeBridge 让 cloud.call 返回超过 48KB 的载荷。
type hugeBridge struct{}

func (hugeBridge) Call(_ context.Context, method string, _ map[string]any) (any, error) {
	if method == "cloud.call" {
		return map[string]any{"ok": true, "data": strings.Repeat("x", evaluateResultCap+1)}, nil
	}
	return map[string]any{"ok": true}, nil
}

// 云函数返回值与 evaluate 同一性质：页面侧没有天然上限，超限同样截断。
func TestCallCloudCapsHugeResults(t *testing.T) {
	messages := exchange(t, Deps{AppBridge: hugeBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_call_cloud","arguments":{"name":"getUser"}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("不该失败: %s", toolText(t, messages, 1))
	}
	var payload struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Truncated {
		t.Fatalf("超大云函数返回必须截断: %s", toolText(t, messages, 1))
	}
}

// hook_drain 的 schema 只是给客户端的提示：未知名字必须在边界拒绝，超宽
// 的 limit/updateLimit 必须被收到与 hook_wait 同一顶帽下（游标语义不变）。
func TestHookDrainValidatesNameAndClampsPageWidth(t *testing.T) {
	messages := exchange(t, Deps{Core: &fakeCore{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_drain","arguments":{"name":"navigator"}}}`+"\n")
	if !toolFailed(t, messages, 1) {
		t.Fatalf("未知钩子名必须拒绝: %s", toolText(t, messages, 1))
	}

	core := &fakeCore{}
	messages = exchange(t, Deps{Core: core},
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hook_drain","arguments":{"name":"wxapi","limit":100000,"afterUpdateSeq":7,"updateLimit":999999}}}`+"\n")
	if toolFailed(t, messages, 2) {
		t.Fatalf("正常分页不该失败: %s", toolText(t, messages, 2))
	}
	if core.lastLimit != hookWaitMaxLimit {
		t.Fatalf("limit 应被收到 %d, got %d", hookWaitMaxLimit, core.lastLimit)
	}
	if core.lastUpdateLimit != hookWaitMaxLimit || core.lastUpdateSeq != 7 {
		t.Fatalf("updateLimit 应被收到且游标保持: updateLimit=%d afterUpdateSeq=%d", core.lastUpdateLimit, core.lastUpdateSeq)
	}
}

// breakpointCore 按 setBreakpointByUrl 的真实形状应答，用于验证旧名断点
// 工具与 debugger_breakpoint 共用一份注册表。
type breakpointCore struct {
	fakeCore
}

func (c *breakpointCore) CDPCommand(_ context.Context, method string, _ map[string]any, _ int) (map[string]any, error) {
	if method == "Debugger.setBreakpointByUrl" {
		return map[string]any{"result": map[string]any{"breakpointId": "1:9", "locations": []any{}}}, nil
	}
	return map[string]any{"result": map[string]any{}}, nil
}

// 旧名 miniapp_set_breakpoint 曾绕开注册表：{action:"list"} 看不见它设的
// 断点。两个名字必须落在同一实现上。
func TestLegacyBreakpointNameFeedsTheRegistry(t *testing.T) {
	messages := exchange(t, Deps{Core: &breakpointCore{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_set_breakpoint","arguments":{"url":"https://usr/app.js","line":12}}}`+"\n"+
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"debugger_breakpoint","arguments":{"action":"list"}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("设断点不该失败: %s", toolText(t, messages, 1))
	}
	var list struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 2)), &list); err != nil {
		t.Fatal(err)
	}
	if list.Count != 1 {
		t.Fatalf("注册表必须看得见旧名设的断点: %s", toolText(t, messages, 2))
	}
}

// 快照里每个读不到的面都以 {error} 内联；Core 未就绪时 engine 键曾整体
// 缺失，客户端会把「无此信息」误读成「一切正常」。
func TestSessionStatusInlinesEngineErrorWhenCoreAbsent(t *testing.T) {
	messages := exchange(t, Deps{AppBridge: &sessionBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_status","arguments":{}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("快照不该失败: %s", toolText(t, messages, 1))
	}
	var payload struct {
		Engine struct {
			Error string `json:"error"`
		} `json:"engine"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Engine.Error == "" {
		t.Fatalf("Core 缺席时 engine 必须带 {error}: %s", toolText(t, messages, 1))
	}
}
