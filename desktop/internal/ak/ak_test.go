package ak

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifyMiniProgramKeepsTokenOptInAndRawBody(t *testing.T) {
	var gotQuery, gotXFF string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotXFF = r.Header.Get("X-Forwarded-For")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ACCESS_TOKEN_WITH_LONG_VALUE",
			"expires_in":   7200,
		})
	}))
	defer server.Close()

	result, err := (&Verifier{Base: server.URL}).Verify(context.Background(), Request{
		Mode: "mini", AccessKey: "wx123", SecretKey: "secret", FakeIP: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid || result.ErrCode != 0 {
		t.Fatalf("result should be valid: %+v", result)
	}
	if result.Token != "" {
		t.Fatalf("token must be omitted by default: %q", result.Token)
	}
	if result.TokenFingerprint == "" {
		t.Fatal("token fingerprint missing")
	}
	if gotXFF != "192.0.2.10" {
		t.Fatalf("fake IP header = %q", gotXFF)
	}
	for _, want := range []string{"appid=wx123", "secret=secret", "grant_type=client_credential"} {
		if !queryContains(gotQuery, want) {
			t.Fatalf("query %q missing %q", gotQuery, want)
		}
	}
	// 报表面板显示的就是这一次真实请求：凭据不掩码，否则那行报文是假的。
	if !strings.Contains(result.RequestURL, "secret=secret") || strings.Contains(result.RequestURL, "***") {
		t.Fatalf("request URL = %q", result.RequestURL)
	}
	body, ok := result.Body.(map[string]any)
	if !ok {
		t.Fatalf("body type = %T", result.Body)
	}
	// 响应体原样返回：access_token 不再被打成 ACCE...ALUE。
	if body["access_token"] != "ACCESS_TOKEN_WITH_LONG_VALUE" {
		t.Fatalf("body token = %#v", body["access_token"])
	}
}

func TestVerifyEnterpriseUsesWorkEndpoint(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 40013, "errmsg": "invalid corpid"})
	}))
	defer server.Close()

	result, err := (&Verifier{Base: server.URL}).Verify(context.Background(), Request{
		Mode: "work", AccessKey: "corp", SecretKey: "secret",
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if path != "/cgi-bin/gettoken" {
		t.Fatalf("path = %q", path)
	}
	if result.Valid || result.ErrCode != 40013 {
		t.Fatalf("invalid credential must be a normal result: %+v", result)
	}
}

func TestVerifyRejectsUnsupportedModeAndBadIP(t *testing.T) {
	verifier := &Verifier{}
	if _, err := verifier.Verify(context.Background(), Request{Mode: "other", AccessKey: "a", SecretKey: "b"}); err == nil {
		t.Fatal("unsupported mode must fail")
	}
	if _, err := verifier.Verify(context.Background(), Request{Mode: "mini", AccessKey: "a", SecretKey: "b", FakeIP: "not-ip"}); err == nil {
		t.Fatal("bad fake IP must fail")
	}
}

func queryContains(raw, pair string) bool {
	for _, item := range splitQuery(raw) {
		if item == pair {
			return true
		}
	}
	return false
}

func splitQuery(raw string) []string {
	var out []string
	start := 0
	for index, char := range raw {
		if char == '&' {
			out = append(out, raw[start:index])
			start = index + 1
		}
	}
	return append(out, raw[start:])
}
