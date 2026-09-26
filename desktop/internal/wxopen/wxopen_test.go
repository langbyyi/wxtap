package wxopen

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, server *httptest.Server, tokenCalls *int64) *Client {
	t.Helper()
	return &Client{
		HTTP:    server.Client(),
		BaseURL: server.URL,
		Token: func(context.Context, string, string, string) (string, int, error) {
			if tokenCalls != nil {
				atomic.AddInt64(tokenCalls, 1)
			}
			return "TOKEN-abc", 7200, nil
		},
	}
}

// The displayed exchange is the one that went out: the URL carries the real
// token and the body is the raw response, so the packet view is not a fiction.
func TestCallEchoesTheTokenInURLAndBody(t *testing.T) {
	var seenQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"TOKEN-abc","count":2,"data":{"openid":["o1"]}}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, nil)
	result, err := client.Call(context.Background(), Request{
		Mode: "oa", AccessKey: "wxapp", SecretKey: "secret", Endpoint: "oa-followers",
		Params: map[string]string{"next_openid": "o9"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(seenQuery, "access_token=TOKEN-abc") || !strings.Contains(seenQuery, "next_openid=o9") {
		t.Fatalf("request query = %q", seenQuery)
	}
	if !strings.Contains(result.URL, "access_token=TOKEN-abc") || !strings.Contains(result.URL, "next_openid=o9") {
		t.Fatalf("echoed URL = %q", result.URL)
	}
	body, ok := result.Body.(map[string]any)
	if !ok {
		t.Fatalf("body type = %#v", result.Body)
	}
	if body["access_token"] != "TOKEN-abc" {
		t.Fatalf("body token = %#v", body["access_token"])
	}
	if body["count"] != float64(2) {
		t.Fatalf("body lost fields: %#v", body)
	}
}

// Image endpoints return bytes, not JSON, and the UI renders them directly.
func TestCallReturnsImageDataURL(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("method/type = %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if payload["scene"] != "s=1" || payload["check_path"] != false || payload["width"] != float64(430) {
			t.Errorf("typed payload = %#v", payload)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer server.Close()

	client := newTestClient(t, server, nil)
	result, err := client.Call(context.Background(), Request{
		Mode: "mini", AccessKey: "wxapp", SecretKey: "secret", Endpoint: "mini-code-unlimited",
		Params: map[string]string{"scene": "s=1", "check_path": "false", "width": "430"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.HasPrefix(result.ImageDataURL, "data:image/png;base64,") {
		t.Fatalf("image data url = %q", result.ImageDataURL)
	}
	if result.Body != nil {
		t.Fatalf("image response must not carry a JSON body: %#v", result.Body)
	}
}

// A credential's token is reused until it expires, and another credential
// never reuses it.
func TestTokenIsCachedPerCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	var tokenCalls int64
	client := newTestClient(t, server, &tokenCalls)
	base := Request{Mode: "mini", Endpoint: "mini-sec-check", Params: map[string]string{"content": "hello", "openid": "o1"}}

	first := base
	first.AccessKey, first.SecretKey = "wx-one", "s1"
	if _, err := client.Call(context.Background(), first); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := client.Call(context.Background(), first); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt64(&tokenCalls); got != 1 {
		t.Fatalf("token lookups = %d, want 1 (cached)", got)
	}

	second := base
	second.AccessKey, second.SecretKey = "wx-two", "s2"
	if _, err := client.Call(context.Background(), second); err != nil {
		t.Fatalf("other credential call: %v", err)
	}
	if got := atomic.LoadInt64(&tokenCalls); got != 2 {
		t.Fatalf("token lookups = %d, want 2 (per credential)", got)
	}
}

// A credential family only reaches its own endpoints, and required parameters
// are enforced before any network call.
func TestCallValidatesModeEndpointAndParams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be sent for an invalid call: %s", r.URL.Path)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	if _, err := client.Call(context.Background(), Request{Mode: "mini", AccessKey: "a", SecretKey: "b", Endpoint: "nope"}); err == nil {
		t.Fatal("unknown endpoint accepted")
	}
	if _, err := client.Call(context.Background(), Request{Mode: "oa", AccessKey: "", SecretKey: "", Endpoint: "oa-followers"}); err == nil {
		t.Fatal("empty credential accepted")
	}
	if _, err := client.Call(context.Background(), Request{Mode: "oa", AccessKey: "a", SecretKey: "", Endpoint: "oa-followers"}); err == nil {
		t.Fatal("blank secret accepted")
	}
	err := func() error {
		_, err := client.Call(context.Background(), Request{Mode: "oa", AccessKey: "a", SecretKey: "b", Endpoint: "mini-code-unlimited", Params: map[string]string{"scene": "x"}})
		return err
	}()
	if err == nil || !strings.Contains(err.Error(), "只适用于") {
		t.Fatalf("cross-family call error = %v", err)
	}
	_, err = client.Call(context.Background(), Request{Mode: "mini", AccessKey: "a", SecretKey: "b", Endpoint: "mini-code-unlimited"})
	if err == nil || !strings.Contains(err.Error(), "scene") {
		t.Fatalf("missing required param error = %v", err)
	}
}

// An expired token is re-fetched instead of being reused, and the refresh
// happens before the server-side expiry.
func TestExpiredTokenIsRefetched(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	var tokenCalls int64
	client := newTestClient(t, server, &tokenCalls)
	clock := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	client.Now = func() time.Time { return clock }
	client.Token = func(context.Context, string, string, string) (string, int, error) {
		atomic.AddInt64(&tokenCalls, 1)
		return "TOKEN-abc", 7200, nil
	}

	request := Request{Mode: "mini", AccessKey: "wx-one", SecretKey: "s1", Endpoint: "mini-sec-check",
		Params: map[string]string{"content": "hello", "openid": "o1"}}
	if _, err := client.Call(context.Background(), request); err != nil {
		t.Fatalf("first call: %v", err)
	}
	clock = clock.Add(10 * time.Minute)
	if _, err := client.Call(context.Background(), request); err != nil {
		t.Fatalf("call inside lifetime: %v", err)
	}
	if got := atomic.LoadInt64(&tokenCalls); got != 1 {
		t.Fatalf("token lookups = %d, want 1 inside the lifetime", got)
	}

	clock = clock.Add(2 * time.Hour)
	if _, err := client.Call(context.Background(), request); err != nil {
		t.Fatalf("call after expiry: %v", err)
	}
	if got := atomic.LoadInt64(&tokenCalls); got != 2 {
		t.Fatalf("token lookups = %d, want 2 after expiry", got)
	}
}

// The console groups endpoints by category, so a new endpoint without one would
// silently vanish from the UI.
func TestEveryEndpointHasACategory(t *testing.T) {
	for _, endpoint := range Endpoints() {
		if strings.TrimSpace(endpoint.Category) == "" {
			t.Errorf("%s has no category", endpoint.ID)
		}
		if strings.TrimSpace(endpoint.Summary) == "" {
			t.Errorf("%s has no summary", endpoint.ID)
		}
	}
}

// 每个接口的 method 与 path 都是对着官方文档核过的，钉在这里：改错一个方法
// （比如把获取标签的 GET 写成 POST）只会在运行时表现为一个 errcode，静态是看不
// 出来的。新增接口不在表里不受影响，但改动既有接口必须一起改这里。
func TestEndpointMethodsMatchTheDocs(t *testing.T) {
	expected := map[string][2]string{
		"mini-code-unlimited":        {"POST", "/wxa/getwxacodeunlimit"},
		"mini-code-path":             {"POST", "/wxa/getwxacode"},
		"mini-qrcode":                {"POST", "/cgi-bin/wxaapp/createwxaqrcode"},
		"mini-sec-check":             {"POST", "/wxa/msg_sec_check"},
		"mini-account-info":          {"POST", "/cgi-bin/account/getaccountbasicinfo"},
		"mini-summary-trend":         {"POST", "/datacube/getweanalysisappiddailysummarytrend"},
		"mini-visit-page":            {"POST", "/datacube/getweanalysisappidvisitpage"},
		"oa-followers":               {"GET", "/cgi-bin/user/get"},
		"oa-user-info":               {"GET", "/cgi-bin/user/info"},
		"oa-user-batchget":           {"POST", "/cgi-bin/user/info/batchget"},
		"oa-tags":                    {"GET", "/cgi-bin/tags/get"},
		"oa-tag-followers":           {"POST", "/cgi-bin/user/tag/get"},
		"oa-menu":                    {"GET", "/cgi-bin/menu/get"},
		"oa-material":                {"POST", "/cgi-bin/material/batchget_material"},
		"oa-material-count":          {"GET", "/cgi-bin/material/get_materialcount"},
		"oa-callback-ip":             {"GET", "/cgi-bin/getcallbackip"},
		"work-user-list":             {"GET", "/cgi-bin/user/simplelist"},
		"work-user-list-detail":      {"GET", "/cgi-bin/user/list"},
		"work-user-get":              {"GET", "/cgi-bin/user/get"},
		"work-department-list":       {"GET", "/cgi-bin/department/list"},
		"work-department-get":        {"GET", "/cgi-bin/department/get"},
		"work-tag-list":              {"GET", "/cgi-bin/tag/list"},
		"work-agent-get":             {"GET", "/cgi-bin/agent/get"},
		"work-api-ip":                {"GET", "/cgi-bin/get_api_domain_ip"},
		"mini-daily-trend":           {"POST", "/datacube/getweanalysisappiddailyvisittrend"},
		"mini-weekly-trend":          {"POST", "/datacube/getweanalysisappidweeklyvisittrend"},
		"mini-monthly-trend":         {"POST", "/datacube/getweanalysisappidmonthlyvisittrend"},
		"mini-visit-distribution":    {"POST", "/datacube/getweanalysisappidvisitdistribution"},
		"mini-user-portrait":         {"POST", "/datacube/getweanalysisappiduserportrait"},
		"mini-weekly-retain":         {"POST", "/datacube/getweanalysisappidweeklyretaininfo"},
		"mini-monthly-retain":        {"POST", "/datacube/getweanalysisappidmonthlyretaininfo"},
		"mini-media-check":           {"POST", "/wxa/media_check_async"},
		"mini-subscribe-templates":   {"GET", "/wxaapi/newtmpl/gettemplate"},
		"oa-material-get":            {"POST", "/cgi-bin/material/get_material"},
		"oa-qrcode-create":           {"POST", "/cgi-bin/qrcode/create"},
		"oa-selfmenu":                {"GET", "/cgi-bin/get_current_selfmenu_info"},
		"oa-tags-of-user":            {"POST", "/cgi-bin/tags/getidlist"},
		"oa-shorturl":                {"POST", "/cgi-bin/shorturl"},
		"oa-quota-get":               {"POST", "/cgi-bin/openapi/quota/get"},
		"oa-rid-get":                 {"POST", "/cgi-bin/openapi/rid/get"},
		"oa-freepublish-batchget":    {"POST", "/cgi-bin/freepublish/batchget"},
		"oa-draft-batchget":          {"POST", "/cgi-bin/draft/batchget"},
		"oa-draft-count":             {"GET", "/cgi-bin/draft/count"},
		"oa-user-summary":            {"POST", "/datacube/getusersummary"},
		"oa-user-cumulate":           {"POST", "/datacube/getusercumulate"},
		"oa-article-summary":         {"POST", "/datacube/getarticlesummary"},
		"oa-article-total":           {"POST", "/datacube/getarticletotal"},
		"work-department-simplelist": {"GET", "/cgi-bin/department/simplelist"},
		"work-tag-members":           {"GET", "/cgi-bin/tag/get"},
	}
	for _, endpoint := range Endpoints() {
		want, known := expected[endpoint.ID]
		if !known {
			continue
		}
		if endpoint.Method != want[0] || endpoint.Path != want[1] {
			t.Errorf("%s = %s %s, want %s %s", endpoint.ID, endpoint.Method, endpoint.Path, want[0], want[1])
		}
	}
}

// The tool has no endpoint that delivers messages to real users; this guards
// the boundary against a future addition.
func TestEndpointTableStaysInspectionOnly(t *testing.T) {
	forbidden := []string{"message/", "custom/send", "subscribe/send", "webhook/send", "template/send"}
	for _, endpoint := range Endpoints() {
		lower := strings.ToLower(endpoint.Path)
		for _, blocked := range forbidden {
			if strings.Contains(lower, blocked) {
				t.Fatalf("%s reaches a message-sending endpoint: %s", endpoint.ID, endpoint.Path)
			}
		}
		if endpoint.Method != http.MethodGet && endpoint.Method != http.MethodPost {
			t.Fatalf("%s uses an unexpected method: %s", endpoint.ID, endpoint.Method)
		}
	}
}
