package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// emitLog must travel through the same a.emit seam every other backend event
// uses: without it, no test could observe the `log` stream (EventsEmit fatals
// outside the Wails lifecycle), and the panel contract would go unverified.
func TestEmitLogBroadcastsThroughEmitSink(t *testing.T) {
	app := NewApp()
	var mu sync.Mutex
	var events []map[string]any
	emitSink = func(name string, payload any) {
		mu.Lock()
		defer mu.Unlock()
		if name == "log" {
			events = append(events, payload.(map[string]any))
		}
	}
	t.Cleanup(func() { emitSink = nil })

	app.emitLog("info", "引擎已启动")
	app.emitLog("error", "注入失败")

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("expected 2 log events, got %d", len(events))
	}
	first, second := events[0], events[1]
	if first["message"] != "引擎已启动" || first["level"] != "info" {
		t.Fatalf("first event: %v", first)
	}
	if second["level"] != "error" {
		t.Fatalf("level must survive the relay: %v", second)
	}
	// 时间戳带毫秒：同一秒内的先后顺序必须可辨。
	if stamp, _ := first["time"].(string); len(stamp) != 12 || stamp[8] != '.' {
		t.Fatalf("timestamp must be 15:04:05.000, got %q", stamp)
	}
	firstSeq, _ := first["seq"].(int64)
	secondSeq, _ := second["seq"].(int64)
	if secondSeq != firstSeq+1 {
		t.Fatalf("seq must be monotonic: %v then %v", firstSeq, secondSeq)
	}
}

// The runtime ring replays what the panel missed before it subscribed, and
// log.clear wipes it (the 清空 button must not leave a backend copy behind).
func TestLogListReplaysRing(t *testing.T) {
	app := NewApp()
	app.dataBase = t.TempDir()
	app.setupIPC()

	app.emitLog("info", "第一条")
	app.emitLog("warn", "第二条")
	app.emitLog("error", "第三条")

	result, err := app.router.Call(context.Background(), "log.list", json.RawMessage(`{"tail":2}`))
	if err != nil {
		t.Fatalf("log.list: %v", err)
	}
	payload, _ := json.Marshal(result)
	var decoded struct {
		Records []runtimeLogRecord `json:"records"`
		NextSeq int64              `json:"nextSeq"`
		Dropped int64              `json:"dropped"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Records) != 2 {
		t.Fatalf("tail=2 must return the newest 2: %s", payload)
	}
	if decoded.Records[0].Message != "第二条" || decoded.Records[1].Message != "第三条" {
		t.Fatalf("order: %s", payload)
	}
	if decoded.Records[1].Level != "error" {
		t.Fatalf("level must round-trip: %s", payload)
	}
	if decoded.Records[1].Seq != 3 || decoded.NextSeq != 3 {
		t.Fatalf("seq cursor: %s", payload)
	}

	if _, err := app.router.Call(context.Background(), "log.clear", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("log.clear: %v", err)
	}
	result, err = app.router.Call(context.Background(), "log.list", json.RawMessage(`{"tail":10}`))
	if err != nil {
		t.Fatalf("log.list after clear: %v", err)
	}
	payload, _ = json.Marshal(result)
	if strings.Contains(string(payload), "第三条") {
		t.Fatalf("clear left records behind: %s", payload)
	}
	// 清空后序号必须继续增长：回放快照靠 seq 去重，复用旧号会把旧行复活。
	next := app.runtimeLog.Append("15:04:05.000", "info", "清空后")
	if next != 4 {
		t.Fatalf("seq must survive clear, got %d", next)
	}
}

// The ring evicts from the head when full and reports cumulative evictions:
// the replay panel turns that reading into its「已丢弃 N 条」notice.
func TestRuntimeLogRingEvictsAndCounts(t *testing.T) {
	ring := &runtimeLog{}
	for i := 0; i < runtimeLogCapacity+7; i++ {
		ring.Append("15:04:05.000", "info", "行")
	}
	records, nextSeq, dropped := ring.Tail(runtimeLogCapacity)
	if len(records) != runtimeLogCapacity {
		t.Fatalf("ring must hold %d, got %d", runtimeLogCapacity, len(records))
	}
	if records[0].Seq != 8 {
		t.Fatalf("head evicted, oldest should be seq 8, got %d", records[0].Seq)
	}
	if nextSeq != runtimeLogCapacity+7 || dropped != 7 {
		t.Fatalf("cursor/dropped: nextSeq=%d dropped=%d", nextSeq, dropped)
	}
}

func TestCoreLogLevelClassification(t *testing.T) {
	cases := map[string]string{
		"[core] 使用静态地址表：微信构建 14161":                                      "info",
		"[core] 已拒绝 9421 端口上来自 http://x 的 WebSocket 握手：来源不是本机":           "info",
		"[hook:error] TypeError: Cannot read property 'id' of undefined": "error",
		"Error: listen EADDRINUSE: address already in use":               "error",
		"FATAL: something unrecoverable":                                 "error",
		"(node:1234) ExperimentalWarning: The Fetch API is experimental": "warn",
		"[core] 构建 14161 无静态地址表，开始自动检测偏移":                                "info",
	}
	for line, want := range cases {
		if got := coreLogLevel(line); got != want {
			t.Fatalf("coreLogLevel(%q) = %q, want %q", line, got, want)
		}
	}
}
