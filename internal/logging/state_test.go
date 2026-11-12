package logging

import (
	"context"
	"testing"

	"github.com/Oluiy/ai-cost-guard/internal/config"
)

func seedConfig() *config.Config {
	return &config.Config{
		Providers: map[string]config.Provider{"openai": {APIKey: "k"}},
		Dashboard: config.DashboardConfig{SessionSecret: "secret-1", SessionTTLHours: 24,
			Users: []config.DashboardUser{{Username: "admin", PasswordHash: "hash-1"}}},
		Users:    map[string]config.Budget{"alice": {DailyLimitUSD: 5}},
		Keys:     map[string]string{"sk-guard-alice": "alice"},
		Fallback: []string{"gpt-4o-mini"},
		Cache:    config.CacheConfig{Enabled: true, TTL: 300},
	}
}

func TestDialect_StateRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		if _, ok, err := s.LoadState(ctx); ok || err != nil {
			t.Fatalf("fresh database: ok=%v err=%v, want ok=false", ok, err)
		}
		want := seedConfig().State()
		if err := s.SaveState(ctx, want); err != nil {
			t.Fatalf("save: %v", err)
		}
		got, ok, err := s.LoadState(ctx)
		if err != nil || !ok {
			t.Fatalf("load: ok=%v err=%v", ok, err)
		}
		if got.Dashboard.SessionSecret != "secret-1" || got.Dashboard.SessionTTLHours != 24 || len(got.Dashboard.Users) != 1 ||
			got.Dashboard.Users[0].PasswordHash != "hash-1" || got.Users["alice"].DailyLimitUSD != 5 ||
			got.Keys["sk-guard-alice"] != "alice" || len(got.Fallback) != 1 || !got.CacheEnabled || got.CacheTTLSeconds != 300 {
			t.Fatalf("state did not round-trip: %+v", got)
		}

		// Saving again replaces (upsert), it doesn't add a second row.
		want.Users["bob"] = config.Budget{DailyLimitUSD: 9}
		if err := s.SaveState(ctx, want); err != nil {
			t.Fatalf("second save: %v", err)
		}
		got, _, _ = s.LoadState(ctx)
		if got.Users["bob"].DailyLimitUSD != 9 {
			t.Fatalf("second save was not applied: %+v", got.Users)
		}
	})
}

// The behaviours that make config.yaml safe to redeploy from: it seeds the
// database exactly once, and after that the database wins.
func TestDialect_ConfigSeedsOnceThenDatabaseWins(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		first := config.NewSettings("", seedConfig())
		seeded, err := first.AttachStore(ctx, s)
		if err != nil || !seeded {
			t.Fatalf("first boot should seed: seeded=%v err=%v", seeded, err)
		}

		// An operator resets the password: this is the whole point.
		d := first.Dashboard()
		d.Users = []config.DashboardUser{{Username: "admin", PasswordHash: "hash-2"}}
		d.SessionSecret = "secret-2"
		if err := first.ReplaceDashboard(d); err != nil {
			t.Fatalf("ReplaceDashboard: %v", err)
		}

		// Redeploy: a fresh process starts from the SAME original config
		// (still carrying hash-1), because the platform re-supplies it.
		second := config.NewSettings("", seedConfig())
		seeded, err = second.AttachStore(ctx, s)
		if err != nil || seeded {
			t.Fatalf("second boot must not re-seed: seeded=%v err=%v", seeded, err)
		}
		got := second.Dashboard()
		if got.Users[0].PasswordHash != "hash-2" || got.SessionSecret != "secret-2" {
			t.Fatalf("the redeploy reverted the password reset to the config's copy: %+v", got)
		}
	})
}

// Two instances share one database: a change made through one must reach
// the other, which is what makes a multi-instance deployment coherent.
func TestDialect_SettingsPropagateBetweenInstances(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		a := config.NewSettings("", seedConfig())
		b := config.NewSettings("", seedConfig())
		for _, st := range []*config.Settings{a, b} {
			if _, err := st.AttachStore(ctx, s); err != nil {
				t.Fatal(err)
			}
		}

		snap := a.Snapshot()
		snap.Users["carol"] = 7
		snap.CacheEnabled = false
		issued, err := a.Apply(snap)
		if err != nil {
			t.Fatalf("Apply on instance A: %v", err)
		}

		if _, ok := b.Budget("carol"); ok {
			t.Fatal("instance B should not see the change before it reloads")
		}
		if err := b.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		if bud, ok := b.Budget("carol"); !ok || bud.DailyLimitUSD != 7 || b.CacheEnabled() {
			t.Fatalf("instance B did not pick up A's change after Reload: budget=%+v ok=%v cache=%v", bud, ok, b.CacheEnabled())
		}
		// The virtual key A issued for the new user must authenticate on B.
		if uid, ok := b.UserForKey(issued["carol"]); !ok || uid != "carol" {
			t.Fatalf("a key issued on instance A doesn't work on instance B: uid=%q ok=%v", uid, ok)
		}
	})
}
