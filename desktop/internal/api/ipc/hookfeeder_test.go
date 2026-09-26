package ipc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeFeederCore records drain calls and returns a configurable page.
// onDrain, when set, runs inside HookDrain (before returning) to simulate
// concurrent feeder calls racing with an in-flight drain.
type fakeFeederCore struct {
	mu            sync.Mutex
	installCalls  int
	installReport map[string]any
	installErr    error
	lastAfterSeq  int64
	lastUpdateSeq int64
	drainCalls    int
	nextSeq       int64
	records       []DrainedRecord
	updates       []DrainedUpdate
	// pageDroppedRecord / pageDroppedUpdate are the cumulative eviction counts
	// every drain response reports (R12): readings the shell only echoes.
	pageDroppedRecord int64
	pageDroppedUpdate int64
	onDrain           func()
	evaluations       []string
	evalResults       []any
}

func (c *fakeFeederCore) InstallHook(ctx context.Context, name string) error {
	_, err := c.InstallHookReport(ctx, name)
	return err
}

func (c *fakeFeederCore) InstallHookReport(ctx context.Context, name string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.installCalls++
	if c.installErr != nil {
		return nil, c.installErr
	}
	if c.installReport != nil {
		return c.installReport, nil
	}
	return map[string]any{"ok": true}, nil
}

// HookDrain follows the Core's contract: only entries after the acknowledged
// cursor come back, at most limit / updateLimit of them per call, and the
// returned next* is the last sequence handed out (the cursor stays put when a
// stream is empty). nextSeq is an extra floor for tests that only care about
// acknowledgement.
func (c *fakeFeederCore) HookDrain(ctx context.Context, name string, afterSeq int64, limit int, afterUpdateSeq int64, updateLimit int) (DrainPage, error) {
	c.mu.Lock()
	c.lastAfterSeq = afterSeq
	c.lastUpdateSeq = afterUpdateSeq
	c.drainCalls++
	records := make([]DrainedRecord, 0, len(c.records))
	for _, record := range c.records {
		if record.Seq > afterSeq {
			records = append(records, record)
		}
	}
	hasMore := false
	if limit > 0 && len(records) > limit {
		records = records[:limit]
		hasMore = true
	}
	next := afterSeq
	if len(records) > 0 {
		next = records[len(records)-1].Seq
	}
	if c.nextSeq > next {
		next = c.nextSeq
	}
	updates := make([]DrainedUpdate, 0, len(c.updates))
	for _, update := range c.updates {
		if update.Seq > afterUpdateSeq {
			updates = append(updates, update)
		}
	}
	if updateLimit > 0 && len(updates) > updateLimit {
		updates = updates[:updateLimit]
	}
	nextUpdate := afterUpdateSeq
	if len(updates) > 0 {
		nextUpdate = updates[len(updates)-1].Seq
	}
	droppedRecords, droppedUpdates := c.pageDroppedRecord, c.pageDroppedUpdate
	c.mu.Unlock()
	if c.onDrain != nil {
		c.onDrain()
	}
	return DrainPage{
		Records: records, Updates: updates, NextSeq: next, NextUpdateSeq: nextUpdate, HasMore: hasMore,
		DroppedRecords: droppedRecords, DroppedUpdates: droppedUpdates,
	}, nil
}

func (c *fakeFeederCore) Evaluate(ctx context.Context, expression string, timeoutMs int) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evaluations = append(c.evaluations, expression)
	if len(c.evalResults) == 0 {
		return nil, nil
	}
	result := c.evalResults[0]
	c.evalResults = c.evalResults[1:]
	return result, nil
}

