package traffic

import (
	"context"
	"testing"
	"time"
)

// The audit plan requires a 10k-record regression recording write, duplicate
// write and read costs. The ceilings are deliberately loose: they catch
// order-of-magnitude degradations (a lost index, a per-row commit) rather
// than benchmark the machine.
func TestTenThousandRecordIngestRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("perf regression skipped in -short mode")
	}
	if raceEnabled {
		t.Skip("wall-clock budgets are meaningless under the race detector")
	}
	repo := openTestRepo(t)
	ctx := context.Background()

	base := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	batch := make([]Record, 0, 10000)
	for i := 0; i < 10000; i++ {
		batch = append(batch, synthRecord(i, base))
	}

	start := time.Now()
	inserted, err := Ingest(ctx, repo, batch)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	writeCost := time.Since(start)
	if inserted != 10000 {
		t.Fatalf("first ingest inserted %d, want 10000", inserted)
	}

	start = time.Now()
	duplicates, err := Ingest(ctx, repo, batch)
	if err != nil {
		t.Fatalf("duplicate ingest: %v", err)
	}
	dupCost := time.Since(start)
	if duplicates != 0 {
		t.Fatalf("duplicate ingest stored %d, want 0 (idempotent)", duplicates)
	}

	start = time.Now()
	var total int
	for pageNumber := 1; ; pageNumber++ {
		page, err := repo.List(ctx, ListFilter{Page: pageNumber, PageSize: 100})
		if err != nil {
			t.Fatalf("page %d: %v", pageNumber, err)
		}
		if len(page.Items) == 0 {
			break
		}
		total += len(page.Items)
	}
	readCost := time.Since(start)
	if total != 10000 {
		t.Fatalf("paged read returned %d, want 10000", total)
	}

	t.Logf("10k ingest: write %s, duplicate write %s, paged read %s", writeCost, dupCost, readCost)
	if writeCost > 10*time.Second {
		t.Fatalf("first write regressed: %s", writeCost)
	}
	if dupCost > 10*time.Second {
		t.Fatalf("duplicate write regressed: %s", dupCost)
	}
	if readCost > 10*time.Second {
		t.Fatalf("paged read regressed: %s", readCost)
	}
}
