-- The idempotency key must include appid: two running mini programs can
-- emit the same api type, timestamp, and page-local sequence without being
-- duplicates of one another.
CREATE TABLE traffic_records_v3 (
    id             TEXT PRIMARY KEY,
    seq            INTEGER NOT NULL,
    captured_at    INTEGER NOT NULL, -- unix nanoseconds
    api_type       TEXT NOT NULL,
    name           TEXT NOT NULL,
    appid          TEXT NOT NULL DEFAULT '',
    method         TEXT NOT NULL DEFAULT '',
    url            TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL CHECK (status IN ('pending', 'success', 'fail')),
    request_bytes  INTEGER NOT NULL DEFAULT 0,
    response_bytes INTEGER NOT NULL DEFAULT 0,
    request_body   BLOB,
    response_body  BLOB,
    UNIQUE (appid, api_type, captured_at, seq)
);

INSERT INTO traffic_records_v3
    SELECT id, seq, captured_at, api_type, name, appid, method, url, status,
           request_bytes, response_bytes, request_body, response_body
    FROM traffic_records;
DROP TABLE traffic_records;
ALTER TABLE traffic_records_v3 RENAME TO traffic_records;

CREATE INDEX IF NOT EXISTS idx_traffic_captured ON traffic_records (captured_at, seq);
CREATE INDEX IF NOT EXISTS idx_traffic_api_type ON traffic_records (api_type);
CREATE INDEX IF NOT EXISTS idx_traffic_status ON traffic_records (status);
CREATE INDEX IF NOT EXISTS idx_traffic_appid_captured ON traffic_records (appid, captured_at DESC, seq DESC);
