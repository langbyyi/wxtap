package traffic

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRecentRecordsForAuditFiltersLimitsAndOrders(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)

	records := []Record{
		{ID: "wxapi-wxapp-1-3", Seq: 3, CapturedAt: base.Add(3 * time.Second), APIType: "wxapi", Name: "request", AppID: "wx-app", Method: "GET", URL: "https://api.example.com/api/orders", Status: StatusSuccess, RequestBody: []byte(`{"seq":3}`)},
		{ID: "cloud-wxapp-2-5", Seq: 5, CapturedAt: base.Add(5 * time.Second), APIType: "cloud", Name: "database", AppID: "wx-app", URL: "https://api.example.com/cloud/query?uid=1", Status: StatusSuccess, RequestBody: []byte(`{"seq":5}`)},
		{ID: "wxapi-wxapp-1-1", Seq: 1, CapturedAt: base.Add(1 * time.Second), APIType: "wxapi", Name: "request", AppID: "wx-app", Method: "POST", URL: "https://other.example.com/api/orders", Status: StatusSuccess, RequestBody: []byte(`{"seq":1}`)},
		{ID: "wxapi-wxapp-1-4", Seq: 4, CapturedAt: base.Add(4 * time.Second), APIType: "wxapi", Name: "request", AppID: "wx-app", Method: "GET", URL: "https://api.example.com/api/users", Status: StatusSuccess, RequestBody: []byte(`{"seq":4}`)},
		{ID: "wxapi-wxapp-1-2", Seq: 2, CapturedAt: base.Add(2 * time.Second), APIType: "wxapi", Name: "request", AppID: "wx-app", Method: "GET", URL: "https://api.example.com/api/old", Status: StatusSuccess, RequestBody: []byte(`{"seq":2}`)},
	}
	if _, err := repo.InsertBatch(ctx, records); err != nil {
		t.Fatalf("insert batch: %v", err)
	}

	all, err := repo.RecentRecordsForAudit(ctx, "", "", 100)
	if err != nil {
		t.Fatalf("recent for audit: %v", err)
	}
	if len(all) != len(records) {
		t.Fatalf("got %d records, want %d", len(all), len(records))
	}
	// Newest first by seq, regardless of captured_at.
	wantOrder := []string{"cloud-wxapp-2-5", "wxapi-wxapp-1-4", "wxapi-wxapp-1-3", "wxapi-wxapp-1-2", "wxapi-wxapp-1-1"}
	for i, want := range wantOrder {
		if all[i].ID != want {
			t.Fatalf("position %d = %s, want %s", i, all[i].ID, want)
		}
	}
	// Bodies travel decompressed, same as RecentRecordsAllApps.
	if string(all[0].RequestBody) != `{"seq":5}` {
		t.Fatalf("request body = %q", all[0].RequestBody)
	}
	if all[0].URL != "https://api.example.com/cloud/query?uid=1" || all[0].Name != "database" || all[0].APIType != "cloud" {
		t.Fatalf("row fields incomplete: %+v", all[0])
	}

	byType, err := repo.RecentRecordsForAudit(ctx, "cloud", "", 100)
	if err != nil {
		t.Fatalf("filter by api type: %v", err)
	}
	if len(byType) != 1 || byType[0].ID != "cloud-wxapp-2-5" {
		t.Fatalf("cloud filter returned %+v", byType)
	}

	byURL, err := repo.RecentRecordsForAudit(ctx, "", "/api/orders", 100)
	if err != nil {
		t.Fatalf("filter by url substring: %v", err)
	}
	if len(byURL) != 2 || byURL[0].ID != "wxapi-wxapp-1-3" || byURL[1].ID != "wxapi-wxapp-1-1" {
		t.Fatalf("url filter returned %+v", byURL)
	}

	combined, err := repo.RecentRecordsForAudit(ctx, "wxapi", "api.example.com", 100)
	if err != nil {
		t.Fatalf("combined filter: %v", err)
	}
	if len(combined) != 3 {
		t.Fatalf("combined filter returned %d records, want 3", len(combined))
	}

	capped, err := repo.RecentRecordsForAudit(ctx, "", "", 3)
	if err != nil {
		t.Fatalf("limit: %v", err)
	}
	if len(capped) != 3 || capped[0].ID != "cloud-wxapp-2-5" {
		t.Fatalf("limit returned %+v", capped)
	}

	floor, err := repo.RecentRecordsForAudit(ctx, "", "", 0)
	if err != nil {
		t.Fatalf("clamped low limit: %v", err)
	}
	if len(floor) != 1 {
		t.Fatalf("limit 0 returned %d records, want 1 (clamp floor)", len(floor))
	}
}

func TestRecentRecordsForAuditClampsHugeLimit(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)

	records := make([]Record, 0, MaxAuditRecords+10)
	for i := 0; i < MaxAuditRecords+10; i++ {
		records = append(records, Record{
			ID:           fmt.Sprintf("wxapi-wxapp-%d-%d", i, i),
			Seq:          int64(i),
			CapturedAt:   base.Add(time.Duration(i) * time.Millisecond),
			APIType:      "wxapi",
			Name:         "request",
			AppID:        "wx-app",
			URL:          fmt.Sprintf("https://api.example.com/x/%d", i),
			Status:       StatusSuccess,
			RequestBody:  []byte(`{}`),
			ResponseBody: []byte(`{}`),
		})
	}
	if _, err := repo.InsertBatch(ctx, records); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	got, err := repo.RecentRecordsForAudit(ctx, "", "", MaxAuditRecords*10)
	if err != nil {
		t.Fatalf("huge limit: %v", err)
	}
	if len(got) != MaxAuditRecords {
		t.Fatalf("got %d records, want clamp %d", len(got), MaxAuditRecords)
	}
}
