package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The console panel reads the ring by sequence number, so the contract is
// "everything after the last seq I saw" — a null payload or a dropped seq
// would leave the view permanently behind.
func TestConsoleListReturnsSequenceOrderedRecords(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	app.consoleLog.Append([]map[string]any{
		{"level": "log", "text": "first"},
		{"level": "error", "text": "second"},
	}, app.consoleLog.Epoch())

	result, err := app.router.Call(context.Background(), "console.list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("console.list: %v", err)
	}
	payload, _ := json.Marshal(result)
	var decoded struct {
		Records []struct {
			Seq    int64          `json:"seq"`
			Record map[string]any `json:"record"`
		} `json:"records"`
		NextSeq int64 `json:"nextSeq"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Records) != 2 || decoded.Records[0].Seq >= decoded.Records[1].Seq {
		t.Fatalf("records: %s", payload)
	}
	if decoded.Records[1].Record["text"] != "second" {
		t.Fatalf("record payload lost: %s", payload)
	}
	if decoded.NextSeq != decoded.Records[1].Seq {
		t.Fatalf("nextSeq %d must be the last handed-out seq: %s", decoded.NextSeq, payload)
	}

	// Reading again from nextSeq yields nothing new: this is what keeps the
	// panel free of duplicated rows.
	again, err := app.router.Call(context.Background(), "console.list",
		json.RawMessage(`{"afterSeq":2}`))
	if err != nil {
		t.Fatalf("console.list afterSeq: %v", err)
	}
	if data, _ := json.Marshal(again); string(data) != `{"dropped":0,"hasMore":false,"nextSeq":2,"records":[]}` {
		t.Fatalf("incremental read: %s", data)
	}
}

// console.clear has to work with no engine running (the user clears the view
// before connecting): clearing the page-side buffer is best-effort, not a
// precondition.
func TestConsoleClearWorksOffline(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")

	app.consoleLog.Append([]map[string]any{{"level": "log", "text": "gone"}}, app.consoleLog.Epoch())

	if _, err := app.router.Call(context.Background(), "console.clear", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("console.clear: %v", err)
	}
	// A panel that had already seen seq 1 asks for what comes after it; the
	// clear must not rewind that marker into rows that no longer exist.
	result, err := app.router.Call(context.Background(), "console.list", json.RawMessage(`{"afterSeq":1}`))
	if err != nil {
		t.Fatalf("console.list: %v", err)
	}
	data, _ := json.Marshal(result)
	if string(data) != `{"dropped":0,"hasMore":false,"nextSeq":1,"records":[]}` {
		t.Fatalf("after clear: %s", data)
	}
}

// 清空后，清空之前就在途的那一批绝不能把行带回来（否则用户刚清掉的内容会以
// 新序号复活）。
func TestConsoleAppendDropsBatchesFromBeforeAClear(t *testing.T) {
	log := &consoleLog{}
	epoch := log.Epoch()
	log.Append([]map[string]any{{"text": "before"}}, epoch)
	log.Clear()

	// 采集器在 Clear 之前取到的 epoch，Clear 之后才 Append。
	log.Append([]map[string]any{{"text": "stale-batch"}}, epoch)

	got, _, _, _ := log.List(0, 10)
	if len(got) != 0 {
		t.Fatalf("a batch drained before the clear must be dropped: %#v", got)
	}
	// 新代号下的记录照常进入。
	log.Append([]map[string]any{{"text": "after"}}, log.Epoch())
	got, _, _, _ = log.List(0, 10)
	if len(got) != 1 || got[0].Record["text"] != "after" {
		t.Fatalf("post-clear records must still land: %#v", got)
	}
}

// tail 请求的是最新 N 条：从 0 开始的增量读在环形缓冲写满之后返回的是最旧
// 一批，与「最近输出」正好相反（MCP 工具就踩过这个）。
func TestConsoleListTailReturnsTheNewestRecords(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	records := make([]map[string]any, 0, consoleCapacity+20)
	for i := 0; i < consoleCapacity+20; i += 1 {
		records = append(records, map[string]any{"text": i})
	}
	app.consoleLog.Append(records, app.consoleLog.Epoch())

	result, err := app.router.Call(context.Background(), "console.list", json.RawMessage(`{"tail":3}`))
	if err != nil {
		t.Fatalf("console.list tail: %v", err)
	}
	payload, _ := json.Marshal(result)
	var decoded struct {
		Records []struct {
			Seq    int64          `json:"seq"`
			Record map[string]any `json:"record"`
		} `json:"records"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Records) != 3 {
		t.Fatalf("tail must return the requested count: %s", payload)
	}
	if decoded.Records[2].Record["text"] != float64(consoleCapacity+19) {
		t.Fatalf("tail must end at the newest record: %s", payload)
	}
}

// tail 的 JSON 契约永远是数组：空环返回 [] 而不是 null（null 会打断按数组
// 迭代的调用方——MCP 工具与前端轮询都这样消费）。超大 limit/tail 同样要
// 如实回答：环形缓冲的上限就是答案的上限，钳位只防无界入参。
func TestConsoleListTailOnAnEmptyRingIsAnEmptyArray(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	for _, params := range []string{`{"tail":5}`, `{"tail":999999}`, `{"limit":999999}`} {
		result, err := app.router.Call(context.Background(), "console.list", json.RawMessage(params))
		if err != nil {
			t.Fatalf("console.list %s: %v", params, err)
		}
		data, _ := json.Marshal(result)
		if !strings.Contains(string(data), `"records":[]`) {
			t.Fatalf("console.list %s must answer with an empty array: %s", params, data)
		}
	}
}

