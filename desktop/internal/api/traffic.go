// Package api holds the binding-layer types the Wails shell exposes to the
// frontend. These are the only traffic methods the Vue app may rely on.
package api

import (
	"context"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// TrafficAPI forwards frontend traffic calls to the storage service.
type TrafficAPI struct {
	service *traffic.Service
}

// NewTrafficAPI wraps a traffic service.
func NewTrafficAPI(service *traffic.Service) *TrafficAPI {
	return &TrafficAPI{service: service}
}

// TrafficListParams is the traffic.list request payload. Page is 1-based; both
// fields are clamped (page below 1 reads as 1, pageSize to 1..MaxListLimit).
type TrafficListParams struct {
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"pageSize,omitempty"`
	Query    string `json:"query,omitempty"`
	APIType  string `json:"apiType,omitempty"`
	Status   string `json:"status,omitempty"`
	AppID    string `json:"appId,omitempty"`
}

// TrafficList returns one page of summaries (100 by default, newest first)
// together with the total the filter matches.
func (a *TrafficAPI) TrafficList(ctx context.Context, params TrafficListParams) (traffic.TrafficPage, error) {
	filter := traffic.ListFilter{
		Page:     params.Page,
		PageSize: params.PageSize,
		Query:    params.Query,
		APIType:  params.APIType,
		Status:   traffic.Status(params.Status),
		AppID:    params.AppID,
	}
	return a.service.List(ctx, filter)
}

// TrafficDeleteParams is the traffic.delete request payload.
type TrafficDeleteParams struct {
	IDs []string `json:"ids"`
}

// TrafficDelete removes the named records.
func (a *TrafficAPI) TrafficDelete(ctx context.Context, params TrafficDeleteParams) (traffic.DeleteResult, error) {
	return a.service.Delete(ctx, params.IDs)
}

// TrafficGetBodyParams is the traffic.getBody request payload.
type TrafficGetBodyParams struct {
	ID   string `json:"id"`
	Part string `json:"part"` // "request" | "response"
}

// TrafficGetBody returns one decompressed body part for a record.
func (a *TrafficAPI) TrafficGetBody(ctx context.Context, params TrafficGetBodyParams) ([]byte, error) {
	return a.service.GetBody(ctx, params.ID, traffic.BodyPart(params.Part))
}

// TrafficStats returns the traffic.stats payload. The dropped-record counters
// stay 0 here: they belong to the page-side hook buffers, not to storage.
func (a *TrafficAPI) TrafficStats(ctx context.Context) (traffic.Stats, error) {
	return a.service.Stats(ctx)
}

// TrafficAppIDs returns one row per mini program that has stored records
// (traffic.appids): the appid, its record count and the newest capture time.
func (a *TrafficAPI) TrafficAppIDs(ctx context.Context) ([]traffic.AppIDStat, error) {
	return a.service.AppIDStats(ctx)
}

// TrafficClear removes every stored record. There is no dry-run counterpart:
// the confirmation dialog shows traffic.stats' record count, which the page has
// already read, and the clear itself is one unconditional statement.
func (a *TrafficAPI) TrafficClear(ctx context.Context) (traffic.DeleteResult, error) {
	return a.service.Clear(ctx)
}
