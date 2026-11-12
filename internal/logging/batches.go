package logging

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Batch and file ownership. The upstream account is shared by every
// FitGuard key, so without this table any key could read, cancel or list
// another user's batches and files by id. A file or batch exists for a
// caller only if FitGuard recorded it as theirs.

func (s *Store) migrateBatches() error {
	realType := "REAL"
	if s.pg {
		realType = "DOUBLE PRECISION"
	}
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS fitguard_files (
	file_id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	models TEXT NOT NULL,
	estimated_usd ` + realType + ` NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS fitguard_batches (
	batch_id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	endpoint TEXT NOT NULL,
	status TEXT NOT NULL,
	output_file_id TEXT NOT NULL,
	error_file_id TEXT NOT NULL,
	estimated_usd ` + realType + ` NOT NULL,
	settled INTEGER NOT NULL,
	cost_usd ` + realType + ` NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_batches_pending ON fitguard_batches(settled);
`)
	return err
}

// BatchFile is an uploaded batch input (or a batch's output) FitGuard knows about.
type BatchFile struct {
	FileID       string
	UserID       string
	Provider     string
	Models       string // JSON array of model names found in the file
	EstimatedUSD float64
}

// Batch is a batch job FitGuard is tracking for cost settlement.
type Batch struct {
	BatchID      string
	UserID       string
	Provider     string
	Endpoint     string
	Status       string
	OutputFileID string
	ErrorFileID  string
	EstimatedUSD float64
	Settled      bool
	CostUSD      float64
}

func (s *Store) SaveBatchFile(ctx context.Context, f BatchFile) error {
	_, err := s.exec(ctx,
		`INSERT INTO fitguard_files (file_id, user_id, provider, models, estimated_usd, created_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (file_id) DO NOTHING`,
		f.FileID, f.UserID, f.Provider, f.Models, f.EstimatedUSD, toDBTime(time.Now()))
	return err
}

// GetBatchFile returns the file only when userID owns it.
func (s *Store) GetBatchFile(ctx context.Context, fileID, userID string) (BatchFile, bool, error) {
	var f BatchFile
	err := s.queryRow(ctx,
		`SELECT file_id, user_id, provider, models, estimated_usd FROM fitguard_files WHERE file_id = ? AND user_id = ?`,
		fileID, userID).Scan(&f.FileID, &f.UserID, &f.Provider, &f.Models, &f.EstimatedUSD)
	if errors.Is(err, sql.ErrNoRows) {
		return BatchFile{}, false, nil
	}
	return f, err == nil, err
}

func (s *Store) SaveBatch(ctx context.Context, b Batch) error {
	now := toDBTime(time.Now())
	_, err := s.exec(ctx,
		`INSERT INTO fitguard_batches (batch_id, user_id, provider, endpoint, status, output_file_id, error_file_id, estimated_usd, settled, cost_usd, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)
		 ON CONFLICT (batch_id) DO NOTHING`,
		b.BatchID, b.UserID, b.Provider, b.Endpoint, b.Status, b.OutputFileID, b.ErrorFileID, b.EstimatedUSD, now, now)
	return err
}

const batchColumns = `batch_id, user_id, provider, endpoint, status, output_file_id, error_file_id, estimated_usd, settled, cost_usd`

func scanBatch(row interface{ Scan(...any) error }) (Batch, error) {
	var b Batch
	var settled int
	err := row.Scan(&b.BatchID, &b.UserID, &b.Provider, &b.Endpoint, &b.Status, &b.OutputFileID, &b.ErrorFileID, &b.EstimatedUSD, &settled, &b.CostUSD)
	b.Settled = settled != 0
	return b, err
}

// GetBatch returns the batch only when userID owns it. An empty userID
// skips the check (the settler works across all users).
func (s *Store) GetBatch(ctx context.Context, batchID, userID string) (Batch, bool, error) {
	q, args := `SELECT `+batchColumns+` FROM fitguard_batches WHERE batch_id = ?`, []any{batchID}
	if userID != "" {
		q, args = q+` AND user_id = ?`, append(args, userID)
	}
	b, err := scanBatch(s.queryRow(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Batch{}, false, nil
	}
	return b, err == nil, err
}

// PendingBatches lists batches whose cost hasn't been settled yet.
func (s *Store) PendingBatches(ctx context.Context) ([]Batch, error) {
	rows, err := s.query(ctx, `SELECT `+batchColumns+` FROM fitguard_batches WHERE settled = 0 ORDER BY created_at LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Batch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UpdateBatchStatus records the latest status and output file ids.
func (s *Store) UpdateBatchStatus(ctx context.Context, batchID, status, outputFileID, errorFileID string) error {
	_, err := s.exec(ctx,
		`UPDATE fitguard_batches SET status = ?, output_file_id = ?, error_file_id = ?, updated_at = ? WHERE batch_id = ?`,
		status, outputFileID, errorFileID, toDBTime(time.Now()), batchID)
	return err
}

// SettleBatch marks the batch settled exactly once. It reports whether
// this call was the one that settled it, so two instances racing to settle
// the same batch can't both log its cost.
func (s *Store) SettleBatch(ctx context.Context, batchID string, costUSD float64) (bool, error) {
	res, err := s.exec(ctx,
		`UPDATE fitguard_batches SET settled = 1, cost_usd = ?, updated_at = ? WHERE batch_id = ? AND settled = 0`,
		costUSD, toDBTime(time.Now()), batchID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// BatchByOutputFile finds userID's batch that produced fileID as its
// output or error file, which is what makes that file theirs to read.
func (s *Store) BatchByOutputFile(ctx context.Context, fileID, userID string) (Batch, bool, error) {
	b, err := scanBatch(s.queryRow(ctx,
		`SELECT `+batchColumns+` FROM fitguard_batches WHERE user_id = ? AND (output_file_id = ? OR error_file_id = ?)`,
		userID, fileID, fileID))
	if errors.Is(err, sql.ErrNoRows) {
		return Batch{}, false, nil
	}
	return b, err == nil, err
}
