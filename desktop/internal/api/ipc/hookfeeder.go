package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Core is the engine surface the hook feeder needs.
type Core interface {
	InstallHook(ctx context.Context, name string) error
	// InstallHookReport returns the page-side install() result verbatim. The
	// page reports {ok:false, reason} when it cannot hook anything, which a
	// report-blind install would silently turn into success.
	InstallHookReport(ctx context.Context, name string) (map[string]any, error)
	HookDrain(ctx context.Context, name string, afterSeq int64, limit int, afterUpdateSeq int64, updateLimit int) (DrainPage, error)
	Evaluate(ctx context.Context, expression string, timeoutMs int) (any, error)
}

// DrainPage is the local shape of engine.HookDrainPage (avoids importing engine).
type DrainPage struct {
	Records       []DrainedRecord `json:"records"`
	Updates       []DrainedUpdate `json:"updates"`
	NextSeq       int64           `json:"nextSeq"`
	NextUpdateSeq int64           `json:"nextUpdateSeq"`
	HasMore       bool            `json:"hasMore"`
	// DroppedRecords / DroppedUpdates are the page-side buffers' cumulative
	// eviction counts (R12): what the hook itself had to throw away because its
	// own buffer overflowed, so the shell can never recover it. They are pure
	// readings — the cursors above neither reset them nor are affected by them.
	DroppedRecords int64 `json:"droppedRecords"`
	DroppedUpdates int64 `json:"droppedUpdates"`
}

// DrainedRecord is one hook.drain record.
type DrainedRecord struct {
	Seq    int64          `json:"seq"`
	Record map[string]any `json:"record"`
}

// DrainedUpdate is one settled-record update frame: an async call that landed
// after its record was already drained.
type DrainedUpdate struct {
	Seq    int64          `json:"seq"`
	Update map[string]any `json:"update"`
}

// FeederStats is the wxapi.stats payload: what the capture panel needs to show
// the buffer state without reading the records themselves.
type FeederStats struct {
	Running   bool  `json:"running"`
	Pending   int   `json:"pending"`
	Dropped   int64 `json:"dropped"`
	Ack       int64 `json:"ack"`
	UpdateAck int64 `json:"updateAck"`
	// PageDroppedRecords / PageDroppedUpdates are the page-side buffers'
	// cumulative evictions (R12), as last reported by a drain response. They
	// complement Dropped instead of overlapping it, but they do not mean the
	// same thing. Dropped counts what the shell's pending buffer evicted: those
	// records were already handed to the UI as capture events and already
	// persisted through the ingest step, so only the poll path lost them (the ack
	// advanced past them - no later drain brings them back, and ResetAck only
	// replays what the page still buffers). These two counters are the records
	// that are gone for good: the page evicted them before anything read them, so
	// they are in no database either. A panel that shows both must say which one
	// it means; the capture pages render "已丢弃" for these two and "未进列表" for
	// Dropped.
	PageDroppedRecords int64 `json:"pageDroppedRecords"`
	PageDroppedUpdates int64 `json:"pageDroppedUpdates"`
}

