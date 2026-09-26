package traffic

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Stats is the traffic.stats payload (roadmap 2.3). Records is always answered,
// and so is the capture range whenever the store holds anything; the fields
// that can be unknown (the range on an empty store) are omitted rather than
// faked, so the frontend's existence checks can tell "not known" from "zero".
type Stats struct {
	Records          int64  `json:"records"`
	OldestCapturedAt string `json:"oldestCapturedAt,omitempty"` // RFC3339Nano
	NewestCapturedAt string `json:"newestCapturedAt,omitempty"` // RFC3339Nano
	// Bytes is the on-disk footprint of the database: page_count × page_size.
	// That is the "库文件占用" reading — it includes indexes and free pages, so
	// it is exactly what a VACUUM can hand back — and it costs two pragma reads
	// instead of a full scan of every body. The WAL is not counted (a deletion
	// checkpoints it away), and records still sitting in the WAL after an
	// insert are therefore slightly under-counted until the next checkpoint.
	Bytes int64 `json:"bytes"`
	// DroppedRecords / DroppedUpdates are the R12 overload counters: records the
	// capture path had to throw away, so the history panel can say an overload
	// happened instead of silently showing a shorter log. They are summed in
	// registerTrafficHandlers (desktop/ipc_bridge.go) from the two feeders'
	// FeederStats: DroppedRecords = Σ(shell pending drops + page-side record
	// drops), DroppedUpdates = Σ page-side update drops (the shell keeps no
	// update buffer of its own). They stay 0 until some capture path drops
	// something: the page zeroes the underlying readings on clearHookedCalls and
	// the shell only repeats the last drain's reading.
	DroppedRecords int64 `json:"droppedRecords"`
	DroppedUpdates int64 `json:"droppedUpdates"`
}

// DeleteResult is the traffic.delete payload. It carries the reclamation story
// explicitly: the rows are gone either way, and a failed VACUUM must stay
// visible without being reported as a failed delete.
type DeleteResult struct {
	Deleted int64 `json:"deleted"`
	// ReclamationFailed is true when the rows were deleted but the follow-up
	// space reclamation (WAL checkpoint / VACUUM) did not complete.
	ReclamationFailed bool `json:"reclamationFailed,omitempty"`
}

// AppIDStat summarizes the stored records of one mini program: how many rows
// carry the appid and when it was last captured. The per-program pickers (the
// traffic filter and the asset target) are built from this list.
type AppIDStat struct {
	AppID string `json:"appid"`
	// Name is a best-effort display label filled in by the IPC layer (learned
	// while the program was connected). The store itself only knows the
	// appid, so it stays empty for programs this installation never debugged.
	Name     string `json:"name,omitempty"`
	Count    int64  `json:"count"`
	LastSeen string `json:"lastSeen"` // RFC3339Nano, UTC
}

