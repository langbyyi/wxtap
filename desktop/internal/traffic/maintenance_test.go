package traffic

import (
	"context"
	"errors"
	"testing"
	"time"
)

// insertSynth stores count synthetic records starting at base, oldest first;
// synthRecord ids are rec-00000, rec-00001, ...
func insertSynth(t *testing.T, repo *Repository, count int, base time.Time) {
	t.Helper()
	records := make([]Record, 0, count)
	for i := 0; i < count; i++ {
		records = append(records, synthRecord(i, base))
	}
	if _, err := repo.InsertBatch(context.Background(), records); err != nil {
		t.Fatalf("insert %d records: %v", count, err)
	}
}

func countRecords(t *testing.T, repo *Repository) int64 {
	t.Helper()
	count, err := repo.countRecords(context.Background())
	if err != nil {
		t.Fatalf("count records: %v", err)
	}
	return count
}

// storedIDs lists the record ids in list order (newest first).
func storedIDs(t *testing.T, repo *Repository) []string {
	t.Helper()
	page, err := repo.List(context.Background(), ListFilter{PageSize: MaxListLimit})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

// reclaimSpy counts the space-reclamation steps. A real VACUUM rewrites the whole
// file, and what these tests assert is *whether* it runs, not how well it packs
// the pages.
type reclaimSpy struct {
	checkpoints int
	vacuums     int
}

func spyOnReclaim(repo *Repository) *reclaimSpy {
	spy := &reclaimSpy{}
	repo.reclaimHooks = &reclaimHooks{
		checkpoint: func(context.Context) error { spy.checkpoints++; return nil },
		vacuum:     func(context.Context) error { spy.vacuums++; return nil },
	}
	return spy
}

// Clear is the only bulk deletion: the store no longer trims itself on a
// retention policy, so this — the user's own confirmed action — is what keeps it
// from growing forever. It reports the rows it removed and leaves nothing behind.
func TestClearRemovesEveryRecord(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	insertSynth(t, repo, 7, time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC))

	result, err := repo.Clear(ctx)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if result.Deleted != 7 {
		t.Fatalf("deleted = %d, want 7", result.Deleted)
	}
	if got := countRecords(t, repo); got != 0 {
		t.Fatalf("records after clear = %d, want 0", got)
	}
	if ids := storedIDs(t, repo); len(ids) != 0 {
		t.Fatalf("clear left records behind: %v", ids)
	}
	stats, err := repo.Stats(ctx)
	if err != nil {
		t.Fatalf("stats after clear: %v", err)
	}
	if stats.Records != 0 || stats.OldestCapturedAt != "" || stats.NewestCapturedAt != "" {
		t.Fatalf("stats after clear = %+v, want 0 records and no range", stats)
	}
}

// Clearing an empty store is a no-op that still succeeds: the action is
// idempotent, and a caller who confirmed a clear must not be told it failed
// because someone else got there first.
func TestClearOnEmptyStoreRemovesNothing(t *testing.T) {
	repo := openTestRepo(t)

	result, err := repo.Clear(context.Background())
	if err != nil {
		t.Fatalf("clear on an empty store: %v", err)
	}
	if result.Deleted != 0 {
		t.Fatalf("deleted = %d, want 0", result.Deleted)
	}
	if result.ReclamationFailed {
		t.Fatalf("clear result = %+v, want no reclamation failure", result)
	}
	if got := countRecords(t, repo); got != 0 {
		t.Fatalf("records = %d, want 0", got)
	}
}

// Every real deletion checkpoints the WAL, and a clear of a non-empty store
// hands the whole file back: 100% of the rows went away, well past the share
// that makes a VACUUM worth its write lock.
func TestClearCheckpointsAndVacuumsANonEmptyStore(t *testing.T) {
	repo := openTestRepo(t)
	spy := spyOnReclaim(repo)
	insertSynth(t, repo, 2, time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC))

	if _, err := repo.Clear(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if spy.checkpoints != 1 {
		t.Fatalf("checkpoints = %d, want 1 after a real clear", spy.checkpoints)
	}
	if spy.vacuums != 1 {
		t.Fatalf("vacuums = %d, want 1: a clear rewrites the file back to its empty size", spy.vacuums)
	}
}