func TestReplayWaitsForPageCompletion(t *testing.T) {
	core := &fakeFeederCore{evalResults: []any{
		nil,
		nil,
		`{"ok":true,"status":"success","result":{"id":7}}`,
	}}
	feeder := NewHookFeeder(core, "wxapi", nil, nil)

	result, err := feeder.Replay(context.Background(), "request", map[string]any{"url": "/demo"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	payload, ok := result.(map[string]any)
	if !ok || payload["ok"] != true || payload["status"] != "success" {
		t.Fatalf("unexpected replay result: %#v", result)
	}
	if len(core.evaluations) != 3 || core.evaluations[0] != "window._wxApiReplayDone=null" || core.evaluations[2] != "window._wxApiReplayDone" {
		t.Fatalf("replay evaluation sequence: %#v", core.evaluations)
	}
}

// A page reload resets the Core-side sequence space while the feeder's ack
// keeps growing; after a fresh hook.install the ack must restart from zero
// or the new records (seq 1..N) are skipped forever.
func TestResetAckRedrainsFromZero(t *testing.T) {
	core := &fakeFeederCore{nextSeq: 423}
	feeder := NewHookFeeder(core, "wxapi", nil, nil)
	if err := feeder.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	feeder.Stop()

	feeder.drainOnce(context.Background())
	if core.lastAfterSeq != 0 {
		t.Fatalf("first drain starts from 0, got %d", core.lastAfterSeq)
	}

	feeder.drainOnce(context.Background())
	if core.lastAfterSeq != 423 {
		t.Fatalf("ack should have advanced to 423, got %d", core.lastAfterSeq)
	}

	feeder.ResetAck()
	feeder.drainOnce(context.Background())
	if core.lastAfterSeq != 0 {
		t.Fatalf("after a reinstall the drain must restart from 0, got %d", core.lastAfterSeq)
	}
}

// ResetAck landing while a drain round-trip is in flight (miniapp switch
// racing the 500ms poll loop) must not be overwritten by that drain's
// stale NextSeq write-back: the next drain still starts from 0.
func TestResetAckBeatsInFlightDrain(t *testing.T) {
	core := &fakeFeederCore{nextSeq: 900}
	feeder := NewHookFeeder(core, "wxapi", nil, nil)
	if err := feeder.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer feeder.Stop()

	// The reset lands while the first drain's HookDrain call is in flight.
	core.mu.Lock()
	core.onDrain = func() { feeder.ResetAck() }
	core.mu.Unlock()
	feeder.drainOnce(context.Background())

	core.mu.Lock()
	core.onDrain = nil
	core.mu.Unlock()
	feeder.drainOnce(context.Background())
	core.mu.Lock()
	afterSeq := core.lastAfterSeq
	core.mu.Unlock()
	if afterSeq != 0 {
		t.Fatalf("stale drain overwrote the reset: next drain used afterSeq %d, want 0", afterSeq)
	}
}

// A ResetAck landing while the ingest round-trip is in flight sits after the
// post-drain stale check. The write-back critical section must re-check the
// generation: otherwise the stale page reaches the UI buffer and its NextSeq
// overwrites the reset, so the fresh records (seq 1..N) are skipped forever.
func TestResetAckDuringIngestDropsStalePage(t *testing.T) {
	core := &fakeFeederCore{
		nextSeq: 900,
		records: []DrainedRecord{{Seq: 900, Record: map[string]any{"k": "v"}}},
	}
	feeder := NewHookFeeder(core, "wxapi", nil, nil)
	feeder.ingest = func(ctx context.Context, batch []DrainedRecord) error {
		feeder.ResetAck() // lands while the ingest round-trip is in flight
		return nil
	}

	feeder.drainOnce(context.Background())
	feeder.drainOnce(context.Background())

	core.mu.Lock()
	afterSeq := core.lastAfterSeq
	core.mu.Unlock()
	if afterSeq != 0 {
		t.Fatalf("stale write-back overwrote the reset: next drain used afterSeq %d, want 0", afterSeq)
	}
	if got, err := feeder.Poll(); err != nil || len(got) != 0 {
		t.Fatalf("stale records reached the UI buffer: %#v (%v)", got, err)
	}
}

// A silent in-realm refresh (repeated setupContext) keeps the page-side
// buffer: after ResetAck the drain re-reads it from seq 0, so every old
// record (same ts, same seq) would be re-delivered to the poll buffer and
// the capture events. Storage is idempotent; the UI is not.
func TestResetAckReplaysAreDeduplicated(t *testing.T) {
	core := &fakeFeederCore{
		records: []DrainedRecord{
			{Seq: 1, Record: map[string]any{"ts": float64(1000), "k": "a"}},
			{Seq: 2, Record: map[string]any{"ts": float64(1001), "k": "b"}},
		},
	}
	captures := 0
	feeder := NewHookFeeder(core, "wxapi", func(map[string]any) { captures++ }, nil)
	if err := feeder.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer feeder.Stop()

	feeder.drainOnce(context.Background())
	first, err := feeder.Poll()
	if err != nil || len(first) != 2 {
		t.Fatalf("first delivery: %d records (%v)", len(first), err)
	}
	if captures != 2 {
		t.Fatalf("first delivery fired %d captures, want 2", captures)
	}

	// Same realm: the buffer still holds both records; ResetAck restarts
	// the drain at 0 and they arrive again with identical identities.
	feeder.ResetAck()
	feeder.drainOnce(context.Background())
	replay, err := feeder.Poll()
	if err != nil || len(replay) != 0 {
		t.Fatalf("replayed records reached the UI buffer: %#v (%v)", replay, err)
	}
	if captures != 2 {
		t.Fatalf("replay fired extra captures: %d, want 2", captures)
	}
}

// ResetAck after a real reload (new realm) restarts seq at 1 with fresh
// capture timestamps; those records must be delivered even though the same
// seq values were seen before the reset.
func TestResetAckFreshRealmRecordsStillDelivered(t *testing.T) {
	core := &fakeFeederCore{
		records: []DrainedRecord{
			{Seq: 1, Record: map[string]any{"ts": float64(1000), "k": "old"}},
		},
	}
	feeder := NewHookFeeder(core, "wxapi", nil, nil)
	if err := feeder.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer feeder.Stop()

	feeder.drainOnce(context.Background())
	if _, err := feeder.Poll(); err != nil {
		t.Fatalf("poll: %v", err)
	}

	feeder.ResetAck()
	core.mu.Lock()
	core.records = []DrainedRecord{
		{Seq: 1, Record: map[string]any{"ts": float64(9000), "k": "new"}},
	}
	core.mu.Unlock()
	feeder.drainOnce(context.Background())

	fresh, err := feeder.Poll()
	if err != nil || len(fresh) != 1 || fresh[0]["k"] != "new" {
		t.Fatalf("fresh-realm record was not delivered: %#v (%v)", fresh, err)
	}
}

func TestDeliveredKeySeparatesApps(t *testing.T) {
	feeder := &HookFeeder{}
	first := DrainedRecord{Seq: 1, Record: map[string]any{"appId": "wx1", "ts": float64(1000)}}
	second := DrainedRecord{Seq: 1, Record: map[string]any{"appId": "wx2", "ts": float64(1000)}}
	if !feeder.markDelivered(first) {
		t.Fatal("first app record was treated as already delivered")
	}
	if !feeder.markDelivered(second) {
		t.Fatal("second app record was dropped because appid was missing from the delivery key")
	}
}

// ---------- settled-record update stream ----------

const settledRID = "wx.request-wxone-1000-1"

func settledUpdate(seq int64) DrainedUpdate {
	return DrainedUpdate{Seq: seq, Update: map[string]any{
		"seq": seq, "rid": settledRID, "status": "success",
		"result": map[string]any{"ok": true}, "durationMs": float64(1830),
	}}
}

// One page may carry a record and its own settled update (a callback that
// landed before the 500ms drain tick). The record must be ingested first: the
// other order makes ApplyUpdates match no row, and the INSERT that follows
// writes the pending version - the record then stays pending in storage and in
// the history view forever, which is the defect this stream exists to fix.
func TestUpdateStreamAppliesAfterIngestWithinOnePage(t *testing.T) {
	core := &fakeFeederCore{
		nextSeq: 1,
		records: []DrainedRecord{{Seq: 1, Record: map[string]any{
			"ts": float64(1000), "rid": settledRID, "status": "pending",
		}}},
		updates: []DrainedUpdate{settledUpdate(1)},
	}
	var order []string
	feeder := NewHookFeederWithUpdates(core, "wxapi", nil,
		func(context.Context, []DrainedRecord) error { order = append(order, "ingest"); return nil },
		func(context.Context, []DrainedUpdate) error { order = append(order, "apply"); return nil },
		nil)

	feeder.drainOnce(context.Background())

	if len(order) != 2 || order[0] != "ingest" || order[1] != "apply" {
		t.Fatalf("the record must be ingested before its update is applied, got %v", order)
	}

	// The buffered record is the map the reader will get: the patch must be
	// visible to the next poll without a rebuild.
	records, err := feeder.Poll()
	if err != nil || len(records) != 1 {
		t.Fatalf("poll: %#v (%v)", records, err)
	}
	if records[0]["status"] != "success" || records[0]["durationMs"] != float64(1830) {
		t.Fatalf("buffered record was not settled in place: %#v", records[0])
	}
	if result, ok := records[0]["result"].(map[string]any); !ok || result["ok"] != true {
		t.Fatalf("settled result missing from the buffered record: %#v", records[0])
	}
}

// The panel patches its own row by rid, so a frame whose record was already
// polled (or dropped) still has to be delivered: the two paths are what make a
// duplicate delivery idempotent.
func TestUpdateOfAlreadyPolledRecordStillEmits(t *testing.T) {
	core := &fakeFeederCore{
		nextSeq: 1,
		records: []DrainedRecord{{Seq: 1, Record: map[string]any{
			"ts": float64(1000), "rid": settledRID, "status": "pending",
		}}},
	}
	var received []map[string]any
	feeder := NewHookFeederWithUpdates(core, "wxapi", nil, nil, nil,
		func(updates []map[string]any) { received = append(received, updates...) })

	feeder.drainOnce(context.Background())
	if records, _ := feeder.Poll(); len(records) != 1 {
		t.Fatalf("record not buffered: %#v", records)
	}

	core.mu.Lock()
	core.updates = []DrainedUpdate{settledUpdate(1)}
	core.mu.Unlock()
	feeder.drainOnce(context.Background())

	if len(received) != 1 || received[0]["rid"] != settledRID || received[0]["durationMs"] != float64(1830) {
		t.Fatalf("update frame not delivered: %#v", received)
	}
	if records, _ := feeder.Poll(); len(records) != 0 {
		t.Fatalf("an update must not resurface a polled record: %#v", records)
	}
}

// Both streams are acknowledged on their own cursor: the record cursor must
// not swallow update frames, and a delivered frame must not come back.
func TestUpdateAckIsIndependentAndResets(t *testing.T) {
	core := &fakeFeederCore{nextSeq: 5, updates: []DrainedUpdate{settledUpdate(3)}}
	emitted := 0
	feeder := NewHookFeederWithUpdates(core, "wxapi", nil, nil, nil,
		func(updates []map[string]any) { emitted += len(updates) })

	feeder.drainOnce(context.Background())
	if emitted != 1 {
		t.Fatalf("update frame delivered %d times, want 1", emitted)
	}
	stats := feeder.Stats()
	if stats.Ack != 5 || stats.UpdateAck != 3 {
		t.Fatalf("cursors must advance independently: %+v", stats)
	}

	// Acknowledged: the next round trip asks from 3 and gets nothing back.
	feeder.drainOnce(context.Background())
	core.mu.Lock()
	asked := core.lastUpdateSeq
	core.mu.Unlock()
	if asked != 3 || emitted != 1 {
		t.Fatalf("update ack not honoured: asked from %d, emitted %d", asked, emitted)
	}

	// A reload restarts the page-side sequence space, so both cursors go back
	// to zero together (storage stays idempotent).
	feeder.ResetAck()
	if stats := feeder.Stats(); stats.Ack != 0 || stats.UpdateAck != 0 {
		t.Fatalf("ResetAck must reset both cursors: %+v", stats)
	}
	feeder.drainOnce(context.Background())
	// 同一帧在 reset 之后重投：游标照样从 0 重新走到末位，但帧本身被去重 FIFO 挡下，
	// 不落库、不补缓冲、不发事件（R11，见 TestReplayedUpdateFramesAreSuppressed）。
	if stats := feeder.Stats(); stats.UpdateAck != 3 {
		t.Fatalf("updateAck 必须重新推进到末位: %+v", stats)
	}
	if emitted != 1 {
		t.Fatalf("reset 后重投的同一帧必须被抑制: emitted = %d", emitted)
	}
}

// ---------- R11 去重 FIFO 覆盖更新流 ----------

// 同 realm 刷新（页面缓冲存活）后 ResetAck 会让最多一整个更新缓冲被重投：多余的往返、
// 每行一次冗余 UPDATE、每帧一次冗余事件。更新帧以 rid 登记进记录流共用的那条 FIFO，
// 重投的整批被抑制；首次投递必须照常落库、照常发事件，换了 rid 的新帧也不能被误杀。
func TestReplayedUpdateFramesAreSuppressed(t *testing.T) {
	core := &fakeFeederCore{
		records: []DrainedRecord{{Seq: 1, Record: map[string]any{
			"ts": float64(1000), "rid": settledRID, "status": "pending",
		}}},
		updates: []DrainedUpdate{settledUpdate(1)},
	}
	applied, emitted := 0, 0
	feeder := NewHookFeederWithUpdates(core, "wxapi", nil, nil,
		func(_ context.Context, batch []DrainedUpdate) error { applied += len(batch); return nil },
		func(frames []map[string]any) { emitted += len(frames) })

	feeder.drainOnce(context.Background())
	if applied != 1 || emitted != 1 {
		t.Fatalf("首次投递必须照常落库并发事件: applied=%d emitted=%d", applied, emitted)
	}

	// 同一批帧在 ResetAck 之后重投（页面缓冲没被清过，seq 与 ts 原样回来）。
	feeder.ResetAck()
	feeder.drainOnce(context.Background())
	if applied != 1 {
		t.Fatalf("重投的落定帧又写了一次库: applied=%d", applied)
	}
	if emitted != 1 {
		t.Fatalf("重投的落定帧又发了一次事件: emitted=%d", emitted)
	}

	// 去重按 rid 计算：另一行的落定帧照常投递。
	core.mu.Lock()
	core.updates = []DrainedUpdate{{Seq: 2, Update: map[string]any{
		"seq": 2, "rid": "wx.request-wxone-2000-9", "status": "success", "durationMs": float64(12),
	}}}
	core.mu.Unlock()
	feeder.drainOnce(context.Background())
	if applied != 2 || emitted != 2 {
		t.Fatalf("新的落定帧被去重误杀: applied=%d emitted=%d", applied, emitted)
	}
}

// A page whose install() answers {ok:false} is not capturing anything: saying
// success would leave the panel showing "capturing" with no records ever.
func TestStartFailsWhenPageReportsNotOK(t *testing.T) {
	core := &fakeFeederCore{installReport: map[string]any{"ok": false, "reason": "no frames with wx found"}}
	feeder := NewHookFeeder(core, "wxapi", nil, nil)

	err := feeder.Start()
	if err == nil {
		t.Fatal("an {ok:false} install report must not report success")
	}
	if !strings.Contains(err.Error(), "no frames with wx found") {
		t.Fatalf("the page-side reason must reach the error: %v", err)
	}
	if stats := feeder.Stats(); stats.Running {
		t.Fatalf("a failed install must leave the feeder stopped: %+v", stats)
	}

	// The panel can retry (the user reloaded the mini program): a failed start
	// must not wedge the feeder.
	core.mu.Lock()
	core.installReport = map[string]any{"ok": true}
	core.mu.Unlock()
	if err := feeder.Start(); err != nil {
		t.Fatalf("retry after a failed install: %v", err)
	}
	defer feeder.Stop()
	if stats := feeder.Stats(); !stats.Running {
		t.Fatalf("the retry must leave the feeder running: %+v", stats)
	}
}

// The console hook answers {ok:false} on WMPF builds that lock the console
// object, yet keeps capturing the error exits (window.onerror /
// unhandledrejection). Its feeder must start anyway and drain that stream —
// with the strict policy those records stayed stranded in the page buffer
// forever while the panel claimed capture was live. An install that *errors*
// (realm dead, no page) must still fail: there is nothing to drain from.
func TestConsoleFeederStartsWhenPageReportsNotOK(t *testing.T) {
	core := &fakeFeederCore{
		installReport: map[string]any{"ok": false, "hookedConsoles": 0, "unwrappableConsoles": 1, "errorsHooked": true},
		records:       []DrainedRecord{{Seq: 1, Record: map[string]any{"type": "console", "level": "error", "text": "boom"}}},
	}
	feeder := NewHookFeeder(core, "console", nil, nil)
	feeder.AcceptAnyInstallReport()

	if err := feeder.Start(); err != nil {
		t.Fatalf("an answered {ok:false} report must still start the console feeder: %v", err)
	}
	feeder.Stop()

	feeder.drainOnce(context.Background())
	pending, err := feeder.Poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(pending) != 1 || pending[0]["text"] != "boom" {
		t.Fatalf("the error record must reach the poll buffer: %#v", pending)
	}

	core.mu.Lock()
	core.installErr = fmt.Errorf("engine not started")
	core.mu.Unlock()
	if err := feeder.Start(); err == nil {
		t.Fatal("an errored install must not report success, even with the relaxed policy")
	}
	if stats := feeder.Stats(); stats.Running {
		t.Fatalf("a failed install must leave the feeder stopped: %+v", stats)
	}
}

// The strict default is untouched: wxapi/cloud must keep refusing to start on
// an {ok:false} page even if a console feeder in the same process relaxed its
// own policy.
func TestAcceptAnyInstallReportIsPerFeeder(t *testing.T) {
	relaxed := &fakeFeederCore{installReport: map[string]any{"ok": false}}
	strict := &fakeFeederCore{installReport: map[string]any{"ok": false}}
	relaxedFeeder := NewHookFeeder(relaxed, "console", nil, nil)
	relaxedFeeder.AcceptAnyInstallReport()

	if err := relaxedFeeder.Start(); err != nil {
		t.Fatalf("relaxed feeder must start: %v", err)
	}
	relaxedFeeder.Stop()
	if err := NewHookFeeder(strict, "wxapi", nil, nil).Start(); err == nil {
		t.Fatal("the default policy must keep failing on {ok:false}")
	}
}

// The poll buffer is what a closed panel leaves behind: it must stay bounded,
// drop the oldest and report how much it dropped.
func TestPendingBufferDropsOldestAndCountsIt(t *testing.T) {
	feeder := NewHookFeeder(&fakeFeederCore{}, "wxapi", nil, nil)
	overflow := 3
	for i := 0; i < maxPendingRecords+overflow; i++ {
		feeder.pushPending(map[string]any{"rid": fmt.Sprintf("rid-%d", i)})
	}

	stats := feeder.Stats()
	if stats.Pending != maxPendingRecords || stats.Dropped != int64(overflow) {
		t.Fatalf("buffer must be bounded and count its drops: %+v", stats)
	}

	records, err := feeder.Poll()
	if err != nil || len(records) != maxPendingRecords {
		t.Fatalf("poll after an overflow: %d records (%v)", len(records), err)
	}
	if records[0]["rid"] != fmt.Sprintf("rid-%d", overflow) {
		t.Fatalf("the oldest records must be the ones dropped, kept %v", records[0]["rid"])
	}
	// The index follows the buffer: a dropped record is not patchable any more.
	for _, dropped := range []string{"rid-0", "rid-1", "rid-2"} {
		if _, leaked := feeder.pendingIndex[dropped]; leaked {
			t.Fatalf("dropped record %s is still indexed", dropped)
		}
	}
	if stats := feeder.Stats(); stats.Pending != 0 {
		t.Fatalf("poll must consume the buffer: %+v", stats)
	}
}

// A dropped record's frames are still delivered (the panel patches what it
// has), but nothing may be patched into the buffer through a stale index entry.
func TestUpdateOfDroppedRecordIsNotPatchedIntoTheBuffer(t *testing.T) {
	feeder := NewHookFeederWithUpdates(&fakeFeederCore{}, "wxapi", nil, nil, nil, nil)
	dropped := map[string]any{"rid": "gone", "status": "pending"}
	feeder.pushPending(dropped)
	for i := 0; i <= maxPendingRecords; i++ {
		feeder.pushPending(map[string]any{"rid": fmt.Sprintf("rid-%d", i)})
	}

	feeder.mu.Lock()
	patched := feeder.patchPending([]DrainedUpdate{{Seq: 1, Update: map[string]any{"rid": "gone", "status": "success"}}})
	feeder.mu.Unlock()

	if len(patched) != 1 {
		t.Fatalf("the frame must still be delivered: %#v", patched)
	}
	if dropped["status"] != "pending" {
		t.Fatalf("a dropped record was patched through a stale index: %#v", dropped)
	}
}

// ---------- F2 事件按批投递 ----------

// A tick delivers its whole page as one event per stream: per-record events
// meant one EventsEmit, one JSON serialization and one webview dispatch per
// record - 200 of each for a 200-record page.
func TestEachStreamDeliversOneBatchPerTick(t *testing.T) {
	records := make([]DrainedRecord, 0, 5)
	for i := 1; i <= 5; i++ {
		records = append(records, DrainedRecord{Seq: int64(i), Record: map[string]any{
			"ts": float64(1000 + i), "rid": fmt.Sprintf("rid-%d", i), "status": "pending",
		}})
	}
	core := &fakeFeederCore{records: records, updates: []DrainedUpdate{settledUpdate(1)}}
	var captureBatches, updateBatches [][]map[string]any
	feeder := NewHookFeederWithUpdates(core, "wxapi",
		func(batch []map[string]any) { captureBatches = append(captureBatches, batch) },
		nil,
		func(context.Context, []DrainedUpdate) error { return nil },
		func(batch []map[string]any) { updateBatches = append(updateBatches, batch) })

	feeder.drainOnce(context.Background())

	if len(captureBatches) != 1 || len(captureBatches[0]) != 5 {
		t.Fatalf("整页记录必须合成一次事件: %#v", captureBatches)
	}
	for i, record := range captureBatches[0] {
		if record["rid"] != fmt.Sprintf("rid-%d", i+1) {
			t.Fatalf("批次内的顺序必须与页面一致: %#v", captureBatches[0])
		}
	}
	if len(updateBatches) != 1 || len(updateBatches[0]) != 1 {
		t.Fatalf("更新帧同样按批投递: %#v", updateBatches)
	}

	// An empty tick must stay silent: an event with an empty array would be
	// pure overhead at 2 ticks per second.
	feeder.drainOnce(context.Background())
	if len(captureBatches) != 1 || len(updateBatches) != 1 {
		t.Fatalf("空 tick 不得发事件: %d 批记录, %d 批更新", len(captureBatches), len(updateBatches))
	}
}

// ---------- R12 页面侧丢弃计数 ----------

// 页面侧的两条缓冲是第一批真的会丢东西的地方：钩子自己的缓冲满了就把最旧的扔掉
// （core/hooks/wxapi.js / cloud.js），并把累计条数随每个 drain 响应报回来。feeder
// 把这个读数原样放进 FeederStats，面板的「已丢弃」才有页面那一半 —— shell 的
// Dropped（pending 溢出）盖不住它，两者互补不重叠。
func TestStatsRepeatThePageDropReading(t *testing.T) {
	core := &fakeFeederCore{
		records:           drainedRecords(2),
		updates:           settledUpdates(1),
		pageDroppedRecord: 17,
		pageDroppedUpdate: 9,
	}
	feeder := NewHookFeederWithUpdates(core, "wxapi", nil, nil, nil, nil)

	feeder.drainOnce(context.Background())

	stats := feeder.Stats()
	if stats.PageDroppedRecords != 17 || stats.PageDroppedUpdates != 9 {
		t.Fatalf("页面侧的累计读数没有进入 Stats: %+v", stats)
	}
	// 页面侧的丢弃不是 shell 的 pending 溢出：两者互不污染，相加才是面板要显示的数。
	if stats.Dropped != 0 {
		t.Fatalf("页面侧丢弃不得计进 shell 的 dropped: %+v", stats)
	}
	if stats.Pending != 2 {
		t.Fatalf("记录照常进缓冲: %+v", stats)
	}

	// 更新流同样只是复述：没有记录、只有落定帧的 tick 也要把读数带回来。
	feeder.Poll()
	core.mu.Lock()
	core.pageDroppedRecord = 20
	core.mu.Unlock()
	feeder.drainOnce(context.Background())
	if stats := feeder.Stats(); stats.PageDroppedRecords != 20 || stats.PageDroppedUpdates != 9 {
		t.Fatalf("空记录 tick 的读数被丢掉了: %+v", stats)
	}
}

// 读数的归零属于页面：clearHookedCalls() 把两条计数器清零，下一个 drain 带回的就是
// 0。所以 shell 侧的 Clear()/ResetAck() 不得替它归零 —— 那会让面板显示一个 shell 编
// 出来、页面从没报告过的数字。
func TestClearAndResetAckLeaveThePageDropReadingAlone(t *testing.T) {
	core := &fakeFeederCore{pageDroppedRecord: 5, pageDroppedUpdate: 3}
	feeder := NewHookFeederWithUpdates(core, "wxapi", nil, nil, nil, nil)
	feeder.drainOnce(context.Background())
	if stats := feeder.Stats(); stats.PageDroppedRecords != 5 || stats.PageDroppedUpdates != 3 {
		t.Fatalf("首次读数未记下: %+v", stats)
	}

	feeder.Clear()
	if stats := feeder.Stats(); stats.PageDroppedRecords != 5 || stats.PageDroppedUpdates != 3 {
		t.Fatalf("Clear 只能复述最近一次读数，不得替页面归零: %+v", stats)
	}
	feeder.ResetAck()
	if stats := feeder.Stats(); stats.PageDroppedRecords != 5 || stats.PageDroppedUpdates != 3 {
		t.Fatalf("ResetAck 不得清零读数: %+v", stats)
	}

	// 页面缓冲被清空之后，下一个 tick 带回的是页面自己的 0，读数这才跟上。
	core.mu.Lock()
	core.pageDroppedRecord, core.pageDroppedUpdate = 0, 0
	core.mu.Unlock()
	feeder.drainOnce(context.Background())
	if stats := feeder.Stats(); stats.PageDroppedRecords != 0 || stats.PageDroppedUpdates != 0 {
		t.Fatalf("页面归零后的读数没有跟上: %+v", stats)
	}
}

// 作废的页面（Clear()/ResetAck() 在往返期间落下）描述的是已经不存在的那份缓冲：它的
// 读数必须跟着整页一起丢掉，否则用户刚点完「清空」，面板还挂着清空前的丢弃数。
func TestVoidedPageDoesNotWriteBackItsDropReading(t *testing.T) {
	core := &fakeFeederCore{pageDroppedRecord: 5, pageDroppedUpdate: 3}
	feeder := NewHookFeederWithUpdates(core, "wxapi", nil, nil, nil, nil)
	core.mu.Lock()
	core.onDrain = func() { feeder.Clear() }
	core.mu.Unlock()

	feeder.drainOnce(context.Background())
	if stats := feeder.Stats(); stats.PageDroppedRecords != 0 || stats.PageDroppedUpdates != 0 {
		t.Fatalf("作废页面的读数被写回了: %+v", stats)
	}

	// Clear 清的是 shell 侧缓冲，页面缓冲还在：恢复正常之后的读数照常写回。
	core.mu.Lock()
	core.onDrain = nil
	core.pageDroppedRecord, core.pageDroppedUpdate = 8, 4
	core.mu.Unlock()
	feeder.drainOnce(context.Background())
	if stats := feeder.Stats(); stats.PageDroppedRecords != 8 || stats.PageDroppedUpdates != 4 {
		t.Fatalf("恢复之后的读数没有跟上: %+v", stats)
	}
}

// 一个 tick 里有续行（页面一次回 200 条）时每一轮都会重报同一份累计值：写回的必须
// 是最后一轮的读数，否则面板显示的丢弃数永远停在续行开始前的那一刻。
func TestDropReadingKeepsTheLastRoundOfTheTick(t *testing.T) {
	core := &fakeFeederCore{records: drainedRecords(2 * feedDrainLimit)}
	feeder := NewHookFeederWithUpdates(core, "wxapi", nil, nil, nil, nil)
	core.mu.Lock()
	core.onDrain = func() {
		core.mu.Lock()
		defer core.mu.Unlock()
		// 第 1 轮报 0，第 2 轮报 4（页面计数器是累计值，只会涨）。
		core.pageDroppedRecord += 4
	}
	core.mu.Unlock()

	feeder.drainOnce(context.Background())

	if core.drainCalls < 2 {
		t.Fatalf("这一页必须续行到第二轮，实际 %d 次 drain", core.drainCalls)
	}
	if stats := feeder.Stats(); stats.PageDroppedRecords != 4 {
		t.Fatalf("续行后写回的不是最后一轮的读数: %+v", stats)
	}
}

// ---------- F3 有界分页 ----------

// wxapi.poll answers one bounded page per call: the panel asks for 500 records
// and cannot be handed the whole buffer (up to maxPendingRecords) in one
// multi-megabyte payload. The order and the consume semantics are unchanged -
// what a page took is gone from the buffer.
func TestPollLimitTakesBoundedPages(t *testing.T) {
	feeder := NewHookFeeder(&fakeFeederCore{}, "wxapi", nil, nil)
	seed := func() {
		for i := 0; i < 5; i++ {
			feeder.pushPending(map[string]any{"rid": fmt.Sprintf("rid-%d", i)})
		}
	}
	for _, test := range []struct {
		name  string
		limit int
		want  int
	}{
		{"零钳到最小值", 0, 1},
		{"负数钳到最小值", -7, 1},
		{"取一页", 2, 2},
		{"超过缓冲", PollDefaultLimit, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			seed()
			records, err := feeder.PollLimit(test.limit)
			if err != nil {
				t.Fatalf("poll: %v", err)
			}
			if len(records) != test.want {
				t.Fatalf("limit=%d 返回 %d 条, want %d", test.limit, len(records), test.want)
			}
			if records[0]["rid"] != "rid-0" {
				t.Fatalf("分页必须保持顺序: %#v", records[0])
			}
			rest, err := feeder.Poll()
			if err != nil || len(rest) != 5-test.want {
				t.Fatalf("取走的记录必须已消费: 剩 %d 条 (%v), want %d", len(rest), err, 5-test.want)
			}
		})
	}
}

// The upper clamp is a hard ceiling, not a suggestion: one poll must never
// return more than pollMaxLimit records however large a limit the caller sends.
func TestPollLimitClampsToTheHardCeiling(t *testing.T) {
	feeder := NewHookFeeder(&fakeFeederCore{}, "wxapi", nil, nil)
	for i := 0; i < pollMaxLimit+100; i++ {
		feeder.pushPending(map[string]any{"rid": fmt.Sprintf("rid-%d", i)})
	}
	records, err := feeder.PollLimit(1 << 20)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(records) != pollMaxLimit {
		t.Fatalf("超大的 limit 必须钳到 %d, got %d", pollMaxLimit, len(records))
	}
}

// An empty buffer still answers with an array on the paginated path: a null
// payload breaks every consumer that iterates the poll result.
func TestPollLimitAnswersArrayWhenEmpty(t *testing.T) {
	feeder := NewHookFeeder(&fakeFeederCore{}, "wxapi", nil, nil)
	for _, limit := range []int{0, -1, 1, PollDefaultLimit, 1 << 20} {
		records, err := feeder.PollLimit(limit)
		if err != nil {
			t.Fatalf("poll limit=%d: %v", limit, err)
		}
		if records == nil || len(records) != 0 {
			t.Fatalf("空缓冲必须回 []，limit=%d got %#v", limit, records)
		}
	}
}

// A partial take must keep the index usable: the records that were handed out
// may not be patched through a stale entry (they are the panel's now), while
// the ones still buffered must stay patchable.
func TestPollLimitRebuildsThePendingIndex(t *testing.T) {
	feeder := NewHookFeederWithUpdates(&fakeFeederCore{}, "wxapi", nil, nil, nil, nil)
	taken := map[string]any{"rid": "taken", "status": "pending"}
	kept := map[string]any{"rid": "kept", "status": "pending"}
	feeder.pushPending(taken)
	feeder.pushPending(kept)
	if records, _ := feeder.PollLimit(1); len(records) != 1 || records[0]["rid"] != "taken" {
		t.Fatalf("poll must take the oldest record: %#v", records)
	}

	feeder.mu.Lock()
	patched := feeder.patchPending([]DrainedUpdate{
		{Seq: 1, Update: map[string]any{"rid": "taken", "status": "success"}},
		{Seq: 2, Update: map[string]any{"rid": "kept", "status": "success"}},
	})
	feeder.mu.Unlock()

	if len(patched) != 2 {
		t.Fatalf("两帧都必须投递: %#v", patched)
	}
	if taken["status"] != "pending" {
		t.Fatalf("已取走的记录不得再被补打: %#v", taken)
	}
	if kept["status"] != "success" {
		t.Fatalf("仍在缓冲里的记录必须可被补打: %#v", kept)
	}
}

// ---------- F4 去重 FIFO 容量 ----------

// recordBufferPatternFor 匹配 core/hooks/wxapi.js 里的容量常量定义。
func recordBufferPatternFor(name string) *regexp.Regexp {
	return regexp.MustCompile(name + `\s*=\s*(\d+)`)
}

// pageBufferCapacity 从钩子源码解析指定的容量常量（R13）。两侧的容量镜像不变量
// 必须跨层读取：Go 侧硬编码 5000 时，把页面缓冲改成 9000 这套测试照样全绿，而不
// 变量已经破了（deliveredCapacity 不再覆盖页面缓冲的重投递）。解析失败直接 Fatal：
// 静默跳过等于这条不变量根本没被守住。
func pageBufferCapacity(t *testing.T, name string) int {
	t.Helper()
	source, err := os.ReadFile(wxapiHookPath(t))
	if err != nil {
		t.Fatalf("读取 core/hooks/wxapi.js: %v", err)
	}
	match := recordBufferPatternFor(name).FindSubmatch(source)
	if match == nil {
		t.Fatalf("core/hooks/wxapi.js 里没有 %s = <数值> 的定义", name)
	}
	capacity, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatalf("%s 不是整数: %v", name, err)
	}
	return capacity
}