const (
	feedPollInterval = 500 * time.Millisecond
	feedDrainLimit   = 200
	feedUpdateLimit  = 200
	replayTimeoutMs  = 10000

	// maxUpdateRounds bounds the update stream's continuation within one tick:
	// one round carries feedUpdateLimit frames, so a tick applies at most
	// maxUpdateRounds × feedUpdateLimit updates and the rest waits for the next
	// tick (the loop must stay responsive to cancellation).
	maxUpdateRounds = 10

	// maxRecordRounds bounds the record stream's continuation within one tick,
	// mirroring maxUpdateRounds (R9): one round carries feedDrainLimit records,
	// so a tick delivers at most maxRecordRounds × feedDrainLimit = 2000 records
	// and the rest waits for the next tick. Every round advances the ack, so the
	// continuation cannot spin. The bound is what keeps one batch inside the
	// frontend's batch queue: a larger single event would be dropped
	// synchronously before the queue's flush, while those records were already
	// consumed from the page buffer - the poll 2s later re-adds them at the tail
	// and the panel ends up trimming the newest records instead of the oldest.
	maxRecordRounds = 10

	// PollDefaultLimit is the wxapi.poll page size when the caller sends no
	// limit. PollMaxLimit is the hard ceiling: one poll returning the whole
	// buffer (maxPendingRecords) would hand the webview a multi-megabyte JSON
	// document to parse and render in a single frame.
	PollDefaultLimit = 500
	pollMaxLimit     = 2000
	pollMinLimit     = 1

	// maxPendingRecords bounds the poll buffer. The panel may stay closed (or
	// poll far slower than the page captures) while records keep arriving;
	// without a cap the shell would grow without limit. The oldest are dropped
	// and counted so the UI can say records were discarded.
	maxPendingRecords = 5000

	// deliveredCapacity bounds the replay-dedup FIFO both streams register in
	// (R11): the page-side buffers hold at most 5000 records and 5000 update
	// frames (core/hooks/wxapi.js), so a same-realm replay after ResetAck can
	// only contain keys among the most recent deliveries, and the FIFO must
	// cover **both** buffers — a replay re-reads the record and the update
	// stream from zero, so the window is records + updates = 10000. 16384
	// keeps every replayed entry covered with headroom. The capacities stay
	// coupled: deliveredCapacity 必须大于页面记录缓冲与更新缓冲之和，否则
	// ResetAck 重投的记录会重复进 pending 并重复发事件（更新帧按 rid 登记在
	// 同一条 FIFO 上，键空间带流标记，两者不会互相误判）。两侧关系由测试从
	// core/hooks/wxapi.js 源码解析后断言，改容量必须同步改这里。
	deliveredCapacity = 16384
)

// HookFeeder keeps one named hook capturing: it installs the hook, drains
// records and settled-record updates on a background loop, pushes each tick's
// records to onCapture (live UI event), persists every batch through ingest,
// buffers records until the frontend polls them (wxapi.poll / cloud.poll
// semantics) and reports every settled call through onUpdate.
type HookFeeder struct {
	core     Core
	hookName string
	// onCapture and onUpdate deliver one tick's whole batch (F2): a page of 200
	// records is one event with an array payload, not 200 events. onUpdate stays
	// nil for hooks without an update stream.
	onCapture func(records []map[string]any)
	ingest    func(ctx context.Context, batch []DrainedRecord) error
	// apply persists the settled fields of the update stream, onUpdate
	// delivers each tick's frames to the UI. Both stay nil for hooks without
	// one.
	apply    func(ctx context.Context, updates []DrainedUpdate) error
	onUpdate func(updates []map[string]any)

	mu        sync.Mutex
	running   bool
	ack       int64
	updateAck int64
	dropped   int64
	// acceptAnyInstallReport relaxes Start()'s install check for hooks whose
	// page-side install() reports {ok:false} as an expected outcome (console on
	// WMPF builds that lock the console object) while still capturing part of
	// the stream. Default false keeps the strict InstallReportOK behaviour.
	acceptAnyInstallReport bool
	// pageDroppedRecords / pageDroppedUpdates are the last cumulative eviction
	// counts the page reported (R12). The shell only repeats the reading: the
	// zeroing belongs to the page-side clearHookedCalls().
	pageDroppedRecords int64
	pageDroppedUpdates int64
	gen                int64 // bumped by ResetAck; stale drain write-backs check it
	pending            []map[string]any
	// pendingIndex maps a buffered record's rid to the buffered map itself, so
	// an update frame can patch the record the panel has not read yet.
	pendingIndex map[string]map[string]any
	cancel       context.CancelFunc

	delivered     map[deliveredKey]struct{}
	deliveredFIFO []deliveredKey
	deliveredPos  int
	wg            sync.WaitGroup
}

