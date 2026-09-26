package main

import "sync"

// consoleCapacity bounds the in-memory console ring. The page-side hook keeps
// its own 1000-record buffer; this one holds what the shell has already
// drained, so a chatty miniapp cannot grow the process without limit.
const consoleCapacity = 1000

// consoleRecord is one captured console line as console.list returns it.
type consoleRecord struct {
	Seq    int64          `json:"seq"`
	Record map[string]any `json:"record"`
}

// consoleLog is a bounded, sequence-numbered ring of captured console records.
//
// The panel reads it with "everything after the last seq I saw" instead of
// mixing a live event stream with a poll, which is what makes the view free of
// duplicated rows. Sequence numbers keep increasing across Clear: the panel's
// marker stays monotonic, so a clear never resurrects old rows.
type consoleLog struct {
	mu      sync.Mutex
	seq     int64
	epoch   int64
	records []consoleRecord
	// clearedThrough is the highest seq that existed when the ring was last
	// cleared. Those records were discarded on purpose, so a later reader must
	// not see them as evictions: the gap report starts from here.
	clearedThrough int64
}

// Epoch reports the current generation of the ring. A collector takes it
// before draining and passes it back to Append: a Clear that lands in between
// must discard that batch, or rows the user just cleared reappear as new ones.
func (c *consoleLog) Epoch() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

// Append adds drained records to the ring, dropping the batch when the ring was
// cleared after the caller took its epoch.
func (c *consoleLog) Append(records []map[string]any, epoch int64) {
	if len(records) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.epoch {
		return
	}
	for _, record := range records {
		c.seq++
		c.records = append(c.records, consoleRecord{Seq: c.seq, Record: record})
	}
	if overflow := len(c.records) - consoleCapacity; overflow > 0 {
		c.records = append(c.records[:0], c.records[overflow:]...)
	}
}

// List returns at most limit records with seq > afterSeq in sequence order, the
// highest seq it returned (or afterSeq when there was nothing new), whether
// more records remain beyond that page, and how many records were evicted
// before the page starts — the only way a caller can tell it missed rows.
func (c *consoleLog) List(afterSeq int64, limit int) (records []consoleRecord, nextSeq int64, hasMore bool, dropped int64) {
	if limit < 1 {
		limit = 1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]consoleRecord, 0, limit)
	nextSeq = afterSeq
	hasMore = false
	if len(c.records) > 0 {
		// A gap is only readable records that were evicted. The last Clear moved
		// everything up to clearedThrough away on the user's behalf, so the gap
		// is measured from whichever of the two is later: a panel that zeroes
		// its marker on clear (the Console view does) must not read the whole
		// cleared run back as "discarded".
		floor := afterSeq
		if c.clearedThrough > floor {
			floor = c.clearedThrough
		}
		if oldest := c.records[0].Seq; oldest > floor+1 {
			dropped = oldest - floor - 1
		}
	}
	for _, entry := range c.records {
		if entry.Seq <= afterSeq {
			continue
		}
		if len(out) == limit {
			hasMore = true
			break
		}
		out = append(out, entry)
		nextSeq = entry.Seq
	}
	return out, nextSeq, hasMore, dropped
}

// Tail returns the newest limit records in sequence order. MCP's
// miniapp_console_log asks for "recent output", which a from-zero List cannot
// express: once the ring is full it would return the oldest rows instead.
// An empty ring answers an empty (non-nil) slice: the JSON contract is always
// an array, never null.
func (c *consoleLog) Tail(limit int) []consoleRecord {
	out := make([]consoleRecord, 0)
	if limit < 1 {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	start := len(c.records) - limit
	if start < 0 {
		start = 0
	}
	return append(out, c.records[start:]...)
}

// Clear drops every buffered record without resetting the sequence, and bumps
// the epoch so an in-flight drain cannot re-add what was just cleared. It also
// remembers how far the sequence had got: everything up to that point left the
// ring by the user's own request, which is not the same thing as an eviction.
func (c *consoleLog) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = nil
	c.clearedThrough = c.seq
	c.epoch++
}
