package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initialize 是 agent 看到的第一份文档：协议版本要协商、能力要声明资源与提示、
// instructions 要把「先 engine_start 再 hook_start 才有数据」的顺序讲清楚。
func TestInitializeNegotiatesAndCarriesInstructions(t *testing.T) {
	messages := exchange(t, Deps{}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2020-01-01"}}`,
	}, "\n")+"\n")

	first := byID(messages, 1)["result"].(map[string]any)
	if first["protocolVersion"] != "2024-11-05" {
		t.Fatalf("支持的版本必须回声: %v", first)
	}
	caps := first["capabilities"].(map[string]any)
	for _, capability := range []string{"tools", "resources", "prompts"} {
		if _, ok := caps[capability]; !ok {
			t.Fatalf("initialize 未声明 %s 能力: %v", capability, caps)
		}
	}
	instructions, _ := first["instructions"].(string)
	if !strings.Contains(instructions, "engine_start") || !strings.Contains(instructions, "hook_start") {
		t.Fatalf("instructions 缺少会话引导顺序: %q", instructions)
	}

	second := byID(messages, 2)["result"].(map[string]any)
	if second["protocolVersion"] != "2025-06-18" {
		t.Fatalf("未知版本应回最新支持版: %v", second)
	}
}

// skills 目录以 skill:// 资源暴露：list 可发现、read 可取原文、猜的 URI
// 必须报错而不是空内容。
func TestSkillsAreReadableResources(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "debugging.md"), []byte("# 在线调试\n\n先 debugger_enable。"), 0o644); err != nil {
		t.Fatal(err)
	}
	messages := exchange(t, Deps{SkillsDir: dir}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"resources/list","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"skill://debugging.md"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"skill://nope.md"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"file:///etc/passwd"}}`,
	}, "\n")+"\n")

	resources := byID(messages, 1)["result"].(map[string]any)["resources"].([]any)
	// 内置参考文档 + 技能文件都在列表里。
	if len(resources) != 3 {
		t.Fatalf("resources/list 应列出 2 个参考 + 1 个技能: %v", resources)
	}
	var entry map[string]any
	for _, item := range resources {
		if candidate := item.(map[string]any); candidate["uri"] == "skill://debugging.md" {
			entry = candidate
		}
	}
	if entry == nil || entry["mimeType"] != "text/markdown" {
		t.Fatalf("资源描述不对: %v", resources)
	}
	if description, _ := entry["description"].(string); description != "在线调试" {
		t.Fatalf("描述应取首个标题: %q", description)
	}

	// 参考资源可读，内容是稳定知识（resources/read 的形状是 contents[].text）。
	refMessages := exchange(t, Deps{SkillsDir: dir}, `{"jsonrpc":"2.0","id":9,"method":"resources/read","params":{"uri":"wxtap://reference/hook-records"}}`+"\n")
	refResult := byID(refMessages, 9)["result"].(map[string]any)
	refContents := refResult["contents"].([]any)
	refText := refContents[0].(map[string]any)["text"].(string)
	if !strings.Contains(refText, "rid") || !strings.Contains(refText, "nextUpdateSeq") {
		t.Fatalf("hook-records 参考应含记录 schema 关键字段: %.200q", refText)
	}

	read := byID(messages, 2)["result"].(map[string]any)
	contents := read["contents"].([]any)
	text := contents[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "debugger_enable") {
		t.Fatalf("resources/read 应返回原文: %v", read)
	}

	if byID(messages, 3)["error"] == nil {
		t.Fatal("不存在的资源必须报错")
	}
	if byID(messages, 4)["error"] == nil {
		t.Fatal("未知 scheme 必须报错")
	}
}

// prompts 面是 agent 的工作流入口：list 稳定可发现，get 给出可执行剧本，
// 打错名字当场报错。
func TestPromptsListAndFetch(t *testing.T) {
	messages := exchange(t, Deps{}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"prompts/list","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"wx_pause_debug"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"no_such_prompt"}}`,
	}, "\n")+"\n")

	prompts := byID(messages, 1)["result"].(map[string]any)["prompts"].([]any)
	names := map[string]bool{}
	for _, item := range prompts {
		names[item.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"wx_debug_session", "wx_traffic_audit", "wx_code_audit", "wx_pause_debug"} {
		if !names[want] {
			t.Fatalf("prompts/list 缺少 %s: %v", want, names)
		}
	}

	result := byID(messages, 2)["result"].(map[string]any)
	messagesPayload := result["messages"].([]any)
	first := messagesPayload[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("prompt 消息应为 user 角色: %v", first)
	}
	text := first["content"].(map[string]any)["text"].(string)
	for _, tool := range []string{"debugger_list_scripts", "debugger_inspect", "debugger_control"} {
		if !strings.Contains(text, tool) {
			t.Fatalf("wx_pause_debug 剧本缺少工具 %s: %q", tool, text)
		}
	}

	if byID(messages, 3)["error"] == nil {
		t.Fatal("未知 prompt 必须报错")
	}
}