// deliveredKey is the identity of one item in the replay-dedup FIFO. Both
// streams register in the same FIFO (R11), so the key space has to keep them
// apart: a record is identified by its storage identity (the miniapp appid, its
// capture timestamp in ms, and the page-local sequence number - the feeder is
// per-hook, so the API-type dimension is fixed), an update frame by the rid of
// the row it settles. `flow` is what separates the two spaces; without it an
// update whose rid equalled some record's appid/ts/seq tuple would suppress
// that record (and the other way round).
type deliveredKey struct {
	flow  flowKind
	appID string
	ts    int64
	seq   int64
	rid   string
}

// flowKind tags which stream a deliveredKey belongs to.
type flowKind uint8

const (
	recordFlow flowKind = 'r'
	updateFlow flowKind = 'u'
)

// NewHookFeeder wires a feeder for one hook. ingest receives the drained
// records with their sequence numbers (for idempotent storage); onCapture
// receives the bare record payload, one call per record (the hooks without an
// update stream - cloud, console - keep this single-record signature).
func NewHookFeeder(core Core, hookName string, onCapture func(map[string]any), ingest func(context.Context, []DrainedRecord) error) *HookFeeder {
	var batch func(records []map[string]any)
	if onCapture != nil {
		batch = func(records []map[string]any) {
			for _, record := range records {
				onCapture(record)
			}
		}
	}
	return &HookFeeder{core: core, hookName: hookName, onCapture: batch, ingest: ingest}
}

// NewHookFeederWithUpdates adds the hook's update stream: apply receives the
// settled frames of every round trip (to overwrite the stored rows) and
// onUpdate receives each tick's frames after the buffer was patched (for live
// UI events). Both callbacks take a batch - one tick delivers at most one
// event per stream (F2). Hooks without an update stream use NewHookFeeder.
func NewHookFeederWithUpdates(core Core, hookName string, onCapture func(records []map[string]any), ingest func(context.Context, []DrainedRecord) error, apply func(context.Context, []DrainedUpdate) error, onUpdate func(updates []map[string]any)) *HookFeeder {
	return &HookFeeder{
		core: core, hookName: hookName,
		onCapture: onCapture, ingest: ingest,
		apply: apply, onUpdate: onUpdate,
	}
}

// InstallReportOK reports whether a page-side install() report means the hook
// is live. Only an explicit false is a failure: a hook that answers without an
// `ok` field cannot be judged, and failing it would stop its capture.
func InstallReportOK(report map[string]any) bool {
	ok, reported := report["ok"].(bool)
	return !reported || ok
}

// InstallFailureReason surfaces the page-side explanation of an {ok:false}
// install report, so the user reads why the hook is not capturing.
func InstallFailureReason(report map[string]any) string {
	for _, key := range []string{"reason", "error"} {
		if reason, ok := report[key].(string); ok && reason != "" {
			return reason
		}
	}
	return "页面未找到可注入的 wx 环境"
}

// AcceptAnyInstallReport makes Start() treat an answered install report as
// success, whatever its `ok` says. This is for hooks that keep part of their
// capture alive on the very pages where install() must answer {ok:false}:
// the console hook wraps console.* when the page allows it and always hooks
// the error exits (window.onerror / unhandledrejection), so on WMPF builds
// that lock the console object the report is {ok:false} while the error
// stream is live - refusing to start would leave those records stranded in
// the page buffer forever. A hook whose install *errored* (no page, realm
// dead) still fails: there is nothing to drain from.
func (f *HookFeeder) AcceptAnyInstallReport() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acceptAnyInstallReport = true
}

