package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/langbyyi/wxtap/desktop/internal/mcp"
)

// MCP 的直通工具把 agent 的调用转成一条 GUI 的 IPC 方法。工具名与 IPC 方法名不同名
// （miniapp_* 那套是历史命名），所以这层映射必须逐条对着**真实注册处**校验：方法改名
// 时，界面侧会被前端的白名单测试拦下，而 MCP 这一侧只会到 agent 真的调用时才回一句
// 「unsupported backend method」——那时已经晚了一整个发布周期。
func TestMCPPassThroughToolsTargetRegisteredMethods(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.setupIPC()

	registered := map[string]bool{}
	for _, method := range app.router.Methods() {
		registered[method] = true
	}
	if len(registered) == 0 {
		t.Fatal("router 没有注册任何方法，这条断言就失去意义")
	}
	if len(mcp.PassThroughTools) == 0 {
		t.Fatal("直通工具表为空：这张表被清空时，下面的循环会静默通过")
	}
	if len(mcp.SelectorTools) == 0 {
		t.Fatal("多方法工具表为空：这张表被清空时，下面的循环会静默通过")
	}
	for tool, spec := range mcp.PassThroughTools {
		if !registered[spec.Method] {
			t.Fatalf("MCP 工具 %s 直通的方法 %s 未在 router 上注册", tool, spec.Method)
		}
	}
	// 一工具落多方法的那些也要逐条校验：目标如果埋在一个 switch 里，改名同样只会
	// 到 agent 调用时才暴露。
	for tool, spec := range mcp.SelectorTools {
		if len(spec.Methods) == 0 {
			t.Fatalf("多方法工具 %s 没有登记任何目标方法", tool)
		}
		for value, method := range spec.Methods {
			if !registered[method] {
				t.Fatalf("MCP 工具 %s 的选择子 %q 落到未注册的方法 %s", tool, value, method)
			}
		}
	}
}

// 直通工具必须与工具的声明同时存在：有映射没有工具定义（或反过来）都会让 agent 看到
// 一个列不出来、或列出来就报未知工具的名字。这里走的是真实的 tools/list，所以断言的是
// 客户端实际看到的那份清单。
func TestMCPPassThroughToolsAreAdvertised(t *testing.T) {
	var out bytes.Buffer
	request := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n"
	if err := mcp.New(mcp.Deps{ToolProfile: "all"}).Serve(strings.NewReader(request), &out); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var message struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &message); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, out.String())
	}
	if len(message.Result.Tools) == 0 {
		t.Fatal("tools/list 没有返回任何工具")
	}
	advertised := map[string]bool{}
	for _, tool := range message.Result.Tools {
		advertised[tool.Name] = true
	}
	for tool := range mcp.PassThroughTools {
		if !advertised[tool] {
			t.Fatalf("直通工具 %s 没有被 tools/list 列出", tool)
		}
	}
	for tool := range mcp.SelectorTools {
		if !advertised[tool] {
			t.Fatalf("多方法工具 %s 没有被 tools/list 列出", tool)
		}
	}
}
