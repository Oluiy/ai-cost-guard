// Package logging persists per-request cost/usage records to SQLite.
package logging

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

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
}

// Store wraps a SQLite database of request records.
type Store struct {
	db *sql.DB
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
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	timestamp TEXT NOT NULL,
	user_id TEXT NOT NULL,
	model TEXT NOT NULL,
	prompt_tokens INTEGER NOT NULL,
	completion_tokens INTEGER NOT NULL,
	cost_usd REAL NOT NULL,
	latency_ms INTEGER NOT NULL,
	cache_hit INTEGER NOT NULL,
	finish_reason TEXT,
	status_code INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_requests_user_ts ON requests(user_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(timestamp);
`)
	return err
}

// Insert writes a request record and returns its assigned ID.
func (s *Store) Insert(ctx context.Context, r Record) (int64, error) {
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO requests
			(timestamp, user_id, model, prompt_tokens, completion_tokens, cost_usd, latency_ms, cache_hit, finish_reason, status_code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		toDBTime(r.Timestamp), r.UserID, r.Model, r.PromptTokens, r.CompletionTokens,
		r.CostUSD, r.LatencyMS, boolToInt(r.CacheHit), r.FinishReason, r.StatusCode,
	)
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
	err := s.db.QueryRowContext(ctx,
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

	rows, err := s.db.QueryContext(ctx, query, args...)
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
	rows, err := s.db.QueryContext(ctx,
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
	step := time.Hour
	if granularity == GranularityDay {
		format = "%Y-%m-%dT00:00:00Z"
		step = 24 * time.Hour
	}

	query := `SELECT strftime('` + format + `', timestamp) AS bucket, SUM(cost_usd), COUNT(*)
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

	rows, err := s.db.QueryContext(ctx, query, args...)
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
	query := `SELECT id, timestamp, user_id, model, prompt_tokens, completion_tokens,
			cost_usd, latency_ms, cache_hit, finish_reason, status_code
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

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying top expensive: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var r Record
		var cacheHit int
		var finishReason sql.NullString
		var timestampStr string
		if err := rows.Scan(&r.ID, &timestampStr, &r.UserID, &r.Model, &r.PromptTokens,
			&r.CompletionTokens, &r.CostUSD, &r.LatencyMS, &cacheHit, &finishReason, &r.StatusCode); err != nil {
			return nil, err
		}
		if t, err := fromDBTime(timestampStr); err == nil {
			r.Timestamp = t
		}
		r.CacheHit = cacheHit != 0
		r.FinishReason = finishReason.String
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
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting requests: %w", err)
	}

	orderBy := ` ORDER BY timestamp DESC`
	if sortBy == "cost" {
		orderBy = ` ORDER BY cost_usd DESC, timestamp DESC`
	}

	query := `SELECT id, timestamp, user_id, model, prompt_tokens, completion_tokens,
			cost_usd, latency_ms, cache_hit, finish_reason, status_code
		 FROM requests` + where + orderBy + ` LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying requests: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var r Record
		var cacheHit int
		var finishReason sql.NullString
		var timestampStr string
		if err := rows.Scan(&r.ID, &timestampStr, &r.UserID, &r.Model, &r.PromptTokens,
			&r.CompletionTokens, &r.CostUSD, &r.LatencyMS, &cacheHit, &finishReason, &r.StatusCode); err != nil {
			return nil, 0, err
		}
		if t, err := fromDBTime(timestampStr); err == nil {
			r.Timestamp = t
		}
		r.CacheHit = cacheHit != 0
		r.FinishReason = finishReason.String
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
	err := s.db.QueryRowContext(ctx,
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
	rows, err := s.db.QueryContext(ctx,
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
