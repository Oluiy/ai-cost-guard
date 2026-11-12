package logging

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Oluiy/ai-cost-guard/internal/config"
)

// The same database that holds the request log also holds the settings
// that change at runtime (see config.StorageConfig): dashboard logins,
// virtual keys, budgets, the fallback list, cache settings. Keeping them
// together means one connection string is all a PaaS deployment needs, and
// a redeploy can't lose either.
//
// They're stored as one JSON document under one name rather than as a
// table per concept. It's a handful of small rows' worth of data that is
// always read and written whole, so a single row can't be half-updated
// (a saved budget without its matching key), and a new setting is a new
// JSON field rather than a migration.
const stateName = "operational"

// The store is what Settings persists through.
var _ config.StateStore = (*Store)(nil)

func (s *Store) migrateState() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS fitguard_state (
	name TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at TEXT NOT NULL
)`)
	return err
}

// LoadState implements config.StateStore. ok is false when nothing has been
// saved yet (a fresh database).
func (s *Store) LoadState(ctx context.Context) (config.State, bool, error) {
	var raw string
	err := s.queryRow(ctx, `SELECT value FROM fitguard_state WHERE name = ?`, stateName).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return config.State{}, false, nil
	}
	if err != nil {
		return config.State{}, false, fmt.Errorf("reading settings: %w", err)
	}
	var st config.State
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return config.State{}, false, fmt.Errorf("settings in the database are not valid JSON (was the row edited by hand?): %w", err)
	}
	return st, true, nil
}

// SaveState implements config.StateStore.
func (s *Store) SaveState(ctx context.Context, st config.State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encoding settings: %w", err)
	}
	// Upsert syntax is shared by SQLite (3.24+) and Postgres.
	_, err = s.exec(ctx,
		`INSERT INTO fitguard_state (name, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		stateName, string(b), toDBTime(time.Now()))
	if err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	return nil
}
