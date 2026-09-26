package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/api"
	"github.com/langbyyi/wxtap/desktop/internal/api/ipc"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// newTrafficContractApp builds an App with a real (temp) traffic store wired to
// the router, so the assertions see exactly what the webview receives.
func newTrafficContractApp(t *testing.T) *App {
	t.Helper()
	base := t.TempDir()
	t.Setenv("WXTAP_DATA_DIR", base)
	t.Setenv("WXTAP_TRAFFIC_DB", filepath.Join(t.TempDir(), "traffic.db"))

	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = base
	repo, err := traffic.Open(resolveDBPath(), resolveMigrationsDir(exeDir()))
	if err != nil {
		t.Fatalf("open traffic db: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	app.repo = repo
	app.trafficAPI = api.NewTrafficAPI(traffic.NewService(repo))
	app.setupIPC()
	return app
}

// settleWaitBudget is how long a test waits for the capture path to come back
// through the real Core subprocess (startFakeCoreApp runs the test binary as the
// fake Core) and the feeder's 500ms tick. Ten seconds is the happy-path figure,
// but a machine running several test binaries at once - CI, or a workstation
// with parallel agents compiling - stretches the first ticks well past it, and a
// flaky wait reads exactly like a missing event. What these waiters assert is
// "the event eventually arrives", so the budget is deliberately generous: a
// genuinely missing emit still fails, just later.
const settleWaitBudget = 30 * time.Second

// contractFields maps each field of one `export type X = {...}` block in
// contracts/traffic.ts to whether it is optional. The Vue app compiles against
// those names, so the IPC payloads are pinned against the published contract
// instead of a second hand-written list that can drift from it.
func contractFields(t *testing.T, name string) map[string]bool {
	t.Helper()
	return contractFieldsIn(t, "traffic.ts", name)
}

// contractFieldsIn is contractFields generalized over the contract file, so
// each domain keeps one contract test reading its own contracts/*.ts.
func contractFieldsIn(t *testing.T, file, name string) map[string]bool {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(root, "contracts", file))
	if err != nil {
		t.Fatalf("read contracts/%s: %v", file, err)
	}
	block := regexp.MustCompile(`(?s)export type ` + name + ` = \{(.*?)\};`).FindStringSubmatch(string(source))
	if block == nil {
		t.Fatalf("contracts/%s has no `export type %s = {...}` block", file, name)
	}
	fields := map[string]bool{}
	line := regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)(\??):`)
	for _, raw := range strings.Split(block[1], "\n") {
		if match := line.FindStringSubmatch(raw); match != nil {
			fields[match[1]] = match[2] == "?"
		}
	}
	if len(fields) == 0 {
		t.Fatalf("parsed no fields out of the %s contract", name)
	}
	return fields
}

func sortedKeys(payload map[string]any) []string {
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func declaredNames(contract map[string]bool) []string {
	names := make([]string, 0, len(contract))
	for name := range contract {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// assertFieldsWithin rejects a payload that carries a field the published
// contract does not declare: the frontend would never read it, and the next
// reader would take it for part of the surface.
func assertFieldsWithin(t *testing.T, payload map[string]any, contract map[string]bool, context string) {
	t.Helper()
	for name := range payload {
		if _, ok := contract[name]; !ok {
			t.Fatalf("%s emits %q, which contracts/traffic.ts does not declare (%v)", context, name, declaredNames(contract))
		}
	}
	for name, optional := range contract {
		if optional {
			continue
		}
		if _, ok := payload[name]; !ok {
			t.Fatalf("%s is missing the required field %q (%v)", context, name, sortedKeys(payload))
		}
	}
}

func assertExactFields(t *testing.T, payload map[string]any, contract map[string]bool, context string) {
	t.Helper()
	got, want := sortedKeys(payload), declaredNames(contract)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s payload fields = %v, contract declares %v", context, got, want)
	}
}

// seedContractTraffic stores two records that exercise every summary field.
func seedContractTraffic(t *testing.T, app *App) {
	t.Helper()
	base := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	records := []traffic.Record{
		{
			ID: "wx1-wxapp11111111-1700000000000-1", Seq: 1, CapturedAt: base,
			APIType: "wx.request", Name: "request", AppID: "wxapp11111111",
			Method: "POST", URL: "https://api.example.com/one", Status: traffic.StatusSuccess,
			DurationMs: 42, RequestBody: []byte(`{"n":1}`), ResponseBody: []byte(`{"ok":true}`),
		},
		{
			ID: "wx2-wxapp22222222-1700000000001-2", Seq: 2, CapturedAt: base.Add(time.Second),
			APIType: "wx.request", Name: "request", AppID: "wxapp22222222",
			Method: "GET", URL: "https://api.example.com/two", Status: traffic.StatusFail,
			RequestBody: []byte(`{"n":2}`), ResponseBody: []byte(`{"ok":false}`),
		},
	}
	if _, err := app.repo.InsertBatch(context.Background(), records); err != nil {
		t.Fatalf("seed records: %v", err)
	}
}

// traffic.list must carry appId in the summary and accept it as a filter: the
// frontend switched from deriving the app out of the record id to asking the
// backend for it (roadmap 2.3).
func TestTrafficListAppIDContract(t *testing.T) {
	app := newTrafficContractApp(t)
	seedContractTraffic(t, app)

	page := callJSON(t, app, "traffic.list", map[string]any{"pageSize": 10})
	// 整页形状也钉住：面板的「共 N 条 · 第 X / Y 页」直接靠这三个字段。
	assertFieldsWithin(t, page, contractFields(t, "TrafficPage"), "traffic.list")
	if page["total"].(float64) != 2 || page["page"].(float64) != 1 || page["pageSize"].(float64) != 10 {
		t.Fatalf("page window = %#v, want total 2 on page 1 with pageSize 10", page)
	}
	items, ok := page["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("traffic.list payload: %#v", page)
	}
	summary := items[0].(map[string]any)
	assertExactFields(t, summary, contractFields(t, "TrafficSummary"), "traffic.list item")
	// 最新在前：items[0] 是后写的那条，所以 appId 按集合判，不认位置。
	carried := map[string]bool{}
	for _, raw := range items {
		carried[raw.(map[string]any)["appId"].(string)] = true
	}
	if !carried["wxapp11111111"] || !carried["wxapp22222222"] {
		t.Fatalf("summaries carry %v, want both stored appIds", carried)
	}

	filtered := callJSON(t, app, "traffic.list", map[string]any{"pageSize": 10, "appId": "wxapp22222222"})
	items, ok = filtered["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("appId filter returned %#v, want exactly one record", filtered)
	}
	if got := items[0].(map[string]any)["appId"]; got != "wxapp22222222" {
		t.Fatalf("appId filter leaked another app: %v", got)
	}
}

// traffic.stats must answer the frozen shape (roadmap 2.3): the capture range is
// omitted on an empty store rather than faked, and the overload counters are
// present but zero until a capture path drops something.
func TestTrafficStatsContract(t *testing.T) {
	app := newTrafficContractApp(t)
	seedContractTraffic(t, app)

	stats := callJSON(t, app, "traffic.stats", map[string]any{})
	contract := contractFields(t, "TrafficStats")
	// 空库时捕获范围那两个字段是缺席而不是报假值，所以这一侧只查：没多报契约之外的
	// 字段，也没漏掉必给的字段。
	assertFieldsWithin(t, stats, contract, "traffic.stats")

	if stats["records"].(float64) != 2 {
		t.Fatalf("records = %v, want 2", stats["records"])
	}
	if stats["oldestCapturedAt"] == "" || stats["newestCapturedAt"] == "" {
		t.Fatalf("capture range missing: %#v", stats)
	}
	if stats["bytes"].(float64) <= 0 {
		t.Fatalf("bytes = %v, want the on-disk footprint", stats["bytes"])
	}
	// 过载计数在还没有任何丢弃时是 0 —— 存在而不是缺席，否则前端会读成「未知」。
	for _, key := range []string{"droppedRecords", "droppedUpdates"} {
		if value, ok := stats[key]; !ok || value.(float64) != 0 {
			t.Fatalf("%s = %v, want 0 before anything was dropped", key, stats[key])
		}
	}
	// 保留策略已删除，stats 不再报 prunedRecords / lastPrunedAt —— 那两块审计的缺席
	// 断言在 TestTrafficClearContract 里。
}

// traffic.clear answers the frozen shape, empties the store, and leaves the
// stats surface with nothing to say about a retention policy: the store no
// longer trims itself, so a clear is the only bulk deletion there is.
func TestTrafficClearContract(t *testing.T) {
	app := newTrafficContractApp(t)
	seedContractTraffic(t, app)
	// 契约里 traffic.clear 与 traffic.delete 是两个同形状的冻结块（Go 侧回的是同一个
	// traffic.DeleteResult）：行已删掉，只有空间回收失败才多一个可选字段。
	clearFields := contractFields(t, "TrafficClear")
	// reclamationFailed 是干净清空唯一省略的字段：它必须声明着（回收失败那条路径要
	// 发它），且必须是可选的（成功路径不该被迫凭空造一个）。
	if optional, declared := clearFields["reclamationFailed"]; !declared || !optional {
		t.Fatalf("traffic.clear must declare reclamationFailed as optional (declared=%v optional=%v)", declared, optional)
	}

	cleared := callJSON(t, app, "traffic.clear", map[string]any{})
	assertFieldsWithin(t, cleared, clearFields, "traffic.clear")
	if cleared["deleted"].(float64) != 2 {
		t.Fatalf("clear result = %#v, want 2 deleted", cleared)
	}

	// 记录是真的没了：不是不显示，而是库里一条不剩。
	stats := callJSON(t, app, "traffic.stats", map[string]any{})
	if stats["records"].(float64) != 0 {
		t.Fatalf("records after clear = %v, want 0", stats["records"])
	}
	if items := callJSON(t, app, "traffic.list", map[string]any{"pageSize": 10})["items"].([]any); len(items) != 0 {
		t.Fatalf("clear left %d records listed", len(items))
	}

	// 没有保留策略，stats 也就不该再报它：prunedRecords / lastPrunedAt 只能是自动清理
	// 的审计，而那套自动清理已经删掉了。
	assertFieldsWithin(t, stats, contractFields(t, "TrafficStats"), "traffic.stats")
	for _, key := range []string{"prunedRecords", "lastPrunedAt"} {
		if value, present := stats[key]; present {
			t.Fatalf("traffic.stats 里不该再有 %s = %v", key, value)
		}
	}
}

// traffic.delete answers the frozen shape and removes exactly the named rows.
func TestTrafficDeleteContract(t *testing.T) {
	app := newTrafficContractApp(t)
	seedContractTraffic(t, app)
	deleteFields := contractFields(t, "TrafficDelete")
	// 与 traffic.clear 同一条：成功的删除不该被迫凭空造一个 reclamationFailed，
	// 但字段必须声明着（回收失败那条路径要发它）。
	if optional, declared := deleteFields["reclamationFailed"]; !declared || !optional {
		t.Fatalf("TrafficDelete must declare reclamationFailed as optional (declared=%v optional=%v)", declared, optional)
	}

	// 参数解析不了就不能猜：猜错的方向只有「删得更多」。
	if _, err := app.router.Call(context.Background(), "traffic.delete", json.RawMessage(`{"ids": 5}`)); err == nil {
		t.Fatal("traffic.delete must refuse params it cannot parse")
	}
	if items := callJSON(t, app, "traffic.list", map[string]any{"pageSize": 10})["items"].([]any); len(items) != 2 {
		t.Fatalf("a refused delete changed the store: %d records", len(items))
	}

	target := "wx1-wxapp11111111-1700000000000-1"
	result := callJSON(t, app, "traffic.delete", map[string]any{"ids": []string{target}})
	assertFieldsWithin(t, result, deleteFields, "traffic.delete")
	if result["deleted"].(float64) != 1 {
		t.Fatalf("delete result = %#v, want 1 deleted", result)
	}
	page := callJSON(t, app, "traffic.list", map[string]any{"pageSize": 10})
	if page["total"].(float64) != 1 {
		t.Fatalf("total after delete = %v, want 1", page["total"])
	}
	if id := page["items"].([]any)[0].(map[string]any)["id"]; id == target {
		t.Fatalf("the deleted record is still listed: %v", id)
	}

	// 重复 id 只算一条，库里已经没有的 id 不是错误：调用方删的是它看得见的那些，
	// 「别人先删了」不该变成一次失败。
	again := callJSON(t, app, "traffic.delete", map[string]any{"ids": []string{target, target, "never-existed"}})
	if again["deleted"].(float64) != 0 {
		t.Fatalf("re-delete = %#v, want 0 deleted", again)
	}
}

// dropReportCore 是 traffic.stats 求和用的 Core 桩：安装回 ok，每个 drain 响应只报
// 一份固定的页面侧丢弃读数，不喂任何记录。
type dropReportCore struct {
	records int64
	updates int64
}

func (c *dropReportCore) InstallHook(context.Context, string) error { return nil }

func (c *dropReportCore) InstallHookReport(context.Context, string) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func (c *dropReportCore) HookDrain(context.Context, string, int64, int, int64, int) (ipc.DrainPage, error) {
	return ipc.DrainPage{DroppedRecords: c.records, DroppedUpdates: c.updates}, nil
}

func (c *dropReportCore) Evaluate(context.Context, string, int) (any, error) { return nil, nil }

// floodCore 顺序产出 total 条记录（每轮 limit 条）外加一份页面侧读数，用来把 shell 侧
// 的 pending 缓冲撑爆 —— 只有真的溢出，dropped 才有一个非零的 shell 分量可测。
type floodCore struct {
	total   int64
	records int64
	updates int64
}

func (c *floodCore) InstallHook(context.Context, string) error { return nil }

func (c *floodCore) InstallHookReport(context.Context, string) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func (c *floodCore) Evaluate(context.Context, string, int) (any, error) { return nil, nil }

func (c *floodCore) HookDrain(_ context.Context, _ string, afterSeq int64, limit int, _ int64, _ int) (ipc.DrainPage, error) {
	page := ipc.DrainPage{NextSeq: afterSeq, DroppedRecords: c.records, DroppedUpdates: c.updates}
	for seq := afterSeq + 1; seq <= c.total && len(page.Records) < limit; seq++ {
		page.Records = append(page.Records, ipc.DrainedRecord{Seq: seq, Record: map[string]any{
			"ts": float64(1000 + seq), "rid": fmt.Sprintf("rid-%d", seq), "status": "pending",
		}})
	}
	if len(page.Records) > 0 {
		page.NextSeq = page.Records[len(page.Records)-1].Seq
	}
	page.HasMore = page.NextSeq < c.total
	return page, nil
}

// floodRecords 明显大于 shell 的 pending 上限（不硬编码那个容量：断言用的是读数本身，
// 只要总量超过上限，溢出与计数就必然发生）。
const floodRecords = 6000

// R12 的冻结求和口径：traffic.stats 的
//
//	droppedRecords = wxapi.Dropped + cloud.Dropped + wxapi.PageDroppedRecords + cloud.PageDroppedRecords
//	droppedUpdates = wxapi.PageDroppedUpdates + cloud.PageDroppedUpdates
//
// （shell 侧没有更新缓冲，更新只有页面侧一项）。两侧互补不重叠：shell 的 dropped 是
// 「页面还留着、下一 tick 能重新 drain 回来」，页面侧是「已经扔掉、谁也捞不回来」。
// feeder 未初始化时按 0 处理，不得报错 —— 历史记录页的「过载提示」不能因为没在抓包就
// 让统计整体失败。
func TestTrafficStatsSumsShellAndPageDrops(t *testing.T) {
	app := newTrafficContractApp(t)
	seedContractTraffic(t, app)

	// 连 feeder 都没有（捕获没开 / 存储仍在但没注册捕获面）：照常回答，丢弃数为 0。
	app.wxapiFeeder, app.cloudFeeder = nil, nil
	stats := callJSON(t, app, "traffic.stats", map[string]any{})
	if stats["droppedRecords"].(float64) != 0 || stats["droppedUpdates"].(float64) != 0 {
		t.Fatalf("没有 feeder 时丢弃数必须是 0，而不是报错或缺失: %#v", stats)
	}

	wxapi := ipc.NewHookFeederWithUpdates(
		&floodCore{total: floodRecords, records: 7, updates: 2}, "wxapi", nil, nil, nil, nil)
	cloud := ipc.NewHookFeederWithUpdates(
		&dropReportCore{records: 3, updates: 1}, "cloud", nil, nil, nil, nil)
	app.wxapiFeeder, app.cloudFeeder = wxapi, cloud
	for _, feeder := range []*ipc.HookFeeder{wxapi, cloud} {
		if err := feeder.Start(); err != nil {
			t.Fatalf("feeder start: %v", err)
		}
		defer feeder.Stop()
	}
	// 等记录全部被确认：那时 shell 侧的丢弃数才是最终值（中途读到的会偏小）。
	waitFor(t, settleWaitBudget, "shell 的 pending 缓冲溢出", func() bool {
		return wxapi.Stats().Ack == floodRecords
	})
	shellDropped := wxapi.Stats().Dropped
	if shellDropped == 0 {
		t.Fatalf("没有真的溢出，求和断言证明不了 shell 分量: %+v", wxapi.Stats())
	}

	stats = callJSON(t, app, "traffic.stats", map[string]any{})
	if got, want := stats["droppedRecords"].(float64), float64(shellDropped+7+3); got != want {
		t.Fatalf("droppedRecords = %v, want %v（shell %d + 页面侧 7 + 3）", got, want, shellDropped)
	}
	if got := stats["droppedUpdates"].(float64); got != 3 {
		t.Fatalf("droppedUpdates = %v, want 3（页面侧 2 + 1）", got)
	}
}

// pageDropReading tries one stats envelope and returns its two page-side readings.
func pageDropReading(t *testing.T, app *App, method string) (int64, int64) {
	t.Helper()
	var envelope struct {
		Result struct {
			PageDroppedRecords int64 `json:"pageDroppedRecords"`
			PageDroppedUpdates int64 `json:"pageDroppedUpdates"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	raw := app.Call(method, `{}`)
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode %s %s: %v", method, raw, err)
	}
	if envelope.Error != nil {
		t.Fatalf("%s: %s", method, envelope.Error.Message)
	}
	return envelope.Result.PageDroppedRecords, envelope.Result.PageDroppedUpdates
}

