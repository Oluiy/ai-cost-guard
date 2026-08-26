package budget

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisEnforcer enforces per-user daily budgets using Redis as a shared,
// atomic ledger, correct across multiple fitguard instances behind a
// load balancer, unlike MemoryEnforcer.
type RedisEnforcer struct {
	client     *redis.Client
	users      BudgetLookup
	prefix     string
	failClosed bool
}

// SetFailClosed makes Reserve reject users with no configured budget
// instead of treating them as unlimited. See
// config.BudgetBackendConfig.FailClosed.
func (e *RedisEnforcer) SetFailClosed(failClosed bool) {
	e.failClosed = failClosed
}

// NewRedisEnforcer connects to Redis at url and returns an Enforcer backed by it.
func NewRedisEnforcer(url string, users BudgetLookup) (*RedisEnforcer, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parsing redis url: %w", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}
	return &RedisEnforcer{client: client, users: users, prefix: "fitguard:budget:"}, nil
}

// budgetKeyTTL bounds how long a day's budget key lives.
const budgetKeyTTL = 26 * time.Hour

// reserveScript atomically checks and commits a reservation in one round trip.
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
	// Same semantics as MemoryEnforcer.
	budget, skip, denied := checkConfigured(e.users, userID, e.failClosed)
	if denied != nil {
		return *denied, noopRelease, nil
	}
	if skip {
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
		// Best-effort: the reservation already succeeded regardless.
		_ = reconcileScript.Run(rctx, e.client, []string{key}, delta).Err()
	}

	return Result{
		Allowed:   true,
		LimitUSD:  budget.DailyLimitUSD,
		SpentUSD:  current,
		Remaining: budget.DailyLimitUSD - current - estimatedCost,
	}, release, nil
}
