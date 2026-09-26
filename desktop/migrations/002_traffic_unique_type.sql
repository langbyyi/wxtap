-- wxapi and cloud pages keep independent sequence counters, so the
-- idempotency key must include the record type: (captured_at, seq) alone
-- silently dropped the second record when both types captured within the
-- same millisecond.
CREATE TABLE traffic_records_v2 (
    id             TEXT PRIMARY KEY,
    seq            INTEGER NOT NULL,
    captured_at    INTEGER NOT NULL, -- unix nanoseconds
    api_type       TEXT NOT NULL,
    name           TEXT NOT NULL,
    method         TEXT NOT NULL DEFAULT '',
    url            TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL CHECK (status IN ('pending', 'success', 'fail')),
    request_bytes  INTEGER NOT NULL DEFAULT 0,
    response_bytes INTEGER NOT NULL DEFAULT 0,
    request_body   BLOB,
    response_body  BLOB,
    UNIQUE (api_type, captured_at, seq)
);

-- The old key never admitted (captured_at, seq) duplicates, so this copy
-- cannot violate the new constraint.
INSERT INTO traffic_records_v2
    SELECT id, seq, captured_at, api_type, name, method, url, status,
           request_bytes, response_bytes, request_body, response_body
    FROM traffic_records;
DROP TABLE traffic_records;
ALTER TABLE traffic_records_v2 RENAME TO traffic_records;

CREATE INDEX IF NOT EXISTS idx_traffic_captured ON traffic_records (captured_at, seq);
CREATE INDEX IF NOT EXISTS idx_traffic_api_type ON traffic_records (api_type);
CREATE INDEX IF NOT EXISTS idx_traffic_status ON traffic_records (status);
