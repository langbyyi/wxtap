// Package ipc hosts the audit surface (assets.* and the
// traffic.curl / traffic.replay / traffic.exportHar helpers): one service
// keeps the asset inventory in memory, drives the long-running build through
// the shared TaskTracker and renders exports.
package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/assets"
	"github.com/langbyyi/wxtap/desktop/internal/replay"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// assetTargetConfigKey is the config.json key holding the asset target's
// appid: the mini program the last build (and the next one) belongs to. The
// archive file name is derived from it, so a restart restores the matching
// inventory without the frontend asking for it.
const assetTargetConfigKey = "assetTargetAppID"

// replayMaxBody is the response-body cut for the audit client (same stance as
// replay.DefaultMaxBodyBytes; pinned here so the audit config reads as one).
const replayMaxBody = 256 << 10

// replayBodyLimit is how much of a replayed body traffic.replay hands back to
// the GUI/agent; the rest is cut with truncated:true.
const replayBodyLimit = 4 << 10

// auditDialog is the native save-file dialog seam. ok=false means the user
// cancelled, mirroring wailsruntime.SaveFileDialog's empty-path answer.
type auditDialog func(defaultName string, pattern string) (path string, ok bool, err error)

// AuditService backs the audit IPC surface: the asset build's run state lives
// behind mu, and the build runs through background (the shell's tracked
// goroutine) so shutdown can drain it.
type AuditService struct {
	repo       *traffic.Repository
	tasks      *TaskTracker
	emit       func(name string, payload any)
	config     *ConfigStore
	dialog     auditDialog
	background func(func())

	mu           sync.Mutex
	assetsRun    bool
	assetsCancel context.CancelFunc
	inventory    *assets.Inventory
	// builtAppID / builtAt identify the loaded inventory: which mini program
	// it belongs to and when it was built (restored archives keep the original
	// build time, so the UI can say "上次构建于 …" instead of faking "now").
	builtAppID string
	builtAt    time.Time
}

// NewAuditService wires the service. tasks / emit / dialog / background may be
// nil in tests: the surface degrades to answers without progress events.
func NewAuditService(repo *traffic.Repository, tasks *TaskTracker, config *ConfigStore, emit func(name string, payload any), dialog auditDialog, background func(func())) *AuditService {
	if background == nil {
		background = func(fn func()) { go fn() }
	}
	s := &AuditService{
		repo: repo, tasks: tasks, config: config, emit: emit,
		dialog: dialog, background: background,
	}
	// 重启后恢复上次目标的清单存档：尽力而为，失败就当没有存档（页面会
	// 显示空清单并提示先构建），不能让启动被一个坏存档挡住。
	if config != nil {
		s.restoreArchive(config.lastAssetTargetAppID())
	}
	return s
}

// assetArchiveDir is where per-program inventory archives live
// (<dataDir>/assets/<appid>.json), derived from the config file's directory.
func (s *AuditService) assetArchiveDir() string {
	return filepath.Join(s.config.Dir(), "assets")
}

// restoreArchive loads the stored inventory of one appid into memory. No
// caller surfaces failures: an unreadable or stale archive is equivalent to
// having never built.
func (s *AuditService) restoreArchive(appid string) {
	appid = strings.TrimSpace(appid)
	if appid == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(s.assetArchiveDir(), appid+".json"))
	if err != nil {
		return
	}
	var archive struct {
		Inventory *assets.Inventory `json:"inventory"`
		BuiltAt   time.Time         `json:"builtAt"`
	}
	if err := json.Unmarshal(data, &archive); err != nil || archive.Inventory == nil {
		return
	}
	s.mu.Lock()
	s.inventory = archive.Inventory
	s.builtAppID = appid
	s.builtAt = archive.BuiltAt
	s.mu.Unlock()
}

