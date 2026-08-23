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

func TestSummarySinceUserFilter(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", CostUSD: 1})
	store.Insert(ctx, Record{Timestamp: now, UserID: "bob", Model: "gpt-4o", CostUSD: 2})

	all, err := store.SummarySince(ctx, now.Add(-time.Hour), "")
	if err != nil {
		t.Fatalf("SummarySince failed: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 users with no filter, got %d", len(all))
	}

	aliceOnly, err := store.SummarySince(ctx, now.Add(-time.Hour), "alice")
	if err != nil {
		t.Fatalf("SummarySince failed: %v", err)
	}
	if len(aliceOnly) != 1 || aliceOnly[0].UserID != "alice" {
		t.Fatalf("expected only alice, got %+v", aliceOnly)
	}
}

func TestDistinctUsers(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	store.Insert(ctx, Record{Timestamp: now, UserID: "bob", Model: "gpt-4o", CostUSD: 1})
	store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", CostUSD: 1})
	store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", CostUSD: 1})

	users, err := store.DistinctUsers(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("DistinctUsers failed: %v", err)
	}
	if len(users) != 2 || users[0] != "alice" || users[1] != "bob" {
		t.Fatalf("expected [alice bob] (deduped, sorted), got %v", users)
	}
}

func TestTimeSeriesSinceGranularityAndUserFilter(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", CostUSD: 3})
	store.Insert(ctx, Record{Timestamp: now, UserID: "bob", Model: "gpt-4o", CostUSD: 5})

	hourly, err := store.TimeSeriesSince(ctx, now.Add(-2*time.Hour), GranularityHour, "")
	if err != nil {
		t.Fatalf("TimeSeriesSince (hour) failed: %v", err)
	}
	var totalHourly float64
	for _, b := range hourly {
		totalHourly += b.CostUSD
	}
	if totalHourly != 8 {
		t.Fatalf("got total %.2f across hourly buckets, want 8.00", totalHourly)
	}

	daily, err := store.TimeSeriesSince(ctx, now.Add(-48*time.Hour), GranularityDay, "alice")
	if err != nil {
		t.Fatalf("TimeSeriesSince (day, alice) failed: %v", err)
	}
	var totalDaily float64
	for _, b := range daily {
		totalDaily += b.CostUSD
	}
	if totalDaily != 3 {
		t.Fatalf("got total %.2f for alice across daily buckets, want 3.00 (bob's spend must not leak in)", totalDaily)
	}
}

func TestTopExpensiveUserFilter(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", CostUSD: 1})
	store.Insert(ctx, Record{Timestamp: now, UserID: "bob", Model: "gpt-4o", CostUSD: 9})

	top, err := store.TopExpensive(ctx, now.Add(-time.Hour), 10, "alice")
	if err != nil {
		t.Fatalf("TopExpensive failed: %v", err)
	}
	if len(top) != 1 || top[0].UserID != "alice" {
		t.Fatalf("expected only alice's request, got %+v", top)
	}
}

