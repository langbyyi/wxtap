package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type scanShapeBridge struct{ root string }

func (b scanShapeBridge) Call(_ context.Context, method string, _ map[string]any) (any, error) {
	if method == "settings.getPaths" {
		return map[string]any{"outputDir": b.root}, nil
	}
	return nil, nil
}

func TestSensitiveScanToolReturnsFindingArray(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wxone"), 0o755); err != nil {
		t.Fatal(err)
	}
	server := New(Deps{
		AppBridge: scanShapeBridge{root: root},
		Scan: func(_ context.Context, _ string, _ func(int, int)) (ScanResult, error) {
			return ScanResult{
				Report: json.RawMessage(`{"result":{"secret":["x"]},"findings":[{"id":"f1","masked":"***"}]}`),
			}, nil
		},
	})

	// 受理：首次调用立即返回异步信封，不阻塞在扫描上。
	result, err := server.callMiniappTool(context.Background(), "miniapp_scan_sensitive", json.RawMessage(`{"appid":"wxone"}`))
	if err != nil {
		t.Fatalf("scan tool: %v", err)
	}
	envelope, ok := result.(map[string]any)
	if !ok || envelope["status"] != "started" || envelope["async"] != true {
		t.Fatalf("accept envelope = %#v", result)
	}

	// 轮询：任务完成后同参调用返回完整结果（来自缓存）。
	dir := filepath.Join(root, "wxone")
	if !server.scanWaitDone(dir, 2*time.Second) {
		t.Fatal("scan did not finish")
	}
	result, err = server.callMiniappTool(context.Background(), "miniapp_scan_sensitive", json.RawMessage(`{"appid":"wxone"}`))
	if err != nil {
		t.Fatalf("scan poll: %v", err)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T", result)
	}
	if payload["cached"] != true {
		t.Fatalf("finished scan should be cached: %#v", payload)
	}
	findings, ok := payload["findings"].([]any)
	if !ok || len(findings) != 1 {
		t.Fatalf("findings shape = %#v", payload["findings"])
	}
	if _, ok := payload["result"].(map[string]any); !ok {
		t.Fatalf("analysis shape = %#v", payload["result"])
	}
}
