// Package logging persists per-request cost/usage records, and the runtime
// settings that must survive a redeploy, to SQLite or Postgres.
package logging

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pterm/pterm"
	_ "modernc.org/sqlite"
)

// timeLayout is SQLite's canonical datetime format, so timestamp columns
// stay usable by strftime()/date() and sort correctly as plain TEXT.
// Timestamps are always stored and compared in UTC.
const timeLayout = "2006-01-02 15:04:05.000"

func toDBTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func fromDBTime(s string) (time.Time, error) {
	return time.ParseInLocation(timeLayout, s, time.UTC)
}

// modelInClause builds an "AND model IN (?,?,...)" fragment plus its args,
// or ("", nil) when providerModels is empty. Shared by every query below
// that accepts a provider filter (see requestFilterWhere's doc comment
// for why this is expressed as a model set rather than a provider column).
func modelInClause(providerModels []string) (string, []any) {
	if len(providerModels) == 0 {
		return "", nil
	}
	placeholders := strings.Repeat("?,", len(providerModels))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(providerModels))
	for i, m := range providerModels {
		args[i] = m
	}
	return ` AND model IN (` + placeholders + `)`, args
}

// Record is one logged proxy request.
type Record struct {
	ID               int64
	Timestamp        time.Time
	UserID           string
	Model            string
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	LatencyMS        int64
	CacheHit         bool
	FinishReason     string
	StatusCode       int

	// Detail stored per request so the dashboard can explain any cost.
	// The component costs are priced at log time and sum to CostUSD, so
	// they stay correct after the price table changes. All zero on rows
	// logged before these columns existed.
	CachedTokens      int
	CacheWriteTokens  int
	InputCostUSD      float64 // uncached input
	CachedCostUSD     float64
	CacheWriteCostUSD float64
	OutputCostUSD     float64
	// ToolUses is server-side tool executions on this request; their
	// per-use fees are not in CostUSD (see proxy.Usage.ToolUses).
	ToolUses int
	// Kind is "chat", "response", "embedding", "image", "speech" or "transcription".
	// Units is that kind's billing unit: images, characters or seconds
	// (chat and embeddings are billed on the token columns instead).
	Kind  string
	Units float64
}

// Store wraps a database of request records: SQLite (the default, a file
// next to the binary) or Postgres (for hosts with no persistent disk, or
// more than one instance). Everything above this file is dialect-agnostic;
// the differences are confined to rebind, the DDL in migrate, the time
// bucket in TimeSeriesSince, and Insert's way of returning the new row id.
type Store struct {
	db *sql.DB
	pg bool
}

// rebind rewrites "?" placeholders to Postgres's "$1, $2, ...". The
// queries here never contain a literal "?", so a plain scan is safe.
func (s *Store) rebind(q string) string {
	if !s.pg {
		return q
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.rebind(q), args...)
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.rebind(q), args...)
}

func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.rebind(q), args...)
}

// DB exposes the underlying handle so the state tables share this
// connection (and so this database) rather than opening a second one.
func (s *Store) DB() *sql.DB { return s.db }

// Postgres reports whether this store is backed by Postgres.
func (s *Store) Postgres() bool { return s.pg }

// OpenPostgres connects to Postgres at url (postgres://user:pass@host/db,
// as handed out by Render, Heroku, Fly and friends) and ensures the schema
// exists. The pool is kept small: these are usually shared,
// connection-limited free-tier databases.
func OpenPostgres(url string) (*Store, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("opening postgres: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)

	// sql.Open doesn't connect; fail now, with a message that says what to
	// check, instead of on the first request.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to postgres: %w (check the URL, that the database is reachable from here, and sslmode)", err)
	}

	s := &Store{db: db, pg: true}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Open opens (creating if needed) the SQLite database at path and ensures
// the schema exists.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite db %s: %w", path, err)
	}
	// SQLite handles one writer at a time; the driver serializes internally
	// but we cap the pool to avoid "database is locked" errors under load.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	restrictDBPermissions(path)
	return s, nil
}

