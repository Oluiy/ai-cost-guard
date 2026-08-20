package budget

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/aicostguard/ai-cost-guard/internal/config"
)

// RedisEnforcer enforces per-user daily budgets using Redis as a shared,
// atomic ledger, so budget enforcement is correct across multiple ai-guard
// instances behind a load balancer — not just within one process, unlike
// MemoryEnforcer.
//
// MemoryEnforcer has two separate sources of truth per user: "already
// spent" (a SQLite query) and "currently reserved" (an in-process map).
// That works because both live in the same process. Across processes it
// doesn't: instance A's in-flight reservation is invisible to instance B,
// and each instance has its own SQLite file, so "already spent" doesn't
// even agree between them either.
//
// RedisEnforcer collapses both into a single atomically-updated running
// total per user per day, stored as one Redis key. Reserve adds the
// request's worst-case estimate to that key immediately — atomically, via
// a Lua script, so the read-check-increment can't race across instances
// the way it would with separate GET then INCR calls. Release then
// adjusts the same key down to the request's real cost once it's known.
// The Redis key is the sole source of truth for "how much of today's
// budget is committed" — there's no separate persisted-spend query to
// fall out of sync with it.
type RedisEnforcer struct {
	client *redis.Client
	users  map[string]config.Budget
	prefix string
}

// NewRedisEnforcer connects to a Redis instance at the given URL
// (e.g. "redis://localhost:6379/0") and returns an Enforcer backed by it.
func NewRedisEnforcer(url string, users map[string]config.Budget) (*RedisEnforcer, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parsing redis url: %w", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}
	return &RedisEnforcer{client: client, users: users, prefix: "aiguard:budget:"}, nil
}

// budgetKeyTTL bounds how long a day's budget key lives: a full UTC day
// plus slack, so a finished day's key expires on its own instead of
// accumulating forever, without needing a cleanup job.
const budgetKeyTTL = 26 * time.Hour

// reserveScript atomically checks whether current+estimate would exceed
// limit, and if not, commits the reservation — all inside Redis, so two
// instances calling this concurrently for the same user can't both read
// "room available" before either commits (the classic race MemoryEnforcer
// closes with an in-process mutex; here Redis's single-threaded script
// execution is the mutex, shared across every instance).
var reserveScript = redis.NewScript(`
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local estimate = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
if current + estimate > limit then
    return {0, tostring(current)}
end
redis.call('INCRBYFLOAT', KEYS[1], estimate)
redis.call('EXPIRE', KEYS[1], ttl)
return {1, tostring(current)}
`)

// reconcileScript applies a (possibly negative) delta to true up a
// reservation's estimate to the request's real cost.
var reconcileScript = redis.NewScript(`
local delta = tonumber(ARGV[1])
if delta ~= 0 then
    redis.call('INCRBYFLOAT', KEYS[1], delta)
end
return 1
`)

func (e *RedisEnforcer) key(userID string) string {
	return e.prefix + userID + ":" + time.Now().UTC().Format("2006-01-02")
}

// reconcileTimeout bounds the best-effort Release call. It deliberately
// doesn't reuse the request's context: by the time Release runs, the
// response has already been sent, so the request context may already be
// cancelled/torn down even though the reconciliation itself is still
// legitimate work worth attempting on its own, short budget.
const reconcileTimeout = 5 * time.Second

func (e *RedisEnforcer) Reserve(ctx context.Context, userID string, estimatedCost float64) (Result, Release, error) {
	budget, hasLimit := e.users[userID]
	if !hasLimit || budget.DailyLimitUSD <= 0 {
		return Result{Allowed: true}, noopRelease, nil
	}

	key := e.key(userID)

	res, err := reserveScript.Run(ctx, e.client, []string{key},
		estimatedCost, budget.DailyLimitUSD, budgetKeyTTL.Seconds()).Result()
	if err != nil {
		return Result{}, noopRelease, fmt.Errorf("checking budget for %s via redis: %w", userID, err)
	}

	arr, ok := res.([]interface{})
	if !ok || len(arr) != 2 {
		return Result{}, noopRelease, fmt.Errorf("unexpected response shape from redis budget script: %#v", res)
	}
	allowed, ok := arr[0].(int64)
	if !ok {
		return Result{}, noopRelease, fmt.Errorf("unexpected \"allowed\" type from redis budget script: %#v (want int64)", arr[0])
	}
	currentStr, ok := arr[1].(string)
	if !ok {
		return Result{}, noopRelease, fmt.Errorf("unexpected \"current\" type from redis budget script: %#v (want string)", arr[1])
	}
	current, err := strconv.ParseFloat(currentStr, 64)
	if err != nil {
		return Result{}, noopRelease, fmt.Errorf("parsing redis budget script response %q: %w", currentStr, err)
	}

	if allowed == 0 {
		return Result{
			Allowed:   false,
			LimitUSD:  budget.DailyLimitUSD,
			SpentUSD:  current,
			Remaining: budget.DailyLimitUSD - current,
		}, noopRelease, nil
	}

	release := func(actualCost float64) {
		delta := actualCost - estimatedCost
		if delta == 0 {
			return
		}
		rctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
		defer cancel()
		// Best-effort: if this fails, the reservation's estimate (an
		// upper bound by construction) stays committed instead of being
		// trued up to the lower real cost. That leaves the budget
		// slightly more conservative than exact, which is the safe
		// direction to fail in — not worth erroring the request that
		// already got its response over.
		_ = reconcileScript.Run(rctx, e.client, []string{key}, delta).Err()
	}

	return Result{
		Allowed:   true,
		LimitUSD:  budget.DailyLimitUSD,
		SpentUSD:  current,
		Remaining: budget.DailyLimitUSD - current - estimatedCost,
	}, release, nil
}