// wxapiHookPath 定位 core/hooks/wxapi.js。go test 的工作目录是包目录，从那里向上
// 找到第一级含该文件的目录（仓库根）；找不到就 Fatal，不静默跳过。
func wxapiHookPath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "core", "hooks", "wxapi.js")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("从测试的工作目录向上找不到 core/hooks/wxapi.js")
		}
		dir = parent
	}
}

// The dedup FIFO exists to cover every entry the page buffers may replay after
// ResetAck. A replay re-reads **both** streams from zero, so the window is the
// record buffer plus the update buffer (core/hooks/wxapi.js, parsed from the
// source - R13); shrinking below that sum makes replayed records reach the poll
// buffer and the capture events a second time, and replayed updates re-apply.
func TestDeliveredFIFOCoversThePageBuffer(t *testing.T) {
	pageRecordCapacity := pageBufferCapacity(t, "RECORD_BUFFER_CAPACITY")
	pageUpdateCapacity := pageBufferCapacity(t, "UPDATE_BUFFER_CAPACITY")
	if deliveredCapacity <= pageRecordCapacity {
		t.Fatalf("deliveredCapacity = %d 必须大于页面记录缓冲容量 %d", deliveredCapacity, pageRecordCapacity)
	}
	if deliveredCapacity <= pageRecordCapacity+pageUpdateCapacity {
		t.Fatalf("deliveredCapacity = %d 必须覆盖两条流的重投窗口：记录缓冲 %d + 更新缓冲 %d = %d",
			deliveredCapacity, pageRecordCapacity, pageUpdateCapacity, pageRecordCapacity+pageUpdateCapacity)
	}
	feeder := &HookFeeder{}
	deliver := func(seq int64) bool {
		return feeder.markDelivered(DrainedRecord{Seq: seq, Record: map[string]any{"ts": float64(seq)}})
	}
	for seq := int64(1); seq <= int64(pageRecordCapacity); seq++ {
		if !deliver(seq) {
			t.Fatalf("首次投递的 seq %d 被判成重复投递", seq)
		}
	}
	if deliver(1) {
		t.Fatal("页面缓冲里可能被重投递的最旧记录必须仍被记住")
	}
	// Only after the FIFO wrapped past it may that record be delivered again.
	for seq := int64(pageRecordCapacity + 1); seq <= deliveredCapacity+1; seq++ {
		deliver(seq)
	}
	if !deliver(1) {
		t.Fatal("被 FIFO 淘汰的记录应可再次投递")
	}
}