// 注解是 agent 的安全信号：只读工具必须标 readOnlyHint，破坏性工具必须标
// destructiveHint，缺一不可（规范默认值对两侧都是错的猜测）。
func TestToolAnnotationsAreAdvertised(t *testing.T) {
	// 注解覆盖全量面；lean 目录里没有的（traffic_list 等）也要带注解。
	messages := exchange(t, Deps{ToolProfile: "all"}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	tools := byID(messages, 1)["result"].(map[string]any)["tools"].([]any)
	byName := map[string]map[string]any{}
	for _, tool := range tools {
		entry := tool.(map[string]any)
		byName[entry["name"].(string)] = entry
	}

	cases := []struct {
		name        string
		readOnly    bool
		destructive bool
	}{
		{"traffic_list", true, false},
		{"debugger_state", true, false},
		{"miniapp_read_file", true, false},
		{"hook_stop", false, true},
		{"engine_stop", false, true},
		{"hook_start", false, false},
		{"miniapp_evaluate", false, false},
		{"miniapp_http_request", false, false},
	}
	for _, want := range cases {
		entry, ok := byName[want.name]
		if !ok {
			t.Fatalf("工具 %s 不在 tools/list 里", want.name)
		}
		annotations, ok := entry["annotations"].(map[string]any)
		if !ok {
			t.Fatalf("工具 %s 缺少 annotations: %v", want.name, entry)
		}
		if annotations["readOnlyHint"] != want.readOnly {
			t.Fatalf("%s readOnlyHint = %v, 应为 %v", want.name, annotations["readOnlyHint"], want.readOnly)
		}
		if annotations["destructiveHint"] != want.destructive {
			t.Fatalf("%s destructiveHint = %v, 应为 %v", want.name, annotations["destructiveHint"], want.destructive)
		}
	}
}

// 调试工具出现在 tools/list 且带 schema：agent 列表后必须能直接看出参数。
func TestDebuggerToolsAreAdvertised(t *testing.T) {
	// lean 目录广播合并形态。
	messages := exchange(t, Deps{}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	seen := map[string]bool{}
	for _, tool := range byID(messages, 1)["result"].(map[string]any)["tools"].([]any) {
		seen[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{
		"debugger_state", "debugger_control", "debugger_breakpoint",
		"debugger_list_scripts", "debugger_inspect", "cdp_command", "miniapp_get_source",
	} {
		if !seen[want] {
			t.Fatalf("lean 目录缺少调试工具 %s", want)
		}
	}
	// all 档案广播单用途旧名。
	messages = exchange(t, Deps{ToolProfile: "all"}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	seen = map[string]bool{}
	for _, tool := range byID(messages, 1)["result"].(map[string]any)["tools"].([]any) {
		seen[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{
		"debugger_enable", "debugger_pause", "debugger_resume",
		"debugger_step_over", "debugger_step_into", "debugger_step_out",
		"debugger_call_stack", "debugger_get_scopes",
		"debugger_evaluate_on_call_frame", "debugger_pause_on_exceptions",
		"miniapp_set_breakpoint", "miniapp_remove_breakpoint",
	} {
		if !seen[want] {
			t.Fatalf("all 档案缺少调试工具 %s", want)
		}
	}
}

func TestResourcesTemplatesListIsEmpty(t *testing.T) {
	messages := exchange(t, Deps{}, `{"jsonrpc":"2.0","id":1,"method":"resources/templates/list","params":{}}`+"\n")
	templates := byID(messages, 1)["result"].(map[string]any)["resourceTemplates"].([]any)
	if len(templates) != 0 {
		t.Fatalf("resourceTemplates 应为空: %v", templates)
	}
}

// inputSchema 是 MCP 规范里 Tool 的必填字段：漏掉一条，严格客户端（官方 SDK
// 逐条做 schema 校验）会拒绝整份 tools/list。两个档案的每个条目都必须带上
// type:object 的 schema。
func TestEveryAdvertisedToolHasInputSchema(t *testing.T) {
	for _, profile := range []string{"", "all"} {
		tools := New(Deps{ToolProfile: profile}).tools()
		if len(tools) == 0 {
			t.Fatalf("档案 %q 的 tools() 为空", profile)
		}
		for _, tool := range tools {
			if tool.InputSchema == nil {
				t.Errorf("工具 %s 缺少 inputSchema（规范必填）", tool.Name)
				continue
			}
			if tool.InputSchema["type"] != "object" {
				t.Errorf("工具 %s 的 inputSchema.type = %v，应为 object", tool.Name, tool.InputSchema["type"])
			}
			if _, ok := tool.InputSchema["properties"]; !ok {
				t.Errorf("工具 %s 的 inputSchema 缺少 properties", tool.Name)
			}
		}
	}
}
