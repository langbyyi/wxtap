package cloud

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/langbyyi/wxtap/desktop/internal/engine"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

type fakeCore struct {
	pages map[int64]engine.HookDrainPage
}

func (f *fakeCore) HookInstall(context.Context, string) (engine.HookInstallRecord, error) {
	return engine.HookInstallRecord{"ok": true}, nil
}

func (f *fakeCore) HookDrain(_ context.Context, _ string, afterSeq int64, _ int) (engine.HookDrainPage, error) {
	if page, ok := f.pages[afterSeq]; ok {
		return page, nil
	}
	return engine.HookDrainPage{Records: []engine.HookDrainedRecord{}, NextSeq: afterSeq}, nil
}

func hookRecord(seq int64, ts int64) engine.HookDrainedRecord {
	return engine.HookDrainedRecord{
		Seq: seq,
		Record: map[string]any{
			"type":   "wx.request",
			"name":   "GET https://api.example.com",
			"appId":  "wx123",
			"ts":     float64(ts),
			"status": "success",
			"data":   map[string]any{"url": "https://api.example.com", "method": "GET"},
			"result": map[string]any{"ok": seq},
		},
	}
}

func TestDrainOnceFollowsPagesAndIngestsAll(t *testing.T) {
	core := &fakeCore{pages: map[int64]engine.HookDrainPage{
		0: {
			Records: []engine.HookDrainedRecord{hookRecord(1, 1700000000000), hookRecord(2, 1700000000001)},
			NextSeq: 2,
			HasMore: true,
		},
		2: {
			Records: []engine.HookDrainedRecord{hookRecord(3, 1700000000002)},
			NextSeq: 3,
			HasMore: false,
		},
	}}

	var batches [][]traffic.Record
	drainer := NewDrainer(core, func(_ context.Context, batch []traffic.Record) (int, error) {
		batches = append(batches, batch)
		return len(batch), nil
	})

	ack, inserted, err := drainer.DrainOnce(context.Background(), "wxapi", 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if ack != 3 {
		t.Fatalf("ack %d, want 3", ack)
	}
	if inserted != 3 {
		t.Fatalf("inserted %d, want 3", inserted)
	}
	total := 0
	for _, batch := range batches {
		total += len(batch)
	}
	if total != 3 {
		t.Fatalf("ingested %d records total, want 3", total)
	}

	first := batches[0][0]
	if first.APIType != "wx.request" || first.URL != "https://api.example.com" || first.Method != "GET" {
		t.Fatalf("record conversion lost fields: %+v", first)
	}
	if first.CapturedAt.UnixMilli() != 1700000000000 {
		t.Fatalf("capturedAt mismatch: %v", first.CapturedAt)
	}
	var requestBody map[string]any
	if err := json.Unmarshal(first.RequestBody, &requestBody); err != nil || requestBody["url"] == nil {
		t.Fatalf("request body not JSON: %v %q", err, first.RequestBody)
	}
	var responseBody map[string]any
	if err := json.Unmarshal(first.ResponseBody, &responseBody); err != nil || responseBody["ok"] == nil {
		t.Fatalf("response body not JSON: %v %q", err, first.ResponseBody)
	}
}

func TestDrainOnceMapsFailStatusAndErrorBody(t *testing.T) {
	core := &fakeCore{pages: map[int64]engine.HookDrainPage{
		0: {
			Records: []engine.HookDrainedRecord{{
				Seq: 1,
				Record: map[string]any{
					"type": "wx.auth", "name": "login", "appId": "wx1",
					"ts": float64(1700000000000), "status": "fail",
					"data": map[string]any{}, "error": "login failed",
				},
			}},
			NextSeq: 1,
		},
	}}

	var ingested []traffic.Record
	drainer := NewDrainer(core, func(_ context.Context, batch []traffic.Record) (int, error) {
		ingested = batch
		return len(batch), nil
	})
	if _, _, err := drainer.DrainOnce(context.Background(), "wxapi", 0); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(ingested) != 1 || ingested[0].Status != traffic.StatusFail {
		t.Fatalf("status not mapped: %+v", ingested)
	}
	var errBody map[string]any
	if err := json.Unmarshal(ingested[0].ResponseBody, &errBody); err != nil || errBody["error"] != "login failed" {
		t.Fatalf("error body not preserved: %q", ingested[0].ResponseBody)
	}
}

// The page stamps a stable rid into every record; it is the traffic record id,
// so the event stream and poll resolve to one row instead of two.
func TestConvertPrefersRIDAsIdentity(t *testing.T) {
	const rid = "wx.request-wxone-1700000000000-7"
	rec := Convert("wxapi", engine.HookDrainedRecord{
		Seq: 7,
		Record: map[string]any{
			"type": "wx.request", "name": "request", "appId": "wxone",
			"ts": float64(1700000000000), "rid": rid, "status": "pending",
		},
	})
	if rec.ID != rid {
		t.Fatalf("id = %q, want the page rid %q", rec.ID, rid)
	}
}

// Records the page sent before rid existed (or with a malformed one) still get
// the computed identity.
func TestConvertFallsBackToComputedIDWithoutRID(t *testing.T) {
	const want = "wx.request-wxone-1700000000000-7"
	cases := []struct {
		name string
		rid  any
	}{
		{"missing", nil},
		{"empty", ""},
		{"not a string", float64(7)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := map[string]any{
				"type": "wx.request", "name": "request", "appId": "wxone",
				"ts": float64(1700000000000), "status": "pending",
			}
			if tc.rid != nil {
				record["rid"] = tc.rid
			}
			rec := Convert("wxapi", engine.HookDrainedRecord{Seq: 7, Record: record})
			if rec.ID != want {
				t.Fatalf("id = %q, want the computed %q", rec.ID, want)
			}
		})
	}
}

// durationMs rides in as a JSON number (float64 after decoding) and is absent
// on records the hook never settled.
func TestConvertReadsDurationMs(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  int64
	}{
		{"missing", nil, 0},
		{"json number", float64(1830), 1830},
		{"integer", int64(1830), 1830},
		{"not a number", "1830", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := map[string]any{
				"type": "wx.request", "name": "request", "appId": "wxone",
				"ts": float64(1700000000000), "status": "success",
			}
			if tc.value != nil {
				record["durationMs"] = tc.value
			}
			rec := Convert("wxapi", engine.HookDrainedRecord{Seq: 7, Record: record})
			if rec.DurationMs != tc.want {
				t.Fatalf("durationMs = %d, want %d", rec.DurationMs, tc.want)
			}
		})
	}
}

func TestDrainOnceSurvivesRecordsWithoutTimestamps(t *testing.T) {
	core := &fakeCore{pages: map[int64]engine.HookDrainPage{
		0: {
			Records: []engine.HookDrainedRecord{{
				Seq:    1,
				Record: map[string]any{"type": "wx.bridge", "name": "naked", "status": "pending"},
			}},
			NextSeq: 1,
		},
	}}

	var ingested []traffic.Record
	drainer := NewDrainer(core, func(_ context.Context, batch []traffic.Record) (int, error) {
		ingested = batch
		return len(batch), nil
	})
	if _, _, err := drainer.DrainOnce(context.Background(), "wxapi", 0); err != nil {
		t.Fatalf("drain without ts: %v", err)
	}
	if len(ingested) != 1 || ingested[0].Status != traffic.StatusPending {
		t.Fatalf("degenerate record not ingested: %+v", ingested)
	}
}