// 缺口必须能被读到：暂停或断连太久时最旧的记录会被淘汰，界面据此提示
// 「已丢弃 N 条」，而不是静默显示一段缺口。
func TestConsoleListReportsTheEvictedGap(t *testing.T) {
	log := &consoleLog{}
	records := make([]map[string]any, 0, consoleCapacity+10)
	for i := 0; i < consoleCapacity+10; i += 1 {
		records = append(records, map[string]any{"text": i})
	}
	log.Append(records, log.Epoch())

	// 调用方停在 seq 1，但 1..10 已经被淘汰。
	got, _, _, dropped := log.List(1, 10)
	if dropped != 9 {
		t.Fatalf("dropped = %d, want 9 (seq 2..10 evicted)", dropped)
	}
	if len(got) != 10 || got[0].Seq != 11 {
		t.Fatalf("first readable record must be the oldest survivor: %#v", got[0])
	}

	// 跟上之后不再报缺口。
	_, nextSeq, _, dropped := log.List(10, 10)
	if dropped != 0 || nextSeq != 20 {
		t.Fatalf("catching up must report no gap: dropped=%d nextSeq=%d", dropped, nextSeq)
	}
}

// 清空之后的缺口口径：序号空间跨 Clear 单调递增，而前端清空时会把读标记归零
// （stores/engine.ts 的 clearConsole），于是下一次读的 afterSeq 是 0 而环形缓冲里
// 的第一条已经是清空之后的序号 —— 中间那段是用户主动清掉的，不是溢出淘汰的，
// 报成「已丢弃 N 条」会让刚清空的面板立刻挂出一条过载警告。
func TestConsoleListDoesNotCountClearedRecordsAsDropped(t *testing.T) {
	log := &consoleLog{}
	for i := 0; i < 5; i += 1 {
		log.Append([]map[string]any{{"text": i}}, log.Epoch())
	}
	log.Clear()
	log.Append([]map[string]any{{"text": "after"}}, log.Epoch())

	got, nextSeq, _, dropped := log.List(0, 10)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0: seq 1..5 was cleared on purpose, not evicted", dropped)
	}
	if len(got) != 1 || nextSeq != 6 {
		t.Fatalf("post-clear read: %d records, nextSeq %d", len(got), nextSeq)
	}
}

// 清空只遮掉它之前的那一段：清空之后真正的环形缓冲淘汰仍要报出来，否则「已丢弃」
// 提示会被这条规则一起吞掉。
func TestConsoleListStillReportsEvictionsAfterAClear(t *testing.T) {
	log := &consoleLog{}
	log.Append([]map[string]any{{"text": "cleared"}}, log.Epoch())
	log.Clear()

	records := make([]map[string]any, 0, consoleCapacity+10)
	for i := 0; i < consoleCapacity+10; i += 1 {
		records = append(records, map[string]any{"text": i})
	}
	log.Append(records, log.Epoch())

	// 序号 1 被 Clear 遮掉，2..11 是清空之后真正被淘汰的 10 条，环形缓冲留下 12..1011。
	got, _, _, dropped := log.List(0, 5)
	if dropped != 10 {
		t.Fatalf("dropped = %d, want 10 (only the post-clear evictions)", dropped)
	}
	if len(got) != 5 || got[0].Seq != 12 {
		t.Fatalf("first readable record: %#v", got[0])
	}
}

func TestConsoleLogBoundsItsRing(t *testing.T) {
	log := &consoleLog{}
	records := make([]map[string]any, 0, consoleCapacity+50)
	for i := 0; i < consoleCapacity+50; i++ {
		records = append(records, map[string]any{"text": i})
	}
	log.Append(records, log.Epoch())

	got, nextSeq, hasMore, _ := log.List(0, consoleCapacity+100)
	if len(got) != consoleCapacity {
		t.Fatalf("ring kept %d records, want %d", len(got), consoleCapacity)
	}
	if hasMore {
		t.Fatal("a fully drained ring has nothing more")
	}
	if nextSeq != int64(consoleCapacity+50) {
		t.Fatalf("nextSeq %d must count every record ever appended", nextSeq)
	}
	if got[0].Seq != 51 {
		t.Fatalf("oldest kept seq %d, want 51 (the newest %d survive)", got[0].Seq, consoleCapacity)
	}
}

// Core 推送的 console 事件必须进 shell 的环形缓冲（Console 页读的就是它）。
// 这条链路的另一半——页内 hook——在当前 WMPF 版本上包不上小程序的 console，
// 所以 CDP 事件是采集的唯一来源。
func TestCoreConsoleEventsLandInTheRing(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	app.handleCoreEvent("console", []byte(`{"type":"console","level":"warn","text":"来自 CDP 的日志","appId":"wxone","ts":1700000000001}`))
	app.handleCoreEvent("status", []byte(`{"frida":true}`)) // 其它事件忽略
	app.handleCoreEvent("console", []byte(`not json`))      // 坏载荷忽略
	app.handleCoreEvent("console", []byte(`{}`))            // 空记录忽略

	result, err := app.router.Call(context.Background(), "console.list", json.RawMessage(`{"tail":5}`))
	if err != nil {
		t.Fatalf("console.list: %v", err)
	}
	payload, _ := json.Marshal(result)
	if !strings.Contains(string(payload), "来自 CDP 的日志") {
		t.Fatalf("console event did not reach the ring: %s", payload)
	}
	var decoded struct {
		Records []struct {
			Record map[string]any `json:"record"`
		} `json:"records"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Records) != 1 || decoded.Records[0].Record["level"] != "warn" {
		t.Fatalf("ring: %s", payload)
	}
}