// Start installs the hook and launches the drain loop. Starting twice is a no-op.
func (f *HookFeeder) Start() error {
	f.mu.Lock()
	if f.running {
		f.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.running = true
	f.wg.Add(1)
	f.mu.Unlock()

	report, err := f.core.InstallHookReport(ctx, f.hookName)
	if err == nil && !f.acceptAnyInstallReport && !InstallReportOK(report) {
		// The page answered {ok:false}: nothing is hooked, so reporting success
		// would leave the panel showing "capturing" with no records ever.
		// Feeders that opted into AcceptAnyInstallReport skip this - their page
		// answers {ok:false} as an expected outcome (see the console hook).
		err = fmt.Errorf("安装 %s 钩子失败：%s", f.hookName, InstallFailureReason(report))
	}
	if err != nil {
		f.wg.Done()
		f.mu.Lock()
		f.running = false
		f.cancel = nil
		f.mu.Unlock()
		cancel()
		return err
	}
	go func() {
		defer f.wg.Done()
		f.loop(ctx)
	}()
	return nil
}

// Stop halts the drain loop (records on the Core side are kept).
func (f *HookFeeder) Stop() {
	f.mu.Lock()
	if !f.running {
		f.mu.Unlock()
		return
	}
	f.running = false
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	f.mu.Unlock()
	// Wait for the active drain round-trip to observe cancellation before the
	// caller closes the engine or traffic database it may be using.
	f.wg.Wait()
}

// Poll returns buffered records and empties the buffer (consume semantics).
func (f *HookFeeder) Poll() ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.takePending(len(f.pending))
}

// PollLimit returns at most limit buffered records and removes exactly those
// from the buffer (a bounded page of the same consume semantics: nothing is
// read twice, nothing is reordered). limit is clamped into
// pollMinLimit..pollMaxLimit, so an out-of-range value reads as the smallest
// page rather than as "everything". The response shape is unchanged - always a
// JSON array, never null.
func (f *HookFeeder) PollLimit(limit int) ([]map[string]any, error) {
	if limit < pollMinLimit {
		limit = pollMinLimit
	}
	if limit > pollMaxLimit {
		limit = pollMaxLimit
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.takePending(limit)
}

// takePending removes and returns the first count buffered records. Callers
// hold the lock. A partial take rebuilds pendingIndex: the records handed out
// are gone from the buffer, so a later update frame must not patch them
// through a stale index entry (a stale entry would also keep them alive).
//
// The returned maps cannot race with patchPending: both branches make them
// unreachable from the index (the full take sets pendingIndex to nil, the
// partial one rebuilds it from the records that stayed), and patchPending only
// ever writes to maps it reaches through that index. Poll() goes through
// takePending(len(pending)) and so empties the index as well.
func (f *HookFeeder) takePending(count int) ([]map[string]any, error) {
	if len(f.pending) <= count {
		out := f.pending
		f.pending = nil
		f.pendingIndex = nil
		if out == nil {
			// The drain payload is always a JSON array; a null payload breaks
			// clients that iterate the poll result.
			out = []map[string]any{}
		}
		return out, nil
	}
	out := make([]map[string]any, count)
	copy(out, f.pending[:count])
	f.pending = f.pending[count:]
	f.pendingIndex = nil
	for _, record := range f.pending {
		if rid := ridOf(record); rid != "" {
			if f.pendingIndex == nil {
				f.pendingIndex = map[string]map[string]any{}
			}
			f.pendingIndex[rid] = record
		}
	}
	return out, nil
}

// Clear drops buffered records (already-drained records stay acknowledged),
// resets the drop counter and voids any drain page that is still in flight:
// without the generation bump a page the feeder had already pulled fills the
// buffer back up right after the user cleared it, and the records "come back".
// The ack is NOT reset - Clear is not a reinstall. The page-side drop readings
// (R12) are deliberately left alone too: clearHookedCalls() zeroes them in the
// page and the next drain reports that zero, so clearing them here would only
// fake a reading the shell has not seen yet.
func (f *HookFeeder) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = nil
	f.pendingIndex = nil
	f.dropped = 0
	f.gen++
}