func TestListRequestsOrderFilterAndPagination(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	// Inserted oldest-first; ListRequests must come back newest-first.
	store.Insert(ctx, Record{Timestamp: now.Add(-3 * time.Minute), UserID: "alice", Model: "gpt-4o", CostUSD: 1})
	store.Insert(ctx, Record{Timestamp: now.Add(-2 * time.Minute), UserID: "alice", Model: "claude-3-haiku", CostUSD: 2})
	store.Insert(ctx, Record{Timestamp: now.Add(-1 * time.Minute), UserID: "bob", Model: "gpt-4o", CostUSD: 3})

	records, total, err := store.ListRequests(ctx, now.Add(-time.Hour), time.Time{}, "", "", "", "time", 10, 0)
	if err != nil {
		t.Fatalf("ListRequests failed: %v", err)
	}
	if total != 3 {
		t.Fatalf("got total %d, want 3", total)
	}
	if len(records) != 3 || records[0].UserID != "bob" || records[2].UserID != "alice" {
		t.Fatalf("expected newest-first order, got %+v", records)
	}

	records, total, err = store.ListRequests(ctx, now.Add(-time.Hour), time.Time{}, "alice", "", "", "time", 10, 0)
	if err != nil {
		t.Fatalf("ListRequests with user filter failed: %v", err)
	}
	if total != 2 || len(records) != 2 {
		t.Fatalf("expected 2 records for alice, got total=%d len=%d", total, len(records))
	}

	records, total, err = store.ListRequests(ctx, now.Add(-time.Hour), time.Time{}, "", "gpt-4o", "", "time", 10, 0)
	if err != nil {
		t.Fatalf("ListRequests with model filter failed: %v", err)
	}
	if total != 2 || len(records) != 2 {
		t.Fatalf("expected 2 gpt-4o records, got total=%d len=%d", total, len(records))
	}

	// Pagination: total reflects the full filtered set even when a page
	// returns fewer rows than that.
	records, total, err = store.ListRequests(ctx, now.Add(-time.Hour), time.Time{}, "", "", "", "time", 1, 1)
	if err != nil {
		t.Fatalf("ListRequests with pagination failed: %v", err)
	}
	if total != 3 {
		t.Fatalf("got total %d, want 3 (pagination shouldn't change the total count)", total)
	}
	if len(records) != 1 || records[0].UserID != "alice" || records[0].Model != "claude-3-haiku" {
		t.Fatalf("expected the second-newest record (offset 1), got %+v", records)
	}
}

func TestListRequestsStatusFilter(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", StatusCode: 200})
	store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", StatusCode: 200})
	store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", StatusCode: 429})
	store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", StatusCode: 500})

	_, total, err := store.ListRequests(ctx, now.Add(-time.Hour), time.Time{}, "", "", "success", "time", 10, 0)
	if err != nil {
		t.Fatalf("ListRequests with status=success failed: %v", err)
	}
	if total != 2 {
		t.Fatalf("got %d success records, want 2", total)
	}

	_, total, err = store.ListRequests(ctx, now.Add(-time.Hour), time.Time{}, "", "", "error", "time", 10, 0)
	if err != nil {
		t.Fatalf("ListRequests with status=error failed: %v", err)
	}
	if total != 2 {
		t.Fatalf("got %d error records, want 2", total)
	}

	_, total, err = store.ListRequests(ctx, now.Add(-time.Hour), time.Time{}, "", "", "not-a-real-value", "time", 10, 0)
	if err != nil {
		t.Fatalf("ListRequests with unrecognized status failed: %v", err)
	}
	if total != 4 {
		t.Fatalf("expected an unrecognized status value to mean no filter, got total=%d", total)
	}
}

func TestListRequestsUntilUpperBound(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	store.Insert(ctx, Record{Timestamp: now.Add(-3 * 24 * time.Hour), UserID: "alice", Model: "gpt-4o", CostUSD: 1})
	store.Insert(ctx, Record{Timestamp: now.Add(-1 * time.Hour), UserID: "alice", Model: "gpt-4o", CostUSD: 2})

	// A zero-value until means "through now" — both records match.
	_, total, err := store.ListRequests(ctx, now.Add(-7*24*time.Hour), time.Time{}, "", "", "", "time", 10, 0)
	if err != nil {
		t.Fatalf("ListRequests failed: %v", err)
	}
	if total != 2 {
		t.Fatalf("got total %d, want 2 with no upper bound", total)
	}

	// An explicit until excludes anything after it, even though it's
	// still within the since..now window.
	_, total, err = store.ListRequests(ctx, now.Add(-7*24*time.Hour), now.Add(-2*24*time.Hour), "", "", "", "time", 10, 0)
	if err != nil {
		t.Fatalf("ListRequests with until failed: %v", err)
	}
	if total != 1 {
		t.Fatalf("got total %d, want 1 (only the 3-day-old record should be within the closed range)", total)
	}
}

