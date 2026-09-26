package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/api"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// settledRecordRID is the storage identity of the synthetic slow call the fake
// Core reports: its record is captured pending and its update frame settles it
// in the same drain page. The formula is the page's (<type>-<appId>-<ts>-<seq>).
const settledRecordRID = "wx.downloadFile-wxfake123-1700000000002-3"

// 契约 5：wxapi.stats 是面板判断"到底有没有在捕获、丢了多少"的唯一来源，
// 七个字段缺一不可（掉线也算一种可渲染状态，不能报错）。pageDropped* 是 R12 的
// 页面侧丢弃读数（补 shell 的 dropped），未接入前恒为 0，但必须存在 —— 前端按
// 存在性判断，缺字段会被读成「未知」。storageAvailable 是 App 侧的存储可用性
// （repo == nil 的降级模式下 feeder 照常 capture，面板只能靠它发现历史库在丢数据），
// 这里没有 repo，所以必须显式输出 false —— 缺失会被前端读成 true。
func TestWxapiStatsReportsBufferAndCursors(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")

	result, err := app.router.Call(context.Background(), "wxapi.stats", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("wxapi.stats must answer with no engine running: %v", err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"running"`, `"pending"`, `"dropped"`, `"ack"`, `"updateAck"`,
		`"pageDroppedRecords"`, `"pageDroppedUpdates"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("wxapi.stats payload %s lacks %s", data, key)
		}
	}
	if !strings.Contains(string(data), `"storageAvailable":false`) {
		t.Fatalf("无 repo 的 wxapi.stats 必须显式输出 storageAvailable:false（缺失会被前端读成 true）: %s", data)
	}
	var stats struct {
		Running            bool  `json:"running"`
		Pending            int   `json:"pending"`
		Dropped            int64 `json:"dropped"`
		Ack                int64 `json:"ack"`
		UpdateAck          int64 `json:"updateAck"`
		PageDroppedRecords int64 `json:"pageDroppedRecords"`
		PageDroppedUpdates int64 `json:"pageDroppedUpdates"`
		StorageAvailable   bool  `json:"storageAvailable"`
	}
	if err := json.Unmarshal(data, &stats); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	if stats.Running || stats.Pending != 0 || stats.Dropped != 0 {
		t.Fatalf("an unstarted feeder must report idle: %s", data)
	}
	if stats.PageDroppedRecords != 0 || stats.PageDroppedUpdates != 0 {
		t.Fatalf("an unstarted feeder has no page reading to report: %s", data)
	}
	if stats.StorageAvailable {
		t.Fatalf("repo == nil 时 storageAvailable 必须是 false: %s", data)
	}
}

// storageAvailable 的另一面：repo 非 nil（正常启动，storage 初始化成功）时必须报
// true。这个字段是实时面板区分「正常捕获」与「实时有记录、历史库一条不进」的唯一
// 依据，两个方向（true / false）都得钉住，false 的方向在上面无 repo 的用例里。
func TestWxapiStatsReportsStorageAvailableWithARepo(t *testing.T) {
	app := newTrafficContractApp(t)

	raw := app.Call("wxapi.stats", `{}`)
	if !strings.Contains(raw, `"storageAvailable":true`) {
		t.Fatalf("repo 非 nil 时 wxapi.stats 必须报 storageAvailable:true: %s", raw)
	}
}

