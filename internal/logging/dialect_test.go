package logging

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every query in this package has a SQLite and a Postgres form (placeholder
// style, the time-bucket function, how an insert returns its id). These
// tests run ONE body of assertions against both, so the dialects can't
// drift apart unnoticed.
//
// SQLite always runs. Postgres runs when FITGUARD_TEST_POSTGRES_URL is set,
// e.g. against a throwaway container:
//
//	docker run -d --name fgpg -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=fitguard -p 55432:5432 postgres:16
//	FITGUARD_TEST_POSTGRES_URL='postgres://postgres:pw@localhost:55432/fitguard?sslmode=disable' go test ./internal/logging/
//
// CI sets it from a service container. Each test gets its own schema, so
// tests don't see each other's rows and nothing is left behind.

type opener struct {
	name string
	open func(t *testing.T) *Store
}

func openers() []opener {
	out := []opener{{"sqlite", func(t *testing.T) *Store {
		s, err := Open(filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}}}
	if base := os.Getenv("FITGUARD_TEST_POSTGRES_URL"); base != "" {
		out = append(out, opener{"postgres", func(t *testing.T) *Store { return openPostgresSchema(t, base) }})
	}
	return out
}

// openPostgresSchema opens a Store confined to a fresh, uniquely named
// schema (via the search_path connection parameter) and drops it afterwards.
func openPostgresSchema(t *testing.T, base string) *Store {
	t.Helper()
	schema := fmt.Sprintf("t_%d_%d", time.Now().UnixNano(), os.Getpid())
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		t.Fatalf("creating test schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		admin.Close()
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := OpenPostgres(u.String())
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func forEachDialect(t *testing.T, body func(t *testing.T, s *Store)) {
	for _, o := range openers() {
		o := o
		t.Run(o.name, func(t *testing.T) { body(t, o.open(t)) })
	}
}

func TestDialect_InsertListFilterAndRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *Store) {
		ctx, now := context.Background(), time.Now().UTC()
		id1, err := s.Insert(ctx, Record{Timestamp: now.Add(-3 * time.Minute), UserID: "alice", Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 100,
			CachedTokens: 800, CostUSD: 0.0025, InputCostUSD: 0.0005, CachedCostUSD: 0.001, OutputCostUSD: 0.001, ToolUses: 1, Kind: "chat", LatencyMS: 812, FinishReason: "stop", StatusCode: 200})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		id2, _ := s.Insert(ctx, Record{Timestamp: now.Add(-2 * time.Minute), UserID: "alice", Model: "claude-sonnet-5", CostUSD: 2, StatusCode: 200})
		s.Insert(ctx, Record{Timestamp: now.Add(-1 * time.Minute), UserID: "bob", Model: "gpt-4o", CostUSD: 3, StatusCode: 502, CacheHit: true})
		if id2 <= id1 {
			t.Errorf("ids should increase: %d then %d", id1, id2)
		}

		since := now.Add(-time.Hour)
		rows, total, err := s.ListRequests(ctx, since, time.Time{}, "", "", "", "time", 10, 0)
		if err != nil || total != 3 || len(rows) != 3 || rows[0].UserID != "bob" {
			t.Fatalf("newest-first list: rows=%d total=%d err=%v", len(rows), total, err)
		}
		var first Record
		for _, r := range rows {
			if r.ID == id1 {
				first = r
			}
		}
		if first.CachedTokens != 800 || first.CachedCostUSD != 0.001 || first.ToolUses != 1 || first.Kind != "chat" || first.FinishReason != "stop" {
			t.Errorf("detail fields did not round-trip: %+v", first)
		}

		if _, n, _ := s.ListRequests(ctx, since, time.Time{}, "alice", "", "", "time", 10, 0); n != 2 {
			t.Errorf("user filter: %d, want 2", n)
		}
		if _, n, _ := s.ListRequests(ctx, since, time.Time{}, "", "", "error", "time", 10, 0); n != 1 {
			t.Errorf("status=error filter: %d, want 1", n)
		}
		if _, n, _ := s.ListRequests(ctx, since, time.Time{}, "", "", "", "time", 10, 0, "gpt-4o"); n != 2 {
			t.Errorf("provider-model filter (IN clause): %d, want 2", n)
		}
		page, total, _ := s.ListRequests(ctx, since, now.Add(-90*time.Second), "", "", "", "cost", 1, 1)
		if total != 2 || len(page) != 1 || page[0].CostUSD != 0.0025 {
			t.Errorf("until + sort=cost + pagination: total=%d page=%+v", total, page)
		}
	})
}

func TestDialect_AggregatesAndTimeSeries(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *Store) {
		ctx, now := context.Background(), time.Now().UTC()
		s.Insert(ctx, Record{Timestamp: now.Add(-10 * time.Minute), UserID: "alice", Model: "gpt-4o", CostUSD: 1, StatusCode: 200, CacheHit: true})
		s.Insert(ctx, Record{Timestamp: now.Add(-5 * time.Minute), UserID: "alice", Model: "gpt-4o", CostUSD: 2, StatusCode: 200})
		s.Insert(ctx, Record{Timestamp: now.Add(-2 * time.Hour), UserID: "bob", Model: "claude-sonnet-5", CostUSD: 4, StatusCode: 200})
		since := now.Add(-24 * time.Hour)

		if got, err := s.SpendSince(ctx, "alice", since); err != nil || got != 3 {
			t.Errorf("SpendSince alice = %v, %v; want 3", got, err)
		}
		sums, err := s.SummarySince(ctx, since, "")
		if err != nil || len(sums) != 2 || sums[0].UserID != "bob" || sums[0].TotalCostUSD != 4 || sums[1].CacheHits != 1 || sums[1].Requests != 2 {
			t.Errorf("SummarySince: %+v err=%v", sums, err)
		}
		ps, err := s.PeriodSummary(ctx, since, time.Time{}, "", "", "")
		if err != nil || ps.TotalCostUSD != 7 || ps.Requests != 3 || ps.CacheHits != 1 {
			t.Errorf("PeriodSummary: %+v err=%v", ps, err)
		}
		if u, _ := s.DistinctUsers(ctx, since); strings.Join(u, ",") != "alice,bob" {
			t.Errorf("DistinctUsers = %v", u)
		}
		if m, _ := s.DistinctModels(ctx, since); strings.Join(m, ",") != "claude-sonnet-5,gpt-4o" {
			t.Errorf("DistinctModels = %v", m)
		}
		top, _ := s.TopExpensive(ctx, since, 2, "")
		if len(top) != 2 || top[0].CostUSD != 4 || top[1].CostUSD != 2 {
			t.Errorf("TopExpensive: %+v", top)
		}
		if top, _ := s.TopExpensive(ctx, since, 5, "", "gpt-4o"); len(top) != 2 {
			t.Errorf("TopExpensive with provider filter: %d rows, want 2", len(top))
		}

		for _, g := range []Granularity{GranularityHour, GranularityDay} {
			buckets, err := s.TimeSeriesSince(ctx, now.Add(-3*time.Hour), g, "")
			if err != nil {
				t.Fatalf("TimeSeriesSince(%s): %v", g, err)
			}
			var total float64
			var reqs int64
			for _, b := range buckets {
				total += b.CostUSD
				reqs += b.Requests
			}
			if total != 7 || reqs != 3 {
				t.Errorf("TimeSeriesSince(%s): buckets sum to $%v / %d requests, want $7 / 3 (%d buckets)", g, total, reqs, len(buckets))
			}
			if g == GranularityHour && len(buckets) < 3 {
				t.Errorf("hourly series should include empty buckets, got %d", len(buckets))
			}
		}
	})
}

func TestDialect_ReopeningIsANoOp(t *testing.T) {
	for _, o := range openers() {
		if o.name != "postgres" {
			continue // sqlite reopening is covered by TestOpenUpgradesAPreDetailDatabase
		}
		t.Run(o.name, func(t *testing.T) {
			s := o.open(t)
			if err := s.migrate(); err != nil {
				t.Fatalf("running migrate a second time failed: %v", err)
			}
		})
	}
}
