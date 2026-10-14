package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

type consoleCoverageBridge struct{ sessionBridge }

func (b *consoleCoverageBridge) Call(ctx context.Context, method string, args map[string]any) (any, error) {
	if method != "console.list" {
		return b.sessionBridge.Call(ctx, method, args)
	}
	limit, ok := args["tail"].(int)
	if !ok || limit < 1 {
		return nil, context.DeadlineExceeded
	}
	records := make([]any, limit)
	for index := range records {
		records[index] = map[string]any{"seq": index + 1001, "record": map[string]any{"text": "recent log", "level": "warn"}}
	}
	return map[string]any{
		"records": records, "available": 1000, "evicted": 1332,
		"truncated": true, "pageDroppedRecords": 7, "pendingDropped": 3,
	}, nil
}

func TestConsoleToolDisclosesHistoryCoverage(t *testing.T) {
	bridge := &consoleCoverageBridge{}
	messages := exchange(t, Deps{AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_console_log","arguments":{}}}`+"\n")
	var payload struct {
		Count    int   `json:"count"`
		Logs     []any `json:"logs"`
		Coverage struct {
			Available   int  `json:"available"`
			Evicted     int  `json:"evicted"`
			Truncated   bool `json:"truncated"`
			PageLost    int  `json:"pageDroppedRecords"`
			PendingLost int  `json:"pendingDropped"`
		} `json:"coverage"`
	}
	text := toolText(t, messages, 1)
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Count != 100 || len(payload.Logs) != 100 || payload.Coverage.Available != 1000 ||
		payload.Coverage.Evicted != 1332 || !payload.Coverage.Truncated ||
		payload.Coverage.PageLost != 7 || payload.Coverage.PendingLost != 3 {
		t.Fatalf("recent logs must disclose their incomplete history: %s", text)
	}
}

func TestSessionStatusIncludesConsoleCoverage(t *testing.T) {
	messages := exchange(t, Deps{Core: &fakeCore{}, AppBridge: &consoleCoverageBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_status","arguments":{}}}`+"\n")
	var payload struct {
		Console struct {
			Available   int `json:"available"`
			Evicted     int `json:"evicted"`
			PageLost    int `json:"pageDroppedRecords"`
			PendingLost int `json:"pendingDropped"`
		} `json:"console"`
	}
	text := toolText(t, messages, 1)
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Console.Available != 1000 || payload.Console.Evicted != 1332 ||
		payload.Console.PageLost != 7 || payload.Console.PendingLost != 3 {
		t.Fatalf("snapshot must distinguish console loss from API capture health: %s", text)
	}
}