// R12 的整条链路：页面侧读数要经 Core 的 drain JSON → engine.HookDrainPage →
// ipc.DrainPage → feeder → 三个 stats 载荷一路走到前端。链上任何一环漏掉字段，面板的
// 「已丢弃」就永远是 0。假 Core 在每个 drain 响应里报 4 / 2（见 app_integration_test.go
// 的 fakePageDropCounters），两个行钩子与 trafic.stats 的求和都必须看到它。
func TestPageDropReadingTravelsTheWholeChain(t *testing.T) {
	app := startFakeCoreApp(t)
	defer app.shutdown(context.Background())

	if err := app.EngineStart(62000); err != nil {
		t.Fatalf("engine.start: %v", err)
	}
	for _, method := range []string{"wxapi.start", "cloud.start"} {
		if envelope := app.Call(method, `{}`); strings.Contains(envelope, `"error"`) {
			t.Fatalf("%s: %s", method, envelope)
		}
	}

	// 两个 feeder 都要先真的读过一页：traffic.stats 的求和依赖两者的读数都到位。
	waitFor(t, settleWaitBudget, "两个行钩子的页面侧读数到达 stats", func() bool {
		wxRecords, wxUpdates := pageDropReading(t, app, "wxapi.stats")
		clRecords, clUpdates := pageDropReading(t, app, "cloud.stats")
		return wxRecords == 4 && wxUpdates == 2 && clRecords == 4 && clUpdates == 2
	})

	// 求和口径：两个 feeder 都没有 shell 侧溢出（假 Core 只喂三条），所以
	// droppedRecords 就是两倍的页面侧记录读数，droppedUpdates 同理。
	stats := callJSON(t, app, "traffic.stats", map[string]any{})
	if got := stats["droppedRecords"].(float64); got != 8 {
		t.Fatalf("droppedRecords = %v, want 8（两个行钩子各 4）", got)
	}
	if got := stats["droppedUpdates"].(float64); got != 4 {
		t.Fatalf("droppedUpdates = %v, want 4（两个行钩子各 2）", got)
	}
}

