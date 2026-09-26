package ipc

import (
	"strings"
	"testing"
)

func TestCloudCallExpressionBuildsCallFunction(t *testing.T) {
	expression, err := CloudCallExpression("login", map[string]any{"a": 1})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := `window.cloudAudit.callFunction('login', {"a":1})`
	if expression != want {
		t.Fatalf("got  %s\nwant %s", expression, want)
	}
}

func TestCloudCallExpressionEscapesName(t *testing.T) {
	expression, err := CloudCallExpression("o'neil", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(expression, `'o\'neil'`) {
		t.Fatalf("name not escaped: %s", expression)
	}
}

func TestCloudContainerExpressionDefaultsToPost(t *testing.T) {
	expression, err := CloudContainerExpression("/api/x", "", map[string]any{"x-token": "t"}, map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, want := range []string{
		`new Promise((resolve)`,
		`path: '/api/x'`,
		`method: 'POST'`,
		`header: {"x-token":"t"}`,
		`data: {"k":"v"}`,
		`success: (res) => resolve(res)`,
		`fail: (err) => resolve({errMsg: (err && err.errMsg) || 'fail'})`,
	} {
		if !strings.Contains(expression, want) {
			t.Fatalf("missing %q in %s", want, expression)
		}
	}
}

func TestCloudContainerExpressionEscapesPathAndMethod(t *testing.T) {
	expression, err := CloudContainerExpression("/a'b", "PU'T", nil, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(expression, `path: '/a\'b'`) || !strings.Contains(expression, `method: 'PU\'T'`) {
		t.Fatalf("path/method not escaped: %s", expression)
	}
}

func TestNormalizeCloudContainerResultParsesJSONString(t *testing.T) {
	got := NormalizeCloudContainerResult(`{"statusCode":200,"data":{"ok":true}}`)
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("want object, got %#v", got)
	}
	if m["statusCode"] != float64(200) {
		t.Fatalf("statusCode: %#v", m["statusCode"])
	}
}

func TestNormalizeCloudContainerResultPassesObjectThrough(t *testing.T) {
	in := map[string]any{"errMsg": "fail"}
	got := NormalizeCloudContainerResult(in)
	m, ok := got.(map[string]any)
	if !ok || m["errMsg"] != "fail" {
		t.Fatalf("got %#v", got)
	}
}

func TestCloudCallExpressionEscapesControlCharacters(t *testing.T) {
	expression, err := CloudCallExpression("a\\b\n'c", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(expression, "\n'c") || !strings.Contains(expression, `a\\b\n\'c`) {
		t.Fatalf("control characters were not escaped: %s", expression)
	}
}
