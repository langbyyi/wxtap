package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/rpc"
)

// TestFakeCoreProcess is a no-op anchor: TestMain runs an in-process fake Core
// when FAKE_CORE=1 is set and the parent spawns this test binary.
func TestFakeCoreProcess(t *testing.T) {}

func TestMain(m *testing.M) {
	if os.Getenv("FAKE_CORE") == "1" {
		runFakeCore()
		return
	}
	os.Exit(m.Run())
}

// runFakeCore speaks the Core RPC protocol over stdio for engine.* methods.
func runFakeCore() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		switch req.Method {
		case "engine.status":
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{"frida":true,"miniapp":true,"devtools":false,"generation":3,"appInfo":{"appid":"wxfake123","name":"Fake 小程序"}}}` + "\n")
		case "wechat.status":
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{"running":true,"pid":10,"version":14161,"path":"C:\\\\Tencent\\\\WeChat\\\\14161\\\\WeChatAppEx.exe","addressTable":true}}` + "\n")
		case "engine.start":
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{"frida":true}}` + "\n")
		case "engine.stop":
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{}}` + "\n")
		case "miniapp.list":
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":[{"id":1,"appid":"wxfake123","name":"Fake 小程序","locked":true},{"id":2,"appid":"","name":"","locked":false}]}` + "\n")
		case "miniapp.switch":
			var params struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(req.Params, &params)
			ok := "false"
			if params.ID == 1 {
				ok = "true"
			}
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{"ok":` + ok + `}}` + "\n")
		case "miniapp.setLock":
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{}}` + "\n")
		case "miniapp.getLock":
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{"enabled":false}}` + "\n")
		case "code.format":
			var params struct {
				Content  string `json:"content"`
				Language string `json:"language"`
			}
			_ = json.Unmarshal(req.Params, &params)
			blob, _ := json.Marshal(map[string]string{"formatted": "formatted(" + params.Language + "):" + params.Content})
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":` + string(blob) + `}` + "\n")
		case "cdp.command":
			var params struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(req.Params, &params)
			result := `{"id":1,"result":{"targetInfos":[{"targetId":"T1","type":"page","title":"demo"}]}}`
			if params.Method == "Target.attachToTarget" {
				result = `{"id":1,"result":{"sessionId":"S9"}}`
			}
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":` + result + `}` + "\n")
		case "hook.install":
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{"ok":true,"hookedCount":8,"frames":2}}` + "\n")
		case "hook.clear":
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{"ok":true}}` + "\n")
		case "hook.uninstall":
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{"ok":true}}` + "\n")
		case "hook.drain":
			var params struct {
				AfterSeq       int64 `json:"afterSeq"`
				AfterUpdateSeq int64 `json:"afterUpdateSeq"`
				UpdateLimit    int   `json:"updateLimit"`
			}
			_ = json.Unmarshal(req.Params, &params)
			// Synthetic stream: two records per drain, never more, plus one
			// settled-update frame that only exists after ack 0.
			if params.AfterSeq == 0 {
				updates := ""
				if params.AfterUpdateSeq == 0 && params.UpdateLimit > 0 {
					updates = `[{"seq":1,"update":{"rid":"wx.request-wxone-1700000000000-1","status":"success","result":{"ok":true},"durationMs":18}},{"seq":2,"update":{"rid":"wx.bridge-wxone-1700000000001-2","status":"fail","error":"boom","durationMs":42}}]`
				}
				_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{"records":[{"seq":1,"record":{"type":"wx.request","name":"GET a","ts":1700000000000,"status":"success"}},{"seq":2,"record":{"type":"wx.bridge","name":"b","ts":1700000000001,"status":"fail","error":"boom"}}],"updates":` + updates + `,"nextSeq":2,"nextUpdateSeq":2,"hasMore":false}}` + "\n")
			} else {
				_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{"records":[],"updates":[],"nextSeq":` + itoa(params.AfterSeq) + `,"nextUpdateSeq":` + itoa(params.AfterUpdateSeq) + `,"hasMore":false}}` + "\n")
			}
		case "cloud.scan":
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":[{"name":"hello","type":"function","params":["id"],"count":1}]}` + "\n")
		case "runtime.evaluate":
			var params struct {
				AwaitPromise bool `json:"awaitPromise"`
			}
			_ = json.Unmarshal(req.Params, &params)
			awaitEcho := "false"
			if params.AwaitPromise {
				awaitEcho = "true"
			}
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"result":{"value":{"route":"pages/index","value":42,"awaitPromise":` + awaitEcho + `}}}` + "\n")
		default:
			_, _ = os.Stdout.WriteString(`{"id":` + itoa(req.ID) + `,"error":{"code":1000,"message":"unknown method: ` + req.Method + `","retryable":false}}` + "\n")
		}
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func fakeCoreCommand() *exec.Cmd {
	// Under `go test`, the running executable is the compiled test binary.
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	cmd := exec.Command(self, "-test.run=TestFakeCoreProcess")
	cmd.Env = append(os.Environ(), "FAKE_CORE=1")
	return cmd
}

func TestStartProcessAndCallStatus(t *testing.T) {
	client, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.Frida || !status.Miniapp || status.Devtools {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.Generation != 3 {
		t.Fatalf("generation not decoded: %+v", status)
	}
	if status.AppInfo == nil || status.AppInfo.AppID != "wxfake123" || status.AppInfo.Name != "Fake 小程序" {
		t.Fatalf("appInfo not decoded: %+v", status.AppInfo)
	}
}

func TestWeChatRunning(t *testing.T) {
	client, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer client.Shutdown()

	running, err := client.WeChatRunning(context.Background())
	if err != nil {
		t.Fatalf("wechat.status: %v", err)
	}
	if !running {
		t.Fatal("wechat.status should decode a running desktop WeChat process")
	}
	status, err := client.WeChatStatus(context.Background())
	if err != nil {
		t.Fatalf("wechat host: %v", err)
	}
	if status.PID != 10 || status.Version != 14161 || !status.AddressTable || !strings.Contains(status.Path, "WeChatAppEx.exe") {
		t.Fatalf("wechat host: %+v", status)
	}
}

func TestCodeFormat(t *testing.T) {
	client, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	formatted, err := client.CodeFormat(ctx, `{"a":1}`, "json")
	if err != nil {
		t.Fatalf("code.format: %v", err)
	}
	if formatted != "formatted(json):{\"a\":1}" {
		t.Fatalf("unexpected formatted payload: %q", formatted)
	}
}

func TestCloudScan(t *testing.T) {
	client, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	items, err := client.CloudScan(ctx)
	if err != nil {
		t.Fatalf("cloud.scan: %v", err)
	}
	if len(items) != 1 || items[0]["name"] != "hello" || items[0]["type"] != "function" {
		t.Fatalf("unexpected cloud scan: %#v", items)
	}
}

func TestMiniappManagement(t *testing.T) {
	client, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	list, err := client.MiniappList(ctx)
	if err != nil {
		t.Fatalf("miniapp.list: %v", err)
	}
	if len(list) != 2 || list[0].ID != 1 || list[0].AppID != "wxfake123" || list[0].Name != "Fake 小程序" || !list[0].Locked {
		t.Fatalf("unexpected list: %+v", list)
	}

	if ok, err := client.MiniappSwitch(ctx, 1); err != nil || !ok {
		t.Fatalf("miniapp.switch: ok=%v err=%v", ok, err)
	}
	if ok, err := client.MiniappSwitch(ctx, 999); err != nil || ok {
		t.Fatalf("miniapp.switch unknown: ok=%v err=%v", ok, err)
	}
	if err := client.MiniappSetLock(ctx, false); err != nil {
		t.Fatalf("miniapp.setLock: %v", err)
	}
	enabled, err := client.MiniappGetLock(ctx)
	if err != nil || enabled {
		t.Fatalf("miniapp.getLock: enabled=%v err=%v", enabled, err)
	}
}

func TestCDPCommandRoundTrip(t *testing.T) {
	client, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.CDPCommand(ctx, "Target.getTargets", map[string]any{}, 8000)
	if err != nil {
		t.Fatalf("cdp.command: %v", err)
	}
	inner, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected response: %+v", resp)
	}
	infos, ok := inner["targetInfos"].([]any)
	if !ok || len(infos) != 1 {
		t.Fatalf("targetInfos missing: %+v", inner)
	}

	resp, err = client.CDPCommand(ctx, "Target.attachToTarget", map[string]any{"targetId": "T1"}, 8000)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if inner, _ := resp["result"].(map[string]any); inner["sessionId"] != "S9" {
		t.Fatalf("attach response: %+v", resp)
	}
}

func TestStartAndStopRoundTrip(t *testing.T) {
	client, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx, 62000); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestHookInstallAndDrain(t *testing.T) {
	client, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	report, err := client.HookInstall(ctx, "wxapi")
	if err != nil {
		t.Fatalf("hook.install: %v", err)
	}
	if report["ok"] != true || report["hookedCount"] != float64(8) {
		t.Fatalf("unexpected install report: %+v", report)
	}

	page, err := client.HookDrain(ctx, "wxapi", 0, 100, 0, 100)
	if err != nil {
		t.Fatalf("hook.drain: %v", err)
	}
	if len(page.Records) != 2 || page.NextSeq != 2 || page.HasMore {
		t.Fatalf("unexpected drain page: %+v", page)
	}
	if page.Records[0].Record["type"] != "wx.request" {
		t.Fatalf("record not decoded: %+v", page.Records[0])
	}
	// The update stream rides the same round trip: its frames and its own
	// acknowledgement cursor are decoded alongside the records.
	if len(page.Updates) != 2 || page.NextUpdateSeq != 2 {
		t.Fatalf("unexpected update page: %+v", page)
	}
	if page.Updates[0].Update["rid"] != "wx.request-wxone-1700000000000-1" || page.Updates[0].Update["status"] != "success" {
		t.Fatalf("update frame not decoded: %+v", page.Updates[0])
	}

	// Draining from the acknowledged points returns nothing new from either
	// stream, and the update cursor is what suppresses the frames.
	empty, err := client.HookDrain(ctx, "wxapi", page.NextSeq, 100, page.NextUpdateSeq, 100)
	if err != nil {
		t.Fatalf("hook.drain ack: %v", err)
	}
	if len(empty.Records) != 0 || len(empty.Updates) != 0 {
		t.Fatalf("acknowledged records resent: %+v / %+v", empty.Records, empty.Updates)
	}
	if empty.NextUpdateSeq != page.NextUpdateSeq {
		t.Fatalf("an idle drain moved the update cursor: %+v", empty)
	}
	if err := client.HookClear(ctx, "wxapi"); err != nil {
		t.Fatalf("hook.clear: %v", err)
	}
	if err := client.HookUninstall(ctx, "wxapi"); err != nil {
		t.Fatalf("hook.uninstall: %v", err)
	}
}

func TestEvaluateReturnsParsedValue(t *testing.T) {
	client, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	value, err := client.Evaluate(ctx, "6 * 7", 5)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	obj, ok := value.(map[string]any)
	if !ok || obj["route"] != "pages/index" {
		t.Fatalf("unexpected evaluate value: %#v", value)
	}
}

func TestEvaluateAwaitPassesAwaitPromise(t *testing.T) {
	client, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	value, err := client.EvaluateAwait(ctx, "Promise.resolve(7)", 5)
	if err != nil {
		t.Fatalf("evaluate await: %v", err)
	}
	obj, ok := value.(map[string]any)
	if !ok || obj["awaitPromise"] != true {
		t.Fatalf("awaitPromise not passed through: %#v", value)
	}
}

// An offline Core must yield a typed retryable error without taking the
// desktop app down: the caller can simply retry or restart later.
func TestOfflineCoreYieldsRetryableError(t *testing.T) {
	cmd := exec.Command("definitely-not-a-real-core-binary-xyz")
	client, err := StartProcess(cmd)
	if err == nil {
		// Some platforms only fail on first write; close and treat as offline.
		client.Shutdown()
		t.Fatal("expected an error starting a nonexistent core")
	}

	var coreErr *rpc.CoreError
	if !errors.As(err, &coreErr) {
		t.Fatalf("expected *rpc.CoreError, got %T: %v", err, err)
	}
	if !coreErr.Retryable {
		t.Fatalf("offline core error must be retryable: %+v", coreErr)
	}

	// A subsequent process start succeeds after the offline failure.
	recovered, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("restart after offline: %v", err)
	}
	recovered.Shutdown()
}

func TestKilledCoreYieldsRetryableErrorWithoutPanic(t *testing.T) {
	client, err := StartProcess(fakeCoreCommand())
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer client.Shutdown()
	_ = client.cmd.Process.Kill()

	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = client.Status(ctx)

	var coreErr *rpc.CoreError
	if !errors.As(err, &coreErr) {
		t.Fatalf("expected *rpc.CoreError after kill, got %T: %v", err, err)
	}
	if !coreErr.Retryable {
		t.Fatalf("killed core error must be retryable: %+v", coreErr)
	}
}

// Every Core stderr line must reach the OnLog callback: the desktop shell
// forwards them as `log` events so the control view shows the live Core
// output into the control view log.
func TestCaptureStderrEmitsLogLines(t *testing.T) {
	reader, writer := io.Pipe()
	client := &Client{}
	lineCh := make(chan string, 64)
	client.OnLog(func(line string) { lineCh <- line })
	go client.captureStderr(reader)

	if _, err := writer.Write([]byte("hook installed\nattach failed\n")); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	for _, want := range []string{"hook installed", "attach failed"} {
		select {
		case got := <-lineCh:
			if got != want {
				t.Fatalf("log line: got %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing log line %q", want)
		}
	}

	// The ring buffer keeps only the last 32 lines, older ones dropped.
	for i := 0; i < 35; i++ {
		if _, err := writer.Write([]byte("extra line\n")); err != nil {
			t.Fatalf("write extra: %v", err)
		}
	}
	for i := 0; i < 35; i++ {
		select {
		case <-lineCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("missing extra line %d", i)
		}
	}
	recent := client.RecentStderr()
	if len(recent) != 32 {
		t.Fatalf("RecentStderr keeps %d lines, want 32", len(recent))
	}
	if recent[0] != "extra line" || recent[31] != "extra line" {
		t.Fatalf("ring buffer kept unexpected lines: %q..%q", recent[0], recent[31])
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

// captureStderr starts inside StartProcess, before the caller registers OnLog.
// Lines captured in that window must be replayed at registration instead of
// being dropped: the first stderr output is usually the most diagnostic.
func TestOnLogReplaysLinesCapturedBeforeRegistration(t *testing.T) {
	reader, writer := io.Pipe()
	client := &Client{}
	go client.captureStderr(reader)

	if _, err := writer.Write([]byte("early boot line\n")); err != nil {
		t.Fatalf("write early: %v", err)
	}
	// Give the scanner a moment to capture the line without a callback.
	time.Sleep(50 * time.Millisecond)

	lineCh := make(chan string, 64)
	client.OnLog(func(line string) { lineCh <- line })

	if _, err := writer.Write([]byte("live line\n")); err != nil {
		t.Fatalf("write live: %v", err)
	}
	for _, want := range []string{"early boot line", "live line"} {
		select {
		case got := <-lineCh:
			if got != want {
				t.Fatalf("replayed order: got %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing line %q", want)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}