// captureTrafficEvents 装上 emitSink（B3 已有的测试缝）并返回这段时间收到的
// traffic:available 载荷。只收这一条事件：其余事件与本组断言无关。
func captureTrafficEvents(t *testing.T) func() []map[string]any {
	t.Helper()
	var mu sync.Mutex
	var payloads []map[string]any
	emitSink = func(name string, payload any) {
		if name != "traffic:available" {
			return
		}
		fields, ok := payload.(map[string]any)
		if !ok {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		payloads = append(payloads, fields)
	}
	t.Cleanup(func() { emitSink = nil })
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), payloads...)
	}
}

// 慢调用的落定：记录先以 pending 入库，几秒后落定帧才到，Go 侧原地覆写已落定的字段。
// 这次写库必须发一条 traffic:available（载荷带 updated），否则历史记录页没有任何事件可
// 依 —— 唯一的事件源是插入分支，只要没有新记录入库，那一行就永远停在「等待中」且不显示
// 耗时，用户只能手点刷新。落定帧由假 Core 与它的 pending 记录放在同一个 tick 投递
// （app_integration_test.go），所以这里走的是真实接线：drain → 入库 → apply → emit。
func TestSettledUpdateEmitsTrafficAvailable(t *testing.T) {
	app := startFakeCoreApp(t)
	defer app.shutdown(context.Background())
	available := captureTrafficEvents(t)

	if err := app.EngineStart(62000); err != nil {
		t.Fatalf("engine.start: %v", err)
	}
	if envelope := app.Call("wxapi.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.start: %s", envelope)
	}

	waitFor(t, settleWaitBudget, "落定写库后的事件", func() bool {
		for _, payload := range available() {
			if _, ok := payload["updated"]; ok {
				return true
			}
		}
		return false
	})

	// 事件只是刷新触发器，页面读到的仍是库里的行：事件发出时那一行必须已经落定。
	items := callJSON(t, app, "traffic.list", map[string]any{"pageSize": 10})["items"].([]any)
	var settled map[string]any
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok && item["id"] == settledRecordRID {
			settled = item
		}
	}
	if settled == nil {
		t.Fatalf("traffic.list 里没有 rid=%s 的行: %#v", settledRecordRID, items)
	}
	if settled["status"] != string(traffic.StatusSuccess) || settled["durationMs"].(float64) != 1830 {
		t.Fatalf("落定行 = %#v, want status success 且 durationMs 1830", settled)
	}

	// 一次落定只发一条事件，且带的是本次改动的行数（假 Core 只投一帧）。载荷没有经过
	// JSON 往返，所以计数是 ApplyUpdates 的 int 原样；int64 一并接受，免得断言绑死在
	// 具体数值类型上。
	var updated int
	for _, payload := range available() {
		switch value := payload["updated"].(type) {
		case int:
			updated += value
		case int64:
			updated += int(value)
		}
	}
	if updated != 1 {
		t.Fatalf("updated 合计 = %d, want 1（每帧落定只计数一次）", updated)
	}
}

