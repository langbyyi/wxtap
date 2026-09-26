package traffic

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errTestReclaim = errors.New("reclamation unavailable")

// The uniqueness key includes api_type, so two records may share the same
// (captured_at, seq) key. A page boundary must still land between them: with an
// ORDER BY that calls them equal, SQLite is free to return either one first on
// each query, and a one-row page then shows the same record twice while the
// other never appears.
func TestListPagesRecordsSharingATimestamp(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	records := []Record{
		{ID: "cloud-1", Seq: 1, CapturedAt: base, APIType: "cloud", Name: "cloud", Status: StatusSuccess},
		{ID: "wxapi-1", Seq: 1, CapturedAt: base, APIType: "wxapi", Name: "wxapi", Status: StatusSuccess},
	}
	if _, err := repo.InsertBatch(ctx, records); err != nil {
		t.Fatalf("insert: %v", err)
	}

	first, err := repo.List(ctx, ListFilter{PageSize: 1})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Items) != 1 {
		t.Fatalf("first page returned %d items, want 1", len(first.Items))
	}
	second, err := repo.List(ctx, ListFilter{Page: 2, PageSize: 1})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Items) != 1 {
		t.Fatalf("second page returned %d items, want 1", len(second.Items))
	}
	if second.Items[0].ID == first.Items[0].ID {
		t.Fatalf("page 2 repeated %q instead of advancing to the other record", first.Items[0].ID)
	}
	if first.Total != 2 || second.Total != 2 {
		t.Fatalf("totals = %d/%d, want 2", first.Total, second.Total)
	}
}

func TestDeleteRemovesOnlyTheNamedRecords(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	insertSynth(t, repo, 5, base)

	result, err := repo.Delete(ctx, []string{"rec-00001", "rec-00003"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if result.Deleted != 2 || result.ReclamationFailed {
		t.Fatalf("delete result = %+v, want 2 deleted with no reclamation failure", result)
	}

	page, err := repo.List(ctx, ListFilter{PageSize: MaxListLimit})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("total after delete = %d, want 3", page.Total)
	}
	for _, item := range page.Items {
		if item.ID == "rec-00001" || item.ID == "rec-00003" {
			t.Fatalf("deleted record %s is still listed", item.ID)
		}
	}

	// The bodies go with the row: a deleted id must no longer answer.
	if _, err := repo.GetBody(ctx, "rec-00001", PartRequest); err == nil {
		t.Fatal("expected an error reading the body of a deleted record")
	}
}

// Deleting what is already gone is not a failure: the caller deletes what it
// can see, and a row someone else removed must not turn into an error that
// leaves the rest of the selection behind. Repeated ids count once.
func TestDeleteToleratesUnknownAndRepeatedIDs(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	insertSynth(t, repo, 3, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))

	result, err := repo.Delete(ctx, []string{"rec-00000", "rec-00000", "", "never-existed"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (a repeated id is one row, an unknown id is none)", result.Deleted)
	}

	empty, err := repo.Delete(ctx, nil)
	if err != nil {
		t.Fatalf("empty delete: %v", err)
	}
	if empty.Deleted != 0 {
		t.Fatalf("empty delete removed %d rows", empty.Deleted)
	}
	if got := countRecords(t, repo); got != 2 {
		t.Fatalf("records left = %d, want 2", got)
	}
}

// A large deletion reclaims space (VACUUM) and a small one only checkpoints the
// WAL. A manual deletion removes exactly the rows the caller named — nothing
// behind it trims records on its own — so the counts below are the whole story
// of what left the store.
func TestDeleteReclaimsLargeDeletionsOnly(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	spy := spyOnReclaim(repo)
	insertSynth(t, repo, 10, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))

	small, err := repo.Delete(ctx, []string{"rec-00000"})
	if err != nil {
		t.Fatalf("small delete: %v", err)
	}
	if small.Deleted != 1 {
		t.Fatalf("small delete removed %d rows, want 1", small.Deleted)
	}
	if spy.vacuums != 0 || spy.checkpoints != 1 {
		t.Fatalf("small delete: vacuums %d, checkpoints %d; want 0 vacuums and 1 checkpoint", spy.vacuums, spy.checkpoints)
	}

	ids := []string{"rec-00001", "rec-00002", "rec-00003", "rec-00004", "rec-00005", "rec-00006"}
	large, err := repo.Delete(ctx, ids)
	if err != nil {
		t.Fatalf("large delete: %v", err)
	}
	if large.Deleted != int64(len(ids)) {
		t.Fatalf("large delete removed %d rows, want %d", large.Deleted, len(ids))
	}
	if spy.vacuums != 1 {
		t.Fatalf("vacuums = %d, want 1 after deleting 7 of 9 rows", spy.vacuums)
	}
	if spy.checkpoints != 2 {
		t.Fatalf("checkpoints = %d, want one per real deletion", spy.checkpoints)
	}
	if got := countRecords(t, repo); got != 3 {
		t.Fatalf("records left = %d, want 3 (only the rows nobody named: 10 - 1 - 6)", got)
	}
}

// The rows are committed before reclamation runs, so a failed checkpoint or
// VACUUM must be reported as a flag rather than as a failed deletion.
func TestDeleteReportsReclamationFailureWithoutFailing(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	insertSynth(t, repo, 3, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	repo.reclaimHooks = &reclaimHooks{
		checkpoint: func(context.Context) error { return errTestReclaim },
		vacuum:     func(context.Context) error { return nil },
	}

	result, err := repo.Delete(ctx, []string{"rec-00000", "rec-00001"})
	if err != nil {
		t.Fatalf("delete must not fail on a reclamation error: %v", err)
	}
	if result.Deleted != 2 || !result.ReclamationFailed {
		t.Fatalf("delete result = %+v, want 2 deleted and the reclamation failure flagged", result)
	}
	if got := countRecords(t, repo); got != 1 {
		t.Fatalf("records left = %d, want 1", got)
	}
}
