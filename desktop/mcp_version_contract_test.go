package main

import (
	"strings"
	"testing"
)

// 外壳装配的 MCP 服务器必须在握手时上报 appVersion。这个版本号曾经在
// desktop/internal/mcp 里又写死了一份 2.0.0，而应用自己报的是 v1.0.0 ——
// 第三处没人对账的版本号，正是这条契约要挡住的东西。
func TestMCPServerAnnouncesTheAppVersion(t *testing.T) {
	var out strings.Builder
	server := buildMCPServer("lean")
	if err := server.Serve(
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"),
		&out,
	); err != nil {
		t.Fatalf("serve: %v", err)
	}
	want := `"version":"` + strings.TrimPrefix(appVersion, "v") + `"`
	if !strings.Contains(out.String(), want) {
		t.Fatalf("MCP 握手应含 %s，实际 %s", want, out.String())
	}
}