// Running reports whether the drain loop is active, i.e. whether this hook is
// currently capturing. The connect-time injection reads it to decide whether a
// rebuilt page realm has to be re-hooked: only a running capture has a page
// context whose sequence space was lost and must be resumed. A hook that is not
// capturing is left uninstalled, so connecting never starts recording.
func (f *HookFeeder) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

// Stats reports the buffer and acknowledgement state for wxapi.stats.
func (f *HookFeeder) Stats() FeederStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return FeederStats{
		Running:            f.running,
		Pending:            len(f.pending),
		Dropped:            f.dropped,
		Ack:                f.ack,
		UpdateAck:          f.updateAck,
		PageDroppedRecords: f.pageDroppedRecords,
		PageDroppedUpdates: f.pageDroppedUpdates,
	}
}

// ResetAck restarts draining from sequence 0. The Core-side sequence space
// lives in the page context: a reload or a miniapp switch restarts it at 1
// while this ack keeps growing, which would silently skip every new record.
// Replays after a reset are harmless - storage is idempotent per (type, ts,
// seq) - so this is called whenever the hook is (re)installed or the
// miniapp switched. The update stream restarts with the record stream. The
// page-side drop readings (R12) stay untouched for the same reason Clear()
// leaves them alone: the reading follows the page, not the cursors, and a
// reloaded realm reports its own zero on the next drain.
func (f *HookFeeder) ResetAck() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ack = 0
	f.updateAck = 0
	f.gen++
}

// Replay invokes the hook's replay entry with an API name and options.
func (f *HookFeeder) Replay(ctx context.Context, apiName string, options map[string]any) (any, error) {
	optionsJSON, err := jsonMarshal(options)
	if err != nil {
		return nil, err
	}
	if _, err := f.core.Evaluate(ctx, "window._wxApiReplayDone=null", replayTimeoutMs); err != nil {
		return nil, err
	}
	expression := "window.wxApiAudit.replay('" + escapeJS(apiName) + "', " + optionsJSON + ")"
	if _, err := f.core.Evaluate(ctx, expression, replayTimeoutMs); err != nil {
		return nil, err
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for attempt := 0; attempt < 30; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
			}
		}
		value, err := f.core.Evaluate(ctx, "window._wxApiReplayDone", replayTimeoutMs)
		if err != nil {
			return nil, err
		}
		text, ok := value.(string)
		if !ok || text == "" {
			continue
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(text), &result); err != nil {
			return map[string]any{"ok": false, "status": "fail", "reason": "parse error"}, nil
		}
		return result, nil
	}
	return map[string]any{"ok": false, "status": "fail", "reason": "调用超时"}, nil
}

func (f *HookFeeder) loop(ctx context.Context) {
	ticker := time.NewTicker(feedPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.drainOnce(ctx)
		}
	}
}