// ---------- R2 更新流续行 ----------

func settledUpdates(count int) []DrainedUpdate {
	updates := make([]DrainedUpdate, 0, count)
	for seq := 1; seq <= count; seq++ {
		updates = append(updates, DrainedUpdate{Seq: int64(seq), Update: map[string]any{
			"seq": int64(seq), "rid": fmt.Sprintf("rid-%d", seq), "status": "success",
		}})
	}
	return updates
}

// One round carries at most feedUpdateLimit frames while the page-side update
// buffer drops its oldest once full: a settled burst larger than one round must
// keep being fetched within the same tick, or updateAck never catches up and the
// affected rows stay pending in storage forever.
func TestUpdateStreamDrainsWholeRoundsWithinOneTick(t *testing.T) {
	total := 3 * feedUpdateLimit
	core := &fakeFeederCore{updates: settledUpdates(total)}
	applied := 0
	var batches [][]map[string]any
	feeder := NewHookFeederWithUpdates(core, "wxapi", nil, nil,
		func(_ context.Context, batch []DrainedUpdate) error { applied += len(batch); return nil },
		func(frames []map[string]any) { batches = append(batches, frames) })

	feeder.drainOnce(context.Background())

	if applied != total {
		t.Fatalf("单次 drainOnce 只应用了 %d 帧, want %d", applied, total)
	}
	if stats := feeder.Stats(); stats.UpdateAck != int64(total) {
		t.Fatalf("updateAck 未推进到末位: %+v", stats)
	}
	core.mu.Lock()
	asked := core.lastUpdateSeq
	core.mu.Unlock()
	if asked != int64(total) {
		t.Fatalf("下一轮必须从末位续取, asked from %d", asked)
	}
	// F2：续行的各轮并进同一批，一个 tick 每流仍只发一次事件。
	if len(batches) != 1 || len(batches[0]) != total {
		t.Fatalf("续行的更新帧必须合成一批: %d 批", len(batches))
	}
}

