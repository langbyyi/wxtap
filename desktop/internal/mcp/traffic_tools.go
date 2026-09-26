package mcp

// 动态分析的两个效率工具：hook_wait 把「触发动作 → 等流量落地」收成一次
// 服务端长轮询（否则 agent 只能紧密循环 hook_drain，烧往返也烧上下文）；
// traffic_records 把 traffic_list + 逐条 traffic_get_body 的 N+1 合并成一页
// 带截断正反文体的聚合结果。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"
)

const (
	hookWaitPollInterval = 250 * time.Millisecond
	hookWaitMaxTimeout   = 25 * time.Second
	hookWaitMaxLimit     = 500

	trafficRecordsMaxLimit = 50
	trafficRecordsMaxBody  = 16 * 1024
	trafficRecordsDefLimit = 20
	trafficRecordsDefBody  = 2 * 1024
)

// hookWait 长轮询 hook 记录流：至少一条 afterSeq 之后的新记录落地（或超时）
// 才返回。只读记录流（updateLimit=0），游标语义与 hook_drain 一致。
func (s *Server) hookWait(ctx context.Context, raw json.RawMessage) (any, error) {
	if s.deps.Core == nil {
		return nil, fmt.Errorf("core engine unavailable")
	}
	args := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}
	name := stringArg(args, "name", "")
	if name != "wxapi" && name != "cloud" {
		return nil, fmt.Errorf("name must be one of: cloud, wxapi")
	}
	afterSeq := int64(intArg(args, "afterSeq", 0))
	limit := intArg(args, "limit", 50)
	if limit <= 0 {
		limit = 50
	}
	if limit > hookWaitMaxLimit {
		limit = hookWaitMaxLimit
	}
	timeoutMs := intArg(args, "timeoutMs", 10000)
	if timeoutMs <= 0 {
		timeoutMs = 10000
	}
	if timeoutMs > int(hookWaitMaxTimeout.Milliseconds()) {
		timeoutMs = int(hookWaitMaxTimeout.Milliseconds())
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	start := time.Now()

	for {
		page, err := s.deps.Core.HookDrain(ctx, name, afterSeq, limit, 0, 0)
		if err != nil {
			return nil, err
		}
		if len(page.Records) > 0 {
			return map[string]any{
				"name": name, "records": page.Records, "nextSeq": page.NextSeq,
				"hasMore": page.HasMore, "waitedMs": time.Since(start).Milliseconds(),
				"timedOut": false,
			}, nil
		}
		if time.Now().After(deadline) {
			return map[string]any{
				"name": name, "records": []DrainedRecord{}, "nextSeq": afterSeq,
				"hasMore": false, "waitedMs": time.Since(start).Milliseconds(), "timedOut": true,
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(hookWaitPollInterval):
		}
	}
}

// trafficRecords 是一页聚合视图：traffic_list 的摘要 + 每条记录截断后的
// 请求/响应文体。二进制文体不内联，标注字节数。
func (s *Server) trafficRecords(ctx context.Context, raw json.RawMessage) (any, error) {
	if s.deps.Traffic == nil {
		return nil, fmt.Errorf("traffic store unavailable")
	}
	args := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}
	limit := intArg(args, "limit", trafficRecordsDefLimit)
	if limit <= 0 {
		limit = trafficRecordsDefLimit
	}
	if limit > trafficRecordsMaxLimit {
		limit = trafficRecordsMaxLimit
	}
	bodyBytes := intArg(args, "bodyBytes", trafficRecordsDefBody)
	if bodyBytes < 0 {
		bodyBytes = 0
	}
	if bodyBytes > trafficRecordsMaxBody {
		bodyBytes = trafficRecordsMaxBody
	}
	page, err := s.deps.Traffic.List(ctx, ListParams{
		Page:   intArg(args, "page", 0),
		Limit:  limit,
		Query:  stringArg(args, "query", ""),
		APType: stringArg(args, "apiType", ""),
		Status: stringArg(args, "status", ""),
		AppID:  stringArg(args, "appId", ""),
	})
	if err != nil {
		return nil, err
	}

	items := make([]map[string]any, 0, len(page.Items))
	for _, item := range page.Items {
		entry := map[string]any{
			"id": item.ID, "seq": item.Seq, "capturedAt": item.CapturedAt,
			"apiType": item.APType, "appId": item.AppID, "name": item.Name,
			"method": item.Method, "url": item.URL, "status": item.Status,
			"requestBytes": item.RequestBytes, "responseBytes": item.ResponseBytes,
			"durationMs": item.DurationMs,
		}
		if bodyBytes > 0 {
			entry["request_body"] = s.bodyPreview(ctx, item.ID, "request", bodyBytes)
			entry["response_body"] = s.bodyPreview(ctx, item.ID, "response", bodyBytes)
		}
		items = append(items, entry)
	}
	return map[string]any{"items": items, "total": page.Total, "count": len(items)}, nil
}

// awaitHookRecords 轮询记录流直到新记录落地或超时；游标全程保持调用者的
// 原值（更新流的窗口也按原参数透传）。超时返回最后一页空结果，由调用方如实
// 上报，不谎报成功。
func (s *Server) awaitHookRecords(ctx context.Context, name string, afterSeq int64, limit int, afterUpdateSeq int64, updateLimit int, waitMs int) (DrainPage, error) {
	if s.deps.Core == nil {
		return DrainPage{}, fmt.Errorf("core engine unavailable")
	}
	if waitMs > int(hookWaitMaxTimeout.Milliseconds()) {
		waitMs = int(hookWaitMaxTimeout.Milliseconds())
	}
	deadline := time.Now().Add(time.Duration(waitMs) * time.Millisecond)
	for {
		select {
		case <-ctx.Done():
			return DrainPage{}, ctx.Err()
		case <-time.After(hookWaitPollInterval):
		}
		page, err := s.deps.Core.HookDrain(ctx, name, afterSeq, limit, afterUpdateSeq, updateLimit)
		if err != nil {
			return DrainPage{}, err
		}
		if len(page.Records) > 0 || !time.Now().Before(deadline) {
			return page, nil
		}
	}
}

// bodyPreview 取一段文体并截断到 max；非 UTF-8（图片等二进制）不内联。
func (s *Server) bodyPreview(ctx context.Context, id string, part string, max int) map[string]any {
	body, err := s.deps.Traffic.GetBody(ctx, id, part)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	if !utf8.Valid(body) {
		return map[string]any{"binary": true, "bytes": len(body)}
	}
	if len(body) <= max {
		return map[string]any{"text": string(body), "bytes": len(body), "truncated": false}
	}
	// 按字节截断会劈开多字节字符；退到 UTF-8 边界，与 miniapp_read_file 同法。
	text := string(body[:max])
	for i := 0; i < 4 && !utf8.ValidString(text); i++ {
		text = text[:len(text)-1]
	}
	return map[string]any{"text": text, "bytes": len(body), "truncated": true}
}
