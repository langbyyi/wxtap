-- Traffic records: summary columns for listing, bodies stored as compressed blobs.
CREATE TABLE IF NOT EXISTS traffic_records (
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
    UNIQUE (captured_at, seq)
);

CREATE INDEX IF NOT EXISTS idx_traffic_captured ON traffic_records (captured_at, seq);
CREATE INDEX IF NOT EXISTS idx_traffic_api_type ON traffic_records (api_type);
CREATE INDEX IF NOT EXISTS idx_traffic_status ON traffic_records (status);