// The continuation is bounded per tick: a burst larger than the round budget
// leaves the rest to the next tick instead of holding the loop.
func TestUpdateStreamRoundsAreBoundedPerTick(t *testing.T) {
	total := (maxUpdateRounds + 1) * feedUpdateLimit
	core := &fakeFeederCore{updates: settledUpdates(total)}
	applied := 0
	feeder := NewHookFeederWithUpdates(core, "wxapi", nil, nil,
		func(_ context.Context, batch []DrainedUpdate) error { applied += len(batch); return nil }, nil)

	feeder.drainOnce(context.Background())

	want := maxUpdateRounds * feedUpdateLimit
	if applied != want {
		t.Fatalf("单 tick 应用 %d 帧, want %d（其余留给下一 tick）", applied, want)
	}
	if stats := feeder.Stats(); stats.UpdateAck != int64(want) {
		t.Fatalf("updateAck = %d, want %d", stats.UpdateAck, want)
	}
	core.mu.Lock()
	rounds := core.drainCalls
	core.mu.Unlock()
	if rounds != maxUpdateRounds {
		t.Fatalf("取数往返 %d 次, want %d", rounds, maxUpdateRounds)
	}

	// The next tick picks the burst up where it stopped.
	feeder.drainOnce(context.Background())
	if applied != total {
		t.Fatalf("下一 tick 未续取剩余的更新帧: %d", applied)
	}
}

