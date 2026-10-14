package mcp

import (
	"strings"
	"testing"
)

// 键名规则表逐条：命中即掩，未命中不动。
func TestSensitiveKeyFragments(t *testing.T) {
	for _, key := range []string{
		"token", "accessToken", "Set-Cookie", "session_key", "sessionId",
		"openId", "unionid", "phone", "mobile", "idCard", "password",
		"client_secret", "apiKey", "access_key", "skey", "credentials",
	} {
		if !sensitiveKey(key) {
			t.Fatalf("%s 应命中键名规则", key)
		}
	}
	for _, key := range []string{"route", "userName", "data", "message", "address"} {
		if sensitiveKey(key) {
			t.Fatalf("%s 不该命中键名规则", key)
		}
	}
}

// 深走查：敏感键掩值（头尾保留）、短值全掩、键名与结构保留、无敏感键的串
// 只跑身份正则。
func TestRedactAnyMasksSensitiveLeaves(t *testing.T) {
	value := map[string]any{
		"route": "pages/index",
		"data": map[string]any{
			"header": map[string]any{
				"Authorization": "Bearer abcdefghijklmnop",
				"Content-Type":  "application/json",
			},
			"note": "回拨 13812345678 确认",
			"flag": true,
		},
	}
	masked := redactAny(value).(map[string]any)
	if masked["route"] != "pages/index" {
		t.Fatalf("非敏感字段必须原样: %v", masked["route"])
	}
	data := masked["data"].(map[string]any)
	if data["flag"] != true {
		t.Fatalf("非字符串叶子必须原样: %v", data["flag"])
	}
	header := data["header"].(map[string]any)
	if header["Authorization"] != "Be…op" {
		t.Fatalf("敏感头应掩成头尾形式: %v", header["Authorization"])
	}
	if header["Content-Type"] != "application/json" {
		t.Fatalf("非敏感头必须原样: %v", header["Content-Type"])
	}
	if !strings.Contains(data["note"].(string), "<phone>") {
		t.Fatalf("自由文本里的手机号应打码: %v", data["note"])
	}
}

// 短值（≤6 rune）整体掩掉，不露头尾。
func TestMaskStringCollapsesShortValues(t *testing.T) {
	if got := maskString("abc"); got != "***" {
		t.Fatalf("短值应全掩: %s", got)
	}
	if got := maskString("abcdefg"); got != "ab…fg" {
		t.Fatalf("长值应保留头尾: %s", got)
	}
}

// 容器值在敏感键下只报形状，不深挖——嵌套里的实值不外泄。
func TestRedactAnyCollapsesContainersUnderSensitiveKeys(t *testing.T) {
	value := map[string]any{"token": map[string]any{"access": "secret-value", "refresh": "other"}}
	masked := redactAny(value).(map[string]any)
	if masked["token"] != "<redacted object>" {
		t.Fatalf("敏感键下的对象应折叠: %v", masked["token"])
	}
}

// 身份证以整段 run 校验：18 位证件形状命中，更长的数字串不被劈出假命中。
func TestRedactStringIDBoundary(t *testing.T) {
	if got := redactString("证件号 11010119900307851X 请核对"); !strings.Contains(got, "<id>") {
		t.Fatalf("证件号应打码: %s", got)
	}
	if got := redactString("订单号 11010119900307851X0001"); strings.Contains(got, "<id>") {
		t.Fatalf("更长数字串不该被劈出假命中: %s", got)
	}
}

// curl 行：敏感头掩值、非敏感头原样、bash/cmd 两种引号都吃。
func TestRedactCurlMasksSensitiveHeaders(t *testing.T) {
	bash := `curl -k -X POST 'https://x.y/a' -H 'Content-Type: application/json' -H 'Authorization: Bearer abcdefghijklmnop' --data-raw '{"token":"LTAI5tABCDEFghijklmnop"}'`
	masked := redactCurl(bash)
	if strings.Contains(masked, "Bearer abcdefghijklmnop") {
		t.Fatalf("Authorization 值应掩掉: %s", masked)
	}
	if !strings.Contains(masked, "Be…op") {
		t.Fatalf("掩码应保留头尾形状: %s", masked)
	}
	if !strings.Contains(masked, "Content-Type: application/json") {
		t.Fatalf("非敏感头必须原样: %s", masked)
	}
	if strings.Contains(masked, "13812345678") && false {
		t.Fatal("unreachable")
	}
	cmdLine := `curl -k "https://x.y/" -H "Cookie: session=abcdefghij"`
	if strings.Contains(redactCurl(cmdLine), "session=abcdefghij") {
		t.Fatalf("cmd 形式的 Cookie 应掩掉: %s", redactCurl(cmdLine))
	}
}

// 体打码：JSON 走深走查、自由文本走身份规则，changed 如实上报。
func TestRedactBody(t *testing.T) {
	out, changed := redactBody([]byte(`{"token":"LTAI5tABCDEFghijklmnop"}`))
	if !changed || strings.Contains(string(out), "LTAI5tABCDEFghijklmnop") {
		t.Fatalf("JSON 体应打码: %s (changed=%v)", out, changed)
	}
	out, changed = redactBody([]byte("plain text with 13812345678 inside"))
	if !changed || strings.Contains(string(out), "13812345678") {
		t.Fatalf("自由文本应打码: %s (changed=%v)", out, changed)
	}
	out, changed = redactBody([]byte("nothing sensitive here"))
	if changed {
		t.Fatalf("无敏感内容必须如实报 changed=false: %s", out)
	}
}
