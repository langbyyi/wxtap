package mcp

// 敏感信息扫描是分钟级的单线程 CPU 活（v1.0.2 实测：16MB 反编译产物在 GUI
// 进程内 98.8s）。同步直通会把所有默认超时的 MCP 客户端饿死，所以两个扫描
// 工具（scan_dir / miniapp_scan_sensitive）走受理/轮询模型：
//
//   - 首次调用启动后台任务，立即回答 {async:true, task_id, status:"started"}；
//   - 相同参数再调一次就是轮询：running 带进度；完成后直接回完整结果；
//   - 目录 mtime 未变时秒回缓存结果，不重扫。
//
// 任务 goroutine 挂在 context.Background() 上（而不是请求 ctx）：客户端断连
// 正是扫描最容易超时的时刻，任务继续跑完进缓存，轮询才能拿到结果。扫描串行
// （slot 容量 1）——它们本来就是 CPU 大户，并发只会互相拖慢。

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"time"
)

// scanMaxCached keeps at most this many finished payloads in memory. A large
// project's shaped result is ~60KB, so the bound is about responsiveness, not
// memory pressure.
const scanMaxCached = 4

// scanJob tracks one running scan. Finished jobs leave the registry: their
// result lives in the cache, which polling reads. A failed job parks its error
// here for exactly one poll to see, then the next call retries.
type scanJob struct {
	progressDone  int64 // atomic
	progressTotal int64 // atomic
	err           error
}

func (j *scanJob) progress() (int, int) {
	return int(atomic.LoadInt64(&j.progressDone)), int(atomic.LoadInt64(&j.progressTotal))
}

type scanCacheEntry struct {
	modTime time.Time
	payload map[string]any
	at      time.Time
}

// requestScan runs the accept/poll/cache protocol for one directory and
// returns the tool payload. The finished payload has the same shape the
// synchronous path used to answer, plus "cached":true; an in-flight job
// answers the async envelope instead.
func (s *Server) requestScan(dir string) map[string]any {
	s.scanMu.Lock()
	if s.scanJobs == nil {
		s.scanJobs = map[string]*scanJob{}
		s.scanCache = map[string]*scanCacheEntry{}
		s.scanSlots = make(chan struct{}, 1)
	}
	// 缓存命中：目录 mtime 与上次扫描一致，结果不可能变化。
	if info, err := os.Stat(dir); err == nil {
		if entry, ok := s.scanCache[dir]; ok && entry.modTime.Equal(info.ModTime()) {
			s.scanMu.Unlock()
			payload := cloneScanPayload(entry.payload)
			payload["cached"] = true
			return payload
		}
	}
	if job, ok := s.scanJobs[dir]; ok {
		if job.err != nil {
			payload := map[string]any{"async": true, "task_id": dir, "status": "error", "error": job.err.Error()}
			// 失败只报一次：下一条同参调用重新受理（重试）。
			delete(s.scanJobs, dir)
			s.scanMu.Unlock()
			return payload
		}
		done, total := job.progress()
		s.scanMu.Unlock()
		return map[string]any{"async": true, "task_id": dir, "status": "running", "files_done": done, "files_total": total}
	}
	job := &scanJob{}
	s.scanJobs[dir] = job
	s.scanMu.Unlock()

	go func() {
		s.scanSlots <- struct{}{}
		defer func() { <-s.scanSlots }()
		// 排队期间同一目录可能已被更早的任务扫完进缓存：直接复用。
		s.scanMu.Lock()
		if info, err := os.Stat(dir); err == nil {
			if entry, ok := s.scanCache[dir]; ok && entry.modTime.Equal(info.ModTime()) {
				delete(s.scanJobs, dir)
				s.scanMu.Unlock()
				return
			}
		}
		s.scanMu.Unlock()

		scan, err := s.deps.Scan(context.Background(), dir, func(done, total int) {
			atomic.StoreInt64(&job.progressDone, int64(done))
			atomic.StoreInt64(&job.progressTotal, int64(total))
		})
		if err != nil {
			s.scanMu.Lock()
			job.err = err
			s.scanMu.Unlock()
			return
		}
		payload := shapeScanPayload(dir, scan)
		payload["cached"] = true
		s.scanMu.Lock()
		var modTime time.Time
		if info, statErr := os.Stat(dir); statErr == nil {
			modTime = info.ModTime()
		}
		s.scanCache[dir] = &scanCacheEntry{modTime: modTime, payload: payload, at: time.Now()}
		s.evictScanCacheLocked()
		delete(s.scanJobs, dir)
		s.scanMu.Unlock()
	}()

	return map[string]any{"async": true, "task_id": dir, "status": "started"}
}

// evictScanCacheLocked drops the oldest entries beyond scanMaxCached. Caller
// holds scanMu.
func (s *Server) evictScanCacheLocked() {
	if len(s.scanCache) <= scanMaxCached {
		return
	}
	var oldestDir string
	var oldest time.Time
	for dir, entry := range s.scanCache {
		if oldestDir == "" || entry.at.Before(oldest) {
			oldestDir, oldest = dir, entry.at
		}
	}
	delete(s.scanCache, oldestDir)
}

// cloneScanPayload shallow-copies the cached map: callers add per-response
// keys (cached) and must not mutate the shared entry.
func cloneScanPayload(payload map[string]any) map[string]any {
	clone := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		clone[key] = value
	}
	return clone
}

// shapeScanPayload builds the tool result from a finished scan: the same shape
// the synchronous path answered (findings deduped and capped by
// shapeScanFindings, analysis counts, categories) before the async model.
func shapeScanPayload(dir string, scan ScanResult) map[string]any {
	report := map[string]any{}
	_ = json.Unmarshal(scan.Report, &report)
	findings, total, truncated := shapeScanFindings(report["findings"])
	analysis, _ := report["result"].(map[string]any)
	if analysis == nil {
		analysis = map[string]any{}
	}
	// scan_dir 与 miniapp_scan_sensitive 各自历史字段（total_size/summary 与
	// scan_path/result）的并集：两个工具的结果消费方都不破坏。
	return map[string]any{
		"scan_path": dir, "files_scanned": scan.FilesScanned,
		"total_size": scan.TotalSize, "summary": scan.Summary,
		"findings": findings, "total_findings": total, "truncated": truncated,
		"result":           analysis,
		"categories_found": mapKeys(scan.Summary),
	}
}

// scanWaitDone polls a scan job to completion (success or parked failure) in
// tests.
func (s *Server) scanWaitDone(dir string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.scanMu.Lock()
		job, running := s.scanJobs[dir]
		failed := running && job.err != nil
		s.scanMu.Unlock()
		if !running || failed {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
