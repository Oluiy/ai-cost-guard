package budget

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Oluiy/ai-cost-guard/internal/config"
)


// RedisEnforcer enforces per-user daily budgets using Redis as a shared,
// atomic ledger, so budget enforcement is correct across multiple ai-guard
// instances behind a load balancer — not just within one process, unlike MemoryEnforcer.
// RedisEnforcer is the Redis-backed implementation of the Enforcer interface,
// providing a distributed, atomic budget enforcement solution.
type RedisEnforcer struct {
	client *redis.Client
	users  map[string]config.Budget
	prefix string
}

// NewRedisEnforcer connects to a Redis instance at the given URL, and returns an Enforcer backed by it.
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

// budgetKeyTTL bounds how long a day's budget key lives: a full UTC day + slack (24 + 2am slack)
const budgetKeyTTL = 26 * time.Hour

// validation check, to prevent deadlocks(mutex lock), and commit the reservation if it passes, redis reserveScript
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

// reconcileTimeout bounds the best-effort Release call.
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
		// Best-effort: if this fails, Reservation still succeeds, so we don't error the request.
		// We don't return the error, because we already have the reservation result to return to the caller.
		_ = reconcileScript.Run(rctx, e.client, []string{key}, delta).Err()
	}

	return Result{
		Allowed:   true,
		LimitUSD:  budget.DailyLimitUSD,
		SpentUSD:  current,
		Remaining: budget.DailyLimitUSD - current - estimatedCost,
	}, release, nil
}