// AppIDStats groups the store by appid, newest capture first. Records with an
// empty appid (captures that could not identify the program) are left out:
// they have no program to select.
func (r *Repository) AppIDStats(ctx context.Context) ([]AppIDStat, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT appid, COUNT(*), MAX(captured_at) FROM traffic_records WHERE appid <> '' GROUP BY appid ORDER BY MAX(captured_at) DESC`)
	if err != nil {
		return nil, fmt.Errorf("traffic appid stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	stats := []AppIDStat{}
	for rows.Next() {
		var stat AppIDStat
		var lastNanos int64
		if err := rows.Scan(&stat.AppID, &stat.Count, &lastNanos); err != nil {
			return nil, fmt.Errorf("scan traffic appid stats: %w", err)
		}
		stat.LastSeen = time.Unix(0, lastNanos).UTC().Format(time.RFC3339Nano)
		stats = append(stats, stat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate traffic appid stats: %w", err)
	}
	return stats, nil
}

// vacuumRatioDenominator is the deletion share that makes a VACUUM worthwhile:
// it holds the write lock while it rewrites the whole file, so it only runs
// once a fifth of the store is gone (roadmap 2.4b). Keeping the threshold as a
// denominator lets the comparison stay in integer arithmetic, so the boundary
// is exact.
const vacuumRatioDenominator = 5

// reclaimHooks replaces the space-reclamation steps. Tests assert *when* a
// checkpoint or a VACUUM runs without paying for the real thing; production
// leaves this nil and the real pragmas run.
type reclaimHooks struct {
	checkpoint func(ctx context.Context) error
	vacuum     func(ctx context.Context) error
}

// Stats reports the size and capture range of the store, plus the capture
// path's overload counters.
func (r *Repository) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	var oldest, newest sql.NullInt64
	row := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(captured_at), MAX(captured_at) FROM traffic_records`)
	if err := row.Scan(&stats.Records, &oldest, &newest); err != nil {
		return Stats{}, fmt.Errorf("traffic stats: %w", err)
	}
	if oldest.Valid {
		stats.OldestCapturedAt = time.Unix(0, oldest.Int64).UTC().Format(time.RFC3339Nano)
	}
	if newest.Valid {
		stats.NewestCapturedAt = time.Unix(0, newest.Int64).UTC().Format(time.RFC3339Nano)
	}
	bytes, err := r.fileBytes(ctx)
	if err != nil {
		return Stats{}, err
	}
	stats.Bytes = bytes
	return stats, nil
}

// fileBytes estimates the database footprint as page_count × page_size.
func (r *Repository) fileBytes(ctx context.Context) (int64, error) {
	var pageCount, pageSize int64
	if err := r.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("traffic page_count: %w", err)
	}
	if err := r.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("traffic page_size: %w", err)
	}
	return pageCount * pageSize, nil
}

// reclaim hands the pages a deletion freed back to the OS: always a WAL
// checkpoint (the freed pages would otherwise sit in the log), and a VACUUM
// only when the deletion was large enough to be worth holding the write lock
// that long (see vacuumRatioDenominator) and the caller asked for it.
//
// It reports a failed step rather than returning an error because by the time
// it runs the rows are already committed as deleted: the caller must not turn a
// reclamation failure into "删除失败" (see DeleteResult.ReclamationFailed). Both
// steps still get their chance — a failed checkpoint does not cancel the VACUUM.
func (r *Repository) reclaim(ctx context.Context, deleted int64, vacuum bool) bool {
	failed := false
	if err := r.checkpoint(ctx); err != nil {
		failed = true
	}
	if deleted <= 0 || !vacuum {
		return failed
	}
	remaining, err := r.countRecords(ctx)
	if err != nil {
		return true
	}
	// deleted/rowsBefore >= 1/vacuumRatioDenominator, in integers so the
	// boundary is exact.
	if deleted*vacuumRatioDenominator < remaining+deleted {
		return failed
	}
	if err := r.vacuum(ctx); err != nil {
		return true
	}
	return failed
}

func (r *Repository) countRecords(ctx context.Context) (int64, error) {
	var count int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_records`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count traffic: %w", err)
	}
	return count, nil
}

// checkpoint truncates the write-ahead log. The pragma answers one row
// (busy, log pages, checkpointed pages); it is scanned rather than ignored so
// the driver does not leave an unconsumed result set behind.
func (r *Repository) checkpoint(ctx context.Context) error {
	if r.reclaimHooks != nil && r.reclaimHooks.checkpoint != nil {
		return r.reclaimHooks.checkpoint(ctx)
	}
	var busy, logPages, checkpointed int
	if err := r.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logPages, &checkpointed); err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}
	return nil
}

// vacuum rewrites the database file to hand the free pages back to the OS.
// VACUUM cannot run inside a transaction, which is why it happens after the
// deletion transaction has committed.
func (r *Repository) vacuum(ctx context.Context) error {
	if r.reclaimHooks != nil && r.reclaimHooks.vacuum != nil {
		return r.reclaimHooks.vacuum(ctx)
	}
	if _, err := r.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}
