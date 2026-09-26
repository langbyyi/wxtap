package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/api"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// TestMain doubles as the fake Core entry point: when the parent spawns this
// test binary as WXTAP_CORE_CMD, FAKE_CORE_MAIN=1 makes it speak the Core RPC
// protocol on stdio instead of running the test suite.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_CORE_MAIN") == "1" {
		// This binary is standing in for node, so it has to answer the version
		// probe the shell makes before spawning Core (requireNodeVersion).
		// Reporting minNodeMajor keeps the fake in step with the real floor.
		if len(os.Args) == 3 && os.Args[1] == "-p" && os.Args[2] == "process.versions.node" {
			fmt.Printf("%d.0.0\n", minNodeMajor)
			return
		}
		runFakeCoreMain()
		return
	}
	os.Exit(m.Run())
}

// TestFakeCoreMainAnchor keeps the fake entry point discoverable.
func TestFakeCoreMainAnchor(t *testing.T) {}

// runFakeCoreMain answers the Core RPC surface the desktop shell drives during
// integration tests: engine lifecycle, hooks (drain feed), miniapp registry,
// CDP and cloud scan.
// hookName reads the hook name out of a hook.* request. Every hook request
// carries one; an unparseable payload yields "", which no hook is ever named.
func hookName(params json.RawMessage) string {
	var payload struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(params, &payload)
	return payload.Name
}