// 一条都没改动时不发事件：落定帧可以跑在入库之前（页面缓冲先 drain 出来），也可以指向
// 一个已经不存在的行，那时写库是空操作，发事件只会白刷一次页面。
func TestApplyHookUpdatesStaysSilentWhenNothingMatched(t *testing.T) {
	app := newTrafficContractApp(t)
	available := captureTrafficEvents(t)

	if err := app.applyHookUpdates(context.Background(), "wxapi", []ipc.DrainedUpdate{
		{Seq: 1, Update: map[string]any{"rid": "ghost-nobody-inserted", "status": "success", "durationMs": float64(12)}},
	}); err != nil {
		t.Fatalf("apply updates: %v", err)
	}
	if got := available(); len(got) != 0 {
		t.Fatalf("没有行被改动却发了 %d 条事件: %#v", len(got), got)
	}
}

// 存储降级（repo == nil）时 ingestHookBatch 静默返回：实时捕获照常，历史库一条
// 不进。日志流里必须留下唯一一条 warn 线索，否则排障时看不出「实时有记录、历史库
// 没数据」是存储初始化失败导致的；两个 feeder 每 500ms 各调一次本函数，逐批 warn
// 会刷爆日志面板，所以同一次运行只允许出现一条。
func TestIngestHookBatchWarnsStorageUnavailableOnce(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	var mu sync.Mutex
	var warns []string
	emitSink = func(name string, payload any) {
		if name != "log" {
			return
		}
		fields, ok := payload.(map[string]any)
		if !ok {
			return
		}
		if level, _ := fields["level"].(string); level != "warn" {
			return
		}
		message, _ := fields["message"].(string)
		mu.Lock()
		defer mu.Unlock()
		warns = append(warns, message)
	}
	t.Cleanup(func() { emitSink = nil })

	// 连续两批（模拟两个 feeder 各 tick 一次）：降级分支必须保持静默成功
	// —— 捕获不该因为存储挂掉而报错，warn 只在第一批时发一条。
	batch := []ipc.DrainedRecord{{Seq: 1, Record: map[string]any{"rid": "r1"}}}
	for i := 0; i < 2; i++ {
		if err := app.ingestHookBatch(context.Background(), "wxapi", batch); err != nil {
			t.Fatalf("降级 ingest 第 %d 批必须静默成功: %v", i+1, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warns) != 1 {
		t.Fatalf("两次降级 ingest 后 warn 日志 = %d 条, want 恰好 1 条: %q", len(warns), warns)
	}
	if !strings.Contains(warns[0], "存储不可用") {
		t.Fatalf("warn 文案必须点明存储不可用: %q", warns[0])
	}
}

// mcpExchange 用真实的 MCP 服务器跑几条 JSON-RPC 请求并逐行解码响应。buildAppMCPServer
// 接的是本 App 的 traffic API，所以这里断言的是 agent 与 GUI 实际共用的那条链路。
func mcpExchange(t *testing.T, app *App, requests ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	input := strings.Join(requests, "\n") + "\n"
	if err := buildAppMCPServer(app, "all").Serve(strings.NewReader(input), &out); err != nil {
		t.Fatalf("mcp serve: %v", err)
	}
	messages := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("解码 MCP 响应 %q: %v", line, err)
		}
		messages = append(messages, message)
	}
	return messages
}

