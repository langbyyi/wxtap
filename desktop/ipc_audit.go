// Audit-surface IPC: the assets audit view plus the per-record
// curl / replay / HAR helpers the traffic and audit views share.
//
// Every handler here is thin: params in, one AuditService call out. The
// service (internal/api/ipc/audit.go) owns the run state, the report, the
// inventory and all clamping, so the MCP tools that forward through appCall
// share the exact same implementation as these GUI entry points.
package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/langbyyi/wxtap/desktop/internal/api/ipc"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// registerAuditHandlers wires the whole audit surface in one place so
// ipc_bridge.go only needs a single call site.
func (a *App) registerAuditHandlers(r *ipc.Router) {
	r.Register("assets.scan", func(_ context.Context, params json.RawMessage) (any, error) {
		var args ipc.AssetsScanParams
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		return a.audit.AssetsScan(args)
	})
	r.Register("assets.list", func(_ context.Context, params json.RawMessage) (any, error) {
		var args ipc.AssetsListParams
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		return a.audit.AssetsList(args), nil
	})
	r.Register("assets.export", func(_ context.Context, params json.RawMessage) (any, error) {
		var args ipc.AssetsExportParams
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		return a.audit.AssetsExport(args)
	})
	r.Register("traffic.curl", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args ipc.TrafficAuditParams
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		return a.audit.TrafficCurl(ctx, args)
	})
	r.Register("traffic.replay", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args ipc.TrafficAuditParams
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		return a.audit.TrafficReplay(ctx, args)
	})
	r.Register("traffic.exportHar", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args ipc.TrafficExportHarParams
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		return a.audit.TrafficExportHar(ctx, args)
	})
}

// newAuditService assembles the AuditService for this shell: emit rides the
// shared event sink, the save dialogs go through Wails runtime (the
// cloud.export pattern), and scans run as tracked background work so shutdown
// can drain them.
func (a *App) newAuditService() *ipc.AuditService {
	dialog := func(defaultName string, pattern string) (string, bool, error) {
		if a.wailsContext() == nil {
			return "", false, errors.New("当前环境没有可用的保存对话框")
		}
		path, err := wailsruntime.SaveFileDialog(a.ctx, wailsruntime.SaveDialogOptions{
			DefaultFilename: defaultName,
			Filters:         []wailsruntime.FileFilter{{DisplayName: pattern, Pattern: pattern}},
		})
		if err != nil {
			return "", false, err
		}
		return path, path != "", nil
	}
	return ipc.NewAuditService(a.repo, a.tasks, a.configStore, a.emit, dialog, a.goBackground)
}