// Nothing deleted means no space to reclaim: the checkpoint still runs, the
// VACUUM does not.
func TestClearOnEmptyStoreVacuumsNothing(t *testing.T) {
	repo := openTestRepo(t)
	spy := spyOnReclaim(repo)

	if _, err := repo.Clear(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if spy.checkpoints != 1 || spy.vacuums != 0 {
		t.Fatalf("empty clear: checkpoints %d, vacuums %d; want 1 checkpoint and 0 vacuums", spy.checkpoints, spy.vacuums)
	}
}

// Space reclamation is best-effort: the rows are committed before it runs, so a
// failing checkpoint or VACUUM must not turn a clear that did happen into
// "清空失败". The deleted count stays accurate and the failure rides on
// ReclamationFailed instead of on the error.
func TestClearReportsReclamationFailureWithoutFailingTheCleanup(t *testing.T) {
	base := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		checkpoint func(context.Context) error
		vacuum     func(context.Context) error
	}{
		{"checkpoint fails", func(context.Context) error { return errors.New("checkpoint busy") }, nil},
		{"vacuum fails", nil, func(context.Context) error { return errors.New("vacuum busy") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := openTestRepo(t)
			insertSynth(t, repo, 10, base)
			hooks := &reclaimHooks{checkpoint: func(context.Context) error { return nil }, vacuum: func(context.Context) error { return nil }}
			if tc.checkpoint != nil {
				hooks.checkpoint = tc.checkpoint
			}
			if tc.vacuum != nil {
				hooks.vacuum = tc.vacuum
			}
			repo.reclaimHooks = hooks

			result, err := repo.Clear(context.Background())
			if err != nil {
				t.Fatalf("a committed clear must not fail on reclamation: %v", err)
			}
			if result.Deleted != 10 {
				t.Fatalf("deleted = %d, want 10", result.Deleted)
			}
			if !result.ReclamationFailed {
				t.Fatal("the reclamation failure must stay visible on the result")
			}
			// The deletion stands: the store is empty.
			if got := countRecords(t, repo); got != 0 {
				t.Fatalf("records after the clear = %d, want 0", got)
			}
		})
	}
}

// The happy path must keep the flag false, otherwise callers learn to ignore it.
func TestClearClearsTheReclamationFlagOnSuccess(t *testing.T) {
	repo := openTestRepo(t)
	spyOnReclaim(repo)
	insertSynth(t, repo, 10, time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC))

	result, err := repo.Clear(context.Background())
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if result.ReclamationFailed {
		t.Fatal("a completed reclamation must not be flagged as failed")
	}
}

func TestStatsReportsShape(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	insertSynth(t, repo, 5, base)

	stats, err := repo.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Records != 5 {
		t.Fatalf("records = %d, want 5", stats.Records)
	}
	if stats.OldestCapturedAt != base.Format(time.RFC3339Nano) {
		t.Fatalf("oldest = %q, want %q", stats.OldestCapturedAt, base.Format(time.RFC3339Nano))
	}
	wantNewest := base.Add(4 * time.Millisecond).Format(time.RFC3339Nano)
	if stats.NewestCapturedAt != wantNewest {
		t.Fatalf("newest = %q, want %q", stats.NewestCapturedAt, wantNewest)
	}
	if stats.Bytes <= 0 {
		t.Fatalf("bytes = %d, want the page_count × page_size footprint", stats.Bytes)
	}

	// A bulk deletion shows up on the stats surface the panel reads: the count
	// and the capture range follow the store, empty included.
	if _, err := repo.Clear(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	stats, err = repo.Stats(ctx)
	if err != nil {
		t.Fatalf("stats after clear: %v", err)
	}
	if stats.Records != 0 || stats.OldestCapturedAt != "" || stats.NewestCapturedAt != "" {
		t.Fatalf("after clear: %+v, want 0 records and no range", stats)
	}
}

func TestStatsOnEmptyStoreReportsNoRange(t *testing.T) {
	repo := openTestRepo(t)
	stats, err := repo.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Records != 0 || stats.OldestCapturedAt != "" || stats.NewestCapturedAt != "" {
		t.Fatalf("empty store stats = %+v, want 0 records and no range", stats)
	}
}