func mcpMessageByID(t *testing.T, messages []map[string]any, id float64) map[string]any {
	t.Helper()
	for _, message := range messages {
		if value, ok := message["id"].(float64); ok && value == id {
			return message
		}
	}
	t.Fatalf("MCP 响应里没有 id=%v: %#v", id, messages)
	return nil
}

// mcpToolText 取出一次 tools/call 的文本载荷（MCP 把结果 JSON 编进 content[0].text）。
func mcpToolText(t *testing.T, message map[string]any) string {
	t.Helper()
	result, ok := message["result"].(map[string]any)
	if !ok {
		t.Fatalf("MCP 响应没有 result: %#v", message)
	}
	if isError, _ := result["isError"].(bool); isError {
		t.Fatalf("MCP 工具报错: %#v", result)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("MCP 响应没有 content: %#v", result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

// GUI 能按 appId 精确筛，agent 面必须看到同一份事实（roadmap 2.5）：traffic_list 的入参
// 有可选的 appId，返回的 item 也带 appId。没有它，agent 只能靠关键词猜哪条记录属于正在
// 审计的那个小程序。
func TestMCPTrafficListCarriesAppID(t *testing.T) {
	app := newTrafficContractApp(t)
	seedContractTraffic(t, app)

	messages := mcpExchange(t, app,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"traffic_list","arguments":{"appId":"wxapp22222222"}}}`,
	)

	// schema：appId 是声明过的可选参数，不是一个会被静默丢掉的未知键。
	var properties map[string]any
	for _, raw := range mcpMessageByID(t, messages, 1)["result"].(map[string]any)["tools"].([]any) {
		tool, _ := raw.(map[string]any)
		if tool["name"] != "traffic_list" {
			continue
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		properties, _ = schema["properties"].(map[string]any)
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				if name == "appId" {
					t.Fatal("appId 必须是可选参数：不带它的老调用方仍要能用")
				}
			}
		}
	}
	if properties == nil {
		t.Fatal("tools/list 里找不到 traffic_list 的 inputSchema")
	}
	if _, ok := properties["appId"]; !ok {
		t.Fatalf("traffic_list 的 schema 必须声明 appId: %v", sortedKeys(properties))
	}

	// 映射：appId 透传到存储的精确过滤，item 也把它带回来。
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(mcpToolText(t, mcpMessageByID(t, messages, 2))), &page); err != nil {
		t.Fatalf("解码 traffic_list 结果: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("traffic_list(appId=wxapp22222222) 返回 %d 条，want 1: %#v", len(page.Items), page.Items)
	}
	if got := page.Items[0]["appId"]; got != "wxapp22222222" {
		t.Fatalf("item appId = %v, want 过滤用的那个 appid（映射漏传/漏回都会在这里现形）", got)
	}
}
