-- The page settles asynchronous calls after the drain has already stored the
-- record as pending: the settled status and how long the call took arrive on a
-- second delivery path and are written in place by
-- Repository.ApplyUpdates. Existing rows predate the update stream, so they
-- keep the 0 default.
ALTER TABLE traffic_records ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0;