// saveArchive persists the built inventory under the target's appid so the
// next start (or a target switch back) can restore it. Builds without an
// appid are not archived: there is no program the archive could claim.
func (s *AuditService) saveArchive(appid string, inventory *assets.Inventory, builtAt time.Time) {
	appid = strings.TrimSpace(appid)
	if appid == "" || inventory == nil {
		return
	}
	payload, err := json.Marshal(struct {
		AppID     string            `json:"appid"`
		BuiltAt   time.Time         `json:"builtAt"`
		Inventory *assets.Inventory `json:"inventory"`
	}{AppID: appid, BuiltAt: builtAt, Inventory: inventory})
	if err != nil {
		return
	}
	dir := s.assetArchiveDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return
	}
	// 写失败只丢"重启后免重建"的便利，不值得打断一次成功的构建。
	_ = writeFileAtomic(filepath.Join(dir, appid+".json"), payload, 0o600)
}

// rememberTarget records the asset target in config.json so the next start
// restores its archive. Best-effort for the same reason as saveArchive.
func (s *AuditService) rememberTarget(appid string) {
	appid = strings.TrimSpace(appid)
	if appid == "" || s.config == nil {
		return
	}
	_ = s.config.Save(context.Background(), map[string]any{assetTargetConfigKey: appid})
}

// Close cancels a running build. Shutdown calls it before draining
// the tracked goroutines so a mid-flight build stops instead of
// running against a closing database.
func (s *AuditService) Close() {
	s.mu.Lock()
	cancelAssets := s.assetsCancel
	s.mu.Unlock()
	if cancelAssets != nil {
		cancelAssets()
	}
}

// upstreamProxy reads replayUpstreamProxy from the config store on every use
// (never cached): the settings panel can change it between two runs, and a
// cached value would silently keep routing replays through a proxy the user
// just removed. Missing store / key / parse failure all read as direct
// connection.
func (s *AuditService) upstreamProxy() string {
	if s.config == nil {
		return ""
	}
	config, err := s.config.Load()
	if err != nil {
		return ""
	}
	proxy, _ := config["replayUpstreamProxy"].(string)
	return strings.TrimSpace(proxy)
}

// saveViaDialog is the shared save tail of the exports: ask for a path,
// guarantee the extension and write it. ok=false is the user's cancel, which
// is an answer, not an error.
func (s *AuditService) saveViaDialog(defaultName, pattern string, data []byte) (string, bool, error) {
	if s.dialog == nil {
		return "", false, fmt.Errorf("当前环境没有可用的保存对话框")
	}
	path, ok, err := s.dialog(defaultName, pattern)
	if err != nil {
		return "", false, err
	}
	if !ok || strings.TrimSpace(path) == "" {
		return "", false, nil
	}
	if ext := strings.TrimPrefix(pattern, "*"); ext != "" && !strings.HasSuffix(strings.ToLower(path), strings.ToLower(ext)) {
		path += ext
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", false, fmt.Errorf("写入文件失败: %w", err)
	}
	return path, true, nil
}

// AssetsScanParams are the assets.scan arguments.
type AssetsScanParams struct {
	Dir string `json:"dir"`
	// AppID narrows the traffic source to one mini program (the asset
	// target). Empty keeps the cross-program read, which only callers that
	// predate per-program targeting should rely on.
	AppID string `json:"appid,omitempty"`
	// IncludeTraffic defaults to true; a JSON false reaches the service as a
	// non-nil pointer, an absent field as nil.
	IncludeTraffic *bool    `json:"includeTraffic"`
	CloudFns       []string `json:"cloudFns"`
}