// 契约 5：wxapi 的事件名由后端发出、由面板监听。两边任一改名都会让落定结果静默
// 消失（事件不生效、poll 又已经把它取走），所以实现与测试共用同一个常量，并断言
// 它的字面量与 bridge.ts 的 supportedEvents 一致。这里刻意不断言 ipc_bridge.go 的
// 源码文本：那种断言改一个局部变量名就红，却证明不了前端到底认不认这个名字。
func TestWxapiEventNamesArePinnedOnBothSides(t *testing.T) {
	// 常量本身的字面量就是契约：前端按字符串监听，Go 侧不能悄悄改名。
	if wxapiCaptureEvent != "wxapi_capture" {
		t.Errorf("wxapiCaptureEvent = %q, want %q", wxapiCaptureEvent, "wxapi_capture")
	}
	if wxapiUpdateEvent != "wxapi_update" {
		t.Errorf("wxapiUpdateEvent = %q, want %q", wxapiUpdateEvent, "wxapi_update")
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
	for _, name := range []string{wxapiCaptureEvent, wxapiUpdateEvent} {
		if !events[name] {
			t.Errorf("bridge.ts 的 supportedEvents 必须声明 %q（契约 6）", name)
		}
	}
	if methods := parseDeclaredSet(string(source), "const supportedMethods = new Set"); !methods["wxapi.stats"] {
		t.Error("bridge.ts 的 supportedMethods 必须声明 'wxapi.stats'（契约 6）")
	}
}

// R6(a)：落定帧必须带合法的 status。cloud.Convert 对未知值会写 pending，于是
// 已落定的行会被 ApplyUpdates 回退成「等待中」——缺失、拼错、大小写不符都必须被
// 拒绝，只有 success / fail 才允许写库。
func TestSettledTrafficRecordAcceptsOnlySettledStatuses(t *testing.T) {
	cases := []struct {
		name   string
		update map[string]any
		ok     bool
		status traffic.Status
	}{
		{"成功", map[string]any{"rid": "r1", "status": "success", "durationMs": float64(12)}, true, traffic.StatusSuccess},
		{"失败", map[string]any{"rid": "r1", "status": "fail", "error": "boom"}, true, traffic.StatusFail},
		{"缺失 status", map[string]any{"rid": "r1", "durationMs": float64(12)}, false, ""},
		{"拼错 status", map[string]any{"rid": "r1", "status": "sucess"}, false, ""}, //nolint:misspell // 故意的拼写错误：页面侧真会送出这种值，它必须被拒绝。
		{"未落定 status", map[string]any{"rid": "r1", "status": "pending"}, false, ""},
		{"大小写不符", map[string]any{"rid": "r1", "status": "Success"}, false, ""},
		{"非字符串 status", map[string]any{"rid": "r1", "status": 1}, false, ""},
		{"空 status", map[string]any{"rid": "r1", "status": ""}, false, ""},
		{"缺 rid", map[string]any{"status": "success"}, false, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			record, ok := settledTrafficRecord(test.update)
			if ok != test.ok {
				t.Fatalf("ok = %v, want %v", ok, test.ok)
			}
			if !test.ok {
				if record.ID != "" || record.Status != "" {
					t.Fatalf("被拒绝的帧不得产出记录: %+v", record)
				}
				return
			}
			if record.Status != test.status {
				t.Fatalf("status = %q, want %q", record.Status, test.status)
			}
			if record.ID != "r1" {
				t.Fatalf("rid 必须是存储行 ID: %q", record.ID)
			}
		})
	}
}

// R10(b)：wxapi.clear 必须先清页面缓冲（HookClear 是一次 CDP 往返），再清 shell 侧的
// pending。反过来的话，这段往返窗口里 500ms 一次的 drain tick 会把页面缓冲里还没被清掉
// 的记录重新 drain 回 pending —— 用户刚清空的面板立刻又冒出旧记录。
//
// 这个顺序没有运行时接缝可测：唯一能造出窗口的观测点是真实 Core 进程（要让「页面缓冲
// 先被清掉」这件事晚于一个 tick 才发生），所以这里对注册段做结构断言。顺序就是这段源码
// 的性质：把两行对调，本用例立刻变红。
func TestWxapiClearClearsThePageBeforeTheShellBuffer(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(root, "desktop", "ipc_bridge.go"))
	if err != nil {
		t.Fatalf("read ipc_bridge.go: %v", err)
	}
	block := handlerSource(string(source), `r.Register("wxapi.clear"`)
	if block == "" {
		t.Fatal("ipc_bridge.go 里找不到 wxapi.clear 的注册段")
	}
	page := strings.Index(block, "HookClear(")
	buffer := strings.Index(block, "wxapiFeeder.Clear()")
	if page < 0 || buffer < 0 {
		t.Fatalf("wxapi.clear 必须同时清页面缓冲与 shell 缓冲: HookClear@%d, feeder.Clear@%d", page, buffer)
	}
	if page > buffer {
		t.Fatal("wxapi.clear 必须先 HookClear 再 feeder.Clear()：反过来的窗口内，tick 会把已清记录重新 drain 回 pending")
	}
}