// drainOnce pulls one tick's worth of work: it keeps draining while the record
// stream reports more (up to maxRecordRounds rounds per tick, R9) or while the
// update stream fills a whole round (up to maxUpdateRounds rounds per tick,
// R2). Both streams are emitted once per tick, as arrays (F2).
func (f *HookFeeder) drainOnce(ctx context.Context) {
	var captures, updates []map[string]any
	// The tick's own generation. A Clear()/ResetAck() landing anywhere in this
	// tick - including between two rounds - voids the whole batch, the rounds
	// collected before it included (R10a).
	f.mu.Lock()
	tickGen := f.gen
	f.mu.Unlock()
	// Delivering at the end of the tick is what makes the event rate
	// independent of the record rate: a cancelled or failed round still hands
	// the rounds that already succeeded to the UI.
	defer func() { f.deliver(tickGen, captures, updates) }()

	recordRounds := 0
	updateRounds := 0
	for {
		f.mu.Lock()
		ack := f.ack
		updateAck := f.updateAck
		gen := f.gen
		f.mu.Unlock()

		// One round trip carries both streams: the records captured since ack
		// and the settled frames since updateAck.
		page, err := f.core.HookDrain(ctx, f.hookName, ack, feedDrainLimit, updateAck, feedUpdateLimit)
		if err != nil {
			return // engine offline: retry on the next tick
		}

		f.mu.Lock()
		// A ResetAck during the round-trip means the sequence space restarted:
		// this page belongs to the old space, so neither its records nor its
		// NextSeq may touch the state (storage is idempotent either way).
		stale := gen != f.gen
		f.mu.Unlock()
		if stale {
			return
		}

		if len(page.Records) > 0 && f.ingest != nil {
			if err := f.ingest(ctx, page.Records); err != nil {
				return // storage issue: retry the whole page next tick
			}
		}
		// 更新流的重投抑制（R11）：命中去重 FIFO 的帧既不再写库，也不再补缓冲、不再
		// 发事件。这里只做命中检查，登记发生在下面的写回区：apply 失败要把这一批留给
		// 下一 tick 重试，先登记会让重试的帧被自己的 FIFO 吞掉，库里的行永远停在
		// 「等待中」。
		fresh := f.freshUpdates(page.Updates)
		if len(fresh) > 0 && f.apply != nil {
			if err := f.apply(ctx, fresh); err != nil {
				return // storage issue: retry the whole page next tick
			}
		}

		f.mu.Lock()
		// Re-check generation under the same lock as the write-backs below:
		// a ResetAck or Clear that landed during the drain or the ingest
		// round-trip voids this page entirely, and check+write must be one
		// critical section or the reset can still slip in between.
		if gen != f.gen {
			f.mu.Unlock()
			return
		}
		batch := make([]map[string]any, 0, len(page.Records))
		for _, drained := range page.Records {
			if !f.markDelivered(drained) {
				continue
			}
			batch = append(batch, drained.Record)
			f.pushPending(drained.Record)
		}
		if page.NextSeq > f.ack {
			f.ack = page.NextSeq
		}
		// The patches land after the records were buffered, so a record that
		// arrived in this very page is settled before the panel can read it.
		patched := f.patchPending(fresh)
		if page.NextUpdateSeq > f.updateAck {
			f.updateAck = page.NextUpdateSeq
		}
		// R12：页面侧两条缓冲的累计丢弃数跟着这一页写回，语义是「最近一次读数」——
		// 页面钩子的 clearHookedCalls() 自己把它归零，下一次 drain 带回的就是 0，
		// shell 不做加减、也不在 Clear() 里替它归零。作废的页面（gen 变了）在上面
		// 已经 return，旧读数不会被写回来。
		f.pageDroppedRecords = page.DroppedRecords
		f.pageDroppedUpdates = page.DroppedUpdates
		// A broken Core that reports HasMore without advancing NextSeq must not
		// spin this loop forever. Process this page once, then let the next
		// tick retry from the same sequence.
		//
		// The record stream's continuation (R9) mirrors the update stream's
		// below: keep asking while a round came back full and a round budget
		// remains, so one tick hands the frontend at most
		// maxRecordRounds × feedDrainLimit records in a single event. Every
		// round advances the ack, so this cannot spin.
		fullRecords := len(page.Records) >= feedDrainLimit
		if fullRecords {
			recordRounds++
		}
		recordMore := fullRecords && page.HasMore && page.NextSeq > ack && recordRounds < maxRecordRounds
		// The update stream's continuation (R2): one round carries at most
		// feedUpdateLimit frames, while the page-side update buffer drops its
		// oldest once it is full - a settled burst larger than one round would
		// leave updateAck permanently behind (rows stay pending forever). Keep
		// asking while a round came back full; every round advances updateAck,
		// so this cannot spin, and the round budget keeps one burst from holding
		// the tick (the rest is taken by the next tick).
		fullUpdates := len(page.Updates) >= feedUpdateLimit
		if fullUpdates {
			updateRounds++
		}
		updateMore := fullUpdates && page.NextUpdateSeq > updateAck && updateRounds < maxUpdateRounds
		f.mu.Unlock()

		captures = append(captures, batch...)
		updates = append(updates, patched...)

		if !recordMore && !updateMore {
			return
		}
	}
}

