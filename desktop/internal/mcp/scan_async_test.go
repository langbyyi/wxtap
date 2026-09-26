package mcp

// 扫描工具受理/轮询/缓存协议的行为测试：大目录扫描改异步后，客户端契约是
// 首调受理、同参轮询、目录未变秒回缓存、失败报一次后重试。fake 扫描器把
// 时序控制权交给测试，不依赖真实扫描耗时。

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// scanDirCall drives one scan_dir tools/call against the server directly and
// returns the decoded payload (Go values, not JSON-roundtripped).
func scanDirCall(t *testing.T, server *Server, dir string) map[string]any {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": "scan_dir", "arguments": map[string]any{"dir": dir}})
	resp := server.callTool(&request{ctx: context.Background(), Params: params})
	if resp.Error != nil {
		t.Fatalf("scan_dir error: %s", resp.Error.Message)
	}
	payload, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T", resp.Result)
	}
	return payload
}

func TestScanDirAcceptsPollsAndCaches(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	server := New(Deps{
		Scan: func(_ context.Context, _ string, progress func(int, int)) (ScanResult, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			progress(1, 2)
			progress(2, 2)
			if n == 1 {
				<-release // 第一次扫描挂起：受理与轮询的时序完全可控
			}
			return ScanResult{
				FilesScanned: 1,
				Summary:      map[string]int{"secret": 1},
				Report:       json.RawMessage(`{"result":{"secret":["x"]},"findings":[{"id":"f1","masked":"***"}]}`),
			}, nil
		},
	})

	// 受理：立即返回 started，不阻塞在扫描上。
	envelope := scanDirCall(t, server, dir)
	if envelope["status"] != "started" || envelope["async"] != true {
		t.Fatalf("accept envelope = %#v", envelope)
	}

	// 轮询挂起中的任务：running 带进度（进度由 fake 上报）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		envelope = scanDirCall(t, server, dir)
		if envelope["status"] != "running" {
			t.Fatalf("running envelope = %#v", envelope)
		}
		if done, _ := envelope["files_done"].(int); done == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress never reached 2/2: %#v", envelope)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 放行 → 完成；轮询拿到缓存里的完整结果。
	close(release)
	if !server.scanWaitDone(dir, 2*time.Second) {
		t.Fatal("scan did not finish")
	}
	done := scanDirCall(t, server, dir)
	if done["cached"] != true {
		t.Fatalf("finished scan should be cached: %#v", done)
	}
	if done["files_scanned"].(int) != 1 {
		t.Fatalf("files_scanned = %#v", done["files_scanned"])
	}
	findings, ok := done["findings"].([]any)
	if !ok || len(findings) != 1 {
		t.Fatalf("findings shape = %#v", done["findings"])
	}

	// 目录没变：仍然秒回缓存，绝不重扫。
	mu.Lock()
	before := calls
	mu.Unlock()
	done = scanDirCall(t, server, dir)
	if done["cached"] != true {
		t.Fatalf("second cached call = %#v", done)
	}
	mu.Lock()
	if calls != before {
		t.Fatalf("cached call re-ran the scanner: %d -> %d", before, calls)
	}
	mu.Unlock()

	// 目录 mtime 变化 → 缓存失效，重新受理并真正重扫。
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(dir, future, future); err != nil {
		t.Fatal(err)
	}
	envelope = scanDirCall(t, server, dir)
	if envelope["status"] != "started" {
		t.Fatalf("mtime change should re-accept, got %#v", envelope)
	}
	if !server.scanWaitDone(dir, 2*time.Second) {
		t.Fatal("rescan did not finish")
	}
	mu.Lock()
	after := calls
	mu.Unlock()
	if after != before+1 {
		t.Fatalf("rescan did not run: %d -> %d", before, after)
	}
}

func TestScanDirReportsFailureOnceThenRetries(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	calls := 0
	server := New(Deps{
		Scan: func(_ context.Context, _ string, _ func(int, int)) (ScanResult, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return ScanResult{}, context.DeadlineExceeded
		},
	})

	envelope := scanDirCall(t, server, dir)
	if envelope["status"] != "started" {
		t.Fatalf("accept = %#v", envelope)
	}
	if !server.scanWaitDone(dir, time.Second) {
		t.Fatal("job did not finish")
	}
	failure := scanDirCall(t, server, dir)
	if failure["status"] != "error" || failure["error"] == nil {
		t.Fatalf("failure envelope = %#v", failure)
	}
	// 错误只报一次：下一条同参调用重新受理。
	envelope = scanDirCall(t, server, dir)
	if envelope["status"] != "started" {
		t.Fatalf("retry after failure = %#v", envelope)
	}
	if !server.scanWaitDone(dir, time.Second) {
		t.Fatal("retry did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("scan calls = %d, want 2", calls)
	}
}
