// MCP stdio mode: adapts the desktop providers to the mcp.Deps surface so an
// external LLM client can drive WxTap. Launched with `WxTap -mcp`.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/langbyyi/wxtap/desktop/internal/api"
	"github.com/langbyyi/wxtap/desktop/internal/engine"
	"github.com/langbyyi/wxtap/desktop/internal/extract"
	"github.com/langbyyi/wxtap/desktop/internal/mcp"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// mcpTraffic adapts the traffic API to the MCP traffic surface.
type mcpTraffic struct {
	api *api.TrafficAPI
}

func (t *mcpTraffic) List(ctx context.Context, params mcp.ListParams) (mcp.Page, error) {
	page, err := t.api.TrafficList(ctx, api.TrafficListParams{
		Page:     params.Page,
		PageSize: params.Limit,
		Query:    params.Query,
		APIType:  params.APType,
		Status:   params.Status,
		AppID:    params.AppID,
	})
	if err != nil {
		return mcp.Page{}, err
	}
	// An empty page must marshal as [] so schema-validated MCP clients can
	// iterate the result.
	out := mcp.Page{Items: []mcp.Item{}, Total: page.Total}
	for _, item := range page.Items {
		out.Items = append(out.Items, mcp.Item{
			ID:            item.ID,
			Seq:           item.Seq,
			CapturedAt:    item.CapturedAt,
			APType:        item.APIType,
			AppID:         item.AppID,
			Name:          item.Name,
			Method:        item.Method,
			URL:           item.URL,
			Status:        string(item.Status),
			RequestBytes:  int(item.RequestBytes),
			ResponseBytes: int(item.ResponseBytes),
			DurationMs:    item.DurationMs,
		})
	}
	return out, nil
}

func (t *mcpTraffic) GetBody(ctx context.Context, id string, part string) ([]byte, error) {
	return t.api.TrafficGetBody(ctx, api.TrafficGetBodyParams{ID: id, Part: part})
}

// mcpCore adapts the engine client to the MCP core surface.
type mcpCore struct {
	client *engine.Client
}

func (c *mcpCore) Status(ctx context.Context) (mcp.Status, error) {
	status, err := c.client.Status(ctx)
	if err != nil {
		return mcp.Status{}, err
	}
	return mcpStatus(status), nil
}

func mcpStatus(status engine.EngineStatus) mcp.Status {
	out := mcp.Status{Frida: status.Frida, Miniapp: status.Miniapp, Devtools: status.Devtools, Generation: status.Generation}
	if status.AppInfo != nil {
		out.AppInfo = &mcp.AppInfo{AppID: status.AppInfo.AppID, Name: status.AppInfo.Name}
	}
	return out
}

// mcpCore adapts the engine client to the MCP core surface. The settled-update
// page size when the caller does not ask for one is internal/mcp's
// defaultUpdateLimit — the shell keeps no second copy of that default.
func (c *mcpCore) HookDrain(ctx context.Context, name string, afterSeq int64, limit int, afterUpdateSeq int64, updateLimit int) (mcp.DrainPage, error) {
	// Records are the tool's primary payload; the settled-update stream rides
	// along so an agent sees the outcome of async calls that landed after their
	// record was drained (without it they look permanently pending).
	page, err := c.client.HookDrain(ctx, name, afterSeq, limit, afterUpdateSeq, updateLimit)
	if err != nil {
		return mcp.DrainPage{}, err
	}
	out := mcp.DrainPage{NextSeq: page.NextSeq, NextUpdateSeq: page.NextUpdateSeq, HasMore: page.HasMore}
	for _, record := range page.Records {
		out.Records = append(out.Records, mcp.DrainedRecord{Seq: record.Seq, Record: record.Record})
	}
	for _, update := range page.Updates {
		out.Updates = append(out.Updates, mcp.DrainedUpdate{Seq: update.Seq, Update: update.Update})
	}
	// 空页必须序列化为 [] 而不是 null：schema 严格的 MCP 客户端会拿 null 去迭代。
	if out.Records == nil {
		out.Records = []mcp.DrainedRecord{}
	}
	if out.Updates == nil {
		out.Updates = []mcp.DrainedUpdate{}
	}
	return out, nil
}

func (c *mcpCore) HookClear(ctx context.Context, name string) error {
	return c.client.HookClear(ctx, name)
}

func (c *mcpCore) CloudScan(ctx context.Context) ([]map[string]any, error) {
	return c.client.CloudScan(ctx)
}

func (c *mcpCore) CDPCommand(ctx context.Context, method string, params map[string]any, timeoutMs int) (map[string]any, error) {
	return c.client.CDPCommand(ctx, method, params, timeoutMs)
}

func (c *mcpCore) DebugState(ctx context.Context, enable bool) (map[string]any, error) {
	return c.client.DebugState(ctx, enable)
}

type mcpAppBridge struct{ app *App }

func (l *mcpAppBridge) Call(ctx context.Context, method string, params map[string]any) (any, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return l.app.router.Call(ctx, method, raw)
}

