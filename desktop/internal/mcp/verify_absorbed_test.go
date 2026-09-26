package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifyAbsorbedCoverage(t *testing.T) {
	lean := map[string]bool{}
	all := map[string]bool{}
	for _, tool := range New(Deps{}).tools() {
		lean[tool.Name] = true
	}
	for _, tool := range New(Deps{ToolProfile: "all"}).tools() {
		all[tool.Name] = true
	}
	absorbed := []string{}
	for name := range all {
		if !lean[name] {
			absorbed = append(absorbed, name)
		}
	}
	// 与 TestAbsorbedToolsStayDispatchable + compat fixture 的并集比对
	data, _ := json.Marshal(absorbed)
	t.Logf("absorbed count: %d -> %s", len(absorbed), string(data))
	// 每个被吸收名都要能被调用（不回答 unknown tool）
	lines := []string{}
	for index, name := range absorbed {
		lines = append(lines, `{"jsonrpc":"2.0","id":`+itoa(index+1)+`,"method":"tools/call","params":{"name":"`+name+`","arguments":{}}}`)
	}
	input := strings.Join(lines, "\n") + "\n"
	var out strings.Builder
	if err := New(Deps{Traffic: nil, Core: &fakeCore{}, AppBridge: fakeAppBridge{}}).Serve(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	for index, name := range absorbed {
		text := toolText(t, decodeAll(t, out.String()), float64(index+1))
		if strings.Contains(text, "unknown tool") {
			t.Errorf("absorbed %s answers unknown tool", name)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func decodeAll(t *testing.T, out string) []map[string]any {
	t.Helper()
	messages := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err == nil {
			messages = append(messages, msg)
		}
	}
	return messages
}
