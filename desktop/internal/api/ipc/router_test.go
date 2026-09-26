package ipc

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- router ----------

func TestRouterUnknownMethodFails(t *testing.T) {
	router := New()
	_, err := router.Call(context.Background(), "nope.method", nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported backend method") {
		t.Fatalf("unknown method must fail clearly: %v", err)
	}
}

func TestRouterDispatchesAndPassesParams(t *testing.T) {
	router := New()
	var gotParams json.RawMessage
	router.Register("config.load", func(_ context.Context, params json.RawMessage) (any, error) {
		gotParams = params
		return map[string]any{"ok": true}, nil
	})
	result, err := router.Call(context.Background(), "config.load", json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(gotParams) != `{"a":1}` {
		t.Fatalf("params not forwarded: %s", gotParams)
	}
	if m, _ := result.(map[string]any); m["ok"] != true {
		t.Fatalf("result wrong: %v", result)
	}
}

// ---------- hook feeder ----------

type feederCore struct {
	mu        sync.Mutex
	installed []string
	pages     map[int64]DrainPage
}

func (f *feederCore) InstallHook(ctx context.Context, name string) error {
	_, err := f.InstallHookReport(ctx, name)
	return err
}

func (f *feederCore) InstallHookReport(_ context.Context, name string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = append(f.installed, name)
	return map[string]any{"ok": true}, nil
}

func (f *feederCore) HookDrain(_ context.Context, _ string, afterSeq int64, _ int, afterUpdateSeq int64, _ int) (DrainPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if page, ok := f.pages[afterSeq]; ok {
		return page, nil
	}
	return DrainPage{NextSeq: afterSeq, NextUpdateSeq: afterUpdateSeq}, nil
}

func (f *feederCore) Evaluate(_ context.Context, expression string, _ int) (any, error) {
	return map[string]any{"replayed": expression}, nil
}

func hookRecord(seq int64, name string) DrainedRecord {
	return DrainedRecord{
		Seq: seq,
		Record: map[string]any{
			"type": "wx.request", "name": name, "appId": "wx1",
			"ts": float64(1700000000000 + seq), "status": "success",
			"data": map[string]any{"url": "https://x.test"}, "result": map[string]any{"ok": seq},
		},
	}
}

func TestHookFeederPollConsumesBufferedRecords(t *testing.T) {
	core := &feederCore{pages: map[int64]DrainPage{
		0: {Records: []DrainedRecord{hookRecord(1, "first"), hookRecord(2, "second")}, NextSeq: 2},
	}}
	var mu sync.Mutex
	var captures []string
	feeder := NewHookFeeder(core, "wxapi", func(record map[string]any) {
		mu.Lock()
		captures = append(captures, record["name"].(string))
		mu.Unlock()
	}, func(_ context.Context, _ []DrainedRecord) error { return nil })

	if err := feeder.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer feeder.Stop()
	// Drain loop must have pulled the page within a tick.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		records, _ := feeder.Poll()
		if len(records) == 2 {
			mu.Lock()
			first, second := captures[0], captures[1]
			mu.Unlock()
			if first != "first" || second != "second" {
				t.Fatalf("capture events out of order: %v, %v", first, second)
			}
			// Poll consumed the buffer: next poll is empty.
			empty, _ := feeder.Poll()
			if len(empty) != 0 {
				t.Fatalf("poll did not consume: %v", empty)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("records never drained")
}

func TestHookFeederIngestsEveryRecord(t *testing.T) {
	core := &feederCore{pages: map[int64]DrainPage{
		0: {Records: []DrainedRecord{hookRecord(1, "a")}, NextSeq: 1},
	}}
	var mu sync.Mutex
	ingested := 0
	feeder := NewHookFeeder(core, "wxapi", func(map[string]any) {},
		func(_ context.Context, batch []DrainedRecord) error {
			mu.Lock()
			ingested += len(batch)
			mu.Unlock()
			return nil
		})
	if err := feeder.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer feeder.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := ingested
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if ingested != 1 {
		t.Fatalf("ingest callback not invoked: %d", ingested)
	}
}

func TestHookFeederClearResetsPending(t *testing.T) {
	core := &feederCore{pages: map[int64]DrainPage{
		0: {Records: []DrainedRecord{hookRecord(1, "a")}, NextSeq: 1},
	}}
	feeder := NewHookFeeder(core, "cloud", func(map[string]any) {}, func(context.Context, []DrainedRecord) error { return nil })
	if err := feeder.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer feeder.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		records, _ := feeder.Poll()
		if len(records) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	feeder.Clear()
	records, _ := feeder.Poll()
	if len(records) != 0 {
		t.Fatalf("clear did not reset pending: %v", records)
	}
}

// A handler panic must not take the whole shell (or the MCP process) down:
// the boundary turns it into an ordinary IPC error and keeps serving.
func TestRouterRecoversHandlerPanic(t *testing.T) {
	router := New()
	router.Register("boom", func(context.Context, json.RawMessage) (any, error) {
		panic("nil map write")
	})
	router.Register("ok", func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"ok": true}, nil
	})

	_, err := router.Call(context.Background(), "boom", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("panic must surface as an error")
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "nil map write") {
		t.Fatalf("panic error: %v", err)
	}

	result, err := router.Call(context.Background(), "ok", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("router must keep serving after a panic: %v", err)
	}
	if result == nil {
		t.Fatal("result missing after panic recovery")
	}
}