// restrictDBPermissions narrows the database file to owner-only. SQLite
// creates it world-readable by default. Best-effort: chmod can fail on
// filesystems without Unix permissions, which isn't worth refusing to
// start over.
func restrictDBPermissions(path string) {
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		pterm.Warning.Printfln("could not restrict permissions on %s (%v); "+
			"other local users may be able to read your request log", path, err)
	}
}

func (s *Store) migrate() error {
	idCol, realType := "INTEGER PRIMARY KEY AUTOINCREMENT", "REAL"
	if s.pg {
		idCol, realType = "BIGSERIAL PRIMARY KEY", "DOUBLE PRECISION"
	}
	// timestamp stays TEXT (UTC, fixed-width) on both: it sorts and
	// compares correctly as a string, and one representation means one
	// code path for every time filter instead of two.
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS requests (
	id ` + idCol + `,
	timestamp TEXT NOT NULL,
	user_id TEXT NOT NULL,
	model TEXT NOT NULL,
	prompt_tokens INTEGER NOT NULL,
	completion_tokens INTEGER NOT NULL,
	cost_usd ` + realType + ` NOT NULL,
	latency_ms BIGINT NOT NULL,
	cache_hit INTEGER NOT NULL,
	finish_reason TEXT,
	status_code INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_requests_user_ts ON requests(user_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(timestamp);
`)
	if err != nil {
		return err
	}
	if err := s.migrateState(); err != nil {
		return fmt.Errorf("creating settings table: %w", err)
	}
	if err := s.migrateBatches(); err != nil {
		return fmt.Errorf("creating batch tables: %w", err)
	}
	return s.addMissingColumns()
}

// detailColumns are the per-request detail added after the first release.
// SQLite can't ADD COLUMN IF NOT EXISTS, so existing databases are
// upgraded in place by checking what's already there; old rows keep
// working and read back as zero/empty.
var detailColumns = []struct{ name, ddl string }{
	{"cached_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"cache_write_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"input_cost_usd", "REAL NOT NULL DEFAULT 0"},
	{"cached_cost_usd", "REAL NOT NULL DEFAULT 0"},
	{"cache_write_cost_usd", "REAL NOT NULL DEFAULT 0"},
	{"output_cost_usd", "REAL NOT NULL DEFAULT 0"},
	{"tool_uses", "INTEGER NOT NULL DEFAULT 0"},
	{"kind", "TEXT NOT NULL DEFAULT ''"},
	{"units", "REAL NOT NULL DEFAULT 0"},
}

func (s *Store) addMissingColumns() error {
	if s.pg {
		for _, c := range detailColumns {
			ddl := strings.ReplaceAll(c.ddl, "REAL", "DOUBLE PRECISION")
			if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN IF NOT EXISTS ` + c.name + ` ` + ddl); err != nil {
				return fmt.Errorf("adding requests.%s: %w", c.name, err)
			}
		}
		return nil
	}
	rows, err := s.db.Query(`PRAGMA table_info(requests)`)
	if err != nil {
		return fmt.Errorf("reading requests schema: %w", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, c := range detailColumns {
		if have[c.name] {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN ` + c.name + ` ` + c.ddl); err != nil {
			return fmt.Errorf("adding requests.%s: %w", c.name, err)
		}
	}
	return nil
}

// recordColumns is the SELECT list scanRecord expects, in order.
const recordColumns = `id, timestamp, user_id, model, prompt_tokens, completion_tokens,
	cost_usd, latency_ms, cache_hit, finish_reason, status_code,
	cached_tokens, cache_write_tokens, input_cost_usd, cached_cost_usd,
	cache_write_cost_usd, output_cost_usd, tool_uses, kind, units`

// scanRecord reads one row selected with recordColumns.
func scanRecord(rows *sql.Rows) (Record, error) {
	var r Record
	var cacheHit int
	var finishReason sql.NullString
	var timestampStr string
	if err := rows.Scan(&r.ID, &timestampStr, &r.UserID, &r.Model, &r.PromptTokens,
		&r.CompletionTokens, &r.CostUSD, &r.LatencyMS, &cacheHit, &finishReason, &r.StatusCode,
		&r.CachedTokens, &r.CacheWriteTokens, &r.InputCostUSD, &r.CachedCostUSD,
		&r.CacheWriteCostUSD, &r.OutputCostUSD, &r.ToolUses, &r.Kind, &r.Units); err != nil {
		return Record{}, err
	}
	if t, err := fromDBTime(timestampStr); err == nil {
		r.Timestamp = t
	}
	r.CacheHit = cacheHit != 0
	r.FinishReason = finishReason.String
	return r, nil
}

// Insert writes a request record and returns its assigned ID.
func (s *Store) Insert(ctx context.Context, r Record) (int64, error) {
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	insert := `INSERT INTO requests
			(timestamp, user_id, model, prompt_tokens, completion_tokens, cost_usd, latency_ms, cache_hit, finish_reason, status_code,
			 cached_tokens, cache_write_tokens, input_cost_usd, cached_cost_usd, cache_write_cost_usd, output_cost_usd, tool_uses, kind, units)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	args := []any{
		toDBTime(r.Timestamp), r.UserID, r.Model, r.PromptTokens, r.CompletionTokens,
		r.CostUSD, r.LatencyMS, boolToInt(r.CacheHit), r.FinishReason, r.StatusCode,
		r.CachedTokens, r.CacheWriteTokens, r.InputCostUSD, r.CachedCostUSD,
		r.CacheWriteCostUSD, r.OutputCostUSD, r.ToolUses, r.Kind, r.Units,
	}
	if s.pg {
		// pgx has no LastInsertId; Postgres hands the id back directly.
		var id int64
		if err := s.queryRow(ctx, insert+` RETURNING id`, args...).Scan(&id); err != nil {
			return 0, fmt.Errorf("inserting request record: %w", err)
		}
		return id, nil
	}
	res, err := s.exec(ctx, insert, args...)
	if err != nil {
		return 0, fmt.Errorf("inserting request record: %w", err)
	}
	return res.LastInsertId()
}

// SpendSince returns the total cost_usd for userID with timestamp >= since.
// Called synchronously from the request path (budget enforcement), so it
// takes ctx to respect the caller's timeout/cancellation rather than
// potentially blocking a response on a stalled query.
func (s *Store) SpendSince(ctx context.Context, userID string, since time.Time) (float64, error) {
	var total sql.NullFloat64
	err := s.queryRow(ctx,
		`SELECT SUM(cost_usd) FROM requests WHERE user_id = ? AND timestamp >= ?`,
		userID, toDBTime(since),
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("querying spend for %s: %w", userID, err)
	}
	return total.Float64, nil
}

// Summary is aggregate spend/usage for a user or overall.
type Summary struct {
	UserID       string
	TotalCostUSD float64
	Requests     int64
	CacheHits    int64
}

// SummarySince returns per-user summaries for activity since the given
// time. If userID is non-empty, results are scoped to that one user.
// providerModels is optional (see requestFilterWhere) and variadic so
// existing callers are unaffected.
func (s *Store) SummarySince(ctx context.Context, since time.Time, userID string, providerModels ...string) ([]Summary, error) {
	query := `SELECT user_id, SUM(cost_usd), COUNT(*), SUM(cache_hit)
		 FROM requests WHERE timestamp >= ?`
	args := []any{toDBTime(since)}
	if userID != "" {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	if clause, clauseArgs := modelInClause(providerModels); clause != "" {
		query += clause
		args = append(args, clauseArgs...)
	}
	query += ` GROUP BY user_id ORDER BY SUM(cost_usd) DESC`

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying summary: %w", err)
	}
	defer rows.Close()

	var out []Summary
	for rows.Next() {
		var sm Summary
		if err := rows.Scan(&sm.UserID, &sm.TotalCostUSD, &sm.Requests, &sm.CacheHits); err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// DistinctUsers returns every user_id with at least one request since the
// given time, alphabetically. Used to populate the dashboard's user
// filter with a stable list, independent of whatever range/user filter
// is currently applied to the rest of the snapshot.
func (s *Store) DistinctUsers(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.query(ctx,
		`SELECT DISTINCT user_id FROM requests WHERE timestamp >= ? ORDER BY user_id ASC`,
		toDBTime(since),
	)
	if err != nil {
		return nil, fmt.Errorf("querying distinct users: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// HourBucket is total spend/requests for one time bucket (an hour or a
// day, depending on the granularity TimeSeriesSince was called with).
type HourBucket struct {
	Hour     time.Time
	CostUSD  float64
	Requests int64
}

// Granularity is how TimeSeriesSince buckets time. Hourly buckets over a
// multi-day range would mean hundreds of bars in a chart meant to be
// readable at a glance, so the dashboard switches to daily buckets once
// the selected range is longer than a day.
type Granularity string

const (
	GranularityHour Granularity = "hour"
	GranularityDay  Granularity = "day"
)

// TimeSeriesSince returns spend buckets from since to now at the given
// granularity, including empty buckets (cost 0) so charts don't have gaps.
// If userID is non-empty, results are scoped to that one user.
// providerModels is optional (see requestFilterWhere) and variadic so
// existing callers are unaffected.
func (s *Store) TimeSeriesSince(ctx context.Context, since time.Time, granularity Granularity, userID string, providerModels ...string) ([]HourBucket, error) {
	format := "%Y-%m-%dT%H:00:00Z"
	pgFormat := `YYYY-MM-DD"T"HH24:00:00"Z"`
	step := time.Hour
	if granularity == GranularityDay {
		format = "%Y-%m-%dT00:00:00Z"
		pgFormat = `YYYY-MM-DD"T"00:00:00"Z"`
		step = 24 * time.Hour
	}
	bucket := `strftime('` + format + `', timestamp)`
	if s.pg {
		bucket = `to_char(timestamp::timestamp, '` + pgFormat + `')`
	}

	query := `SELECT ` + bucket + ` AS bucket, SUM(cost_usd), COUNT(*)
		 FROM requests WHERE timestamp >= ?`
	args := []any{toDBTime(since)}
	if userID != "" {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	if clause, clauseArgs := modelInClause(providerModels); clause != "" {
		query += clause
		args = append(args, clauseArgs...)
	}
	query += ` GROUP BY bucket ORDER BY bucket ASC`

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying time series: %w", err)
	}
	defer rows.Close()

	byBucket := make(map[string]HourBucket)
	for rows.Next() {
		var bucketStr sql.NullString
		var cost float64
		var requests int64
		if err := rows.Scan(&bucketStr, &cost, &requests); err != nil {
			return nil, err
		}
		if !bucketStr.Valid {
			continue
		}
		t, err := time.Parse(time.RFC3339, bucketStr.String)
		if err != nil {
			continue
		}
		byBucket[bucketStr.String] = HourBucket{Hour: t, CostUSD: cost, Requests: requests}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Fill in empty buckets so the chart has a continuous axis.
	start := since.UTC().Truncate(step)
	end := time.Now().UTC().Truncate(step)
	var out []HourBucket
	for t := start; !t.After(end); t = t.Add(step) {
		key := t.Format(time.RFC3339)
		if b, ok := byBucket[key]; ok {
			out = append(out, b)
		} else {
			out = append(out, HourBucket{Hour: t})
		}
	}
	return out, nil
}

// TopExpensive returns the most expensive recent requests. If userID is
// non-empty, results are scoped to that one user. providerModels is
// optional (see requestFilterWhere) and variadic so existing callers are
// unaffected.
func (s *Store) TopExpensive(ctx context.Context, since time.Time, limit int, userID string, providerModels ...string) ([]Record, error) {
	query := `SELECT ` + recordColumns + `
		 FROM requests WHERE timestamp >= ?`
	args := []any{toDBTime(since)}
	if userID != "" {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	if clause, clauseArgs := modelInClause(providerModels); clause != "" {
		query += clause
		args = append(args, clauseArgs...)
	}
	query += ` ORDER BY cost_usd DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying top expensive: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// requestFilterWhere builds the shared WHERE clause for ListRequests and
// PeriodSummary. until is optional; zero value means no upper bound.
// providerModels, when non-empty, additionally restricts to that exact
// set of model names — how a "provider" filter is expressed at this
// layer, since requests aren't tagged with a provider column: the
// dashboard resolves a provider name to the set of logged model names
// that route to it (see cost.ProviderFor) and passes that set down here.
func requestFilterWhere(since, until time.Time, userID, model, status string, providerModels []string) (string, []any) {
	where := ` WHERE timestamp >= ?`
	args := []any{toDBTime(since)}
	if !until.IsZero() {
		where += ` AND timestamp <= ?`
		args = append(args, toDBTime(until))
	}
	if userID != "" {
		where += ` AND user_id = ?`
		args = append(args, userID)
	}
	if model != "" {
		where += ` AND model = ?`
		args = append(args, model)
	}
	if clause, clauseArgs := modelInClause(providerModels); clause != "" {
		where += clause
		args = append(args, clauseArgs...)
	}
	switch status {
	case "success":
		where += ` AND status_code < 400`
	case "error":
		where += ` AND status_code >= 400`
	}
	return where, args
}

// ListRequests returns a page of request records plus the total count
// matching the same filters. userID/model are optional. status is ""
// (no filter), "success" (status_code < 400), or "error" (>= 400).
// sortBy is "time" (default) or "cost". providerModels is optional (see
// requestFilterWhere) and variadic so existing callers are unaffected.
func (s *Store) ListRequests(ctx context.Context, since, until time.Time, userID, model, status, sortBy string, limit, offset int, providerModels ...string) ([]Record, int64, error) {
	where, args := requestFilterWhere(since, until, userID, model, status, providerModels)

	var total int64
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM requests`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting requests: %w", err)
	}

	orderBy := ` ORDER BY timestamp DESC`
	if sortBy == "cost" {
		orderBy = ` ORDER BY cost_usd DESC, timestamp DESC`
	}

	query := `SELECT ` + recordColumns + `
		 FROM requests` + where + orderBy + ` LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying requests: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// PeriodSummary is an aggregate total for one date range + filter set,
// used by the report/export feature.
type PeriodSummary struct {
	TotalCostUSD float64
	Requests     int64
	CacheHits    int64
}

// PeriodSummary computes totals for [since, until] (until optional, zero
// value = through now), scoped by the same userID/model/status/
// providerModels filters ListRequests uses.
func (s *Store) PeriodSummary(ctx context.Context, since, until time.Time, userID, model, status string, providerModels ...string) (PeriodSummary, error) {
	where, args := requestFilterWhere(since, until, userID, model, status, providerModels)

	var out PeriodSummary
	err := s.queryRow(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0), COUNT(*), COALESCE(SUM(cache_hit), 0) FROM requests`+where,
		args...,
	).Scan(&out.TotalCostUSD, &out.Requests, &out.CacheHits)
	if err != nil {
		return PeriodSummary{}, fmt.Errorf("querying period summary: %w", err)
	}
	return out, nil
}

// DistinctModels returns every model with at least one request since the
// given time, alphabetically. Mirrors DistinctUsers — populates the
// requests page's model filter.
func (s *Store) DistinctModels(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.query(ctx,
		`SELECT DISTINCT model FROM requests WHERE timestamp >= ? ORDER BY model ASC`,
		toDBTime(since),
	)
	if err != nil {
		return nil, fmt.Errorf("querying distinct models: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CountRequests returns how many request rows the store holds.
func (s *Store) CountRequests(ctx context.Context) (int64, error) {
	var n int64
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM requests`).Scan(&n)
	return n, err
}

// RecordsAfter returns up to limit rows with id > afterID, oldest id first.
// Used to copy a store's history somewhere else page by page.
func (s *Store) RecordsAfter(ctx context.Context, afterID int64, limit int) ([]Record, error) {
	rows, err := s.query(ctx, `SELECT `+recordColumns+` FROM requests WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
