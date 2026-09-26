package traffic

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// listSQL selects summary columns only; body blobs are fetched separately by GetBody.
const listSQL = `SELECT id, seq, captured_at, api_type, appid, name, method, url, status, request_bytes, response_bytes, duration_ms
FROM traffic_records`

const bodySQL = `SELECT request_body, response_body FROM traffic_records WHERE id = ?`

// MaxListLimit caps one page of traffic summaries. The limit arrives from
// frontend and MCP callers.
const MaxListLimit = 1000

// Repository owns the SQLite connection and applies migrations on open.
type Repository struct {
	db *sql.DB

	// reclaimHooks replaces the space-reclamation steps in tests; nil means the
	// real PRAGMA/VACUUM run.
	reclaimHooks *reclaimHooks
}

// Open opens (creating if needed) the database at dbPath and applies any
// pending migrations from migrationsDir (sorted by file name).
func Open(dbPath, migrationsDir string) (*Repository, error) {
	dsn := "file:" + dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := applyMigrations(db, migrationsDir); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Repository{db: db}, nil
}

// Close releases the database connection.
func (r *Repository) Close() error {
	return r.db.Close()
}

func applyMigrations(db *sql.DB, migrationsDir string) error {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, name := range files {
		var applied int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}
		script, err := os.ReadFile(filepath.Join(migrationsDir, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(string(script)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, time.Now().UnixNano()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// InsertBatch stores records idempotently: a batch replayed with the same
// (api_type, captured_at, seq) keys inserts nothing new. It returns the
// inserted count.
func (r *Repository) InsertBatch(ctx context.Context, records []Record) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO traffic_records
		(id, seq, captured_at, api_type, name, appid, method, url, status, request_bytes, response_bytes, request_body, response_body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	inserted := 0
	for _, record := range records {
		compressedRequest, err := compress(record.RequestBody)
		if err != nil {
			return inserted, fmt.Errorf("compress request body %s: %w", record.ID, err)
		}
		compressedResponse, err := compress(record.ResponseBody)
		if err != nil {
			return inserted, fmt.Errorf("compress response body %s: %w", record.ID, err)
		}
		result, err := stmt.ExecContext(ctx,
			record.ID, record.Seq, record.CapturedAt.UnixNano(), record.APIType, record.Name, record.AppID,
			record.Method, record.URL, string(record.Status),
			len(record.RequestBody), len(record.ResponseBody),
			compressedRequest, compressedResponse,
		)
		if err != nil {
			return inserted, fmt.Errorf("insert record %s: %w", record.ID, err)
		}
		if affected, err := result.RowsAffected(); err == nil && affected > 0 {
			inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		return inserted, fmt.Errorf("commit insert: %w", err)
	}
	return inserted, nil
}

// ApplyUpdates writes the settled fields of records that are already stored and
// returns the number of rows it changed.
//
// Only the response side and the duration are written: a settle frame arrives
// after the pending insert, and rewriting the request side or captured_at
// would replace captured evidence with whatever the late frame happens to
// carry. A frame for a row the store has not seen yet (the page buffer drained
// ahead of the insert) matches nothing and is not an error, so a replay of the
// same updates is safe.
//
// A frame that carries no response at all (ResponseBody nil, hence a zero
// response byte count) settles status and duration_ms only and leaves the
// stored response_body/response_bytes untouched. The update stream can deliver
// the terminal status and the call time before — or without — the response
// payload, so overwriting unconditionally would blank a body the record stream
// had already committed: silent, irreversible data loss that no later frame
// can repair. Carrying a response (including an explicitly empty body) means
// overwriting all four settled fields, so "empty response" stays
// distinguishable from "no response in this frame".
func (r *Repository) ApplyUpdates(ctx context.Context, records []Record) (int, error) {
	if len(records) == 0 {
		return 0, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Two statements instead of CASE WHEN: a frame either carries a response
	// (all four settled fields) or does not (status and duration only), and the
	// choice is per record in the same transaction.
	withResponse, err := tx.PrepareContext(ctx, `UPDATE traffic_records
		SET status = ?, response_body = ?, response_bytes = ?, duration_ms = ?
		WHERE id = ?`)
	if err != nil {
		return 0, fmt.Errorf("prepare update: %w", err)
	}
	defer func() { _ = withResponse.Close() }()

	statusOnly, err := tx.PrepareContext(ctx, `UPDATE traffic_records
		SET status = ?, duration_ms = ?
		WHERE id = ?`)
	if err != nil {
		return 0, fmt.Errorf("prepare status-only update: %w", err)
	}
	defer func() { _ = statusOnly.Close() }()

	updated := 0
	for _, record := range records {
		var result sql.Result
		if record.ResponseBody == nil {
			result, err = statusOnly.ExecContext(ctx, string(record.Status), record.DurationMs, record.ID)
		} else {
			compressedResponse, compressErr := compress(record.ResponseBody)
			if compressErr != nil {
				return updated, fmt.Errorf("compress response body %s: %w", record.ID, compressErr)
			}
			result, err = withResponse.ExecContext(ctx, string(record.Status), compressedResponse,
				len(record.ResponseBody), record.DurationMs, record.ID)
		}
		if err != nil {
			return updated, fmt.Errorf("update record %s: %w", record.ID, err)
		}
		if affected, err := result.RowsAffected(); err == nil {
			updated += int(affected)
		}
	}
	if err := tx.Commit(); err != nil {
		return updated, fmt.Errorf("commit update: %w", err)
	}
	return updated, nil
}

// listWhere builds the WHERE clause shared by a page and the count that
// describes it. Both must see the same conditions: the panel renders "共 N 条"
// next to the rows of one page, and a count computed from a different filter
// would contradict them.
func listWhere(filter ListFilter) (string, []any) {
	var clauses []string
	var args []any
	if filter.Query != "" {
		pattern := "%" + filter.Query + "%"
		clauses = append(clauses, `(name LIKE ? OR url LIKE ?)`)
		args = append(args, pattern, pattern)
	}
	if filter.APIType != "" {
		clauses = append(clauses, `api_type = ?`)
		args = append(args, filter.APIType)
	}
	if filter.AppID != "" {
		clauses = append(clauses, `appid = ?`)
		args = append(args, filter.AppID)
	}
	if filter.Status != "" {
		clauses = append(clauses, `status = ?`)
		args = append(args, string(filter.Status))
	}
	return strings.Join(clauses, " AND "), args
}

// scanSummaries materialises summary rows; bodies are never part of a list.
func scanSummaries(rows *sql.Rows) ([]TrafficSummary, error) {
	items := []TrafficSummary{}
	for rows.Next() {
		var summary TrafficSummary
		var capturedNanos int64
		if err := rows.Scan(&summary.ID, &summary.Seq, &capturedNanos, &summary.APIType, &summary.AppID, &summary.Name,
			&summary.Method, &summary.URL, &summary.Status, &summary.RequestBytes, &summary.ResponseBytes,
			&summary.DurationMs); err != nil {
			return nil, fmt.Errorf("scan summary: %w", err)
		}
		summary.CapturedAt = time.Unix(0, capturedNanos).UTC().Format(time.RFC3339Nano)
		items = append(items, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate summaries: %w", err)
	}
	return items, nil
}

// List returns one page of summaries newest first, plus how many rows the same
// filter matches in total. The page number is 1-based and the size is clamped
// to [1, MaxListLimit]; both take effect in the returned page.
func (r *Repository) List(ctx context.Context, filter ListFilter) (TrafficPage, error) {
	if filter.Status != "" && !filter.Status.Valid() {
		return TrafficPage{}, fmt.Errorf("invalid status %q", filter.Status)
	}
	pageSize := filter.PageSize
	if pageSize <= 0 {
		pageSize = DefaultPageLimit
	}
	if pageSize > MaxListLimit {
		pageSize = MaxListLimit
	}
	page := filter.Page
	if page < 1 {
		page = 1
	}
	where, args := listWhere(filter)
	suffix := ""
	if where != "" {
		suffix = " WHERE " + where
	}

	// The count and the page run in one read transaction: two independent
	// statements could straddle an insert and report a total that does not
	// describe the rows travelling with it, and the panel shows both together.
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TrafficPage{}, fmt.Errorf("begin list: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_records`+suffix, args...).Scan(&total); err != nil {
		return TrafficPage{}, fmt.Errorf("count traffic: %w", err)
	}

	// Newest first: page 1 is the window a user watching a capture in progress
	// wants, and it keeps the page a returned deletion leaves behind stable.
	// The id tie-break is load-bearing — the uniqueness key includes api_type,
	// so (captured_at, seq) alone does not order two records in the same
	// millisecond — and OFFSET needs a total order to avoid skipping rows.
	query := listSQL + suffix + ` ORDER BY captured_at DESC, seq DESC, id DESC LIMIT ? OFFSET ?`
	rows, err := tx.QueryContext(ctx, query, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return TrafficPage{}, fmt.Errorf("list traffic: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items, err := scanSummaries(rows)
	if err != nil {
		return TrafficPage{}, err
	}
	return TrafficPage{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// deleteChunk bounds one DELETE statement. modernc.org/sqlite inherits
// SQLITE_MAX_VARIABLE_NUMBER (32766 in the builds we ship), and a select-all
// page delete can hand us up to MaxListLimit ids per page — chunking keeps the
// statement well inside the limit no matter how many pages a caller collects.
const deleteChunk = 500

// Delete removes the records with the given ids and reports how many rows went
// away.
//
// A row that is already gone (or an id listed twice) is not an error: the
// caller deletes what it can see, and "someone else got there first" must not
// turn into a failed delete that leaves the rest of the selection in place.
// Repeating an id would otherwise be counted twice, so the ids are deduplicated
// first: the returned count means rows removed.
func (r *Repository) Delete(ctx context.Context, ids []string) (DeleteResult, error) {
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return DeleteResult{}, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var deleted int64
	for start := 0; start < len(unique); start += deleteChunk {
		chunk := unique[start:min(start+deleteChunk, len(unique))]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		// id is the primary key, so this is an index lookup per id rather than a
		// scan of a table whose rows carry two body blobs.
		//nolint:gosec // G202: the concatenated text is only the "?" placeholder list; every id is bound through args.
		res, err := tx.ExecContext(ctx, `DELETE FROM traffic_records WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return DeleteResult{}, fmt.Errorf("delete traffic: %w", err)
		}
		if affected, err := res.RowsAffected(); err == nil {
			deleted += affected
		}
	}
	if err := tx.Commit(); err != nil {
		// Nothing was removed, so no count may travel with the error: the caller
		// would report a deletion that never happened.
		return DeleteResult{}, fmt.Errorf("commit delete: %w", err)
	}
	return DeleteResult{Deleted: deleted, ReclamationFailed: r.reclaim(ctx, deleted, true)}, nil
}

// Clear removes every stored record and reports how many rows went away. It is
// the only bulk deletion: the store no longer trims itself on a retention
// policy, so this — the user's own confirmed action — is what keeps it from
// growing forever.
//
// Space reclamation follows Delete's rule (always a WAL checkpoint, a VACUUM
// only once the deletion is large enough to be worth the write lock), which for
// a full clear means the file is rewritten back to its empty size.
func (r *Repository) Clear(ctx context.Context) (DeleteResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("begin clear: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM traffic_records`)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("clear traffic: %w", err)
	}
	deleted, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		// Nothing was removed, so no count may travel with the error: the caller
		// would report a deletion that never happened.
		return DeleteResult{}, fmt.Errorf("commit clear: %w", err)
	}
	return DeleteResult{Deleted: deleted, ReclamationFailed: r.reclaim(ctx, deleted, true)}, nil
}

// RecentRecordsDefault and MaxRecentRecords bound one recent-records read.
// The caller gets decompressed bodies, so scanning the whole database would
// make the read unbounded.
const (
	RecentRecordsDefault = 200
	MaxRecentRecords     = 1000
)

// RecentRecordsAllApps returns the newest records across every mini program,
// including decompressed bodies. The limit keeps the read bounded when the
// store holds traffic from many apps.
func (r *Repository) RecentRecordsAllApps(ctx context.Context, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = RecentRecordsDefault
	}
	if limit > MaxRecentRecords {
		limit = MaxRecentRecords
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, seq, captured_at, api_type, name, appid, method, url, status, request_body, response_body
		FROM traffic_records ORDER BY captured_at DESC, seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent traffic: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanRecordRows(rows)
}

// scanRecordRows materialises traffic rows together with their decompressed
// bodies; both recent-record readers share it.
func scanRecordRows(rows *sql.Rows) ([]Record, error) {
	records := []Record{}
	for rows.Next() {
		var record Record
		var capturedNanos int64
		var status string
		var request, response []byte
		if err := rows.Scan(&record.ID, &record.Seq, &capturedNanos, &record.APIType, &record.Name, &record.AppID,
			&record.Method, &record.URL, &status, &request, &response); err != nil {
			return nil, fmt.Errorf("scan recent traffic: %w", err)
		}
		record.CapturedAt = time.Unix(0, capturedNanos).UTC()
		record.Status = Status(status)
		var err error
		record.RequestBody, err = decompressLimited(request, MaxCorrelationBodyBytes)
		if err != nil {
			return nil, fmt.Errorf("decompress request body %s: %w", record.ID, err)
		}
		record.ResponseBody, err = decompressLimited(response, MaxCorrelationBodyBytes)
		if err != nil {
			return nil, fmt.Errorf("decompress response body %s: %w", record.ID, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent traffic: %w", err)
	}
	return records, nil
}

// MaxAuditRecords bounds one RecentRecordsForAudit read. The audit engine
// replays what it reads against live endpoints, so the bound keeps both the
// query and the resulting replay campaign finite.
const MaxAuditRecords = 500

// RecentRecordsForAudit returns the newest records for the audit engine,
// including decompressed bodies with the same semantics as
// RecentRecordsAllApps. An empty apiType skips the API-type filter; a
// non-empty urlQuery keeps only records whose url contains it as a substring.
// The limit is clamped into [1, MaxAuditRecords] (0 and negatives read as 1).
func (r *Repository) RecentRecordsForAudit(ctx context.Context, apiType, urlQuery string, limit int) ([]Record, error) {
	return r.recentRecordsForAudit(ctx, "", apiType, urlQuery, limit)
}

// RecentRecordsForAppID narrows RecentRecordsForAudit to one mini program
// (appid exact match). Empty appid reads across programs, matching
// RecentRecordsForAudit.
func (r *Repository) RecentRecordsForAppID(ctx context.Context, appid, apiType, urlQuery string, limit int) ([]Record, error) {
	return r.recentRecordsForAudit(ctx, appid, apiType, urlQuery, limit)
}

func (r *Repository) recentRecordsForAudit(ctx context.Context, appid, apiType, urlQuery string, limit int) ([]Record, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > MaxAuditRecords {
		limit = MaxAuditRecords
	}
	var clauses []string
	var args []any
	if appid != "" {
		clauses = append(clauses, `appid = ?`)
		args = append(args, appid)
	}
	if apiType != "" {
		clauses = append(clauses, `api_type = ?`)
		args = append(args, apiType)
	}
	if urlQuery != "" {
		clauses = append(clauses, `url LIKE ?`)
		args = append(args, "%"+urlQuery+"%")
	}
	suffix := ""
	if len(clauses) > 0 {
		suffix = " WHERE " + strings.Join(clauses, " AND ")
	}
	// seq alone does not order rows across capture sessions, so id is the
	// deterministic tie-break (same rationale as List's ordering). suffix 只由
	// 上面两张固定子句表拼出，值全部走 ? 参数，不存在外部注入面。
	//nolint:gosec // G202: suffix is built from a fixed clause set; every value is a bound parameter.
	rows, err := r.db.QueryContext(ctx, `SELECT id, seq, captured_at, api_type, name, appid, method, url, status, request_body, response_body
		FROM traffic_records`+suffix+` ORDER BY seq DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("recent traffic for audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanRecordRows(rows)
}

// GetBody returns the decompressed request or response body of one record.
func (r *Repository) GetBody(ctx context.Context, id string, part BodyPart) ([]byte, error) {
	if part != PartRequest && part != PartResponse {
		return nil, fmt.Errorf("invalid body part %q", part)
	}
	var request, response []byte
	err := r.db.QueryRowContext(ctx, bodySQL, id).Scan(&request, &response)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("traffic record %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("load body for %s: %w", id, err)
	}
	blob := request
	if part == PartResponse {
		blob = response
	}
	if blob == nil {
		return nil, nil
	}
	// Body viewers do not need unbounded inflation; keep a hostile database
	// from turning one GetBody call into a decompression bomb.
	return decompressLimited(blob, MaxBodyBytes)
}

// compressThreshold is the body size below which gzip costs more than it
// saves on hook-sized payloads; small bodies are stored raw.
const compressThreshold = 512

func compress(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if len(data) < compressThreshold {
		return data, nil
	}
	var buf bytes.Buffer
	// BestSpeed keeps the ingest hot path cheap; the bodies are already
	// mostly compressible JSON so the ratio loss is marginal.
	writer, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const MaxCorrelationBodyBytes = 2 << 20

// MaxBodyBytes bounds a single explicit body fetch.
const MaxBodyBytes = 16 << 20

func decompressLimited(blob []byte, limit int) ([]byte, error) {
	if limit <= 0 || len(blob) < 2 || blob[0] != 0x1f || blob[1] != 0x8b {
		if limit > 0 && len(blob) > limit {
			return blob[:limit], nil
		}
		return blob, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = reader.Close() }()

	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read gzip: %w", err)
	}
	if len(data) > limit {
		data = data[:limit]
	}
	return data, nil
}
