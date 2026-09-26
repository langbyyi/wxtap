package cloudapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func addr(port int, path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
}

func post(t *testing.T, url string, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func TestGetReturnsUsage(t *testing.T) {
	s := New(func(string, map[string]any) (any, error) { return nil, nil })
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}
	resp, err := http.Get(addr(s.Port(), "/"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	if payload["service"] != "WxTap Cloud API" {
		t.Fatalf("service: %v", payload)
	}
	if !strings.Contains(payload["usage"].(string), "POST /call") {
		t.Fatalf("usage: %v", payload)
	}
}

func TestPostCallDispatchesCloudCall(t *testing.T) {
	var gotMethod string
	var gotParams map[string]any
	s := New(func(method string, params map[string]any) (any, error) {
		gotMethod, gotParams = method, params
		return map[string]any{"ok": true, "status": "success"}, nil
	})
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}
	status, body := post(t, addr(s.Port(), "/call"), `{"name":"login","data":{"a":1}}`)
	if status != 200 {
		t.Fatalf("status: %d body %s", status, body)
	}
	if gotMethod != "cloud.call" {
		t.Fatalf("method: %s", gotMethod)
	}
	if gotParams["name"] != "login" {
		t.Fatalf("params: %v", gotParams)
	}
	if !strings.Contains(body, `"status":"success"`) {
		t.Fatalf("body: %s", body)
	}
}

func TestPostCallRoutesContainerCalls(t *testing.T) {
	var gotMethod string
	var gotParams map[string]any
	s := New(func(method string, params map[string]any) (any, error) {
		gotMethod, gotParams = method, params
		return map[string]any{"ok": true}, nil
	})
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}
	status, _ := post(t, addr(s.Port(), "/call"), `{"name":"anything","data":{"path":"/api/x","header":{"t":"1"},"data":{"k":2}}}`)
	if status != 200 {
		t.Fatalf("status: %d", status)
	}
	if gotMethod != "cloud.call_container" {
		t.Fatalf("method: %s", gotMethod)
	}
	if gotParams["method"] != "POST" || gotParams["path"] != "/api/x" {
		t.Fatalf("params: %v", gotParams)
	}
}

// Only a data object that carries both a path and a method/header key is
// treated as a container call; a bare path stays a plain function call.
func TestPostCallKeepsPlainPathDataAsCloudCall(t *testing.T) {
	var gotMethod string
	s := New(func(method string, _ map[string]any) (any, error) {
		gotMethod = method
		return map[string]any{"ok": true}, nil
	})
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}

	status, body := post(t, addr(s.Port(), "/call"), `{"name":"login","data":{"path":"/api/x"}}`)
	if status != 200 || gotMethod != "cloud.call" {
		t.Fatalf("method %s status %d body %s", gotMethod, status, body)
	}
}

// Container calls may also arrive with a method and no header key.
func TestPostCallRoutesContainerCallWithMethodOnly(t *testing.T) {
	var gotMethod string
	var gotParams map[string]any
	s := New(func(method string, params map[string]any) (any, error) {
		gotMethod, gotParams = method, params
		return map[string]any{"ok": true}, nil
	})
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}

	status, _ := post(t, addr(s.Port(), "/call"), `{"name":"c1","data":{"path":"/api/y","method":"GET","data":"body"}}`)
	if status != 200 || gotMethod != "cloud.call_container" {
		t.Fatalf("method %s status %d", gotMethod, status)
	}
	if gotParams["method"] != "GET" || gotParams["path"] != "/api/y" || gotParams["header"] != nil {
		t.Fatalf("params: %v", gotParams)
	}
}

// Non-object payloads pass through to cloud.call untouched.
func TestPostCallPassesArrayDataToCloudCall(t *testing.T) {
	var gotData any
	s := New(func(_ string, params map[string]any) (any, error) {
		gotData = params["data"]
		return map[string]any{"ok": true}, nil
	})
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}

	status, _ := post(t, addr(s.Port(), "/call"), `{"name":"batch","data":[1,2,3]}`)
	items, ok := gotData.([]any)
	if status != 200 || !ok || len(items) != 3 {
		t.Fatalf("status %d data %#v", status, gotData)
	}
}