// AssetsScan accepts one inventory build. The build runs in
// the background and only acceptance is answered synchronously.
func (s *AuditService) AssetsScan(params AssetsScanParams) (map[string]any, error) {
	includeTraffic := params.IncludeTraffic == nil || *params.IncludeTraffic
	hasCloud := false
	for _, name := range params.CloudFns {
		if strings.TrimSpace(name) != "" {
			hasCloud = true
			break
		}
	}
	if strings.TrimSpace(params.Dir) == "" && !includeTraffic && !hasCloud {
		return map[string]any{"ok": false, "error": "没有可用来源"}, nil
	}

	s.mu.Lock()
	if s.assetsRun {
		s.mu.Unlock()
		return map[string]any{"ok": false, "error": "已有构建任务在运行"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	task := TaskState{}
	if s.tasks != nil {
		task = s.tasks.Start("assets.scan", "构建资产清单", 0)
	}
	s.assetsRun = true
	s.assetsCancel = cancel
	s.mu.Unlock()

	s.background(func() {
		defer func() {
			s.mu.Lock()
			s.assetsRun = false
			s.assetsCancel = nil
			s.mu.Unlock()
		}()
		s.runAssetsScan(ctx, params, task.ID)
	})
	return map[string]any{"ok": true, "async": true, "taskId": task.ID}, nil
}

func (s *AuditService) runAssetsScan(ctx context.Context, params AssetsScanParams, taskID string) {
	fail := func(message string) {
		if taskID != "" && s.tasks != nil {
			_, _ = s.tasks.Finish(taskID, "failed", message, "")
		}
		s.emitAssetsProgress("error", 0, 0, message)
	}

	var records []traffic.Record
	if params.IncludeTraffic == nil || *params.IncludeTraffic {
		switch {
		case s.repo == nil && strings.TrimSpace(params.Dir) == "" && len(params.CloudFns) == 0:
			fail("存储初始化失败，流量记录不可用")
			return
		case s.repo != nil:
			var err error
			// 清单是单个小程序的档案：流量按目标 appid 收窄（空 appid 保持
			// 旧的跨程序口径，供不带 appid 的调用方兜底）。
			records, err = s.repo.RecentRecordsForAppID(ctx, params.AppID, "", "", traffic.MaxAuditRecords)
			if err != nil {
				fail(err.Error())
				return
			}
		}
	}
	if ctx.Err() != nil {
		fail("构建已取消")
		return
	}
	inventory, err := assets.Build(params.Dir, records, params.CloudFns)
	if err != nil {
		fail(err.Error())
		return
	}
	builtAt := time.Now().UTC()
	s.mu.Lock()
	s.inventory = inventory
	s.builtAppID = strings.TrimSpace(params.AppID)
	s.builtAt = builtAt
	s.mu.Unlock()
	// 目标与清单一起落盘：下次启动（或切回该小程序）直接恢复这份清单。
	s.rememberTarget(params.AppID)
	s.saveArchive(params.AppID, inventory, builtAt)
	if taskID != "" && s.tasks != nil {
		_, _ = s.tasks.Finish(taskID, "done", "构建完成", "")
	}
	s.emitAssetsProgress("done", inventory.Total, inventory.Total, "构建完成")
}

func (s *AuditService) emitAssetsProgress(status string, current, total int, message string) {
	if s.emit == nil {
		return
	}
	payload := map[string]any{"status": status, "current": current, "total": total}
	if message != "" {
		payload["message"] = message
	}
	s.emit("assets_progress", payload)
}

// AssetsListParams are the assets.list arguments.
type AssetsListParams struct {
	Kind   string `json:"kind"`
	Host   string `json:"host"`
	Query  string `json:"query"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// AssetsList pages the stored inventory. Before any build it answers an empty
// page (the assets view loads first) instead of an error.
func (s *AuditService) AssetsList(params AssetsListParams) map[string]any {
	if params.Limit <= 0 {
		params.Limit = 100
	}
	if params.Limit > 500 {
		params.Limit = 500
	}
	if params.Offset < 0 {
		params.Offset = 0
	}

	s.mu.Lock()
	inventory := s.inventory
	s.mu.Unlock()
	if inventory == nil {
		return map[string]any{"ok": true, "total": 0, "hosts": []assets.HostStat{}, "items": []assets.Asset{}}
	}
	view := inventory.Filter(params.Kind, params.Host, params.Query)
	if params.Offset > view.Total {
		params.Offset = view.Total
	}
	end := params.Offset + params.Limit
	if end > view.Total {
		end = view.Total
	}
	items := view.Items[params.Offset:end]
	if items == nil {
		items = []assets.Asset{}
	}
	hosts := view.Hosts
	if hosts == nil {
		hosts = []assets.HostStat{}
	}
	s.mu.Lock()
	builtAppID, builtAt := s.builtAppID, s.builtAt
	s.mu.Unlock()
	response := map[string]any{"ok": true, "total": view.Total, "hosts": hosts, "items": items}
	if builtAppID != "" {
		response["appid"] = builtAppID
	}
	if !builtAt.IsZero() {
		response["builtAt"] = builtAt.Format(time.RFC3339)
	}
	return response
}

// AssetsExportParams are the assets.export arguments.
type AssetsExportParams struct {
	Format string `json:"format"`
	Save   bool   `json:"save"`
	Kind   string `json:"kind"`
	Host   string `json:"host"`
}

// assetsExportExtensions maps export formats onto save-dialog extensions; the
// line formats (nuclei/httpx/txt) all travel as .txt.
var assetsExportExtensions = map[string]string{
	"json": ".json",
	"csv":  ".csv",
	"txt":  ".txt",
}

// AssetsExport renders the inventory (optionally filtered) in one of the
// export formats: save=true writes it next to the user-chosen path, otherwise
// the content comes back inline.
func (s *AuditService) AssetsExport(params AssetsExportParams) (map[string]any, error) {
	s.mu.Lock()
	inventory := s.inventory
	s.mu.Unlock()
	if inventory == nil {
		return map[string]any{"ok": false, "error": "没有可导出的资产清单，请先构建"}, nil
	}
	content, err := inventory.Filter(params.Kind, params.Host, "").Export(params.Format)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}
	if !params.Save {
		return map[string]any{"ok": true, "content": content}, nil
	}
	ext, known := assetsExportExtensions[params.Format]
	if !known {
		ext = ".txt"
	}
	path, ok, err := s.saveViaDialog("assets_export"+ext, "*"+ext, []byte(content))
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]any{"ok": false, "reason": "用户取消"}, nil
	}
	return map[string]any{"ok": true, "path": path}, nil
}

// TrafficAuditParams names one stored record for traffic.curl / traffic.replay.
type TrafficAuditParams struct {
	ID string `json:"id"`
}

// auditRecord fetches one record from the recent audit window (the same 500
// newest records the inventory build reads). A record outside the window reads
// as missing: the audit surface is deliberately bounded.
func (s *AuditService) auditRecord(ctx context.Context, id string) (traffic.Record, bool, error) {
	if s.repo == nil {
		return traffic.Record{}, false, fmt.Errorf("存储初始化失败，流量记录不可用")
	}
	records, err := s.repo.RecentRecordsForAudit(ctx, "", "", traffic.MaxAuditRecords)
	if err != nil {
		return traffic.Record{}, false, err
	}
	for _, record := range records {
		if record.ID == id {
			return record, true, nil
		}
	}
	return traffic.Record{}, false, nil
}

// TrafficCurl renders one stored record as curl command lines.
func (s *AuditService) TrafficCurl(ctx context.Context, params TrafficAuditParams) (map[string]any, error) {
	record, found, err := s.auditRecord(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return map[string]any{"ok": false, "error": "记录不存在"}, nil
	}
	target, err := replay.ParseRecord(record)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}
	bash, cmdLine := target.BuildCurl()
	return map[string]any{"ok": true, "bash": bash, "cmd": cmdLine}, nil
}

// TrafficReplay sends one stored record through the configured upstream proxy
// and answers with a bounded, honest record of what happened: transport
// failures land in error (ok stays true — the replay itself ran), the body is
// cut at replayBodyLimit and marked truncated.
func (s *AuditService) TrafficReplay(ctx context.Context, params TrafficAuditParams) (map[string]any, error) {
	record, found, err := s.auditRecord(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return map[string]any{"ok": false, "error": "记录不存在"}, nil
	}
	target, err := replay.ParseRecord(record)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}
	client, err := replay.NewClient(s.upstreamProxy(), 0, replayMaxBody)
	if err != nil {
		return nil, err
	}
	result := client.Send(ctx, target)
	body := result.Body
	truncated := result.Truncated
	if len(body) > replayBodyLimit {
		body = body[:replayBodyLimit]
		truncated = true
	}
	payload := map[string]any{
		"ok":        true,
		"status":    result.StatusCode,
		"elapsedMs": result.ElapsedMs,
		// 传输层读到的字节可能不是合法 UTF-8：按字符串回传前清洗，不丢截断事实。
		"body":      strings.ToValidUTF8(string(body), "\uFFFD"),
		"truncated": truncated,
	}
	if result.Err != "" {
		payload["error"] = result.Err
	}
	return payload, nil
}

// TrafficExportHarParams are the traffic.exportHar arguments.
type TrafficExportHarParams struct {
	IDs []string `json:"ids"`
}

// TrafficExportHar renders captured requests as a HAR 1.2 document and saves
// it next to the user-chosen path. ids empty means the recent request window;
// explicit ids pick from the same window (older rows cannot be named).
func (s *AuditService) TrafficExportHar(ctx context.Context, params TrafficExportHarParams) (map[string]any, error) {
	if s.repo == nil {
		return nil, fmt.Errorf("存储初始化失败，流量记录不可用")
	}
	records, err := s.repo.RecentRecordsForAudit(ctx, "", "", traffic.MaxAuditRecords)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]traffic.Record, len(records))
	for _, record := range records {
		byID[record.ID] = record
	}
	entries := []replay.HAREntry{}
	if len(params.IDs) == 0 {
		for _, record := range records {
			if entry, ok := harEntryOfRecord(record); ok {
				entries = append(entries, entry)
			}
		}
	} else {
		for _, id := range params.IDs {
			record, found := byID[id]
			if !found {
				continue
			}
			if entry, ok := harEntryOfRecord(record); ok {
				entries = append(entries, entry)
			}
		}
	}
	data := replay.BuildHAR(entries)
	path, ok, err := s.saveViaDialog("traffic_export.har", "*.har", data)
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]any{"ok": false, "reason": "用户取消"}, nil
	}
	return map[string]any{"ok": true, "path": path}, nil
}

// harEntryOfRecord reconstructs one HAR entry from a stored record. The
// request side parses like any replay target; the response side is
// best-effort: the wx.request hook stores the page-side result object
// ({statusCode, header, data}), so status/headers/body come from there and
// stay zero/empty when the record does not carry them — a missing field is
// never replaced with a plausible value.
func harEntryOfRecord(record traffic.Record) (replay.HAREntry, bool) {
	target, err := replay.ParseRecord(record)
	if err != nil {
		return replay.HAREntry{}, false
	}
	entry := replay.HAREntry{
		When:   record.CapturedAt,
		Target: target,
	}
	if len(record.ResponseBody) == 0 {
		return entry, true
	}
	var result struct {
		StatusCode int             `json:"statusCode"`
		Header     map[string]any  `json:"header"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(record.ResponseBody, &result); err != nil {
		// 不带结构化结果的记录（纯文本/错误帧）：状态未知即 0，原文照放进 body。
		entry.Body = record.ResponseBody
		return entry, true
	}
	entry.Status = result.StatusCode
	if len(result.Header) > 0 {
		entry.RespHeaders = make(map[string]string, len(result.Header))
		for key, value := range result.Header {
			entry.RespHeaders[key] = scalarString(value)
		}
	}
	if trimmed := strings.TrimSpace(string(result.Data)); trimmed != "" && trimmed != "null" {
		if trimmed[0] == '"' {
			var text string
			if json.Unmarshal(result.Data, &text) == nil {
				entry.Body = []byte(text)
			}
		} else {
			entry.Body = []byte(trimmed)
		}
	}
	return entry, true
}

// scalarString renders one JSON-decoded header value the way it would appear
// on the wire: strings verbatim, everything else as compact JSON.
func scalarString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(encoded)
	}
}