// wxapi.clear 的响应形状不变（{ok:true}），调用返回后 shell 侧的 poll 缓冲必须为空。
func TestWxapiClearEmptiesThePollBuffer(t *testing.T) {
	app := startFakeCoreApp(t)
	defer app.shutdown(context.Background())

	if err := app.EngineStart(62000); err != nil {
		t.Fatalf("engine.start: %v", err)
	}
	if envelope := app.Call("wxapi.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.start: %s", envelope)
	}
	waitFor(t, 10*time.Second, "三条记录进入 poll 缓冲", func() bool {
		return wxapiPending(t, app) == 3
	})

	cleared := app.Call("wxapi.clear", `{}`)
	if !strings.Contains(cleared, `"ok":true`) {
		t.Fatalf("wxapi.clear 的响应形状变了: %s", cleared)
	}
	if stats := app.Call("wxapi.stats", `{}`); !strings.Contains(stats, `"pending":0`) {
		t.Fatalf("clear 之后 pending 必须归零: %s", stats)
	}
	if records := wxapiPoll(t, app, `{}`); len(records) != 0 {
		t.Fatalf("clear 之后 poll 必须为空: %#v", records)
	}
}

// handlerSource 截取一段 r.Register(...) 处理段：从声明处到下一个注册项。
func handlerSource(source, declaration string) string {
	start := strings.Index(source, declaration)
	if start < 0 {
		return ""
	}
	rest := source[start:]
	if next := strings.Index(rest[len(declaration):], "\n\tr.Register("); next >= 0 {
		return rest[:len(declaration)+next]
	}
	return rest
}

// parseDeclaredSet extracts the string literals of a `const NAME = new Set(…)`
// declaration, ignoring any generic argument between the name and the call.
func parseDeclaredSet(source, declaration string) map[string]bool {
	start := strings.Index(source, declaration)
	if start < 0 {
		return nil
	}
	open := strings.Index(source[start:], "[")
	end := strings.Index(source[start:], "]);")
	if open < 0 || end < 0 || open > end {
		return nil
	}
	block := source[start+open+1 : start+end]
	values := map[string]bool{}
	for _, quote := range []string{"'", "`"} {
		for rest := block; ; {
			from := strings.Index(rest, quote)
			if from < 0 {
				break
			}
			to := strings.Index(rest[from+1:], quote)
			if to < 0 {
				break
			}
			if name := rest[from+1 : from+1+to]; name != "" && !strings.ContainsAny(name, " \n\t") {
				values[name] = true
			}
			rest = rest[from+1+to+1:]
		}
	}
	return values
}

// The desktop chain the panel depends on, end to end: a slow call is captured
// while it is still pending, and its settled frame arrives in the same drain
// page. The record must be stored first and the update applied after it, or the
// UPDATE matches no row and the record stays pending in the history view - the
// very defect this stream exists to fix.
func TestSettledUpdateReachesStorageAndPoll(t *testing.T) {
	app := startFakeCoreApp(t)
	defer app.shutdown(context.Background())

	if err := app.EngineStart(62000); err != nil {
		t.Fatalf("engine.start: %v", err)
	}
	if envelope := app.Call("wxapi.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.start: %s", envelope)
	}

	var settled traffic.TrafficSummary
	waitFor(t, 10*time.Second, "the settled record in SQLite", func() bool {
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
	if settled.ResponseBytes == 0 {
		t.Fatalf("the settled update carried no response bytes: %+v", settled)
	}

	// The same settled state is what the panel's poll returns: an update must
	// patch the buffered record in place, not just storage.
	//
	// 与 cloud 侧同一条用例同源：库由 tick 的 ingest/apply 写，而 pending 缓冲
	// 要等这次 tick 更靠后的写回段才填，所以「库里已落定」不蕴含「缓冲里已经有」。
	// 必须等 poll 自己给出答案，否则就是在 A 上等待、在 B 上断言。
	waitFor(t, 10*time.Second, "poll 返回落定后的记录", func() bool {
		polled := app.Call("wxapi.poll", `{}`)
		return strings.Contains(polled, `"rid":"`+settledRecordRID+`"`) &&
			strings.Contains(polled, `"status":"success"`) &&
			strings.Contains(polled, `"durationMs":1830`)
	})

	stats := app.Call("wxapi.stats", `{}`)
	for _, want := range []string{`"running":true`, `"dropped":0`} {
		if !strings.Contains(stats, want) {
			t.Fatalf("wxapi.stats %s lacks %s", stats, want)
		}
	}
}
