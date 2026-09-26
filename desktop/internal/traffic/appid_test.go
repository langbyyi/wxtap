package traffic

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestIngestKeepsSameKeyAcrossAppIDs(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 20, 12, 30, 0, 0, time.UTC)
	records := []Record{
		{ID: "wx1-1", Seq: 1, CapturedAt: base, APIType: "wx.request", AppID: "wx1", Name: "one", Status: StatusSuccess},
		{ID: "wx2-1", Seq: 1, CapturedAt: base, APIType: "wx.request", AppID: "wx2", Name: "two", Status: StatusSuccess},
	}
	inserted, err := repo.InsertBatch(ctx, records)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if inserted != 2 {
		t.Fatalf("inserted %d records, want 2 (appid must participate in the idempotency key)", inserted)
	}
	duplicates, err := repo.InsertBatch(ctx, records)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if duplicates != 0 {
		t.Fatalf("replay inserted %d records, want 0", duplicates)
	}
}

func TestDecompressLimitedCapsBody(t *testing.T) {
	const limit = 1024
	source := bytes.Repeat([]byte("a"), limit+100)
	compressed, err := compress(source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decompressLimited(compressed, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != limit {
		t.Fatalf("decompressed body length = %d, want %d", len(got), limit)
	}
}

func TestInsertKeepsSameSequenceAcrossApps(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	records := []Record{
		{ID: "app1-1", Seq: 1, CapturedAt: base, APIType: "wx.request", Name: "one", AppID: "wx-app-1", Status: StatusSuccess},
		{ID: "app2-1", Seq: 1, CapturedAt: base, APIType: "wx.request", Name: "two", AppID: "wx-app-2", Status: StatusSuccess},
	}
	inserted, err := repo.InsertBatch(ctx, records)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if inserted != 2 {
		t.Fatalf("inserted = %d, want both apps retained", inserted)
	}
	got, err := repo.RecentRecordsAllApps(ctx, 10)
	if err != nil {
		t.Fatalf("recent records: %v", err)
	}
	apps := map[string]bool{}
	for _, record := range got {
		apps[record.AppID] = true
	}
	if len(got) != 2 || !apps["wx-app-1"] || !apps["wx-app-2"] {
		t.Fatalf("recent records = %#v, want one row per app", got)
	}
}
