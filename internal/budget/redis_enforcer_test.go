package budget

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testRedisURL is used by every test in this file. Redis isn't a Go
// dependency you get for free like SQLite (modernc.org/sqlite is pure Go
// and needs nothing installed) — these tests need a real server, so they
// skip cleanly if one isn't reachable rather than failing CI/local runs
// that don't have Redis set up. Run `redis-server` locally to exercise them.
const testRedisURL = "redis://localhost:6379/15" // db 15: unlikely to collide with real use

func requireRedis(t *testing.T) *redis.Client {
	t.Helper()
	opts, err := redis.ParseURL(testRedisURL)
	if err != nil {
		t.Fatalf("parsing test redis url: %v", err)
	}
	opts.MaxRetries = -1 // fail fast on the reachability probe instead of retrying 5x
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("skipping: no redis reachable at %s (%v) — run `redis-server` to exercise this test", testRedisURL, err)
	}
	t.Cleanup(func() {
		client.FlushDB(context.Background())
		client.Close()
	})
	client.FlushDB(context.Background())
	return client
}

func TestRedisEnforcer_UnderLimitAllowed(t *testing.T) {
	requireRedis(t)
	e, err := NewRedisEnforcer(testRedisURL, budgetMap{"alice": {DailyLimitUSD: 5}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, release, err := e.Reserve(context.Background(), "alice", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatal("expected reservation under the limit to be allowed")
	}
	release(2)
}

func TestRedisEnforcer_OverEstimateBlocked(t *testing.T) {
	requireRedis(t)
	e, err := NewRedisEnforcer(testRedisURL, budgetMap{"alice": {DailyLimitUSD: 5}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, _, err := e.Reserve(context.Background(), "alice", 5.01)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected an estimate above the limit to be blocked")
	}
}

func TestRedisEnforcer_NoConfiguredBudgetAlwaysAllowed(t *testing.T) {
	requireRedis(t)
	e, err := NewRedisEnforcer(testRedisURL, budgetMap{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	result, release, err := e.Reserve(context.Background(), "alice", 999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatal("expected user with no configured budget to always be allowed")
	}
	release(0) // must not panic
}

// Reservation reconciliation: the estimate reserved up front is a
// worst-case ceiling, and Release trues it up to the real cost. Reserve
// $4 (of a $5 limit), release at the real cost of $1 — the other $3
// should become available again, not stay locked up as if $4 were spent.
func TestRedisEnforcer_ReleaseReconcilesToActualCost(t *testing.T) {
	requireRedis(t)
	e, err := NewRedisEnforcer(testRedisURL, budgetMap{"alice": {DailyLimitUSD: 5}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result1, release1, err := e.Reserve(context.Background(), "alice", 4)
	if err != nil || !result1.Allowed {
		t.Fatalf("expected first reservation to be allowed, got %+v, err=%v", result1, err)
	}
	release1(1) // actual cost was only $1, not the $4 estimate

	// $1 is now committed; a further $3.99 reservation should fit under
	// the $5 limit, proving the other $3 of the original estimate was
	// freed rather than staying reserved.
	result2, release2, err := e.Reserve(context.Background(), "alice", 3.99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result2.Allowed {
		t.Fatalf("expected reservation to fit after reconciliation freed unused budget, got %+v", result2)
	}
	release2(3.99)
}

// This is the whole point of RedisEnforcer: two independent Enforcer
// objects — standing in for two separate ai-guard processes — sharing
// nothing but Redis must still never jointly over-admit a user's budget.
// MemoryEnforcer can't make this guarantee (each instance's in-flight map
// is invisible to the other); this test would fail against two
// MemoryEnforcers sharing only a common SQLite spend query, which is
// exactly the gap this backend exists to close.
func TestRedisEnforcer_TwoInstancesShareOneBudget(t *testing.T) {
	requireRedis(t)
	users := budgetMap{"alice": {DailyLimitUSD: 10}}
	instanceA, err := NewRedisEnforcer(testRedisURL, users)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	instanceB, err := NewRedisEnforcer(testRedisURL, users)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const attempts = 50
	const costEach = 1.0 // 50 attempts x $1 across two instances against a $10 limit -> at most 10 admitted total

	var admitted int64
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		instance := instanceA
		if i%2 == 0 {
			instance = instanceB
		}
		go func(e *RedisEnforcer) {
			defer wg.Done()
			result, release, err := e.Reserve(context.Background(), "alice", costEach)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if result.Allowed {
				atomic.AddInt64(&admitted, 1)
				release(costEach)
			}
		}(instance)
	}
	wg.Wait()

	if admitted > 10 {
		t.Fatalf("two instances jointly admitted %d requests at $1 each against a shared $10 limit; "+
			"budget was over-committed across instances", admitted)
	}
}