// ---------- R9 记录流续行的轮数预算 ----------

// drainedRecords builds count records with distinct capture times and rids (the
// delivery identity the dedup FIFO needs).
func drainedRecords(count int) []DrainedRecord {
	records := make([]DrainedRecord, 0, count)
	for seq := 1; seq <= count; seq++ {
		records = append(records, DrainedRecord{Seq: int64(seq), Record: map[string]any{
			"ts": float64(1000 + seq), "rid": fmt.Sprintf("rid-%d", seq), "status": "pending",
		}})
	}
	return records
}

// 记录流的续行与更新流同形，也必须按轮数封顶：页面在 500ms 里积压超过一轮的量时，
// 没有预算的实现会把整页（最多 5000 条）合成一个事件投出去。前端 createBatchQueue
// 的 enqueue 是同步循环，在 120ms 的 flush 之前就把最旧的一批丢掉 —— 而这些记录已经
// 从 shell 的 pending 里消费掉了，2s 后的 poll 只会把它们追加到列表尾部，随后被
// ITEM_LIMIT 裁掉的反而是最新的一批。单 tick 上界 = maxRecordRounds × feedDrainLimit。
func TestRecordStreamRoundsAreBoundedPerTick(t *testing.T) {
	// 3 倍单 tick 上界：足够看清「只投一批、且只投预算内的量」。
	total := 3 * maxRecordRounds * feedDrainLimit
	core := &fakeFeederCore{records: drainedRecords(total)}
	var batches [][]map[string]any
	feeder := NewHookFeederWithUpdates(core, "wxapi",
		func(batch []map[string]any) { batches = append(batches, batch) },
		nil, nil, nil)

	feeder.drainOnce(context.Background())

	want := maxRecordRounds * feedDrainLimit
	if len(batches) != 1 {
		t.Fatalf("整 tick 只允许一次事件, got %d 批", len(batches))
	}
	if len(batches[0]) != want {
		t.Fatalf("单 tick 投递 %d 条, want %d（其余留给下一 tick）", len(batches[0]), want)
	}
	for i, record := range batches[0] {
		if record["rid"] != fmt.Sprintf("rid-%d", i+1) {
			t.Fatalf("批次内的顺序必须与页面一致: 第 %d 条是 %v", i, record["rid"])
		}
	}
	if stats := feeder.Stats(); stats.Ack != int64(want) {
		t.Fatalf("ack = %d, want %d", stats.Ack, want)
	}
	core.mu.Lock()
	rounds := core.drainCalls
	core.mu.Unlock()
	if rounds != maxRecordRounds {
		t.Fatalf("取数往返 %d 次, want %d（每轮推进 ack，不会自旋）", rounds, maxRecordRounds)
	}

	// 下一 tick 从末位续取，同样受预算限制。
	feeder.drainOnce(context.Background())
	if len(batches) != 2 || len(batches[1]) != want {
		t.Fatalf("下一 tick 必须续取一批 %d 条, got %d 批", want, len(batches))
	}
	if first := batches[1][0]["rid"]; first != fmt.Sprintf("rid-%d", want+1) {
		t.Fatalf("下一 tick 未从末位续取: 首条 %v, want rid-%d", first, want+1)
	}
	if stats := feeder.Stats(); stats.Ack != int64(2*want) {
		t.Fatalf("续取后 ack = %d, want %d", stats.Ack, 2*want)
	}
}