func TestPostCallRejectsUnknownPathAndMethod(t *testing.T) {
	calls := 0
	s := New(func(string, map[string]any) (any, error) {
		calls++
		return map[string]any{"ok": true}, nil
	})
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}

	if status, _ := post(t, addr(s.Port(), "/other"), `{"name":"login"}`); status != 404 {
		t.Fatalf("unknown path status: %d", status)
	}
	req, err := http.NewRequest(http.MethodPut, addr(s.Port(), "/call"), strings.NewReader(`{"name":"login"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 404 {
		t.Fatalf("unsupported method status: %d", resp.StatusCode)
	}
	if calls != 0 {
		t.Fatalf("dispatch ran for rejected requests: %d", calls)
	}
}
func TestPostCallValidation(t *testing.T) {
	s := New(func(string, map[string]any) (any, error) { return nil, nil })
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}

	status, body := post(t, addr(s.Port(), "/call"), `{"data":{}}`)
	if status != 400 || !strings.Contains(body, "missing 'name' field") {
		t.Fatalf("missing name: %d %s", status, body)
	}

	status, body = post(t, addr(s.Port(), "/call"), `{bad json`)
	if status != 400 || !strings.Contains(body, "invalid JSON") {
		t.Fatalf("bad json: %d %s", status, body)
	}

	status, body = post(t, addr(s.Port(), "/other"), `{}`)
	if status != 404 || !strings.Contains(body, "not found") {
		t.Fatalf("not found: %d %s", status, body)
	}
}

func TestPostCallDispatchErrorYields500(t *testing.T) {
	s := New(func(string, map[string]any) (any, error) {
		return nil, &testError{"engine offline"}
	})
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}
	status, body := post(t, addr(s.Port(), "/call"), `{"name":"f"}`)
	if status != 500 || !strings.Contains(body, "engine offline") {
		t.Fatalf("status: %d body %s", status, body)
	}
}

func TestStartStopLifecycle(t *testing.T) {
	s := New(func(string, map[string]any) (any, error) { return nil, nil })
	if s.Running() {
		t.Fatal("not running before start")
	}
	if err := s.Start(18931); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !s.Running() || s.Port() != 18931 {
		t.Fatalf("running=%v port=%d", s.Running(), s.Port())
	}
	// Starting again restarts cleanly (stop-then-start).
	if err := s.Start(18932); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if s.Port() != 18932 {
		t.Fatalf("port after restart: %d", s.Port())
	}
	s.Stop()
	if s.Running() || s.Port() != 0 {
		t.Fatalf("after stop: running=%v port=%d", s.Running(), s.Port())
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// The loopback HTTP forwarder must bound request bodies: cloud payloads are
// operator-sized, not unbounded, and an oversized body must fail fast.
func TestCloudAPIPostRejectsOversizedBody(t *testing.T) {
	s := New(func(string, map[string]any) (any, error) { return map[string]any{"ok": true}, nil })
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}

	payload := `{"name":"login","data":"` + strings.Repeat("a", maxCloudAPIBodyBytes+1024) + `"}`
	resp, err := http.Post(addr(s.Port(), "/call"), "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status %d want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

// A local HTTP surface must not be readable by arbitrary web pages: browsers
// send Origin on cross-origin requests, and without an
// Access-Control-Allow-Origin header the response (and the preflight) is
// denied. Native callers (curl, MCP clients) never need CORS.
func TestCloudAPIRejectsCrossOriginBrowserAccess(t *testing.T) {
	s := New(func(string, map[string]any) (any, error) { return map[string]any{"ok": true}, nil })
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Preflight from a browser must not be granted.
	req, err := http.NewRequest(http.MethodOptions, addr(s.Port(), "/call"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Content-Type")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if origin := resp.Header.Get("Access-Control-Allow-Origin"); origin != "" {
		t.Fatalf("preflight granted cross-origin access: %q", origin)
	}

	// A normal GET response must not advertise a wildcard origin either.
	getResp, err := http.Get(addr(s.Port(), "/"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = getResp.Body.Close() }()
	if origin := getResp.Header.Get("Access-Control-Allow-Origin"); origin != "" {
		t.Fatalf("GET advertised %q", origin)
	}
}

// A hostile page never needs a preflight: a text/plain POST is a CORS "simple
// request", so the browser sends it straight at the loopback server. Removing
// the wildcard response header alone therefore still let any page the user
// visits drive cloud calls, which is exactly what the CORS comment claims to
// prevent.
func TestCloudAPIRejectsSimpleCrossOriginPost(t *testing.T) {
	dispatched := false
	s := New(func(string, map[string]any) (any, error) {
		dispatched = true
		return map[string]any{"ok": true}, nil
	})
	defer s.Stop()
	if err := s.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, addr(s.Port(), "/call"), strings.NewReader(`{"name":"evil","data":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST: status %d want %d", resp.StatusCode, http.StatusForbidden)
	}
	if dispatched {
		t.Fatal("a cross-origin page drove a cloud call")
	}
}

// The Origin gate must not lock out the callers this surface exists for.
func TestCloudAPIAllowsNativeAndLoopbackCallers(t *testing.T) {
	cases := []struct {
		name   string
		origin string
	}{
		{name: "native caller without Origin"},
		{name: "bundled devtools frontend", origin: "http://127.0.0.1:5555"},
		{name: "devtools scheme", origin: "devtools://devtools/bundled/inspector.html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dispatched := false
			s := New(func(string, map[string]any) (any, error) {
				dispatched = true
				return map[string]any{"ok": true}, nil
			})
			defer s.Stop()
			if err := s.Start(0); err != nil {
				t.Fatalf("start: %v", err)
			}
			req, err := http.NewRequest(http.MethodPost, addr(s.Port(), "/call"), strings.NewReader(`{"name":"login"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d want %d", resp.StatusCode, http.StatusOK)
			}
			if !dispatched {
				t.Fatal("call was not dispatched")
			}
		})
	}
}
