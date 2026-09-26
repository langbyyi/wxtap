package traffic

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func openTestRepo(t *testing.T) *Repository {
	t.Helper()
	repo, err := Open(filepath.Join(t.TempDir(), "traffic.db"), filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// The macOS data directory is ~/Library/Application Support/WxTap, so the
// database path contains a space and reaches SQLite inside a file: URI. A path
// that breaks the URI there fails on every mac and on no other platform, which
// is exactly the kind of breakage the Windows-only test suite cannot see.
func TestOpenHandlesADataDirectoryWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Application Support", "WxTap")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	repo, err := Open(filepath.Join(dir, "traffic.db"), filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("open repository under a spaced path: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	// Open() only pings and migrates, so also prove a real write lands in the
	// file the spaced path names.
	ctx := context.Background()
	if _, err := repo.InsertBatch(ctx, []Record{synthRecord(1, time.Now())}); err != nil {
		t.Fatalf("insert under a spaced path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "traffic.db")); err != nil {
		t.Fatalf("database was not created at the spaced path: %v", err)
	}
}

func synthRecord(i int, base time.Time) Record {
	return Record{
		ID:           fmt.Sprintf("rec-%05d", i),
		Seq:          int64(i + 1),
		CapturedAt:   base.Add(time.Duration(i) * time.Millisecond),
		APIType:      "wxapi",
		Name:         "request",
		Method:       "POST",
		URL:          "https://api.example.com/path/" + strconv.Itoa(i),
		Status:       StatusSuccess,
		RequestBody:  []byte(`{"n":` + strconv.Itoa(i) + `}`),
		ResponseBody: []byte(`{"ok":` + strconv.Itoa(i) + `}`),
	}
}

func TestRecentRecordsAllAppsSpansAppIDsInOrder(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)

	records := []Record{
		{ID: "a-1", Seq: 1, CapturedAt: base, APIType: "wxapi", Name: "request", AppID: "wx-a", Status: StatusSuccess, RequestBody: []byte(`{"k":"a1"}`)},
		{ID: "b-1", Seq: 2, CapturedAt: base.Add(time.Second), APIType: "wxapi", Name: "request", AppID: "wx-b", Status: StatusSuccess, RequestBody: []byte(`{"k":"b1"}`)},
		{ID: "b-2", Seq: 3, CapturedAt: base.Add(2 * time.Second), APIType: "wxapi", Name: "request", AppID: "wx-b", Status: StatusSuccess, ResponseBody: []byte(`{"k":"b2"}`)},
	}
	if _, err := repo.InsertBatch(ctx, records); err != nil {
		t.Fatalf("insert batch: %v", err)
	}

	got, err := repo.RecentRecordsAllApps(ctx, 2)
	if err != nil {
		t.Fatalf("recent records: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("records = %d, want 2", len(got))
	}
	if got[0].ID != "b-2" || got[1].ID != "b-1" {
		t.Fatalf("order = %s,%s, want b-2,b-1", got[0].ID, got[1].ID)
	}
	// Bodies are returned decompressed for the caller-side scan.
	if string(got[0].ResponseBody) != `{"k":"b2"}` {
		t.Fatalf("response body = %q", got[0].ResponseBody)
	}

	all, err := repo.RecentRecordsAllApps(ctx, 0)
	if err != nil {
		t.Fatalf("recent records (default limit): %v", err)
	}
	if len(all) != len(records) {
		t.Fatalf("default limit returned %d records, want %d", len(all), len(records))
	}
}
func TestListPagesTenThousandRecordsNewestFirst(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)

	records := make([]Record, 0, 10000)
	for i := 0; i < 10000; i++ {
		records = append(records, synthRecord(i, base))
	}
	inserted, err := repo.InsertBatch(ctx, records)
	if err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	if inserted != len(records) {
		t.Fatalf("inserted %d records, want %d", inserted, len(records))
	}

	page, err := repo.List(ctx, ListFilter{PageSize: 100})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if page.Total != 10000 {
		t.Fatalf("total = %d, want 10000", page.Total)
	}
	// The window that took effect is echoed back: a caller that asked for
	// nothing must not have to assume which page it got.
	if page.Page != 1 || page.PageSize != 100 {
		t.Fatalf("echoed window = page %d size %d, want page 1 size 100", page.Page, page.PageSize)
	}
	if len(page.Items) != 100 {
		t.Fatalf("first page has %d items, want 100", len(page.Items))
	}
	// Page 1 is the newest window, and inside it the order is newest first.
	if page.Items[0].ID != "rec-09999" || page.Items[99].ID != "rec-09900" {
		t.Fatalf("unexpected first-page boundaries: %s..%s", page.Items[0].ID, page.Items[99].ID)
	}

	// Walk every page. OFFSET needs a total order: if two records compared equal
	// a page boundary could land between them and one of the pair would either
	// repeat or vanish.
	seen := make(map[string]bool, 10000)
	lastSeq := int64(math.MaxInt64)
	for pageNumber := 1; pageNumber <= 100; pageNumber++ {
		current, err := repo.List(ctx, ListFilter{Page: pageNumber, PageSize: 100})
		if err != nil {
			t.Fatalf("page %d: %v", pageNumber, err)
		}
		if current.Page != pageNumber {
			t.Fatalf("page %d echoed as %d", pageNumber, current.Page)
		}
		if len(current.Items) != 100 {
			t.Fatalf("page %d has %d items, want 100", pageNumber, len(current.Items))
		}
		for _, item := range current.Items {
			if seen[item.ID] {
				t.Fatalf("duplicate record %s across pages", item.ID)
			}
			seen[item.ID] = true
			if item.Seq >= lastSeq {
				t.Fatalf("sequence went forwards: %d after %d", item.Seq, lastSeq)
			}
			lastSeq = item.Seq
		}
	}
	if len(seen) != 10000 {
		t.Fatalf("walked %d records in total, want 10000", len(seen))
	}
	// The oldest record is the last row of the last page.
	lastPage, err := repo.List(ctx, ListFilter{Page: 100, PageSize: 100})
	if err != nil {
		t.Fatalf("last page: %v", err)
	}
	if lastPage.Items[99].ID != "rec-00000" {
		t.Fatalf("last row = %s, want rec-00000", lastPage.Items[99].ID)
	}

	// A page past the end is empty, not an error, and still reports the real
	// total so the caller can clamp its own page number back into range.
	beyond, err := repo.List(ctx, ListFilter{Page: 101, PageSize: 100})
	if err != nil {
		t.Fatalf("page past the end: %v", err)
	}
	if len(beyond.Items) != 0 || beyond.Total != 10000 || beyond.Page != 101 {
		t.Fatalf("page 101 = %d items, total %d, page %d", len(beyond.Items), beyond.Total, beyond.Page)
	}
}

func TestListNeverLoadsBodyColumns(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)

	record := synthRecord(0, base)
	if _, err := repo.InsertBatch(ctx, []Record{record}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	page, err := repo.List(ctx, ListFilter{PageSize: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("list returned %d items, want 1", len(page.Items))
	}
	summary := page.Items[0]
	if summary.RequestBytes != int64(len(record.RequestBody)) || summary.ResponseBytes != int64(len(record.ResponseBody)) {
		t.Fatalf("summary byte counts: %d/%d, want %d/%d",
			summary.RequestBytes, summary.ResponseBytes, len(record.RequestBody), len(record.ResponseBody))
	}

	// Structural guard: the list statement must not reference body columns.
	if strings.Contains(strings.ToLower(listSQL), "request_body") || strings.Contains(strings.ToLower(listSQL), "response_body") {
		t.Fatalf("list statement touches body columns: %s", listSQL)
	}

	gotRequest, err := repo.GetBody(ctx, record.ID, PartRequest)
	if err != nil {
		t.Fatalf("get request body: %v", err)
	}
	if string(gotRequest) != string(record.RequestBody) {
		t.Fatalf("request body mismatch: %q", gotRequest)
	}
	gotResponse, err := repo.GetBody(ctx, record.ID, PartResponse)
	if err != nil {
		t.Fatalf("get response body: %v", err)
	}
	if string(gotResponse) != string(record.ResponseBody) {
		t.Fatalf("response body mismatch: %q", gotResponse)
	}
	if _, err := repo.GetBody(ctx, "missing", PartRequest); err == nil {
		t.Fatal("expected an error for a missing record body")
	}
}

func TestListAppliesFilters(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)

	records := []Record{
		{ID: "a", Seq: 1, CapturedAt: base, APIType: "wxapi", Name: "login", URL: "https://wx.qq.com/login", Status: StatusSuccess, ResponseBody: []byte("x")},
		{ID: "b", Seq: 2, CapturedAt: base.Add(time.Millisecond), APIType: "wxapi", Name: "request", URL: "https://api.example.com/data", Status: StatusFail},
		{ID: "c", Seq: 3, CapturedAt: base.Add(2 * time.Millisecond), APIType: "cloud", Name: "callFunction", URL: "https://cloud.tcb.qq.com", Status: StatusPending},
	}
	if _, err := repo.InsertBatch(ctx, records); err != nil {
		t.Fatalf("insert: %v", err)
	}

	cases := []struct {
		name   string
		filter ListFilter
		want   []string
	}{
		{"query matches url", ListFilter{PageSize: 10, Query: "example.com"}, []string{"b"}},
		{"query matches name", ListFilter{PageSize: 10, Query: "LOGIN"}, []string{"a"}},
		{"api type", ListFilter{PageSize: 10, APIType: "cloud"}, []string{"c"}},
		{"status", ListFilter{PageSize: 10, Status: StatusFail}, []string{"b"}},
		{"combined", ListFilter{PageSize: 10, Query: "https://", Status: StatusPending}, []string{"c"}},
		{"no match", ListFilter{PageSize: 10, Query: "nothing-matches"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := repo.List(ctx, tc.filter)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			var got []string
			for _, item := range page.Items {
				got = append(got, item.ID)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The appId filter runs in SQL over the whole store: the frontend used to
// derive the app from the record id and filter only the pages it had already
// loaded, which silently hid every record further down the history.
func TestListFiltersByAppID(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)

	records := []Record{
		{ID: "a-1", Seq: 1, CapturedAt: base, APIType: "wxapi", Name: "one", AppID: "wx-app-1", Status: StatusSuccess},
		{ID: "a-2", Seq: 2, CapturedAt: base.Add(time.Millisecond), APIType: "wxapi", Name: "two", AppID: "wx-app-1", Status: StatusSuccess},
		{ID: "b-1", Seq: 3, CapturedAt: base.Add(2 * time.Millisecond), APIType: "wxapi", Name: "three", AppID: "wx-app-2", Status: StatusSuccess},
		{ID: "none", Seq: 4, CapturedAt: base.Add(3 * time.Millisecond), APIType: "wxapi", Name: "four", Status: StatusSuccess},
	}
	if _, err := repo.InsertBatch(ctx, records); err != nil {
		t.Fatalf("insert: %v", err)
	}

	page, err := repo.List(ctx, ListFilter{PageSize: 10, AppID: "wx-app-1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Newest first, and the total describes the same filter as the rows: the
	// panel prints 共 N 条 next to them.
	if len(page.Items) != 2 || page.Items[0].ID != "a-2" || page.Items[1].ID != "a-1" {
		t.Fatalf("appId filter returned %+v, want the two wx-app-1 records newest first", page.Items)
	}
	if page.Total != 2 {
		t.Fatalf("total = %d, want 2 (the count must use the filter, not the whole store)", page.Total)
	}
	if page.Items[0].AppID != "wx-app-1" {
		t.Fatalf("summary appId = %q, want the stored appid", page.Items[0].AppID)
	}

	// The filter composes with the others and never leaks across apps.
	page, err = repo.List(ctx, ListFilter{PageSize: 10, AppID: "wx-app-2", Status: StatusSuccess})
	if err != nil {
		t.Fatalf("combined list: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "b-1" {
		t.Fatalf("combined filter returned %+v, want only b-1", page.Items)
	}

	page, err = repo.List(ctx, ListFilter{PageSize: 10, Query: "wx-app-1"})
	if err != nil {
		t.Fatalf("query list: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("the keyword filter must still search name/url only, got %+v", page.Items)
	}
}

// wxapi and cloud pages keep independent sequence counters, so the same
// (captured_at, seq) pair occurs across types; the uniqueness key must
// include the type or one of the two records is silently dropped.
func TestIngestKeepsSameKeyAcrossTypes(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)

	wxapi := synthRecord(0, base) // seq 1 at base
	cloud := synthRecord(1, base)
	cloud.APIType = "cloud"
	cloud.CapturedAt = base // same instant as the wxapi record
	cloud.Seq = wxapi.Seq   // same seq: collides under the old key without the type

	inserted, err := repo.InsertBatch(ctx, []Record{wxapi, cloud})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if inserted != 2 {
		t.Fatalf("inserted %d records, want 2 (cross-type keys must not collide)", inserted)
	}

	// A replayed batch stays idempotent.
	duplicates, err := repo.InsertBatch(ctx, []Record{wxapi, cloud})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if duplicates != 0 {
		t.Fatalf("replay stored %d records, want 0", duplicates)
	}

	page, err := repo.List(ctx, ListFilter{PageSize: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("list returned %d records, want 2", len(page.Items))
	}
}

// The 002 migration rebuilds the table (SQLite cannot alter a UNIQUE
// constraint); rows written under the 001 schema must survive the upgrade.
func TestMigration002PreservesExistingRows(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "traffic.db")

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
			method         TEXT NOT NULL DEFAULT '',
			url            TEXT NOT NULL DEFAULT '',
			status         TEXT NOT NULL CHECK (status IN ('pending', 'success', 'fail')),
			request_bytes  INTEGER NOT NULL DEFAULT 0,
			response_bytes INTEGER NOT NULL DEFAULT 0,
			request_body   BLOB,
			response_body  BLOB,
			UNIQUE (captured_at, seq)
		);
		CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL);
		INSERT INTO schema_migrations VALUES ('001_traffic.sql', 1);
		INSERT INTO traffic_records (id, seq, captured_at, api_type, name, url, status)
			VALUES ('keep-me', 1, 1789776000000000000, 'wxapi', 'request', 'https://wx.qq.com/x', 'success');
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
}

// TestListClampsPageWindow keeps untrusted paging input out of the result
// slice and out of the SQL: a multi-million-row page size used to size
// make([]TrafficSummary, 0, limit), and a page number below 1 used to produce a
// negative OFFSET, which SQLite reads as "no offset" instead of the first page.
func TestListClampsPageWindow(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)

	records := make([]Record, 0, MaxListLimit+50)
	for i := 0; i < MaxListLimit+50; i++ {
		records = append(records, synthRecord(i, base))
	}
	if _, err := repo.InsertBatch(ctx, records); err != nil {
		t.Fatalf("insert batch: %v", err)
	}

	page, err := repo.List(ctx, ListFilter{PageSize: 2_000_000})
	if err != nil {
		t.Fatalf("oversized page size: %v", err)
	}
	if len(page.Items) != MaxListLimit || page.PageSize != MaxListLimit {
		t.Fatalf("oversized page size returned %d items with size %d, want the %d cap",
			len(page.Items), page.PageSize, MaxListLimit)
	}

	page, err = repo.List(ctx, ListFilter{PageSize: math.MaxInt})
	if err != nil {
		t.Fatalf("max int page size: %v", err)
	}
	if len(page.Items) != MaxListLimit {
		t.Fatalf("max int page size returned %d items, want %d", len(page.Items), MaxListLimit)
	}

	// Page 0 and negative pages read as the first page: the newest records.
	for _, requested := range []int{0, -1, -1000} {
		page, err = repo.List(ctx, ListFilter{Page: requested, PageSize: 10})
		if err != nil {
			t.Fatalf("page %d: %v", requested, err)
		}
		if page.Page != 1 {
			t.Fatalf("page %d echoed as %d, want 1", requested, page.Page)
		}
		if len(page.Items) == 0 || page.Items[0].ID != "rec-01049" {
			t.Fatalf("page %d started at %+v, want the newest record", requested, page.Items)
		}
	}
}
