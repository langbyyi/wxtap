package traffic

import (
	"context"
	"testing"
	"time"
)

// buildAppIDFixture stores records for two mini programs plus one record with
// an unknown (empty) appid, and returns the shared base time.
func buildAppIDFixture(t *testing.T) (*Repository, context.Context, time.Time) {
	t.Helper()
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	records := []Record{
		{ID: "wxapi-a-1", Seq: 1, CapturedAt: base.Add(1 * time.Second), APIType: "wxapi", Name: "request", AppID: "wx-app-a", Method: "GET", URL: "https://a.example.com/x", Status: StatusSuccess},
		{ID: "wxapi-b-2", Seq: 2, CapturedAt: base.Add(2 * time.Second), APIType: "wxapi", Name: "request", AppID: "wx-app-b", Method: "GET", URL: "https://b.example.com/y", Status: StatusSuccess},
		{ID: "wxapi-a-3", Seq: 3, CapturedAt: base.Add(3 * time.Second), APIType: "wxapi", Name: "request", AppID: "wx-app-a", Method: "POST", URL: "https://a.example.com/z", Status: StatusSuccess},
		{ID: "wxapi-?-4", Seq: 4, CapturedAt: base.Add(4 * time.Second), APIType: "wxapi", Name: "request", AppID: "", Method: "GET", URL: "https://unknown.example.com/w", Status: StatusSuccess},
	}
	if _, err := repo.InsertBatch(ctx, records); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	return repo, ctx, base
}

func TestRecentRecordsForAppIDScopesToOneProgram(t *testing.T) {
	repo, ctx, _ := buildAppIDFixture(t)

	scoped, err := repo.RecentRecordsForAppID(ctx, "wx-app-a", "", "", 100)
	if err != nil {
		t.Fatalf("recent for appid: %v", err)
	}
	if len(scoped) != 2 || scoped[0].ID != "wxapi-a-3" || scoped[1].ID != "wxapi-a-1" {
		t.Fatalf("scoped read returned %+v", scoped)
	}
	for _, record := range scoped {
		if record.AppID != "wx-app-a" {
			t.Fatalf("scoped read leaked other program: %+v", record)
		}
	}

	// 空 appid 与 RecentRecordsForAudit 同口径：跨程序读全部（含未知 appid）。
	all, err := repo.RecentRecordsForAppID(ctx, "", "", "", 100)
	if err != nil {
		t.Fatalf("empty-appid read: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("empty-appid read got %d records, want 4", len(all))
	}

	// appid 过滤与其他条件可叠加。
	combo, err := repo.RecentRecordsForAppID(ctx, "wx-app-a", "wxapi", "/z", 100)
	if err != nil {
		t.Fatalf("combined filter: %v", err)
	}
	if len(combo) != 1 || combo[0].ID != "wxapi-a-3" {
		t.Fatalf("combined filter returned %+v", combo)
	}

	// 未知 appid 读到空集而不是报错。
	missing, err := repo.RecentRecordsForAppID(ctx, "wx-none", "", "", 100)
	if err != nil {
		t.Fatalf("unknown appid read: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("unknown appid read returned %+v", missing)
	}
}

func TestAppIDStatsGroupsAndOrders(t *testing.T) {
	repo, ctx, base := buildAppIDFixture(t)

	stats, err := repo.AppIDStats(ctx)
	if err != nil {
		t.Fatalf("appid stats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("appid stats got %d rows, want 2: %+v", len(stats), stats)
	}
	// 最新捕获的小程序排前面：b(2s) 在 a(3s 前) 之后 —— a 的最新是 3s，b 是 2s，a 更新。
	if stats[0].AppID != "wx-app-a" || stats[1].AppID != "wx-app-b" {
		t.Fatalf("appid stats order = %s, %s; want a, b", stats[0].AppID, stats[1].AppID)
	}
	if stats[0].Count != 2 || stats[1].Count != 1 {
		t.Fatalf("appid stats counts = %d, %d; want 2, 1", stats[0].Count, stats[1].Count)
	}
	wantLast := base.Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
	if stats[0].LastSeen != wantLast {
		t.Fatalf("lastSeen = %s, want %s", stats[0].LastSeen, wantLast)
	}
	// 空 appid 的记录不进分组：它没有可选择的程序。
	for _, stat := range stats {
		if stat.AppID == "" {
			t.Fatalf("empty appid leaked into stats: %+v", stat)
		}
	}
}
