// Package budget enforces per-user daily spending limits.
package budget

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aicostguard/ai-cost-guard/internal/config"
)

// SpendLookup returns total persisted spend for a user since a given time.
// Satisfied by *logging.Store.
type SpendLookup interface {
	SpendSince(ctx context.Context, userID string, since time.Time) (float64, error)
}

// Result describes the outcome of a budget check.
type Result struct {
	Allowed   bool
	LimitUSD  float64
	SpentUSD  float64
	Remaining float64
}

// Release reconciles a Reserve call's estimated cost with the request's
// actual cost (0 if the request never completed, e.g. every provider
// failed). Call it exactly once per successful Reserve, regardless of
// outcome.
type Release func(actualCost float64)

var noopRelease Release = func(float64) {}

// Enforcer checks whether a user has room for a request estimated to cost
// estimatedCost, and if so, reserves that amount against their daily
// budget until the returned Release is called with the request's actual
// cost. Users with no configured budget are always allowed and get a
// no-op Release.
//
// There are two implementations: MemoryEnforcer, correct within a single
// ai-guard process, and RedisEnforcer, correct across multiple ai-guard
// instances sharing one Redis (see redis_enforcer.go's doc comment for
// why that distinction matters).
type Enforcer interface {
	Reserve(ctx context.Context, userID string, estimatedCost float64) (Result, Release, error)
}

// MemoryEnforcer enforces budgets within a single process.
//
// Persisted spend (in SpendLookup) only reflects requests that have
// finished and been logged, so two concurrent requests can both read
// "under budget" before either finishes — a classic check-then-act race,
// and exactly the gap a runaway loop slips through. MemoryEnforcer closes
// it by tracking in-flight *reservations* in memory: Reserve adds its
// worst-case estimated cost to a running in-flight total before allowing
// the request to proceed, so a concurrent request sees it immediately,
// not after a round trip to the database.
//
// This in-flight tracking is process-local: if you run more than one
// ai-guard instance behind a load balancer, each has its own view of
// in-flight reservations (and, since each also has its own SQLite file,
// its own view of persisted spend too) — a user's real budget can be
// exceeded by roughly (instance count)×. Use RedisEnforcer instead if
// you're running more than one instance.
type MemoryEnforcer struct {
	store SpendLookup
	users map[string]config.Budget

	mu       sync.Mutex
	inFlight map[string]float64 // userID -> sum of reserved, not-yet-released cost
}

// New creates a MemoryEnforcer backed by store, using per-user limits from cfg.
func New(store SpendLookup, users map[string]config.Budget) *MemoryEnforcer {
	return &MemoryEnforcer{store: store, users: users, inFlight: make(map[string]float64)}
}

func (e *MemoryEnforcer) Reserve(ctx context.Context, userID string, estimatedCost float64) (Result, Release, error) {
	budget, hasLimit := e.users[userID]
	if !hasLimit || budget.DailyLimitUSD <= 0 {
		return Result{Allowed: true}, noopRelease, nil
	}

	since := startOfDayUTC(time.Now())

	e.mu.Lock()
	defer e.mu.Unlock()

	spent, err := e.store.SpendSince(ctx, userID, since)
	if err != nil {
		return Result{}, noopRelease, fmt.Errorf("checking budget for %s: %w", userID, err)
	}

	reserved := e.inFlight[userID]
	projected := spent + reserved + estimatedCost

	if projected > budget.DailyLimitUSD {
		return Result{
			Allowed:   false,
			LimitUSD:  budget.DailyLimitUSD,
			SpentUSD:  spent,
			Remaining: budget.DailyLimitUSD - spent - reserved,
		}, noopRelease, nil
	}

	e.inFlight[userID] = reserved + estimatedCost
	// The real cost, once known, is persisted separately via the logging
	// store — SpendSince will pick it up on the next Reserve. Release only
	// needs to free this reservation's placeholder, not reconcile it
	// against the actual cost.
	release := func(float64) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.inFlight[userID] -= estimatedCost
		if e.inFlight[userID] < 0 {
			e.inFlight[userID] = 0
		}
	}

	return Result{
		Allowed:   true,
		LimitUSD:  budget.DailyLimitUSD,
		SpentUSD:  spent,
		Remaining: budget.DailyLimitUSD - projected,
	}, release, nil
}

func startOfDayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
