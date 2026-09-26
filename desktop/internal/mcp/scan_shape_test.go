package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
		Scan: func(context.Context, string) (ScanResult, error) {
			return ScanResult{
				Report: json.RawMessage(`{"result":{"secret":["x"]},"findings":[{"id":"f1","masked":"***"}]}`),
			}, nil
		},
	})

	result, err := server.callMiniappTool(context.Background(), "miniapp_scan_sensitive", json.RawMessage(`{"appid":"wxone"}`))
	if err != nil {
		t.Fatalf("scan tool: %v", err)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T", result)
	}
	findings, ok := payload["findings"].([]any)
	if !ok || len(findings) != 1 {
		t.Fatalf("findings shape = %#v", payload["findings"])
	}
	if _, ok := payload["result"].(map[string]any); !ok {
		t.Fatalf("analysis shape = %#v", payload["result"])
	}
}