// ---------- R5 Clear 作废在途页 ----------

// Clear landing while a drain round-trip is in flight must void that page: the
// records the user just cleared would otherwise be written back into the buffer
// and delivered again ("清空的记录复活").
func TestClearVoidsInFlightDrainPage(t *testing.T) {
	core := &fakeFeederCore{
		nextSeq: 7,
		records: []DrainedRecord{{Seq: 7, Record: map[string]any{"ts": float64(1000), "rid": "r7"}}},
	}
	batches := 0
	feeder := NewHookFeeder(core, "wxapi", func(map[string]any) { batches++ }, nil)
	core.mu.Lock()
	core.onDrain = func() { feeder.Clear() }
	core.mu.Unlock()

	feeder.drainOnce(context.Background())

	if records, _ := feeder.Poll(); len(records) != 0 {
		t.Fatalf("Clear 之后到达的旧页重新填充了缓冲: %#v", records)
	}
	if batches != 0 {
		t.Fatalf("旧页发了 %d 次事件, want 0", batches)
	}
	if stats := feeder.Stats(); stats.Ack != 0 {
		t.Fatalf("旧页推进了 ack: %+v", stats)
	}
}

// The same page arriving after the ingest round-trip (Clear landed under it) is
// voided by the post-ingest generation re-check.
func TestClearDuringIngestDropsStalePage(t *testing.T) {
	core := &fakeFeederCore{
		nextSeq: 7,
		records: []DrainedRecord{{Seq: 7, Record: map[string]any{"ts": float64(1000), "rid": "r7"}}},
	}
	feeder := NewHookFeeder(core, "wxapi", nil, nil)
	feeder.ingest = func(context.Context, []DrainedRecord) error {
		feeder.Clear()
		return nil
	}

	feeder.drainOnce(context.Background())

	if records, _ := feeder.Poll(); len(records) != 0 {
		t.Fatalf("Clear 之后到达的旧页重新填充了缓冲: %#v", records)
	}
	if stats := feeder.Stats(); stats.Ack != 0 || stats.UpdateAck != 0 {
		t.Fatalf("旧页推进了游标: %+v", stats)
	}
}

