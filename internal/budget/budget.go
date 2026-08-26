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

// BudgetLookup resolves a user's configured daily limit and whether one
// exists at all. An interface, not a map, because limits can change
// while ai-guard runs. Satisfied by *config.Settings.
type BudgetLookup interface {
	Budget(userID string) (config.Budget, bool)
}

// Result describes the outcome of a budget check.
type Result struct {
	Allowed   bool
	LimitUSD  float64
	SpentUSD  float64
	Remaining float64
	// Reason explains a denial that isn't a plain over-the-limit case.
	// Empty otherwise.
	Reason string
}

// Release reconciles a Reserve call's estimated cost with the real cost
// (0 if the request never completed). Call exactly once per Reserve.
type Release func(actualCost float64)

var noopRelease Release = func(float64) {}

// Enforcer enforces budget limits on incoming requests.
type Enforcer interface {
	Reserve(ctx context.Context, userID string, estimatedCost float64) (Result, Release, error)
}

// MemoryEnforcer enforces budgets within a single process. See
// RedisEnforcer for the cross-instance version.
type MemoryEnforcer struct {
	store      SpendLookup
	users      BudgetLookup
	failClosed bool

	mu       sync.Mutex
	inFlight map[string]float64 // userID -> sum of reserved, not-yet-released cost
}

// New creates a MemoryEnforcer backed by store, using per-user limits from cfg.
func New(store SpendLookup, users BudgetLookup) *MemoryEnforcer {
	return &MemoryEnforcer{store: store, users: users, inFlight: make(map[string]float64)}
}

// NewFailClosed is New, but rejects users that have no configured budget
// rather than treating them as unlimited. See config.BudgetBackendConfig.FailClosed.
func NewFailClosed(store SpendLookup, users BudgetLookup, failClosed bool) *MemoryEnforcer {
	e := New(store, users)
	e.failClosed = failClosed
	return e
}

// NoBudgetReason is the denial message used when fail-closed enforcement
// is on and a key's user has no configured budget.
const NoBudgetReason = "no daily budget is configured for this key's user, and budget.fail_closed is enabled"

// checkConfigured classifies a user against the configured budgets.
// Returns a denial as *Result, not an error, since callers treat a
// Reserve error as "check broke" and let the request through. An
// explicit daily_limit_usd of 0 means unmetered; a missing users: entry
// is what fail_closed guards against.
func checkConfigured(users BudgetLookup, userID string, failClosed bool) (b config.Budget, skip bool, denied *Result) {
	budget, hasLimit := users.Budget(userID)
	if !hasLimit {
		if failClosed {
			return config.Budget{}, false, &Result{Allowed: false, Reason: NoBudgetReason}
		}
		return config.Budget{}, true, nil
	}
	if budget.DailyLimitUSD <= 0 {
		return budget, true, nil
	}
	return budget, false, nil
}

func (e *MemoryEnforcer) Reserve(ctx context.Context, userID string, estimatedCost float64) (Result, Release, error) {
	budget, skip, denied := checkConfigured(e.users, userID, e.failClosed)
	if denied != nil {
		return *denied, noopRelease, nil
	}
	if skip {
		return Result{Allowed: true}, noopRelease, nil
	}

	since := startOfDayUTC(time.Now())

	e.mu.Lock()
	defer e.mu.Unlock()

	spent, err := e.store.SpendSince(ctx, userID, since)
	if err != nil {
		// fail_closed treats "couldn't check" as "refuse", not "allow".
		if e.failClosed {
			return Result{
				Allowed: false,
				Reason:  "budget lookup failed and budget.fail_closed is enabled",
			}, noopRelease, nil
		}
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