// freshUpdates returns the update frames the dedup FIFO has not delivered yet
// (R11), so a replayed round costs neither a storage UPDATE nor a patch nor an
// event. It only *checks* the FIFO - registration happens in the write-back
// critical section (patchPending) once the round can no longer fail, so a
// failed apply leaves the frames to be retried by the next tick. A frame
// without a rid is always fresh: it cannot be tracked.
func (f *HookFeeder) freshUpdates(updates []DrainedUpdate) []DrainedUpdate {
	if len(updates) == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]DrainedUpdate, 0, len(updates))
	for _, update := range updates {
		if key, ok := updateKeyOf(update.Update); ok {
			if _, dup := f.delivered[key]; dup {
				continue
			}
		}
		out = append(out, update)
	}
	return out
}

// deliver pushes one tick's batches to the UI: at most one event per stream,
// each carrying an array (F2). The single-record callbacks of NewHookFeeder
// were wrapped per record at construction, so cloud/console keep their event
// shape. A generation change since the tick started (Clear() or ResetAck(),
// R10a) drops the whole batch: a Clear landing between two rounds would
// otherwise hand the panel the records the user just emptied right after the
// click. Dropping on ResetAck loses nothing - the same records are drained
// again from seq 0 on the next tick.
func (f *HookFeeder) deliver(tickGen int64, captures, updates []map[string]any) {
	f.mu.Lock()
	stale := f.gen != tickGen
	f.mu.Unlock()
	if stale {
		return
	}
	if len(captures) > 0 && f.onCapture != nil {
		f.onCapture(captures)
	}
	if len(updates) > 0 && f.onUpdate != nil {
		f.onUpdate(updates)
	}
}

// pushPending appends a record to the poll buffer under its rid so a later
// update frame can patch it in place (the buffer holds map references, so the
// patch is what the next poll returns). The buffer is bounded: the oldest
// entry is dropped and counted, the newest is never lost.
func (f *HookFeeder) pushPending(record map[string]any) {
	f.pending = append(f.pending, record)
	if rid := ridOf(record); rid != "" {
		if f.pendingIndex == nil {
			f.pendingIndex = map[string]map[string]any{}
		}
		f.pendingIndex[rid] = record
	}
	for len(f.pending) > maxPendingRecords {
		oldest := f.pending[0]
		f.pending = f.pending[1:]
		f.dropped++
		// The index names the newest buffered record of a rid; dropping the
		// oldest occurrence of a duplicated rid would orphan that entry, but a
		// rid only repeats for a record without a usable ts, which
		// markDelivered cannot deduplicate.
		if rid := ridOf(oldest); rid != "" {
			delete(f.pendingIndex, rid)
		}
	}
}

// patchPending folds every update frame into the buffered record with the same
// rid and returns the frames to deliver. A frame whose record was already
// polled (or dropped) still reaches the UI: the panel patches its own row by
// rid, which is what keeps the two delivery paths idempotent. Registering the
// frame in the dedup FIFO happens here, in the same critical section as the
// patch: a replayed frame is dropped before it can patch anything (R11), and a
// round that never reaches this point (failed apply, stale page) stays
// unregistered and is retried by the next tick.
func (f *HookFeeder) patchPending(updates []DrainedUpdate) []map[string]any {
	if len(updates) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(updates))
	for _, update := range updates {
		if !f.markUpdateDelivered(update.Update) {
			continue // 重投的落定帧：不补缓冲、不投递（首次投递已经落定过）
		}
		if record := f.pendingIndex[ridOf(update.Update)]; record != nil {
			patchSettled(record, update.Update)
		}
		out = append(out, update.Update)
	}
	return out
}

