// Package budget enforces per-user daily spending limits.
package budget

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Oluiy/ai-cost-guard/internal/config"
)

// SpendLookup returns total persisted spend for a user since a given time. Satisfied by *logging.Store.
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

// Release is a function that reconciles a Reserve call's estimated cost with the request's actual cost (0 if the request never completed, e.g. every provider failed).
// Call it exactly once per successful Reserve, regardless of outcome.
type Release func(actualCost float64)

var noopRelease Release = func(float64) {}

// Enforcers are responsible for enforcing budget limits on incoming requests, including memory-based and Redis-based implementations.
type Enforcer interface {
	Reserve(ctx context.Context, userID string, estimatedCost float64) (Result, Release, error)
}

// MemoryEnforcer enforces budgets within a single process.
// RedisEnforcer enforces budgets using a Redis-based store.
// The MemoryEnforcer is a simple in-memory implementation that does not require a Redis store.


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
	// Reservation is released when the actual cost is known, so we can adjust the in-flight total.
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
