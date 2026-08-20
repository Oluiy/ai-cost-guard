package logging

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestInsertAndSpendSince(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	yesterday := now.Add(-24 * time.Hour)

	if _, err := store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", CostUSD: 1.5}); err != nil {
		t.Fatalf("Insert failed: %v", err)
	}
	if _, err := store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", CostUSD: 2.5}); err != nil {
		t.Fatalf("Insert failed: %v", err)
	}
	// Old record outside the "today" window should not count.
	if _, err := store.Insert(ctx, Record{Timestamp: yesterday.Add(-time.Hour), UserID: "alice", Model: "gpt-4o", CostUSD: 100}); err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	spend, err := store.SpendSince(ctx, "alice", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendSince failed: %v", err)
	}
	if spend != 4.0 {
		t.Fatalf("got spend %.2f, want 4.00", spend)
	}

	spend, err = store.SpendSince(ctx, "bob", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendSince failed: %v", err)
	}
	if spend != 0 {
		t.Fatalf("expected 0 spend for user with no records, got %.2f", spend)
	}
}
