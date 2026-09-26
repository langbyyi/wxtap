package traffic

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// settledFrame is what the page's update stream hands the store: a rid, the
// settled status, the settled response body and the call duration. The record
// it describes was already inserted as pending by the record stream.
func settledFrame(id string, status Status, body []byte, durationMs int64) Record {
	return Record{ID: id, Status: status, ResponseBody: body, DurationMs: durationMs}
}

func TestListReturnsDurationMs(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

	record := synthRecord(0, base)
	record.Status = StatusPending
	record.ResponseBody = nil
	if _, err := repo.InsertBatch(ctx, []Record{record}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// A pending record lists with duration 0 rather than a missing field.
	page, err := repo.List(ctx, ListFilter{PageSize: 10})
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].DurationMs != 0 {
		t.Fatalf("pending summary = %#v, want durationMs 0", page.Items)
	}

	if _, err := repo.ApplyUpdates(ctx, []Record{settledFrame(record.ID, StatusSuccess, []byte(`{"ok":true}`), 1830)}); err != nil {
		t.Fatalf("apply updates: %v", err)
	}

	page, err = repo.List(ctx, ListFilter{PageSize: 10})
	if err != nil {
		t.Fatalf("list settled: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("list returned %d items, want 1", len(page.Items))
	}
	summary := page.Items[0]
	if summary.ID != record.ID || summary.Seq != record.Seq {
		t.Fatalf("summary identity changed: %#v", summary)
	}
	if summary.DurationMs != 1830 {
		t.Fatalf("durationMs = %d, want 1830", summary.DurationMs)
	}
	if summary.Status != StatusSuccess {
		t.Fatalf("status = %q, want success", summary.Status)
	}
	if summary.ResponseBytes != int64(len(`{"ok":true}`)) {
		t.Fatalf("responseBytes = %d, want %d", summary.ResponseBytes, len(`{"ok":true}`))
	}
}

// ApplyUpdates writes the settled response side only: the request side and the
// capture instant were committed by the pending insert and are evidence the
// late frame must not overwrite.
func TestApplyUpdatesLeavesRequestSideUntouched(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()

	base := time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC)
	record := Record{
		ID: "wx.request-wxone-1700000000000-7", Seq: 7, CapturedAt: base,
		APIType: "wx.request", Name: "request", AppID: "wxone", Method: "POST",
		URL: "https://api.example.com/slow", Status: StatusPending,
		RequestBody: []byte(`{"id":7}`),
	}
	if _, err := repo.InsertBatch(ctx, []Record{record}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	frame := settledFrame(record.ID, StatusFail, []byte(`{"error":"request:fail timeout"}`), 1830)
	// Poison every request-side field: the update statement must ignore them.
	frame.Seq = 999
	frame.CapturedAt = base.Add(24 * time.Hour)
	frame.APIType = "tampered"
	frame.Name = "tampered"
	frame.AppID = "tampered"
	frame.Method = "DELETE"
	frame.URL = "https://tampered.example.com"
	frame.RequestBody = []byte(`{"tampered":true}`)

	updated, err := repo.ApplyUpdates(ctx, []Record{frame})
	if err != nil {
		t.Fatalf("apply updates: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated %d rows, want 1", updated)
	}

	page, err := repo.List(ctx, ListFilter{PageSize: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("list returned %d items, want 1", len(page.Items))
	}
	summary := page.Items[0]
	if summary.Seq != 7 || summary.APIType != "wx.request" || summary.Name != "request" {
		t.Fatalf("request-side identity changed: %#v", summary)
	}
	if summary.Method != "POST" || summary.URL != "https://api.example.com/slow" {
		t.Fatalf("request-side fields changed: %#v", summary)
	}
	if summary.CapturedAt != base.Format(time.RFC3339Nano) {
		t.Fatalf("capturedAt = %s, want %s", summary.CapturedAt, base.Format(time.RFC3339Nano))
	}
	if summary.Status != StatusFail || summary.DurationMs != 1830 {
		t.Fatalf("settled fields not written: %#v", summary)
	}

	requestBody, err := repo.GetBody(ctx, record.ID, PartRequest)
	if err != nil {
		t.Fatalf("get request body: %v", err)
	}
	if string(requestBody) != string(record.RequestBody) {
		t.Fatalf("request body overwritten: %q", requestBody)
	}
	responseBody, err := repo.GetBody(ctx, record.ID, PartResponse)
	if err != nil {
		t.Fatalf("get response body: %v", err)
	}
	if string(responseBody) != `{"error":"request:fail timeout"}` {
		t.Fatalf("response body = %q", responseBody)
	}

	// captured_at survives too: the row is still the newest by capture order
	// and still carries its original appid.
	recent, err := repo.RecentRecordsAllApps(ctx, 10)
	if err != nil {
		t.Fatalf("recent records: %v", err)
	}
	if len(recent) != 1 || !recent[0].CapturedAt.Equal(base) || recent[0].AppID != "wxone" {
		t.Fatalf("captured_at/appid changed: %#v", recent)
	}
}

// Settled bodies go through the same compression as inserted ones, so a large
// response must round trip out of GetBody.
func TestApplyUpdatesCompressesLargeResponseBody(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	record := synthRecord(0, base)
	record.Status = StatusPending
	record.ResponseBody = nil
	if _, err := repo.InsertBatch(ctx, []Record{record}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	large := bytes.Repeat([]byte("payload-"), 512) // 4KiB, gzip-worthy
	if _, err := repo.ApplyUpdates(ctx, []Record{settledFrame(record.ID, StatusSuccess, large, 42)}); err != nil {
		t.Fatalf("apply updates: %v", err)
	}

	got, err := repo.GetBody(ctx, record.ID, PartResponse)
	if err != nil {
		t.Fatalf("get response body: %v", err)
	}
	if !bytes.Equal(got, large) {
		t.Fatalf("large response body did not round trip: %d bytes", len(got))
	}
}

// A frame can outrun the insert it belongs to (the page buffer drains ahead of
// the store); matching no row is not an error.
func TestApplyUpdatesIgnoresUnmatchedIDs(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 23, 13, 0, 0, 0, time.UTC)

	record := synthRecord(0, base)
	record.Status = StatusPending
	if _, err := repo.InsertBatch(ctx, []Record{record}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	updated, err := repo.ApplyUpdates(ctx, []Record{
		settledFrame("ghost-record-nobody-inserted", StatusSuccess, []byte(`{"ok":true}`), 12),
		settledFrame(record.ID, StatusSuccess, []byte(`{"ok":true}`), 12),
	})
	if err != nil {
		t.Fatalf("unmatched id must not fail: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated %d rows, want 1", updated)
	}

	page, err := repo.List(ctx, ListFilter{PageSize: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != record.ID || page.Items[0].DurationMs != 12 {
		t.Fatalf("unexpected rows after unmatched update: %#v", page.Items)
	}

	updated, err = repo.ApplyUpdates(ctx, nil)
	if err != nil || updated != 0 {
		t.Fatalf("empty batch = %d, %v; want 0, nil", updated, err)
	}
}

// The update stream has its own cursor: a frame replayed after a lost ack must
// leave the same state behind.
func TestApplyUpdatesIsIdempotentOnReplay(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 23, 14, 0, 0, 0, time.UTC)

	record := synthRecord(0, base)
	record.Status = StatusPending
	record.ResponseBody = nil
	if _, err := repo.InsertBatch(ctx, []Record{record}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	frame := settledFrame(record.ID, StatusSuccess, []byte(`{"ok":true}`), 1830)
	first, err := repo.ApplyUpdates(ctx, []Record{frame})
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if first != 1 {
		t.Fatalf("first apply updated %d rows, want 1", first)
	}

	second, err := repo.ApplyUpdates(ctx, []Record{frame})
	if err != nil {
		t.Fatalf("replay apply: %v", err)
	}
	if second != 1 {
		t.Fatalf("replay updated %d rows, want 1 (SQLite counts matched rows)", second)
	}

	page, err := repo.List(ctx, ListFilter{PageSize: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("replay duplicated the record: %d rows", len(page.Items))
	}
	if page.Items[0].Status != StatusSuccess || page.Items[0].DurationMs != 1830 || page.Items[0].ResponseBytes != int64(len(`{"ok":true}`)) {
		t.Fatalf("replay changed the stored state: %#v", page.Items[0])
	}
}

// summaryByID returns the listed summary of one stored record.
func summaryByID(t *testing.T, repo *Repository, id string) TrafficSummary {
	t.Helper()
	page, err := repo.List(context.Background(), ListFilter{PageSize: MaxListLimit})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, item := range page.Items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("record %s is not listed", id)
	return TrafficSummary{}
}

// R6(b): a settle frame can carry the terminal status and the call duration
// without the response payload (the update stream may deliver those first, or
// the call may have no result/error the hook could report). Such a frame must
// settle status and duration and leave the stored response alone — overwriting
// it with the frame's absent body would blank a response the record stream had
// already committed, silently and irreversibly.
func TestApplyUpdatesKeepsStoredResponseWhenFrameCarriesNone(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)

	small := synthRecord(0, base)
	small.Status = StatusPending
	small.ResponseBody = []byte(`{"stored":true}`) // 15 bytes, stored raw

	large := synthRecord(1, base)
	large.Status = StatusPending
	large.ResponseBody = bytes.Repeat([]byte("payload-"), 512) // 4KiB, stored gzipped

	if _, err := repo.InsertBatch(ctx, []Record{small, large}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The shape settledTrafficRecord builds for a status-only update frame:
	// rid, status and durationMs, with neither result nor error → nil body.
	updated, err := repo.ApplyUpdates(ctx, []Record{
		{ID: small.ID, Status: StatusSuccess, DurationMs: 1830},
		{ID: large.ID, Status: StatusFail, DurationMs: 41},
	})
	if err != nil {
		t.Fatalf("apply status-only frames: %v", err)
	}
	if updated != 2 {
		t.Fatalf("updated %d rows, want 2", updated)
	}

	for _, want := range []struct {
		record    Record
		status    Status
		duration  int64
		wantBytes int64
	}{
		{small, StatusSuccess, 1830, int64(len(small.ResponseBody))},
		{large, StatusFail, 41, int64(len(large.ResponseBody))},
	} {
		summary := summaryByID(t, repo, want.record.ID)
		if summary.Status != want.status || summary.DurationMs != want.duration {
			t.Fatalf("record %s settled fields = %#v, want status %q duration %d",
				want.record.ID, summary, want.status, want.duration)
		}
		if summary.ResponseBytes != want.wantBytes {
			t.Fatalf("record %s responseBytes = %d after a status-only frame, want the stored %d",
				want.record.ID, summary.ResponseBytes, want.wantBytes)
		}
		body, err := repo.GetBody(ctx, want.record.ID, PartResponse)
		if err != nil {
			t.Fatalf("get response body %s: %v", want.record.ID, err)
		}
		if !bytes.Equal(body, want.record.ResponseBody) {
			t.Fatalf("record %s stored response body changed: %d bytes, want %d",
				want.record.ID, len(body), len(want.record.ResponseBody))
		}
	}
}

// The other half of R6(b): a frame that does carry a response still overwrites
// the settled fields, large bodies included, through the same compression path
// as an insert.
func TestApplyUpdatesOverwritesStoredResponseWhenFrameCarriesOne(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 23, 16, 0, 0, 0, time.UTC)

	stale := []byte(`{"stale":true}`)            // 14 bytes, stored raw
	fresh := bytes.Repeat([]byte("fresh-"), 512) // 3KiB, gzip-worthy

	record := synthRecord(0, base)
	record.Status = StatusPending
	record.ResponseBody = stale
	if _, err := repo.InsertBatch(ctx, []Record{record}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	updated, err := repo.ApplyUpdates(ctx, []Record{settledFrame(record.ID, StatusSuccess, fresh, 77)})
	if err != nil {
		t.Fatalf("apply update: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated %d rows, want 1", updated)
	}

	summary := summaryByID(t, repo, record.ID)
	if summary.Status != StatusSuccess || summary.DurationMs != 77 {
		t.Fatalf("settled fields = %#v, want success/77", summary)
	}
	if summary.ResponseBytes != int64(len(fresh)) {
		t.Fatalf("responseBytes = %d, want %d", summary.ResponseBytes, len(fresh))
	}
	body, err := repo.GetBody(ctx, record.ID, PartResponse)
	if err != nil {
		t.Fatalf("get response body: %v", err)
	}
	if !bytes.Equal(body, fresh) {
		t.Fatalf("stored response was not overwritten: %d bytes, want %d", len(body), len(fresh))
	}
}

// The boundary R6(b) hangs on: an explicitly empty body is a carried response,
// not an absent one, so it overwrites (response bytes drop to 0) while a nil
// body preserves. Without this, "the call returned nothing" and "this frame
// says nothing about the response" would be the same frame.
func TestApplyUpdatesTreatsExplicitEmptyBodyAsCarriedResponse(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 23, 17, 0, 0, 0, time.UTC)

	record := synthRecord(0, base)
	record.Status = StatusPending
	record.ResponseBody = bytes.Repeat([]byte("payload-"), 512) // 4KiB stored
	if _, err := repo.InsertBatch(ctx, []Record{record}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	frame := settledFrame(record.ID, StatusSuccess, []byte{}, 9)
	updated, err := repo.ApplyUpdates(ctx, []Record{frame})
	if err != nil {
		t.Fatalf("apply empty-body frame: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated %d rows, want 1", updated)
	}

	summary := summaryByID(t, repo, record.ID)
	if summary.ResponseBytes != 0 {
		t.Fatalf("responseBytes = %d after an explicitly empty response, want 0", summary.ResponseBytes)
	}
	if summary.Status != StatusSuccess || summary.DurationMs != 9 {
		t.Fatalf("settled fields = %#v, want success/9", summary)
	}
	body, err := repo.GetBody(ctx, record.ID, PartResponse)
	if err != nil {
		t.Fatalf("get response body: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("stored response body = %q, want empty", body)
	}
}

// The status-only statement tolerates the same non-errors as the full one: a
// frame for a row the store has not seen, and an empty batch.
func TestApplyUpdatesStatusOnlyToleratesUnmatchedIDs(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()

	updated, err := repo.ApplyUpdates(ctx, []Record{
		{ID: "ghost-record-nobody-inserted", Status: StatusSuccess, DurationMs: 12},
	})
	if err != nil {
		t.Fatalf("unmatched status-only frame must not fail: %v", err)
	}
	if updated != 0 {
		t.Fatalf("updated %d rows for an unmatched status-only frame, want 0", updated)
	}

	updated, err = repo.ApplyUpdates(ctx, nil)
	if err != nil || updated != 0 {
		t.Fatalf("empty batch = %d, %v; want 0, nil", updated, err)
	}
}

// Rows written before the update stream existed keep their data across the
// 005 migration and gain the column with its 0 default.
func TestMigration005AddsDurationToExistingRows(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "traffic.db")

	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	_, err = seed.Exec(`
		CREATE TABLE traffic_records (
			id             TEXT PRIMARY KEY,
			seq            INTEGER NOT NULL,
			captured_at    INTEGER NOT NULL,
			api_type       TEXT NOT NULL,
			name           TEXT NOT NULL,
			appid          TEXT NOT NULL DEFAULT '',
			method         TEXT NOT NULL DEFAULT '',
			url            TEXT NOT NULL DEFAULT '',
			status         TEXT NOT NULL CHECK (status IN ('pending', 'success', 'fail')),
			request_bytes  INTEGER NOT NULL DEFAULT 0,
			response_bytes INTEGER NOT NULL DEFAULT 0,
			request_body   BLOB,
			response_body  BLOB,
			UNIQUE (appid, api_type, captured_at, seq)
		);
		CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL);
		INSERT INTO schema_migrations VALUES
			('001_traffic.sql', 1), ('002_traffic_unique_type.sql', 1),
			('003_traffic_appid.sql', 1), ('004_traffic_appid_unique.sql', 1);
		INSERT INTO traffic_records (id, seq, captured_at, api_type, name, appid, url, status)
			VALUES ('keep-me', 1, 1789776000000000000, 'wx.request', 'request', 'wxone', 'https://wx.qq.com/x', 'pending');
	`)
	if err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	repo, err := Open(dbPath, filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("reopen with migrations: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	page, err := repo.List(ctx, ListFilter{PageSize: 10})
	if err != nil {
		t.Fatalf("list after migration: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "keep-me" {
		t.Fatalf("pre-existing rows did not survive the migration: %#v", page.Items)
	}
	if page.Items[0].DurationMs != 0 {
		t.Fatalf("pre-existing row durationMs = %d, want the 0 default", page.Items[0].DurationMs)
	}

	// The new column is writable on rows the migration backfilled.
	updated, err := repo.ApplyUpdates(ctx, []Record{settledFrame("keep-me", StatusSuccess, []byte(`{"ok":true}`), 902)})
	if err != nil {
		t.Fatalf("apply updates after migration: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated %d rows, want 1", updated)
	}
	page, err = repo.List(ctx, ListFilter{PageSize: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Items[0].DurationMs != 902 || page.Items[0].Status != StatusSuccess {
		t.Fatalf("settled fields not written to a migrated row: %#v", page.Items[0])
	}
}
