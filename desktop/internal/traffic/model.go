// Package traffic persists captured wxapi/cloud traffic records in SQLite and
// serves paginated summaries with bodies read on demand.
package traffic

import (
	"time"
)

// Status is the lifecycle state of a captured record.
type Status string

const (
	StatusPending Status = "pending"
	StatusSuccess Status = "success"
	StatusFail    Status = "fail"
)

// Valid reports whether the status is one of the three contract values.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusSuccess, StatusFail:
		return true
	default:
		return false
	}
}

// BodyPart selects which compressed blob to fetch.
type BodyPart string

const (
	PartRequest  BodyPart = "request"
	PartResponse BodyPart = "response"
)

// TrafficSummary is the list model the frontend depends on; it never carries bodies.
type TrafficSummary struct {
	ID         string `json:"id"`
	Seq        int64  `json:"seq"`
	CapturedAt string `json:"capturedAt"` // RFC3339Nano
	APIType    string `json:"apiType"`
	// AppID is the miniapp that produced the record (empty when the capture
	// carried none). It travels with the summary so callers can filter and
	// display by app instead of deriving it from the record id.
	AppID         string `json:"appId"`
	Name          string `json:"name"`
	Method        string `json:"method,omitempty"`
	URL           string `json:"url,omitempty"`
	Status        Status `json:"status"`
	RequestBytes  int64  `json:"requestBytes"`
	ResponseBytes int64  `json:"responseBytes"`
	DurationMs    int64  `json:"durationMs"`
}

// TrafficPage is one page of summaries, newest first, together with the window
// it came from. Page and PageSize echo the values that took effect (the request
// may have asked for anything), and Total is how many rows the same filter
// matches — the panel's "共 N 条" and its page count both come from here, so a
// caller never has to guess whether it is on the last page.
type TrafficPage struct {
	Items    []TrafficSummary `json:"items"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"pageSize"`
}

// Record is a full traffic record including uncompressed bodies.
type Record struct {
	ID           string
	Seq          int64
	CapturedAt   time.Time
	APIType      string
	Name         string
	AppID        string
	Method       string
	URL          string
	Status       Status
	DurationMs   int64 `json:"durationMs"` // settled call time in ms; 0 while pending
	RequestBody  []byte
	ResponseBody []byte
}

// ListFilter narrows a page of summaries. Zero values mean "no filter".
type ListFilter struct {
	// Page is the 1-based page number; anything below 1 reads as 1 (a caller
	// that asked for page 0 meant the first page).
	Page int
	// PageSize is the rows per page; <= 0 takes DefaultPageLimit.
	PageSize int
	Query    string
	APIType  string
	Status   Status
	// AppID matches the stored appid exactly (the frontend used to derive it
	// from the record id, which only worked for one rid shape).
	AppID string
}
