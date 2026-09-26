package api

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

func openFixture(t *testing.T) (*traffic.Repository, *traffic.Service) {
	t.Helper()
	repo, err := traffic.Open(filepath.Join(t.TempDir(), "traffic.db"), filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo, traffic.NewService(repo)
}

func TestTrafficListForwardsPageAndPageSize(t *testing.T) {
	repo, service := openFixture(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)

	records := make([]traffic.Record, 0, 250)
	for i := 0; i < 250; i++ {
		records = append(records, traffic.Record{
			ID:         fmt.Sprintf("rec-%03d", i),
			Seq:        int64(i + 1),
			CapturedAt: base.Add(time.Duration(i) * time.Millisecond),
			APIType:    "wx.request",
			Name:       "GET https://api.example.com",
			URL:        "https://api.example.com/" + fmt.Sprintf("%d", i),
			Status:     traffic.StatusSuccess,
		})
	}
	if _, err := traffic.Ingest(ctx, repo, records); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	api := NewTrafficAPI(service)
	page, err := api.TrafficList(ctx, TrafficListParams{PageSize: 100})
	if err != nil {
		t.Fatalf("TrafficList: %v", err)
	}
	// The window and the total travel back with the rows: the panel renders
	// them together, so a caller must not have to re-ask for either.
	if len(page.Items) != 100 || page.Total != 250 || page.Page != 1 || page.PageSize != 100 {
		t.Fatalf("first page: %d items, total %d, page %d size %d", len(page.Items), page.Total, page.Page, page.PageSize)
	}
	if page.Items[0].ID != "rec-249" || page.Items[99].ID != "rec-150" {
		t.Fatalf("first page boundaries: %s..%s, want newest first", page.Items[0].ID, page.Items[99].ID)
	}

	second, err := api.TrafficList(ctx, TrafficListParams{Page: 2, PageSize: 100})
	if err != nil {
		t.Fatalf("TrafficList page 2: %v", err)
	}
	if len(second.Items) != 100 || second.Items[0].ID != "rec-149" {
		t.Fatalf("second page starts at %s, want the next window", second.Items[0].ID)
	}

	filtered, err := api.TrafficList(ctx, TrafficListParams{PageSize: 100, APIType: "cloud"})
	if err != nil {
		t.Fatalf("TrafficList filter: %v", err)
	}
	if len(filtered.Items) != 0 || filtered.Total != 0 {
		t.Fatalf("expected empty cloud page, got %d items with total %d", len(filtered.Items), filtered.Total)
	}
}

func TestTrafficDeleteForwardsIDs(t *testing.T) {
	repo, service := openFixture(t)
	ctx := context.Background()
	if _, err := traffic.Ingest(ctx, repo, []traffic.Record{
		{ID: "rec-001", Seq: 1, CapturedAt: time.Now().UTC(), APIType: "wx.request", Name: "one", Status: traffic.StatusSuccess},
		{ID: "rec-002", Seq: 2, CapturedAt: time.Now().UTC(), APIType: "wx.request", Name: "two", Status: traffic.StatusSuccess},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	api := NewTrafficAPI(service)
	result, err := api.TrafficDelete(ctx, TrafficDeleteParams{IDs: []string{"rec-001"}})
	if err != nil {
		t.Fatalf("TrafficDelete: %v", err)
	}
	if result.Deleted != 1 || result.ReclamationFailed {
		t.Fatalf("delete result = %+v, want 1 deleted", result)
	}

	page, err := api.TrafficList(ctx, TrafficListParams{})
	if err != nil {
		t.Fatalf("TrafficList: %v", err)
	}
	if page.Total != 1 || page.Items[0].ID != "rec-002" {
		t.Fatalf("store after delete = %d items, first %s", page.Total, page.Items[0].ID)
	}
}

func TestTrafficGetBodyReturnsSinglePart(t *testing.T) {
	repo, service := openFixture(t)
	ctx := context.Background()

	record := traffic.Record{
		ID:           "rec-001",
		Seq:          1,
		CapturedAt:   time.Now().UTC(),
		APIType:      "wx.request",
		Name:         "POST https://api.example.com",
		Method:       "POST",
		URL:          "https://api.example.com",
		Status:       traffic.StatusSuccess,
		RequestBody:  []byte(`{"payload":"request-side"}`),
		ResponseBody: []byte(`{"payload":"response-side"}`),
	}
	if _, err := traffic.Ingest(ctx, repo, []traffic.Record{record}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	api := NewTrafficAPI(service)
	got, err := api.TrafficGetBody(ctx, TrafficGetBodyParams{ID: "rec-001", Part: "request"})
	if err != nil {
		t.Fatalf("TrafficGetBody request: %v", err)
	}
	if string(got) != string(record.RequestBody) {
		t.Fatalf("request body mismatch: %q", got)
	}

	got, err = api.TrafficGetBody(ctx, TrafficGetBodyParams{ID: "rec-001", Part: "response"})
	if err != nil {
		t.Fatalf("TrafficGetBody response: %v", err)
	}
	if string(got) != string(record.ResponseBody) {
		t.Fatalf("response body mismatch: %q", got)
	}

	if _, err := api.TrafficGetBody(ctx, TrafficGetBodyParams{ID: "missing", Part: "request"}); err == nil {
		t.Fatal("expected error for missing body")
	}
}
