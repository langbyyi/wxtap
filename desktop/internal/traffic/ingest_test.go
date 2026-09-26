package traffic

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openIngestRepo(t *testing.T) *Repository {
	t.Helper()
	repo, err := Open(filepath.Join(t.TempDir(), "traffic.db"), filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func hookRecords(n int, base time.Time) []Record {
	records := make([]Record, 0, n)
	for i := 0; i < n; i++ {
		records = append(records, Record{
			ID:           fmt.Sprintf("hook-%06d", i),
			Seq:          int64(i + 1),
			CapturedAt:   base.Add(time.Duration(i) * time.Millisecond),
			APIType:      "wx.request",
			Name:         "GET https://api.example.com/" + fmt.Sprint(i),
			Method:       "GET",
			URL:          "https://api.example.com/" + fmt.Sprint(i),
			Status:       StatusSuccess,
			RequestBody:  []byte(fmt.Sprintf(`{"i":%d}`, i)),
			ResponseBody: []byte(fmt.Sprintf(`{"ok":%d}`, i)),
		})
	}
	return records
}

// A batch replayed by the ingestion path must not duplicate sequence records.
func TestIngestRepeatedBatchDoesNotDuplicate(t *testing.T) {
	repo := openIngestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	batch := hookRecords(50, base)

	first, err := Ingest(ctx, repo, batch)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if first != len(batch) {
		t.Fatalf("first ingest inserted %d, want %d", first, len(batch))
	}

	second, err := Ingest(ctx, repo, batch)
	if err != nil {
		t.Fatalf("replayed ingest: %v", err)
	}
	if second != 0 {
		t.Fatalf("replayed ingest inserted %d duplicates, want 0", second)
	}

	page, err := repo.List(ctx, ListFilter{PageSize: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != len(batch) {
		t.Fatalf("stored %d records after replay, want %d", len(page.Items), len(batch))
	}
	// Every record must still be queryable by body without corruption.
	body, err := repo.GetBody(ctx, batch[10].ID, PartRequest)
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	if string(body) != string(batch[10].RequestBody) {
		t.Fatalf("body corrupted after replay: %q", body)
	}
}

// A replay that reuses (captured_at, seq) with a new id must not create a
// second row either — the sequence key is the deduplication contract.
func TestIngestSameSequenceDifferentIDIsIgnored(t *testing.T) {
	repo := openIngestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	original := hookRecords(3, base)
	if _, err := Ingest(ctx, repo, original); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	resent := hookRecords(3, base)
	for i := range resent {
		resent[i].ID = fmt.Sprintf("resent-%06d", i)
	}
	inserted, err := Ingest(ctx, repo, resent)
	if err != nil {
		t.Fatalf("replayed ingest: %v", err)
	}
	if inserted != 0 {
		t.Fatalf("replayed ingest with new ids inserted %d rows, want 0", inserted)
	}

	page, err := repo.List(ctx, ListFilter{PageSize: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("stored %d records, want 3", len(page.Items))
	}
	// Every stored row is still an original: a replayed batch must not overwrite
	// a row it did not own. Checked across the page rather than at a fixed
	// position, which is the list's newest-first boundary and not the point.
	for _, item := range page.Items {
		if !strings.HasPrefix(item.ID, "hook-") {
			t.Fatalf("original record replaced: id %s", item.ID)
		}
	}
}

func BenchmarkIngest10000(b *testing.B) {
	repo, err := Open(filepath.Join(b.TempDir(), "traffic.db"), filepath.Join("..", "..", "migrations"))
	if err != nil {
		b.Fatalf("open repository: %v", err)
	}
	defer func() { _ = repo.Close() }()
	ctx := context.Background()
	batch := hookRecords(10000, time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Ingest(ctx, repo, batch); err != nil {
			b.Fatalf("ingest: %v", err)
		}
	}
}

// BenchmarkIngest10000Fresh measures the actual write path: every iteration
// shifts the batch timestamps so (captured_at, seq) dedup never kicks in.
func BenchmarkIngest10000Fresh(b *testing.B) {
	repo, err := Open(filepath.Join(b.TempDir(), "traffic.db"), filepath.Join("..", "..", "migrations"))
	if err != nil {
		b.Fatalf("open repository: %v", err)
	}
	defer func() { _ = repo.Close() }()
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := hookRecords(10000, base.Add(time.Duration(i)*time.Hour))
		if _, err := Ingest(ctx, repo, batch); err != nil {
			b.Fatalf("ingest: %v", err)
		}
	}
}
