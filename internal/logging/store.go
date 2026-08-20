// Package logging persists per-request cost/usage records to SQLite.
package logging

import (
	"context"
	"database/sql"
	"fmt"
	"time"

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
	return s, nil
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

// SummarySince returns per-user summaries for all activity since the given time.
func (s *Store) SummarySince(ctx context.Context, since time.Time) ([]Summary, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, SUM(cost_usd), COUNT(*), SUM(cache_hit)
		 FROM requests WHERE timestamp >= ? GROUP BY user_id ORDER BY SUM(cost_usd) DESC`,
		toDBTime(since),
	)
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

// HourBucket is total spend/requests for one hour.
type HourBucket struct {
	Hour     time.Time
	CostUSD  float64
	Requests int64
}

// TimeSeriesSince returns hourly spend buckets from since to now, including
// empty hours (cost 0) so charts don't have gaps.
func (s *Store) TimeSeriesSince(ctx context.Context, since time.Time) ([]HourBucket, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT strftime('%Y-%m-%dT%H:00:00Z', timestamp) AS hour, SUM(cost_usd), COUNT(*)
		 FROM requests WHERE timestamp >= ? GROUP BY hour ORDER BY hour ASC`,
		toDBTime(since),
	)
	if err != nil {
		return nil, fmt.Errorf("querying time series: %w", err)
	}
	defer rows.Close()

	byHour := make(map[string]HourBucket)
	for rows.Next() {
		var hourStr sql.NullString
		var cost float64
		var requests int64
		if err := rows.Scan(&hourStr, &cost, &requests); err != nil {
			return nil, err
		}
		if !hourStr.Valid {
			continue
		}
		t, err := time.Parse(time.RFC3339, hourStr.String)
		if err != nil {
			continue
		}
		byHour[hourStr.String] = HourBucket{Hour: t, CostUSD: cost, Requests: requests}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Fill in empty hours so the chart has a continuous axis.
	start := since.UTC().Truncate(time.Hour)
	end := time.Now().UTC().Truncate(time.Hour)
	var out []HourBucket
	for h := start; !h.After(end); h = h.Add(time.Hour) {
		key := h.Format(time.RFC3339)
		if b, ok := byHour[key]; ok {
			out = append(out, b)
		} else {
			out = append(out, HourBucket{Hour: h})
		}
	}
	return out, nil
}

// TopExpensive returns the most expensive recent requests.
func (s *Store) TopExpensive(ctx context.Context, since time.Time, limit int) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, timestamp, user_id, model, prompt_tokens, completion_tokens,
			cost_usd, latency_ms, cache_hit, finish_reason, status_code
		 FROM requests WHERE timestamp >= ? ORDER BY cost_usd DESC LIMIT ?`,
		toDBTime(since), limit,
	)
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
