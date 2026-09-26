package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/api"
	"github.com/langbyyi/wxtap/desktop/internal/cloud"
	"github.com/langbyyi/wxtap/desktop/internal/engine"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// cloudStats mirrors the FeederStats payload cloud.stats answers with: the
// capture state the panel cannot read from the records alone, plus the shell
// 侧的存储可用性（storageAvailable 与 wxapi.stats 同语义，repo 非 nil 才是 true）。
type cloudStats struct {
	Running   bool  `json:"running"`
	Pending   int   `json:"pending"`
	Dropped   int64 `json:"dropped"`
	Ack       int64 `json:"ack"`
	UpdateAck int64 `json:"updateAck"`
	// R12：页面侧缓冲的累计丢弃读数，与 wxapi.stats 同形。
	PageDroppedRecords int64 `json:"pageDroppedRecords"`
	PageDroppedUpdates int64 `json:"pageDroppedUpdates"`
	// 存储初始化失败的降级模式（repo == nil）下 feeder 照常 capture，面板只能靠
	// 这个字段发现历史库在悄悄丢数据。
	StorageAvailable bool `json:"storageAvailable"`
}

// readCloudStats decodes one cloud.stats envelope.
func readCloudStats(t *testing.T, app *App) cloudStats {
	t.Helper()
	var envelope struct {
		Result cloudStats `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	raw := app.Call("cloud.stats", `{}`)
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode cloud.stats %s: %v", raw, err)
	}
	if envelope.Error != nil {
		t.Fatalf("cloud.stats: %s", envelope.Error.Message)
	}
	return envelope.Result
}

// cloudPoll decodes one cloud.poll envelope (the handler answers a bare array).
func cloudPoll(t *testing.T, app *App, params string) []map[string]any {
	t.Helper()
	var decoded struct {
		Result []map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	envelope := app.Call("cloud.poll", params)
	if err := json.Unmarshal([]byte(envelope), &decoded); err != nil {
		t.Fatalf("decode %s: %v", envelope, err)
	}
	if decoded.Error != nil {
		t.Fatalf("cloud.poll %s: %s", params, decoded.Error.Message)
	}
	return decoded.Result
}

// 契约 2.2：cloud.stats 与 wxapi.stats 同形（FeederStats），七个字段缺一不可
// （pageDropped* 是 R12 的页面侧丢弃读数，见 wxapi_contract_test.go），
// 掉线也算一种可渲染状态，不能报错。storageAvailable 同形且必须显式输出：
// 这里没有 repo，false 不能被 omitempty 吞掉 —— 缺失会被前端读成 true。
func TestCloudStatsReportsBufferAndCursors(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")

	result, err := app.router.Call(context.Background(), "cloud.stats", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("cloud.stats must answer with no engine running: %v", err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"running"`, `"pending"`, `"dropped"`, `"ack"`, `"updateAck"`,
		`"pageDroppedRecords"`, `"pageDroppedUpdates"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("cloud.stats payload %s lacks %s", data, key)
		}
	}
	if !strings.Contains(string(data), `"storageAvailable":false`) {
		t.Fatalf("无 repo 的 cloud.stats 必须显式输出 storageAvailable:false（缺失会被前端读成 true）: %s", data)
	}
	stats := readCloudStats(t, app)
	if stats.Running || stats.Pending != 0 || stats.Dropped != 0 {
		t.Fatalf("an unstarted cloud feeder must report idle: %#v", stats)
	}
	if stats.PageDroppedRecords != 0 || stats.PageDroppedUpdates != 0 {
		t.Fatalf("an unstarted cloud feeder has no page reading to report: %#v", stats)
	}
	if stats.StorageAvailable {
		t.Fatalf("repo == nil 时 storageAvailable 必须是 false: %#v", stats)
	}
}

// storageAvailable 的另一面：repo 非 nil（正常启动，storage 初始化成功）时必须报
// true，方向与 wxapi.stats 的一致性由同一个 captureStats 包装保证。false 的方向在
// 上面无 repo 的用例里。
func TestCloudStatsReportsStorageAvailableWithARepo(t *testing.T) {
	app := newTrafficContractApp(t)

	stats := readCloudStats(t, app)
	if !stats.StorageAvailable {
		t.Fatalf("repo 非 nil 时 cloud.stats 必须报 storageAvailable:true: %#v", stats)
	}
}

// 契约 2.1/2.2：wxapi 的那套事件名契约同样适用于云函数。cloud_capture 两侧都有
// （后端常量 + bridge.ts 的 supportedEvents）；cloud_update 这一侧只有后端常量 ——
// 面板的 cloud_update 监听与白名单条目属 Wave B2（cloud 前端对齐），本波按路线图
// 不碰前端，所以这里刻意不解析 bridge.ts 去要一个还不存在的条目（那会让 go test
// 在 A1 落地时变红），而是把常量字面量钉死：前端加白名单时按的就是这个字符串。
func TestCloudEventNamesArePinnedOnBothSides(t *testing.T) {
	if cloudCaptureEvent != "cloud_capture" {
		t.Errorf("cloudCaptureEvent = %q, want %q", cloudCaptureEvent, "cloud_capture")
	}
	if cloudUpdateEvent != "cloud_update" {
		t.Errorf("cloudUpdateEvent = %q, want %q（Wave B2 的 bridge.ts 白名单与 CloudView 监听必须用这个名字）",
			cloudUpdateEvent, "cloud_update")
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(root, "desktop", "frontend", "src", "api", "bridge.ts"))
	if err != nil {
		t.Fatalf("read bridge.ts: %v", err)
	}
	events := parseDeclaredSet(string(source), "const supportedEvents = new Set")
	if len(events) == 0 {
		t.Fatal("no supportedEvents entries parsed from bridge.ts")
	}
	// 云函数面板已经在监听 cloud_capture：改名会静默丢掉实时行，两侧必须同时改。
	if !events[cloudCaptureEvent] {
		t.Errorf("bridge.ts 的 supportedEvents 必须声明 %q", cloudCaptureEvent)
	}
}

// 契约 2.2：cloud.poll 与 wxapi.poll 同一套分页规则 —— 缺省 500、钳制 1..2000、
// 响应仍是数组。空缓冲下的钳制边界与真实记录下的分页都在这里走一遍。
func TestCloudPollLimitPagesTheBuffer(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	// 空缓冲：缺省与三个越界值都只能回 []，不能回 null（列表消费方会崩）。
	for _, params := range []string{`{}`, `{"limit":0}`, `{"limit":-5}`, `{"limit":999999}`} {
		result, err := app.router.Call(context.Background(), "cloud.poll", json.RawMessage(params))
		if err != nil {
			t.Fatalf("cloud.poll %s: %v", params, err)
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("cloud.poll %s marshal: %v", params, err)
		}
		if string(data) != "[]" {
			t.Fatalf("cloud.poll %s payload = %s, want []", params, data)
		}
	}
}

// cloud.poll 的 limit 与 wxapi.poll 同规则（缺省 500、钳 1..2000），且消费语义不变：
// 一页一条地取恰好取完，不重不漏。这里用真实 Core（假 Core）喂三条记录。
func TestCloudPollLimitClampsLikeWxapi(t *testing.T) {
	app := startFakeCoreApp(t)
	defer app.shutdown(context.Background())

	if err := app.EngineStart(62000); err != nil {
		t.Fatalf("engine.start: %v", err)
	}
	if envelope := app.Call("cloud.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("cloud.start: %s", envelope)
	}
	waitFor(t, 10*time.Second, "三条云函数记录进入 poll 缓冲", func() bool {
		return readCloudStats(t, app).Pending == 3
	})

	// limit=0 与负数读作最小页 1 条，绝不是「取全部」：若 0 被当成缺省值，这里会
	// 一次拿回三条。超大值钳到上限而不是报错。
	for _, params := range []string{`{"limit":0}`, `{"limit":-1}`, `{"limit":999999}`} {
		page := cloudPoll(t, app, params)
		if len(page) != 1 {
			t.Fatalf("cloud.poll %s 返回 %d 条, want 1（钳制后的一页）", params, len(page))
		}
	}
	// 缓冲排空后回到空数组：分页不改变响应形状。
	if envelope := app.Call("cloud.poll", `{}`); !strings.Contains(envelope, `"result":[]`) {
		t.Fatalf("空缓冲必须回 []，got %s", envelope)
	}
}

// 云函数更新流的端到端链路（2.2）：假 Core 的一页里同时有「仍待落定的记录」与
// 「它的落定帧」，两者 rid 相同。记录必须先入库、更新后落定，否则 UPDATE 匹配不到
// 行，库里永远停在「等待中」—— 这正是更新流要修掉的缺陷。真实 SQLite，不 mock 存储。
//
// 这一条同时证明 cloudFeeder 确实挂在更新流上：若它仍是 NewHookFeeder（apply 为
// nil），下面的行只会是 pending、duration_ms 永远为 0。
func TestCloudSettledUpdateReachesStorageAndPoll(t *testing.T) {
	app := startFakeCoreApp(t)
	defer app.shutdown(context.Background())

	if err := app.EngineStart(62000); err != nil {
		t.Fatalf("engine.start: %v", err)
	}
	if envelope := app.Call("cloud.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("cloud.start: %s", envelope)
	}

	var settled traffic.TrafficSummary
	waitFor(t, 10*time.Second, "落定的云函数记录进入 SQLite", func() bool {
		page, err := app.TrafficList(api.TrafficListParams{PageSize: 100})
		if err != nil {
			return false
		}
		for _, item := range page.Items {
			if item.ID == settledRecordRID {
				settled = item
				return item.Status == traffic.StatusSuccess && item.DurationMs == 1830
			}
		}
		return false
	})
	if settled.DurationMs == 0 {
		t.Fatalf("落定帧没有把耗时写进库: %+v", settled)
	}
	if settled.ResponseBytes == 0 {
		t.Fatalf("落定帧没有把返回值写进库: %+v", settled)
	}

	// 面板的 poll 看到的是同一份事实：更新帧要就地把缓冲里的记录补成终态。
	//
	// 这里必须等 poll 自己给出答案，不能等库落定后就断言。库由同一个 tick 的
	// ingest/apply 写，而 pending 缓冲要等这次 tick **更靠后的写回段**才填
	// （pushPending 与 patchPending 都在 ingest/apply 之后，同一把锁内），所以
	// 「库里已落定」并不蕴含「缓冲里已经有」。等在 A 上、断言在 B 上，就是这条
	// 用例在 -race 下偶发失败、报 {"result":[]} 的成因：TrafficList(库) 比写回段
	// 慢一些，窗口很窄，所以只在调度被拉长时才露头。
	waitFor(t, 10*time.Second, "poll 返回落定后的云函数记录", func() bool {
		polled := app.Call("cloud.poll", `{}`)
		return strings.Contains(polled, `"rid":"`+settledRecordRID+`"`) &&
			strings.Contains(polled, `"status":"success"`) &&
			strings.Contains(polled, `"durationMs":1830`)
	})

	stats := readCloudStats(t, app)
	if !stats.Running || stats.Dropped != 0 {
		t.Fatalf("cloud.stats 与捕获状态不符: %#v", stats)
	}
	// 更新游标已推进：这一 tick 的落定帧被确认过，不会被下一 tick 重投。
	if stats.UpdateAck != 1 {
		t.Fatalf("更新游标未推进: %#v", stats)
	}
}

// 2.1：页面的 rid 公式是 "<type>-<appId>-<ts>-<seq>"，cloud.Convert 在记录没带 rid
// 时按同一公式兜底（旧版页面送的记录）。两侧必须逐字段一致，否则事件流与 poll 两条
// 投递路径会对同一次调用算出两个身份，去重与就地补丁全部失效。
//
// 这一条钉住 Go 侧；JS 侧用同样的输入钉在 core/src/hooks/cloud-hook-vm.test.ts 的
// "derives rid exactly like traffic_records.id"。
func TestCloudConvertRidFallbackMatchesThePageFormula(t *testing.T) {
	drained := engine.HookDrainedRecord{Seq: 7, Record: map[string]any{
		"type": "function", "name": "hello", "appId": "wxone",
		"ts": float64(1700000000000), "status": "success",
	}}
	if rec := cloud.Convert("cloud", drained); rec.ID != "function-wxone-1700000000000-7" {
		t.Fatalf("Convert 的兜底 rid = %q, want %q", rec.ID, "function-wxone-1700000000000-7")
	}

	// appId 缺失时两处同样收敛：页面在 rid 里留空段，Go 的 asString 也得到空串
	if rec := cloud.Convert("cloud", engine.HookDrainedRecord{Seq: 9, Record: map[string]any{
		"ts": float64(1700000000000), "type": "function",
	}}); rec.ID != "function--1700000000000-9" {
		t.Fatalf("空 appId 的兜底 rid = %q, want %q", rec.ID, "function--1700000000000-9")
	}

	// 页面带了 rid 时以它为准：同一条记录的两条投递路径（事件流 / poll）必须收敛到
	// 同一个存储行，公式只在旧记录上兜底。
	withRid := drained
	withRid.Record = map[string]any{
		"rid": "function-wxone-1700000000000-7", "type": "function", "appId": "wxone",
		"ts": float64(1700000000000), "status": "success",
	}
	if rec := cloud.Convert("cloud", withRid); rec.ID != "function-wxone-1700000000000-7" {
		t.Fatalf("Convert 必须优先取 rid: %q", rec.ID)
	}
}

// 2.2：落定帧 → traffic.Record 的映射按 hook 名参数化，wxapi 与 cloud 共用一份逻辑，
// 存储身份仍由 rid 决定（两个 hook 的同名帧落到各自的行上）。hookName 只影响没有
// type 的帧的 API 类型兜底，不影响 ID —— ID 一旦按 hook 名拼，两条投递路径就会分叉。
func TestSettledUpdateFrameIsParameterizedByHookName(t *testing.T) {
	frame := map[string]any{"rid": "r1", "status": "success", "durationMs": float64(12)}

	cloudRecord, ok := settledTrafficRecordFor("cloud", frame)
	if !ok {
		t.Fatal("合法的落定帧必须被接受")
	}
	if cloudRecord.ID != "r1" || cloudRecord.APIType != "cloud" {
		t.Fatalf("cloud 落定帧映射错误: %+v", cloudRecord)
	}
	if cloudRecord.DurationMs != 12 || cloudRecord.Status != traffic.StatusSuccess {
		t.Fatalf("落定字段映射错误: %+v", cloudRecord)
	}

	wxapiRecord, ok := settledTrafficRecordFor("wxapi", frame)
	if !ok {
		t.Fatal("wxapi 分支必须共用同一套校验")
	}
	if wxapiRecord.ID != cloudRecord.ID {
		t.Fatalf("ID 必须由 rid 决定，与 hook 名无关: %q vs %q", wxapiRecord.ID, cloudRecord.ID)
	}
	if wxapiRecord.APIType != "wxapi" {
		t.Fatalf("wxapi 落定帧的 API 类型兜底错误: %+v", wxapiRecord)
	}

	// 单参入口是 wxapi_contract_test.go 冻结的形状（本波不能改那个文件）。
	shim, ok := settledTrafficRecord(frame)
	if !ok || shim.ID != wxapiRecord.ID || shim.APIType != wxapiRecord.APIType {
		t.Fatalf("settledTrafficRecord 必须是 wxapi 分支的入口: %+v", shim)
	}

	// 未落定的状态一律拒绝：cloud.Convert 会把未知状态写成 pending，落定行会被
	// ApplyUpdates 回退成「等待中」。
	for _, bad := range []map[string]any{
		{"status": "success"},
		{"rid": "r1"},
		{"rid": "r1", "status": "pending"},
	} {
		if record, ok := settledTrafficRecordFor("cloud", bad); ok {
			t.Fatalf("非法落定帧 %v 必须被拒绝, got %+v", bad, record)
		}
	}
}

// 跨层容量镜像（roadmap §4）：页面两条缓冲的重投递窗口必须落在 shell 去重 FIFO 的
// 容量之内。wxapi.js 的那份由 internal/api/ipc 的测试守（它读得到 deliveredCapacity
// 常量）；cloud.js 现在也开出了同名的两条缓冲，这里用源码解析 + 常量解析来守同一
// 条不变量，两侧都不硬编码数字。
func TestCloudPageBuffersFitTheShellDedupFIFO(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	hookSource, err := os.ReadFile(filepath.Join(root, "core", "hooks", "cloud.js"))
	if err != nil {
		t.Fatalf("读取 core/hooks/cloud.js: %v", err)
	}
	recordCapacity := hookCapacity(t, string(hookSource), "RECORD_BUFFER_CAPACITY")
	updateCapacity := hookCapacity(t, string(hookSource), "UPDATE_BUFFER_CAPACITY")

	feederSource, err := os.ReadFile(filepath.Join("internal", "api", "ipc", "hookfeeder.go"))
	if err != nil {
		t.Fatalf("读取 hookfeeder.go: %v", err)
	}
	delivered := goConst(t, string(feederSource), "deliveredCapacity")
	if recordCapacity+updateCapacity > delivered {
		t.Fatalf("deliveredCapacity = %d 必须覆盖 cloud.js 两条缓冲的重投递窗口：记录 %d + 更新 %d = %d",
			delivered, recordCapacity, updateCapacity, recordCapacity+updateCapacity)
	}
}

// R10(b) 对云函数同样适用（路线图 §2.1）：cloud.clear 必须先清页面缓冲（HookClear
// 是一次 CDP 往返），再清 shell 侧的 pending。反过来的话，这段往返窗口里 500ms 一次的
// drain tick 会把页面缓冲里还没被清掉的记录重新 drain 回 pending —— 用户刚清空的云函数
// 面板立刻又冒出旧记录。这条顺序没有运行时接缝可测（要造出窗口得让「页面缓冲先被清掉」
// 晚于一个 tick，只有真实 Core 进程能给出），所以在注册段上做结构断言：两行对调即红。
func TestCloudClearClearsThePageBeforeTheShellBuffer(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(root, "desktop", "ipc_bridge.go"))
	if err != nil {
		t.Fatalf("read ipc_bridge.go: %v", err)
	}
	block := handlerSource(string(source), `r.Register("cloud.clear"`)
	if block == "" {
		t.Fatal("ipc_bridge.go 里找不到 cloud.clear 的注册段")
	}
	page := strings.Index(block, "HookClear(")
	buffer := strings.Index(block, "cloudFeeder.Clear()")
	if page < 0 || buffer < 0 {
		t.Fatalf("cloud.clear 必须同时清页面缓冲与 shell 缓冲: HookClear@%d, feeder.Clear@%d", page, buffer)
	}
	if page > buffer {
		t.Fatal("cloud.clear 必须先 HookClear 再 cloudFeeder.Clear()：反过来的窗口内，tick 会把已清记录重新 drain 回 pending")
	}
}

// F2 与 wxapi 对齐：一 tick 的多条记录 / 多条落定帧只发一条事件，载荷是数组。云函数
// 侧原先是把批回调扇出成逐条 emit —— 一 tick 200 条就是 200 次 EventsEmit、200 次 JSON
// 序列化和 200 次 webview 派发，事件率被记录率拖着走。前端 CloudView 已经同时接受数组
// 与单对象，所以这次只改后端的投递形状。
//
// 这里走真实接线（setupIPC 里注册的那个 cloudFeeder）：假 Core 一页喂三条记录 + 一条
// 落定帧，emitSink 收下后端真正投出去的事件。
func TestCloudDeliversOneBatchEventPerTick(t *testing.T) {
	app := startFakeCoreApp(t)
	defer app.shutdown(context.Background())

	var mu sync.Mutex
	events := map[string][]any{}
	emitSink = func(name string, payload any) {
		mu.Lock()
		defer mu.Unlock()
		events[name] = append(events[name], payload)
	}
	t.Cleanup(func() { emitSink = nil })
	batches := func(name string) []any {
		mu.Lock()
		defer mu.Unlock()
		return append([]any(nil), events[name]...)
	}

	if err := app.EngineStart(62000); err != nil {
		t.Fatalf("engine.start: %v", err)
	}
	if envelope := app.Call("cloud.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("cloud.start: %s", envelope)
	}
	waitFor(t, 10*time.Second, "一 tick 的云函数事件到齐", func() bool {
		return len(batches(cloudCaptureEvent)) == 1 && len(batches(cloudUpdateEvent)) == 1
	})

	captures := batches(cloudCaptureEvent)
	capture, ok := captures[0].([]map[string]any)
	if !ok {
		t.Fatalf("cloud_capture 的载荷必须是数组: %#v", captures[0])
	}
	if len(capture) != 3 {
		t.Fatalf("一 tick 的三条记录必须合成一条事件，实际 %d 条", len(capture))
	}
	for i, want := range []float64{1700000000000, 1700000000001, 1700000000002} {
		if capture[i]["ts"] != want {
			t.Fatalf("批内顺序必须与页内一致: %#v", capture)
		}
	}

	updates := batches(cloudUpdateEvent)
	frames, ok := updates[0].([]map[string]any)
	if !ok {
		t.Fatalf("cloud_update 的载荷必须是数组: %#v", updates[0])
	}
	if len(frames) != 1 || frames[0]["rid"] != settledRecordRID || frames[0]["status"] != "success" {
		t.Fatalf("落定帧载荷必须是一批内层 update 对象: %#v", frames)
	}
	if _, wrapped := frames[0]["update"]; wrapped {
		t.Fatalf("事件载荷是内层 update 对象，不是 {seq, update} 外层: %#v", frames[0])
	}

	// 空 tick 保持安静：2 次/秒的空事件是纯开销。
	time.Sleep(1200 * time.Millisecond)
	if got := batches(cloudCaptureEvent); len(got) != 1 {
		t.Fatalf("空 tick 又发了 %d 条 cloud_capture", len(got))
	}
	if got := batches(cloudUpdateEvent); len(got) != 1 {
		t.Fatalf("空 tick 又发了 %d 条 cloud_update", len(got))
	}
}

// R10(b) 对 hook.clear 同样适用：它一次覆盖三个钩子（wxapi / cloud /
// console），两行也必须按同一顺序 —— 先清页面缓冲（HookClear 是一次 CDP 往返），再清
// shell 侧的 pending。反过来的话，这段往返窗口里 500ms 一次的 drain tick 会把页面缓冲里
// 还没被清掉的记录重新 drain 回 pending，用户刚清空的记录立刻复活。这条顺序同样没有运行
// 时接缝可测（要造出窗口得让「页面缓冲先被清掉」晚于一个 tick，只有真实 Core 进程能给
// 出），所以在注册段上做结构断言：两行对调即红。
func TestHookClearClearsThePageBeforeTheShellBuffers(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(root, "desktop", "ipc_bridge.go"))
	if err != nil {
		t.Fatalf("read ipc_bridge.go: %v", err)
	}
	block := handlerSource(string(source), `r.Register("hook.clear"`)
	if block == "" {
		t.Fatal("ipc_bridge.go 里找不到 hook.clear 的注册段")
	}
	page := strings.Index(block, "HookClear(")
	shell := strings.Index(block, "feeder.Clear()")
	if page < 0 || shell < 0 {
		t.Fatalf("hook.clear 必须同时清页面缓冲与 shell 缓冲: HookClear@%d, feeder.Clear@%d", page, shell)
	}
	if page > shell {
		t.Fatal("hook.clear 必须先 HookClear 再 feeder.Clear()：反过来的窗口内，tick 会把已清记录重新 drain 回 pending")
	}
	// 三个钩子一起对齐：哪一路都要能解析出对应的 feeder，缺一路就是漏清。
	for _, field := range []string{"a.wxapiFeeder", "a.cloudFeeder", "a.consoleFeeder"} {
		if !strings.Contains(block, field) {
			t.Fatalf("hook.clear 必须覆盖 %s（三个钩子一起对齐）", field)
		}
	}
}

// hookCapacity 从钩子源码里解析 `<name> = <数值>`；解析不到直接 Fatal —— 静默跳过
// 等于这条不变量根本没被守住。
func hookCapacity(t *testing.T, source, name string) int {
	t.Helper()
	match := regexp.MustCompile(name + `\s*=\s*(\d+)`).FindStringSubmatch(source)
	if match == nil {
		t.Fatalf("core/hooks/cloud.js 里没有 %s = <数值> 的定义，跨层容量断言无法进行", name)
	}
	value, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("%s 不是整数: %v", name, err)
	}
	return value
}

// goConst 从 Go 源码里解析 `<name> = <数值>` 常量。
func goConst(t *testing.T, source, name string) int {
	t.Helper()
	match := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*(\d+)`).FindStringSubmatch(source)
	if match == nil {
		t.Fatalf("hookfeeder.go 里没有 %s = <数值> 的常量定义", name)
	}
	value, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("%s 不是整数: %v", name, err)
	}
	return value
}
