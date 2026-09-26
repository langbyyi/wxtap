package main

import "sync"

// runtimeLogCapacity bounds the in-memory runtime log ring. It mirrors the
// console ring: a chatty Core must not grow the process without limit, and the
// panel only renders the newest few hundred lines anyway.
const runtimeLogCapacity = 1000

// runtimeLogRecord is one runtime log line as log.list returns it.
type runtimeLogRecord struct {
	Seq     int64  `json:"seq"`
	Time    string `json:"time,omitempty"`
	Level   string `json:"level,omitempty"`
	Message string `json:"message"`
}

// runtimeLog is a bounded, sequence-numbered ring of runtime log lines.
//
// The `log` Wails event is fire-and-forget: a line emitted before the webview
// subscribed (startup, early engine output) is gone for the panel. Append
// keeps a replayable copy so log.list can hand those lines back once the
// frontend asks. Sequence numbers keep increasing across Clear, so a frontend
// marker (or a dedupe against delivered events) never sees stale rows again.
type runtimeLog struct {
	mu      sync.Mutex
	seq     int64
	dropped int64
	records []runtimeLogRecord
}

// Append stores one line and returns its sequence number. Append happens
// before the event is broadcast so a replay snapshot is guaranteed to cover
// every event already delivered.
func (l *runtimeLog) Append(time, level, message string) int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	l.records = append(l.records, runtimeLogRecord{Seq: l.seq, Time: time, Level: level, Message: message})
	if overflow := len(l.records) - runtimeLogCapacity; overflow > 0 {
		l.records = l.records[overflow:]
		l.dropped += int64(overflow)
	}
	return l.seq
}

// Tail returns the newest limit records in sequence order (the whole ring when
// limit is unset), the highest returned seq (0 when empty), and how many
// records the ring has evicted in total — the reading a replay panel turns
// into its「已丢弃 N 条」notice.
func (l *runtimeLog) Tail(limit int) ([]runtimeLogRecord, int64, int64) {
	if limit < 1 || limit > runtimeLogCapacity {
		limit = runtimeLogCapacity
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	start := len(l.records) - limit
	if start < 0 {
		start = 0
	}
	out := make([]runtimeLogRecord, len(l.records)-start)
	copy(out, l.records[start:])
	nextSeq := int64(0)
	if len(out) > 0 {
		nextSeq = out[len(out)-1].Seq
	}
	return out, nextSeq, l.dropped
}

// Clear drops every buffered record and the eviction counter (the user just
// acknowledged that history) without resetting the sequence: live events must
// never reuse a seq a replay has already handed out.
func (l *runtimeLog) Clear() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = nil
	l.dropped = 0
}