func runFakeCoreMain() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	writer := bufio.NewWriter(os.Stdout)
	defer func() { _ = writer.Flush() }()

	reply := func(id int64, result string) {
		_, _ = writer.WriteString(`{"id":` + strconv.FormatInt(id, 10) + `,"result":` + result + "}\n")
		_ = writer.Flush()
	}

	// Optional request log: a test that has to assert what the shell drove Core
	// to do (which hooks it installed, in which order) points FAKE_CORE_CALL_LOG
	// at a file and reads one tab-separated "method<TAB>params" line per request.
	// Unset means no logging, so every other test pays nothing.
	var callLog *os.File
	if path := os.Getenv("FAKE_CORE_CALL_LOG"); path != "" {
		callLog, _ = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		defer func() { _ = callLog.Close() }()
	}

	// The hook drain feed is per hook (the real Core keeps one page-side buffer
	// per hook). The three records below stand for the calls the miniapp has
	// made, and the buffer is modelled as a *pure read*: it holds them while the
	// hook is installed, and only uninstalling it takes them away — the real
	// hook's drain does not consume the buffer either, the caller's sequence
	// number is the only thing that moves.
	//
	// A one-shot page made the shell's own legitimate re-reads look like lost
	// data: the connect-time re-hook of a running capture (and every 开启捕获)
	// resets the sequence cursor on purpose, and a round trip that a Clear
	// invalidated is retried from the same cursor. Both re-read the same three
	// records, and the shell's replay-dedup FIFO (R11) is what keeps a re-read
	// from being delivered twice — so the fake must hand them over again, not
	// answer with an empty page.
	uninstalled := map[string]bool{}

	for scanner.Scan() {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if callLog != nil {
			_, _ = callLog.WriteString(req.Method + "\t" + string(req.Params) + "\n")
		}
		switch req.Method {
		case "engine.start":
			reply(req.ID, `{"frida":true,"miniapp":true,"devtools":false}`)
		case "engine.stop":
			reply(req.ID, `{}`)
		case "engine.status":
			reply(req.ID, `{"frida":true,"miniapp":true,"devtools":false,"generation":`+
				strconv.FormatInt(fakeGeneration(), 10)+`,`+
				`"appInfo":{"appid":"wxfake123","name":"Fake 小程序"}}`)
		case "hook.install":
			delete(uninstalled, hookName(req.Params))
			reply(req.ID, `{"ok":true,"hookedCount":8,"frames":2}`)
		case "hook.clear":
			// 清页内缓冲之后小程序照常继续调用，缓冲里又会有记录；假 Core 没有真实
			// 页面活动，所以那三条继续代表「页内现有的记录」。清空的效果由 shell 侧
			// 承担（pending 归零 + 重投抑制），见 hook.drain 的说明。
			reply(req.ID, `{"ok":true}`)
		case "hook.uninstall":
			uninstalled[hookName(req.Params)] = true
			reply(req.ID, `{"ok":true}`)
		case "hook.drain":
			var params struct {
				Name           string `json:"name"`
				AfterSeq       int64  `json:"afterSeq"`
				AfterUpdateSeq int64  `json:"afterUpdateSeq"`
			}
			_ = json.Unmarshal(req.Params, &params)
			// The settled-update stream: the slow record below finishes in the
			// same tick it was captured in, so the page carries the pending
			// record and its own update frame together.
			updates := `[]`
			nextUpdateSeq := params.AfterUpdateSeq
			if params.AfterUpdateSeq == 0 {
				updates = `[{"seq":1,"update":{"seq":1,"rid":"` + settledRecordRID + `","status":"success",` +
					`"result":{"statusCode":200},"durationMs":1830,"settledAt":1700000001830}}]`
				nextUpdateSeq = 1
			}
			if !uninstalled[params.Name] && params.AfterSeq < 3 {
				// A body past traffic.compressThreshold exercises the gzip store
				// path; a tiny one would only prove raw passthrough.
				body := strings.Repeat("x", 4096)
				reply(req.ID, `{"records":[`+
					`{"seq":1,"record":{"type":"wx.request","name":"GET a","ts":1700000000000,"status":"success",`+
					`"data":{"url":"https://api.example.com/x","method":"POST","body":"`+body+`"},"result":{"ok":true}}},`+
					`{"seq":2,"record":{"type":"cloud.call","name":"login","ts":1700000000001,"status":"fail","error":"boom"}},`+
					`{"seq":3,"record":{"type":"wx.downloadFile","name":"file.zip","appId":"wxfake123",`+
					`"ts":1700000000002,"status":"pending","rid":"`+settledRecordRID+`",`+
					`"data":{"url":"https://cdn.example.com/f.zip"}}}`+
					`],"updates":`+updates+`,"nextSeq":3,"nextUpdateSeq":`+strconv.FormatInt(nextUpdateSeq, 10)+
					`,"hasMore":false,`+fakePageDropCounters+`}`)
				continue
			}
			reply(req.ID, `{"records":[],"updates":`+updates+`,"nextSeq":`+strconv.FormatInt(params.AfterSeq, 10)+
				`,"nextUpdateSeq":`+strconv.FormatInt(nextUpdateSeq, 10)+`,"hasMore":false,`+fakePageDropCounters+`}`)
		case "miniapp.list":
			reply(req.ID, `[{"id":1,"appid":"wxfake123","name":"Fake 小程序","locked":true}]`)
		case "miniapp.getLock":
			reply(req.ID, `{"enabled":true}`)
		case "miniapp.setLock":
			reply(req.ID, `{}`)
		case "miniapp.switch":
			reply(req.ID, `{"ok":true}`)
		case "cdp.command":
			reply(req.ID, `{"id":1,"result":{"targetInfos":[{"targetId":"T1","type":"page","title":"demo"}]}}`)
		case "runtime.evaluate":
			reply(req.ID, `{"value":42}`)
		case "cloud.scan":
			reply(req.ID, `[{"name":"hello","type":"function","params":["id"],"count":1}]`)
		case "code.format":
			reply(req.ID, `{"formatted":"formatted"}`)
		default:
			_, _ = writer.WriteString(`{"id":` + strconv.FormatInt(req.ID, 10) +
				`,"error":{"code":"unknown","message":"unknown method: ` + req.Method + `"}}` + "\n")
			_ = writer.Flush()
		}
	}
}

