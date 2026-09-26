package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// wxapi.poll and cloud.poll answer with JSON arrays even when there is nothing
// to report; a null payload breaks list consumers. The paginated path must
// keep that shape for every limit, including the out-of-range ones.
func TestPollHandlersReturnArraysWhenEmpty(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	for _, method := range []string{"wxapi.poll", "cloud.poll"} {
		result, err := app.router.Call(context.Background(), method, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("%s marshal: %v", method, err)
		}
		if string(data) != "[]" {
			t.Fatalf("%s payload = %s, want []", method, data)
		}
	}
	// 钳制边界同样只能回数组：0/负数读作最小页（1 条），超大值读作上限，
	// 但空缓冲下三者都必须回 []，不能回 null。
	for _, params := range []string{`{"limit":0}`, `{"limit":-5}`, `{"limit":999999}`} {
		result, err := app.router.Call(context.Background(), "wxapi.poll", json.RawMessage(params))
		if err != nil {
			t.Fatalf("wxapi.poll %s: %v", params, err)
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("wxapi.poll %s marshal: %v", params, err)
		}
		if string(data) != "[]" {
			t.Fatalf("wxapi.poll %s payload = %s, want []", params, data)
		}
	}
}

// wxapi.poll 的有界分页（F3）：一次只回一页，limit 钳制在 1..2000（缺省 500），
// 顺序与消费语义不变 —— 多轮 poll 恰好取完缓冲，且不重不漏。
func TestWxapiPollLimitPagesTheBuffer(t *testing.T) {
	app := startFakeCoreApp(t)
	defer app.shutdown(context.Background())

	if err := app.EngineStart(62000); err != nil {
		t.Fatalf("engine.start: %v", err)
	}
	if envelope := app.Call("wxapi.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.start: %s", envelope)
	}
	// 假 Core 只喂一页三条记录；等它们全部进入 poll 缓冲再分页取。
	waitFor(t, 10*time.Second, "三条记录进入 poll 缓冲", func() bool {
		return wxapiPending(t, app) == 3
	})

	// limit=0 与负数读作最小页 1 条，绝不是「取全部」：若 0 被当成缺省值，
	// 这里会一次拿回三条。
	for _, params := range []string{`{"limit":0}`, `{"limit":-1}`} {
		page := wxapiPoll(t, app, params)
		if len(page) != 1 {
			t.Fatalf("%s 返回 %d 条, want 1（钳到最小值）", params, len(page))
		}
	}
	// 超大的 limit 仍要能取走剩余记录（钳到上限 2000，不是报错）。
	if page := wxapiPoll(t, app, `{"limit":999999}`); len(page) != 1 {
		t.Fatalf("超大 limit 返回 %d 条, want 1", len(page))
	}
	// 缓冲排空后回到空数组：分页不改变响应形状。
	if envelope := app.Call("wxapi.poll", `{}`); !strings.Contains(envelope, `"result":[]`) {
		t.Fatalf("空缓冲必须回 []，got %s", envelope)
	}
}

// 分页不得丢记录：一页一条地取，取到的必须是三条不同记录且保持捕获顺序。
func TestWxapiPollLimitKeepsEveryRecordInOrder(t *testing.T) {
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

	var seen []float64
	for i := 0; i < 10; i++ {
		page := wxapiPoll(t, app, `{"limit":1}`)
		if len(page) == 0 {
			break
		}
		if len(page) != 1 {
			t.Fatalf("limit=1 返回了 %d 条", len(page))
		}
		ts, ok := page[0]["ts"].(float64)
		if !ok {
			t.Fatalf("记录缺少捕获时间: %#v", page[0])
		}
		seen = append(seen, ts)
	}
	want := []float64{1700000000000, 1700000000001, 1700000000002}
	if len(seen) != len(want) {
		t.Fatalf("分页取到 %d 条记录, want %d: %v", len(seen), len(want), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("分页顺序被改变: %v, want %v", seen, want)
		}
	}
}

// wxapiPoll decodes one wxapi.poll envelope (the handler answers a bare array).
func wxapiPoll(t *testing.T, app *App, params string) []map[string]any {
	t.Helper()
	envelope := app.Call("wxapi.poll", params)
	var decoded struct {
		Result []map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(envelope), &decoded); err != nil {
		t.Fatalf("decode %s: %v", envelope, err)
	}
	if decoded.Error != nil {
		t.Fatalf("wxapi.poll %s: %s", params, decoded.Error.Message)
	}
	return decoded.Result
}

// wxapiPending reads the buffered record count from wxapi.stats.
func wxapiPending(t *testing.T, app *App) int {
	t.Helper()
	var decoded struct {
		Result struct {
			Pending int `json:"pending"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(app.Call("wxapi.stats", `{}`)), &decoded); err != nil {
		t.Fatalf("decode wxapi.stats: %v", err)
	}
	return decoded.Result.Pending
}
