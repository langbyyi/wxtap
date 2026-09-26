-- Traffic records carry the miniapp appid so records from two running targets
-- are never mixed up in one another's listings or appid-scoped lookups.
ALTER TABLE traffic_records ADD COLUMN appid TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_traffic_appid_captured
    ON traffic_records (appid, captured_at DESC, seq DESC);