// fakeGeneration is the generation the fake Core reports on engine.status. Tests
// that need to simulate a page-realm rebuild (the capture lifecycle has to
// survive one) write the new value into FAKE_CORE_GENERATION_FILE; the fake
// re-reads it on every status request, so the shell's poller observes the change
// the same way it would after a reload. Anything unreadable means the default.
func fakeGeneration() int64 {
	path := os.Getenv("FAKE_CORE_GENERATION_FILE")
	if path == "" {
		return 1
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 1
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || value < 0 {
		return 1
	}
	return value
}

// fakePageDropCounters is appended to every fake drain response: the page-side
// drop readings (R12) a real hook repeats verbatim. Non-zero so the whole chain
// - Core JSON → engine.HookDrainPage → ipc.DrainPage → FeederStats → the three
// stats payloads - has something to carry.
const fakePageDropCounters = `"droppedRecords":4,"droppedUpdates":2`

// startFakeCoreApp boots the desktop shell against the fake Core and returns
// an App whose engine can be started for real (spawn + stdio RPC).
func startFakeCoreApp(t *testing.T) *App {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	script := filepath.Join(t.TempDir(), "cli.js")
	if err := os.WriteFile(script, []byte("// fake core placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FAKE_CORE_MAIN", "1")
	t.Setenv("WXTAP_CORE_CMD", exe)
	t.Setenv("WXTAP_CORE_SCRIPT", script)
	t.Setenv("WXTAP_TRAFFIC_DB", filepath.Join(t.TempDir(), "traffic.db"))
	t.Setenv("WXTAP_PACKAGES_DIR", t.TempDir())

	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	// startup() is Wails-only, so mirror its storage wiring here: without a
	// repository the drain path has nowhere to persist records.
	repo, err := traffic.Open(resolveDBPath(), resolveMigrationsDir(exeDir()))
	if err != nil {
		t.Fatalf("open traffic db: %v", err)
	}
	app.repo = repo
	app.trafficAPI = api.NewTrafficAPI(traffic.NewService(repo))
	app.setupIPC()
	return app
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The SessionKey tool must be able to pull its three inputs straight out of the
// captured packets instead of asking the user to copy them by hand.
func TestSessionKeyScanTrafficReadsCapturedPackets(t *testing.T) {
	app := startFakeCoreApp(t)
	defer app.shutdown(context.Background())

	records := []traffic.Record{{
		ID: "cap-1", Seq: 7, CapturedAt: time.Unix(1700000000, 0).UTC(),
		APIType: "wxapi", Name: "wx.login", AppID: "wxfake123", Status: traffic.StatusSuccess,
		Method: "POST", URL: "https://api.example.com/session",
		ResponseBody: []byte(`{"session_key":"tiihtNczf5v6AKRyjwEUhQ==","iv":"Xsdni3/wBgoPUlvmCMljyA=="}`),
	}}
	if _, err := app.repo.InsertBatch(context.Background(), records); err != nil {
		t.Fatalf("insert capture: %v", err)
	}

	envelope := app.Call("sessionkey.scanTraffic", `{}`)
	if strings.Contains(envelope, `"error"`) {
		t.Fatalf("sessionkey.scanTraffic: %s", envelope)
	}
	var decoded struct {
		Result struct {
			Scanned  int `json:"scanned"`
			Findings []struct {
				Kind   string `json:"kind"`
				Value  string `json:"value"`
				Masked string `json:"masked"`
				Source string `json:"source"`
			} `json:"findings"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(envelope), &decoded); err != nil {
		t.Fatalf("decode envelope %s: %v", envelope, err)
	}
	if decoded.Result.Scanned != 1 {
		t.Fatalf("scanned = %d, want 1", decoded.Result.Scanned)
	}
	kinds := map[string]bool{}
	for _, finding := range decoded.Result.Findings {
		kinds[finding.Kind] = true
		if finding.Masked == finding.Value {
			t.Fatalf("finding %+v must be masked for display", finding)
		}
		if !strings.HasPrefix(finding.Source, "traffic#cap-1 ") {
			t.Fatalf("source = %q, want the packet it came from", finding.Source)
		}
	}
	for _, kind := range []string{"session_key", "iv"} {
		if !kinds[kind] {
			t.Fatalf("missing %s finding: %s", kind, envelope)
		}
	}
}

// The desktop shell must drive the whole offline chain end to end: spawn the
// Core process, talk stdio RPC, drain hook records into SQLite, and expose
// them through the IPC surface the frontend uses.
func TestEngineLifecycleAndHookDrainEndToEnd(t *testing.T) {
	app := startFakeCoreApp(t)
	defer app.shutdown(context.Background())

	if err := app.EngineStart(62000); err != nil {
		t.Fatalf("engine.start: %v", err)
	}
	status, err := app.EngineStatus()
	if err != nil {
		t.Fatalf("engine.status: %v", err)
	}
	if !status.Frida || !status.Miniapp {
		t.Fatalf("status should report the fake Core state: %+v", status)
	}

	// hook.install + drain loop (500ms tick) must reach storage.
	installed := app.Call("wxapi.start", `{}`)
	if strings.Contains(installed, `"error"`) {
		t.Fatalf("wxapi.start: %s", installed)
	}
	waitFor(t, 10*time.Second, "hook records in SQLite", func() bool {
		page, err := app.TrafficList(api.TrafficListParams{PageSize: 100})
		return err == nil && len(page.Items) >= 2
	})

	// Records reach the UI buffer too, without duplicating the SQLite rows.
	// 先等缓冲本身（stats 的 pending 是只读的，不像 poll 会排空），再取一次：
	// poll 是排空读，直接调用它并断言就是「等在库上、断言在缓冲上」，而空缓冲
	// 同样能通过 envelope 检查 —— 那样这条注释声称的事其实没被验证。
	waitFor(t, 10*time.Second, "两条记录进入 poll 缓冲", func() bool {
		return wxapiPending(t, app) >= 2
	})
	if page := wxapiPoll(t, app, `{}`); len(page) < 2 {
		t.Fatalf("poll 只取到 %d 条，库里已有两条：%+v", len(page), page)
	}
	page, err := app.TrafficList(api.TrafficListParams{PageSize: 100})
	if err != nil {
		t.Fatalf("traffic.list: %v", err)
	}
	if len(page.Items) < 2 {
		t.Fatalf("drained records missing from storage: %+v", page.Items)
	}

	// Bodies have to survive the store round trip (gzip on insert): both the
	// traffic view binding and the MCP tool read them back decompressed.
	var requestRecord traffic.TrafficSummary
	for _, item := range page.Items {
		if item.APIType == "wx.request" {
			requestRecord = item
			break
		}
	}
	if requestRecord.ID == "" {
		t.Fatalf("drained wx.request record missing: %+v", page.Items)
	}
	if requestRecord.URL != "https://api.example.com/x" || requestRecord.Method != "POST" {
		t.Fatalf("request metadata lost in conversion: %+v", requestRecord)
	}
	body, err := app.TrafficGetBody(api.TrafficGetBodyParams{ID: requestRecord.ID, Part: "request"})
	if err != nil {
		t.Fatalf("traffic.getBody request: %v", err)
	}
	if want := strings.Repeat("x", 4096); !strings.Contains(string(body), want) {
		t.Fatalf("compressed request body round trip lost data (%d bytes)", len(body))
	}
	response, err := app.TrafficGetBody(api.TrafficGetBodyParams{ID: requestRecord.ID, Part: "response"})
	if err != nil {
		t.Fatalf("traffic.getBody response: %v", err)
	}
	if !strings.Contains(string(response), `"ok":true`) {
		t.Fatalf("response body round trip lost data: %s", response)
	}
	if _, err := app.TrafficGetBody(api.TrafficGetBodyParams{ID: "missing", Part: "request"}); err == nil {
		t.Fatal("traffic.getBody must fail for an unknown record")
	}
	if _, err := app.TrafficGetBody(api.TrafficGetBodyParams{ID: requestRecord.ID, Part: "nope"}); err == nil {
		t.Fatal("traffic.getBody must reject an unknown body part")
	}

	// miniapp.list flows through the same stdio channel.
	listed := app.Call("miniapp.list", `{}`)
	if !strings.Contains(listed, "wxfake123") {
		t.Fatalf("miniapp.list should carry the fake appid: %s", listed)
	}

	if err := app.EngineStop(); err != nil {
		t.Fatalf("engine.stop: %v", err)
	}
}

func TestEngineStartSurfacesTypedFailureWithoutCore(t *testing.T) {
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")
	t.Setenv("WXTAP_CORE_SCRIPT", filepath.Join(t.TempDir(), "cli.js"))
	t.Setenv("WXTAP_TRAFFIC_DB", filepath.Join(t.TempDir(), "traffic.db"))

	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()
	defer app.shutdown(context.Background())

	err := app.EngineStart(62000)
	if err == nil {
		t.Fatal("engine.start must fail when the Core runtime is missing")
	}
	// A pinned override is the operator's own choice, so the error has to name
	// the command that failed instead of guessing at an install they may
	// already have.
	if !strings.Contains(err.Error(), "wxtap-missing-node-xyz") {
		t.Fatalf("failure should name the failing runtime: %v", err)
	}
}
