package ipc

// AuditService 的服务级回归：契约测试（desktop/audit_contract_test.go）走真实
// router 钉形状，这里补上需要精确控制时序与对话框的几条——重放的截断与错误
// 通道、导出的取消/写盘、上游代理的现读现用、findings 的钳制与过滤。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

func openAuditFixture(t *testing.T) *traffic.Repository {
	t.Helper()
	repo, err := traffic.Open(filepath.Join(t.TempDir(), "traffic.db"), filepath.Join("..", "..", "..", "migrations"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// auditHookRecord builds one replayable wx.request record the way
// cloud.Convert stores them: the request body is the hook payload
// {url, method, data, header}.
func auditHookRecord(id string, at time.Time, rawURL string) traffic.Record {
	payload, err := json.Marshal(map[string]any{
		"url": rawURL, "method": "POST",
		"data": `{"uid":"1001"}`,
		"header": map[string]string{
			"Authorization": "Bearer t",
			"Content-Type":  "application/json",
		},
	})
	if err != nil {
		panic(err)
	}
	return traffic.Record{
		ID: id, Seq: 1, CapturedAt: at,
		APIType: "wx.request", Name: "request",
		Method: "POST", URL: rawURL, Status: traffic.StatusSuccess,
		RequestBody: payload,
	}
}

// traffic.replay：正常回包走成功通道；传输层失败落在 error 字段而 ok 保持
// true——被测后端的拒绝是测试结果，不是故障。
func TestTrafficReplayAnswersHonestChannels(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer backend.Close()

	repo := openAuditFixture(t)
	if _, err := traffic.Ingest(context.Background(), repo, []traffic.Record{
		auditHookRecord("rec-1", time.Now(), backend.URL+"/api/user"),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	service := NewAuditService(repo, nil, nil, nil, nil, nil)

	result, err := service.TrafficReplay(context.Background(), TrafficAuditParams{ID: "rec-1"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if result["ok"] != true || result["status"].(int) != 200 || result["body"] != `{"code":0}` {
		t.Fatalf("replay result = %#v", result)
	}
	if _, has := result["error"]; has {
		t.Fatalf("成功回包不得带 error: %#v", result)
	}

	missing, err := service.TrafficReplay(context.Background(), TrafficAuditParams{ID: "ghost"})
	if err != nil {
		t.Fatalf("missing replay: %v", err)
	}
	if missing["ok"] != false || missing["error"] != "记录不存在" {
		t.Fatalf("missing = %#v", missing)
	}
}

// 超过 4KB 的响应体截断并如实标注；配置的上游代理在每次执行时现读。
func TestTrafficReplayTruncatesBodyAndReadsProxyConfig(t *testing.T) {
	big := strings.Repeat("x", replayBodyLimit+100)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer backend.Close()

	repo := openAuditFixture(t)
	if _, err := traffic.Ingest(context.Background(), repo, []traffic.Record{
		auditHookRecord("rec-big", time.Now(), backend.URL+"/big"),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	base := t.TempDir()
	store := NewConfigStore(base)
	// 非法代理 URL 必须当场报错，而不是悄悄直连（静默换路由等于骗人）。
	if err := store.Save(context.Background(), map[string]any{"replayUpstreamProxy": "not-a-url"}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := NewAuditService(repo, nil, store, nil, nil, nil)
	if _, err := service.TrafficReplay(context.Background(), TrafficAuditParams{ID: "rec-big"}); err == nil {
		t.Fatal("非法上游代理必须报错")
	}

	// 清掉代理：同一份服务、同一条记录，这次走直连并截断。
	if err := store.Save(context.Background(), map[string]any{"replayUpstreamProxy": ""}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	result, err := service.TrafficReplay(context.Background(), TrafficAuditParams{ID: "rec-big"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if result["truncated"] != true || len(result["body"].(string)) != replayBodyLimit {
		t.Fatalf("截断标注不对: truncated=%v len=%d", result["truncated"], len(result["body"].(string)))
	}
}

// traffic.exportHar：写出的文件是合法 HAR，条目来自最近窗口；显式 ids 从同
// 一窗口挑。
func TestTrafficExportHarWritesParseableDocument(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer backend.Close()

	repo := openAuditFixture(t)
	records := []traffic.Record{auditHookRecord("rec-h1", time.Now(), backend.URL+"/api")}
	// 页内结果对象：statusCode/header/data 是 HAR 响应侧的唯一来源。
	records[0].ResponseBody = []byte(`{"statusCode":200,"header":{"Content-Type":"application/json"},"data":"hello"}`)
	if _, err := traffic.Ingest(context.Background(), repo, records); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	dir := t.TempDir()
	service := NewAuditService(repo, nil, nil, nil, func(defaultName, pattern string) (string, bool, error) {
		if defaultName == "" || pattern != "*.har" {
			t.Fatalf("HAR 对话框参数不对: %q %q", defaultName, pattern)
		}
		return filepath.Join(dir, "s.har"), true, nil
	}, nil)
	result, err := service.TrafficExportHar(context.Background(), TrafficExportHarParams{})
	if err != nil || result["path"] == nil {
		t.Fatalf("导出 = %#v err=%v", result, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "s.har"))
	if err != nil {
		t.Fatalf("read har: %v", err)
	}
	var doc struct {
		Log struct {
			Creator struct{ Name string } `json:"creator"`
			Entries []struct {
				Request struct {
					URL     string              `json:"url"`
					Headers []map[string]string `json:"headers"`
				} `json:"request"`
				Response struct {
					Status  int `json:"status"`
					Content struct {
						Text     string `json:"text"`
						MimeType string `json:"mimeType"`
					} `json:"content"`
				} `json:"response"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("HAR 必须可解析: %v", err)
	}
	if len(doc.Log.Entries) != 1 || doc.Log.Entries[0].Request.URL != backend.URL+"/api" {
		t.Fatalf("HAR 条目不对: %s", got)
	}
	if doc.Log.Entries[0].Response.Status != 200 || doc.Log.Entries[0].Response.Content.Text != "hello" {
		t.Fatalf("响应侧必须来自页内结果对象: %+v", doc.Log.Entries[0].Response)
	}
}

// 导出对话框在 GUI 之外不可用：报错而不是假装成功。
func TestExportWithoutDialogFailsHonestly(t *testing.T) {
	service := NewAuditService(openAuditFixture(t), nil, nil, nil, nil, nil)
	if _, err := service.TrafficExportHar(context.Background(), TrafficExportHarParams{}); err == nil {
		t.Fatal("无对话框必须报错")
	}
}
