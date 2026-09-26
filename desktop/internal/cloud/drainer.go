// Package cloud bridges the Core's wxapi/cloud hook stream into the traffic
// store: it drains hook records page by page, converts each record into a
// traffic.Record and ingests them idempotently (captured_at + seq unique).
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/engine"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// Core is the subset of the engine client the drainer depends on.
type Core interface {
	HookDrain(ctx context.Context, name string, afterSeq int64, limit int) (engine.HookDrainPage, error)
}

// IngestFunc persists one batch of records; returns the inserted count.
type IngestFunc func(ctx context.Context, batch []traffic.Record) (int, error)

// DefaultDrainLimit bounds each hook.drain round trip.
const DefaultDrainLimit = 200

// Drainer pulls hook records from the Core and ingests them into the store.
type Drainer struct {
	core   Core
	ingest IngestFunc
	limit  int
}

// NewDrainer wires a drainer over any Core implementing HookDrain.
func NewDrainer(core Core, ingest IngestFunc) *Drainer {
	return &Drainer{core: core, ingest: ingest, limit: DefaultDrainLimit}
}

// DrainOnce follows hook.drain pages until hasMore is false, ingesting every
// record. It returns the new acknowledged sequence (the caller keeps it as its
// afterSeq pointer) and the number of ingested records.
func (d *Drainer) DrainOnce(ctx context.Context, name string, afterSeq int64) (int64, int, error) {
	ack := afterSeq
	inserted := 0
	for {
		page, err := d.core.HookDrain(ctx, name, ack, d.limit)
		if err != nil {
			return ack, inserted, err
		}
		if len(page.Records) > 0 {
			batch := make([]traffic.Record, 0, len(page.Records))
			for _, drained := range page.Records {
				batch = append(batch, Convert(name, drained))
			}
			n, err := d.ingest(ctx, batch)
			if err != nil {
				return ack, inserted, err
			}
			inserted += n
		}
		nextAck := ack
		if page.NextSeq > nextAck {
			nextAck = page.NextSeq
		}
		if !page.HasMore || nextAck <= ack {
			ack = nextAck
			return ack, inserted, nil
		}
		ack = nextAck
	}
}

// Convert turns one IPC-shaped hook record
// ({type,name,appId,ts,status,data,result?,error?}) into a traffic record.
func Convert(hookName string, drained engine.HookDrainedRecord) traffic.Record {
	record := drained.Record

	rec := traffic.Record{
		Status: traffic.StatusPending,
	}
	rec.Name = asString(record["name"])
	rec.AppID = asString(record["appId"])
	if rec.AppID == "" {
		rec.AppID = asString(record["appid"])
	}
	rec.APIType = asString(record["type"])
	if rec.APIType == "" {
		rec.APIType = hookName
	}

	// Identity includes appid, capture time and hook sequence so two running
	// mini programs can not collide even when they emit the same local seq.
	// The page stamps the same formula into rid, so a record delivered by the
	// event stream and by poll resolves to one row; fall back to recomputing it
	// for records the page sent before rid existed.
	ts := asInt64(record["ts"])
	if ts > 0 {
		rec.CapturedAt = time.UnixMilli(ts)
	}
	rec.Seq = drained.Seq
	rec.ID = asString(record["rid"])
	if rec.ID == "" {
		rec.ID = fmt.Sprintf("%s-%s-%d-%d", rec.APIType, rec.AppID, ts, drained.Seq)
	}
	// The hook settles asynchronously: a record may arrive pending and only
	// later be updated with how long the call took.
	rec.DurationMs = asInt64(record["durationMs"])

	switch asString(record["status"]) {
	case "success":
		rec.Status = traffic.StatusSuccess
	case "fail":
		rec.Status = traffic.StatusFail
	}

	// Request side: the hook's data payload, JSON-serialized.
	if data := record["data"]; data != nil {
		rec.RequestBody = mustJSON(data)
		if obj, ok := data.(map[string]any); ok {
			rec.URL = asString(obj["url"])
			rec.Method = asString(obj["method"])
		}
	}

	// Response side: result on success, error on failure.
	if result := record["result"]; result != nil {
		rec.ResponseBody = mustJSON(result)
	} else if errMsg := record["error"]; errMsg != nil {
		rec.ResponseBody = mustJSON(map[string]any{"error": errMsg})
	}

	return rec
}

func asString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func asInt64(value any) int64 {
	switch n := value.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}

func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"marshal_error":true}`)
	}
	return data
}
