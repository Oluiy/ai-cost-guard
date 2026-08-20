package budget

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aicostguard/ai-cost-guard/internal/config"
)

// fakeStore stubs SpendLookup with a fixed spend per user.
type fakeStore struct {
	spend map[string]float64
	err   error
}

func (f *fakeStore) SpendSince(_ context.Context, userID string, _ time.Time) (float64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.spend[userID], nil
}

func TestReserve_NoConfiguredBudgetAlwaysAllowed(t *testing.T) {
	e := New(&fakeStore{spend: map[string]float64{"alice": 1000}}, map[string]config.Budget{})

	result, release, err := e.Reserve(context.Background(), "alice", 999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatal("expected user with no configured budget to always be allowed")
	}
	release(0) // must not panic even though nothing was reserved
}

func TestReserve_UnderLimitAllowed(t *testing.T) {
	e := New(&fakeStore{spend: map[string]float64{"alice": 2}}, map[string]config.Budget{
		"alice": {DailyLimitUSD: 5},
	})

	result, release, err := e.Reserve(context.Background(), "alice", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatal("expected user under their daily limit to be allowed")
	}
	if result.Remaining != 2 {
		t.Fatalf("got remaining %.2f, want 2.00 (5 limit - 2 spent - 1 reserved)", result.Remaining)
	}
	release(0)
}

func TestReserve_EstimateThatWouldExceedLimitBlocked(t *testing.T) {
	e := New(&fakeStore{spend: map[string]float64{"alice": 4.5}}, map[string]config.Budget{
		"alice": {DailyLimitUSD: 5},
	})

	// Already-spent (4.5) + this request's worst-case estimate (1) exceeds
	// the limit, even though persisted spend alone is still under it.
	result, _, err := e.Reserve(context.Background(), "alice", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected request whose worst-case estimate would exceed the limit to be blocked")
	}
}

func TestReserve_OverLimitBlocked(t *testing.T) {
	e := New(&fakeStore{spend: map[string]float64{"alice": 5.5}}, map[string]config.Budget{
		"alice": {DailyLimitUSD: 5},
	})

	result, _, err := e.Reserve(context.Background(), "alice", 0.01)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected user over their daily limit to be blocked")
	}
}

func TestReserve_DifferentUserUnaffected(t *testing.T) {
	e := New(&fakeStore{spend: map[string]float64{"alice": 100}}, map[string]config.Budget{
		"alice": {DailyLimitUSD: 5},
		"bob":   {DailyLimitUSD: 5},
	})

	result, release, err := e.Reserve(context.Background(), "bob", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatal("expected bob's budget to be independent of alice's spend")
	}
	release(0)
}

func TestReserve_SecondReservationSeesFirstInFlight(t *testing.T) {
	// Persisted spend is 0, but a still-in-flight reservation should count
	// against the limit exactly like this is the race window Reserve exists
	// to close: two concurrent requests must not both see "room" for the
	// same dollar.
	e := New(&fakeStore{spend: map[string]float64{"alice": 0}}, map[string]config.Budget{
		"alice": {DailyLimitUSD: 5},
	})

	result1, release1, err := e.Reserve(context.Background(), "alice", 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result1.Allowed {
		t.Fatal("expected first reservation of 4 against a 5 limit to be allowed")
	}

	result2, _, err := e.Reserve(context.Background(), "alice", 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result2.Allowed {
		t.Fatal("expected second concurrent reservation to be blocked by the first's still-in-flight amount")
	}

	release1(0)

	result3, release3, err := e.Reserve(context.Background(), "alice", 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result3.Allowed {
		t.Fatal("expected reservation to succeed once the prior one was released")
	}
	release3(0)
}

func TestReserve_ConcurrentReservationsNeverOverAdmit(t *testing.T) {
	e := New(&fakeStore{spend: map[string]float64{"alice": 0}}, map[string]config.Budget{
		"alice": {DailyLimitUSD: 10},
	})

	const attempts = 50
	const costEach = 1.0 // 50 attempts x $1 against a $10 limit -> at most 10 can be admitted

	var admitted int64
	var wg sync.WaitGroup
	releases := make(chan Release, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, release, err := e.Reserve(context.Background(), "alice", costEach)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if result.Allowed {
				atomic.AddInt64(&admitted, 1)
				releases <- release
			}
		}()
	}
	wg.Wait()
	close(releases)
	for release := range releases {
		release(0)
	}

	if admitted > 10 {
		t.Fatalf("admitted %d requests at $1 each against a $10 limit; budget was over-committed", admitted)
	}
}

func TestReserve_ErrorFromStorePropagates(t *testing.T) {
	e := New(&fakeStore{err: errBoom}, map[string]config.Budget{
		"alice": {DailyLimitUSD: 5},
	})

	_, _, err := e.Reserve(context.Background(), "alice", 1)
	if err == nil {
		t.Fatal("expected store error to propagate")
	}
}

var errBoom = &storeError{"boom"}

type storeError struct{ msg string }

func (e *storeError) Error() string { return e.msg }
