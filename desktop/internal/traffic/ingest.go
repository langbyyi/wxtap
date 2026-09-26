package traffic

import "context"

// Ingest stores a drained Hook batch idempotently. Records are keyed by
// (captured_at, seq), so a batch replayed after a lost acknowledgement — or
// resent with regenerated ids — never duplicates sequence records. It returns
// the number of newly inserted rows.
//
// This is the Go endpoint of the seq+drain protocol: the Core hook buffers
// records with monotonic seq numbers, the desktop drains with afterSeq, and a
// crash between drain and ack is recovered by simply re-draining.
func Ingest(ctx context.Context, repo *Repository, batch []Record) (int, error) {
	return repo.InsertBatch(ctx, batch)
}