func TestListRequestsSortByCost(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	// Inserted cheapest-first and oldest-first, so a sort=cost result can
	// only match by actually sorting on cost, not by coincidentally
	// matching insertion or timestamp order.
	store.Insert(ctx, Record{Timestamp: now.Add(-3 * time.Minute), UserID: "alice", Model: "gpt-4o", CostUSD: 1})
	store.Insert(ctx, Record{Timestamp: now.Add(-2 * time.Minute), UserID: "alice", Model: "gpt-4o", CostUSD: 9})
	store.Insert(ctx, Record{Timestamp: now.Add(-1 * time.Minute), UserID: "alice", Model: "gpt-4o", CostUSD: 5})

	records, _, err := store.ListRequests(ctx, now.Add(-time.Hour), time.Time{}, "", "", "", "cost", 10, 0)
	if err != nil {
		t.Fatalf("ListRequests with sort=cost failed: %v", err)
	}
	if len(records) != 3 || records[0].CostUSD != 9 || records[1].CostUSD != 5 || records[2].CostUSD != 1 {
		t.Fatalf("expected cost-descending order [9 5 1], got %+v", records)
	}

	// An unrecognized sort value falls back to time-descending (the
	// default), not an error.
	records, _, err = store.ListRequests(ctx, now.Add(-time.Hour), time.Time{}, "", "", "", "not-a-real-sort", 10, 0)
	if err != nil {
		t.Fatalf("ListRequests with unrecognized sort failed: %v", err)
	}
	if len(records) != 3 || records[0].CostUSD != 5 || records[2].CostUSD != 1 {
		t.Fatalf("expected time-descending fallback order [5(newest) 9 1(oldest)], got %+v", records)
	}
}

func TestPeriodSummary(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	store.Insert(ctx, Record{Timestamp: now.Add(-3 * 24 * time.Hour), UserID: "alice", Model: "gpt-4o", CostUSD: 1, CacheHit: true})
	store.Insert(ctx, Record{Timestamp: now.Add(-1 * time.Hour), UserID: "alice", Model: "gpt-4o", CostUSD: 2, CacheHit: false})
	store.Insert(ctx, Record{Timestamp: now.Add(-30 * time.Minute), UserID: "bob", Model: "claude-3-haiku", CostUSD: 5, CacheHit: true})

	// Whole window, no filters: all three.
	sum, err := store.PeriodSummary(ctx, now.Add(-7*24*time.Hour), time.Time{}, "", "", "")
	if err != nil {
		t.Fatalf("PeriodSummary failed: %v", err)
	}
	if sum.Requests != 3 || sum.TotalCostUSD != 8 || sum.CacheHits != 2 {
		t.Fatalf("got %+v, want {TotalCostUSD:8 Requests:3 CacheHits:2}", sum)
	}

	// Closed range excludes the 3-day-old record; user filter excludes bob.
	sum, err = store.PeriodSummary(ctx, now.Add(-2*24*time.Hour), time.Time{}, "alice", "", "")
	if err != nil {
		t.Fatalf("PeriodSummary with range+user filter failed: %v", err)
	}
	if sum.Requests != 1 || sum.TotalCostUSD != 2 || sum.CacheHits != 0 {
		t.Fatalf("got %+v, want {TotalCostUSD:2 Requests:1 CacheHits:0}", sum)
	}

	// No matching rows: zero values, not an error.
	sum, err = store.PeriodSummary(ctx, now.Add(-7*24*time.Hour), time.Time{}, "nobody", "", "")
	if err != nil {
		t.Fatalf("PeriodSummary with no matches failed: %v", err)
	}
	if sum.Requests != 0 || sum.TotalCostUSD != 0 || sum.CacheHits != 0 {
		t.Fatalf("got %+v, want all-zero for no matching rows", sum)
	}
}

func TestDistinctModels(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", CostUSD: 1})
	store.Insert(ctx, Record{Timestamp: now, UserID: "bob", Model: "claude-3-haiku", CostUSD: 1})
	store.Insert(ctx, Record{Timestamp: now, UserID: "alice", Model: "gpt-4o", CostUSD: 1})

	models, err := store.DistinctModels(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("DistinctModels failed: %v", err)
	}
	if len(models) != 2 || models[0] != "claude-3-haiku" || models[1] != "gpt-4o" {
		t.Fatalf("got %v, want [claude-3-haiku gpt-4o]", models)
	}
}