// Clear is not a reinstall: the ack keeps its position (already-drained records
// stay acknowledged, they must not be drained a second time), only the drop
// counter restarts - the user emptied the buffer on purpose.
func TestClearKeepsAckAndResetsTheDropCounter(t *testing.T) {
	feeder := NewHookFeeder(&fakeFeederCore{}, "wxapi", nil, nil)
	for i := 0; i < maxPendingRecords+3; i++ {
		feeder.pushPending(map[string]any{"rid": fmt.Sprintf("rid-%d", i)})
	}
	if stats := feeder.Stats(); stats.Dropped != 3 {
		t.Fatalf("缓冲越界必须计数: %+v", stats)
	}
	feeder.mu.Lock()
	feeder.ack, feeder.updateAck = 42, 41
	feeder.mu.Unlock()

	feeder.Clear()

	stats := feeder.Stats()
	if stats.Pending != 0 || stats.Dropped != 0 {
		t.Fatalf("Clear 必须清空缓冲并把丢弃计数归零: %+v", stats)
	}
	if stats.Ack != 42 || stats.UpdateAck != 41 {
		t.Fatalf("Clear 不得重置游标: %+v", stats)
	}
}

// ---------- R10(a) 作废轮次的批不得投递 ----------

// Clear 落在多轮收集之间时，这一 tick 早前几轮已经收进批里的记录同样不能投递：
// 用户点了「清空」，面板不能在这一 tick 结束时又收到清空前的记录（defer deliver 无条件
// 投递的正是这一批）。对 ResetAck 也安全：同一批记录会从 seq 0 重新 drain 并投递。
func TestClearBetweenRoundsDropsTheCollectedBatch(t *testing.T) {
	total := 3 * feedDrainLimit
	core := &fakeFeederCore{records: drainedRecords(total)}
	var batches [][]map[string]any
	feeder := NewHookFeederWithUpdates(core, "wxapi",
		func(batch []map[string]any) { batches = append(batches, batch) },
		nil, nil, nil)
	// 第 2 轮取数时用户点了清空：第 1 轮的 200 条已经在批里，其余还没取。
	core.mu.Lock()
	core.onDrain = func() {
		core.mu.Lock()
		calls := core.drainCalls
		core.mu.Unlock()
		if calls == 2 {
			feeder.Clear()
		}
	}
	core.mu.Unlock()

	feeder.drainOnce(context.Background())

	if len(batches) != 0 {
		t.Fatalf("作废轮次收集的批被投递了: %d 批, 首批 %d 条", len(batches), len(batches[0]))
	}
	if records, _ := feeder.Poll(); len(records) != 0 {
		t.Fatalf("Clear 之后旧记录重新进入了缓冲: %#v", records)
	}
}

// ResetAck 落在多轮收集之间时，被作废的那一批不投递，但也不会丢：已经收进缓冲的
// 前几轮留在缓冲里（下一次 poll 照常读到），下一 tick 从 seq 0 重新 drain 时它们被
// 去重 FIFO 认定成已投递而跳过，批里只剩这一 tick 没来得及收的那部分 —— 作废是延迟，
// 不是丢失，也不会重复投递。
func TestResetAckBetweenRoundsRedeliversTheBatch(t *testing.T) {
	total := 2 * feedDrainLimit
	core := &fakeFeederCore{records: drainedRecords(total)}
	var batches [][]map[string]any
	feeder := NewHookFeederWithUpdates(core, "wxapi",
		func(batch []map[string]any) { batches = append(batches, batch) },
		nil, nil, nil)
	core.mu.Lock()
	core.onDrain = func() {
		core.mu.Lock()
		calls := core.drainCalls
		core.mu.Unlock()
		if calls == 2 {
			feeder.ResetAck()
		}
	}
	core.mu.Unlock()

	feeder.drainOnce(context.Background())
	if len(batches) != 0 {
		t.Fatalf("作废轮次收集的批被投递了: %d 批", len(batches))
	}
	if records, _ := feeder.Poll(); len(records) != feedDrainLimit {
		t.Fatalf("作废不得丢掉已收进缓冲的记录: %d 条, want %d", len(records), feedDrainLimit)
	}

	core.mu.Lock()
	core.onDrain = nil
	core.mu.Unlock()
	feeder.drainOnce(context.Background())
	if len(batches) != 1 || len(batches[0]) != feedDrainLimit {
		t.Fatalf("重新 drain 后必须补投未收集的那一轮: %d 批", len(batches))
	}
	for i, record := range batches[0] {
		if want := fmt.Sprintf("rid-%d", feedDrainLimit+i+1); record["rid"] != want {
			t.Fatalf("重投的批顺序被改变: 第 %d 条是 %v, want %s", i, record["rid"], want)
		}
	}
}