// patchSettled folds a settled frame into its buffered record: the status, the
// duration and whichever of result/error the page reported. Absent keys are
// left alone, so a frame that only carries a status cannot wipe the payload.
func patchSettled(record, update map[string]any) {
	for _, key := range []string{"status", "durationMs"} {
		if value, ok := update[key]; ok {
			record[key] = value
		}
	}
	if value, ok := update["result"]; ok {
		record["result"] = value
	}
	if value, ok := update["error"]; ok {
		record["error"] = value
	}
}

// ridOf reads a record's stable identity (traffic_records.id): the key that
// lets the event stream and the poll path recognise the same capture.
func ridOf(record map[string]any) string {
	rid, _ := record["rid"].(string)
	return rid
}

// markDelivered registers a record's identity in the bounded FIFO set and
// reports whether it was already delivered. A silent in-realm refresh
// (repeated setupContext) keeps the page-side buffer: after ResetAck the
// drain re-reads it from seq 0 and would re-deliver up to 5000 old records
// to the poll buffer and capture events. Storage is idempotent by
// (api_type, captured_at, seq) but the UI is not, and replayed records
// carry their original capture ts, so the FIFO — sized above the page
// buffer (deliveredCapacity > 5000) — filters them without ever dropping
// fresh data. Records without a usable ts are never tracked: dedup must not
// risk data loss.
func (f *HookFeeder) markDelivered(drained DrainedRecord) bool {
	key, ok := deliveredKeyOf(drained)
	if !ok {
		return true
	}
	return f.markKey(key)
}

// markUpdateDelivered registers an update frame's rid in the same FIFO and
// reports whether the frame is fresh (R11). A replay of the same frames (the
// page-side update buffer survived a same-realm ResetAck) is then suppressed
// instead of costing one redundant UPDATE per row plus an event for each. A
// frame without a rid is never tracked and never suppressed: dedup must not
// risk data loss. Callers hold the lock.
func (f *HookFeeder) markUpdateDelivered(update map[string]any) bool {
	key, ok := updateKeyOf(update)
	if !ok {
		return true
	}
	return f.markKey(key)
}

// markKey is the bounded FIFO itself: it reports whether key is fresh and, if
// so, registers it, evicting the oldest entry once the FIFO is full. Callers
// hold the lock.
func (f *HookFeeder) markKey(key deliveredKey) bool {
	if f.delivered == nil {
		f.delivered = make(map[deliveredKey]struct{}, deliveredCapacity)
		f.deliveredFIFO = make([]deliveredKey, 0, deliveredCapacity)
	}
	if _, dup := f.delivered[key]; dup {
		return false
	}
	f.delivered[key] = struct{}{}
	if len(f.deliveredFIFO) < deliveredCapacity {
		f.deliveredFIFO = append(f.deliveredFIFO, key)
	} else {
		delete(f.delivered, f.deliveredFIFO[f.deliveredPos])
		f.deliveredFIFO[f.deliveredPos] = key
		f.deliveredPos = (f.deliveredPos + 1) % deliveredCapacity
	}
	return true
}

func deliveredKeyOf(drained DrainedRecord) (deliveredKey, bool) {
	var ts int64
	switch v := drained.Record["ts"].(type) {
	case float64:
		ts = int64(v)
	case int64:
		ts = v
	case int:
		ts = int64(v)
	default:
		return deliveredKey{}, false
	}
	if ts == 0 {
		return deliveredKey{}, false
	}
	appID, _ := drained.Record["appId"].(string)
	return deliveredKey{flow: recordFlow, appID: appID, ts: ts, seq: drained.Seq}, true
}

// updateKeyOf reads the FIFO identity of one settled frame: its rid, which is
// the storage identity of the row it settles. A frame without a rid is not
// trackable.
func updateKeyOf(update map[string]any) (deliveredKey, bool) {
	rid := ridOf(update)
	if rid == "" {
		return deliveredKey{}, false
	}
	return deliveredKey{flow: updateFlow, rid: rid}, true
}