// mcpScan adapts the extract scanner to the MCP scan tool.
func mcpScan(ctx context.Context, dir string) (mcp.ScanResult, error) {
	_ = ctx
	// A mistyped path must not read as a clean 0-file scan.
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return mcp.ScanResult{}, fmt.Errorf("directory not found: %s", dir)
	}
	result := extract.ScanFiles(dir)
	summary := map[string]int{}
	for key, values := range result.Analysis {
		summary[key] = len(values)
	}
	// Keep the raw findings out of the summary payload; the caller fetches
	// details via the report file. But include them (bounded by JSON size
	// naturally) for small result sets.
	report := map[string]any{"files_scanned": len(result.FilesScanned), "result": result.Analysis, "findings": result.Findings}
	raw, err := json.Marshal(report)
	if err != nil {
		return mcp.ScanResult{}, err
	}
	return mcp.ScanResult{
		FilesScanned: len(result.FilesScanned),
		TotalSize:    result.TotalSize,
		Summary:      summary,
		Report:       raw,
	}, nil
}

// buildMCPServer assembles the MCP server over fresh providers. profile is
// the tools/list advertisement ("lean" or "all").
func buildMCPServer(profile string) *mcp.Server {
	app := NewApp()
	deps := mcp.Deps{Scan: mcpScan, SkillsDir: resolveSkillsDir(), ToolProfile: profile, AllowedCodeRoot: app.extractOutputRoot(), Version: appVersion}

	repo, err := traffic.Open(resolveDBPath(), resolveMigrationsDir(exeDir()))
	if err == nil {
		app.repo = repo
		app.trafficAPI = api.NewTrafficAPI(traffic.NewService(repo))
		deps.Traffic = &mcpTraffic{api: app.trafficAPI}
	}
	app.setupIPC()
	deps.Core = &appMCPCore{bridge: &engineBridge{app: app}}
	deps.AppBridge = &mcpAppBridge{app: app}

	return mcp.New(deps)
}

// appMCPCore adapts the App's engine bridge to the MCP core surface: the SSE
// server shares the GUI's Core process instead of spawning a second one.
type appMCPCore struct{ bridge *engineBridge }

func (c *appMCPCore) Status(ctx context.Context) (mcp.Status, error) {
	client, err := c.bridge.Engine()
	if err != nil {
		return mcp.Status{}, err
	}
	status, err := client.Status(ctx)
	if err != nil {
		return mcp.Status{}, err
	}
	return mcpStatus(status), nil
}

func (c *appMCPCore) HookDrain(ctx context.Context, name string, afterSeq int64, limit int, afterUpdateSeq int64, updateLimit int) (mcp.DrainPage, error) {
	client, err := c.bridge.Engine()
	if err != nil {
		return mcp.DrainPage{}, err
	}
	core := &mcpCore{client: client}
	return core.HookDrain(ctx, name, afterSeq, limit, afterUpdateSeq, updateLimit)
}

func (c *appMCPCore) HookClear(ctx context.Context, name string) error {
	client, err := c.bridge.Engine()
	if err != nil {
		return err
	}
	return client.HookClear(ctx, name)
}

func (c *appMCPCore) CloudScan(ctx context.Context) ([]map[string]any, error) {
	client, err := c.bridge.Engine()
	if err != nil {
		return nil, err
	}
	return client.CloudScan(ctx)
}

func (c *appMCPCore) CDPCommand(ctx context.Context, method string, params map[string]any, timeoutMs int) (map[string]any, error) {
	client, err := c.bridge.Engine()
	if err != nil {
		return nil, err
	}
	return client.CDPCommand(ctx, method, params, timeoutMs)
}

func (c *appMCPCore) DebugState(ctx context.Context, enable bool) (map[string]any, error) {
	client, err := c.bridge.Engine()
	if err != nil {
		return nil, err
	}
	return client.DebugState(ctx, enable)
}

// buildAppMCPServer assembles the MCP server over the App's live providers —
// traffic API is reused, the engine is reached on demand via the bridge.
func buildAppMCPServer(a *App, profile string) *mcp.Server {
	deps := mcp.Deps{
		Scan: mcpScan, Core: &appMCPCore{bridge: &engineBridge{app: a}},
		AppBridge: &mcpAppBridge{app: a}, SkillsDir: resolveSkillsDir(),
		ToolProfile: profile, AllowedCodeRoot: a.extractOutputRoot(), Version: appVersion,
	}
	if a.trafficAPI != nil {
		deps.Traffic = &mcpTraffic{api: a.trafficAPI}
	}
	return mcp.New(deps)
}

// runMCPStdio serves MCP over stdio until EOF. Stdout belongs to the MCP
// transport, so diagnostics go to stderr only.
func runMCPStdio(profile string) int {
	server := buildMCPServer(profile)
	if err := server.Serve(os.Stdin, os.Stdout); err != nil {
		return 1
	}
	return 0
}
