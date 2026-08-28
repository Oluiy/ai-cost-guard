package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func baseConfig() *Config {
	return &Config{
		Port:    8787,
		DataDir: ".",
		Providers: map[string]Provider{
			"openai":    {APIKey: "sk-real-secret", BaseURL: "https://api.openai.com/v1"},
			"anthropic": {APIKey: "sk-ant-secret"},
		},
		Cache:     CacheConfig{Enabled: true, TTL: 300, Backend: "memory"},
		Users:     map[string]Budget{"alice": {DailyLimitUSD: 5}},
		Keys:      map[string]string{"sk-guard-abc": "alice"},
		Fallback:  []string{"gpt-4o-mini"},
		Dashboard: DashboardConfig{SessionSecret: "super-secret-signing-key"},
	}
}

func TestSettings_ReadsCurrentValues(t *testing.T) {
	s := NewSettings("", baseConfig())

	if !s.CacheEnabled() {
		t.Error("CacheEnabled = false, want true")
	}
	if got := s.CacheTTLSeconds(); got != 300 {
		t.Errorf("CacheTTLSeconds = %d, want 300", got)
	}
	if b, ok := s.Budget("alice"); !ok || b.DailyLimitUSD != 5 {
		t.Errorf("Budget(alice) = %v, %v; want 5, true", b.DailyLimitUSD, ok)
	}
	if _, ok := s.Budget("nobody"); ok {
		t.Error("Budget(nobody) reported a limit that was never configured")
	}
}

func TestSettings_ApplyChangesWhatTheRequestPathReads(t *testing.T) {
	s := NewSettings("", baseConfig())

	_, err := s.Apply(Editable{
		CacheEnabled:    false,
		CacheTTLSeconds: 25200, // 7h, the scheduled-job case
		Fallback:        []string{"claude-haiku-4-5"},
		Users:           map[string]float64{"alice": 12, "bob": 3},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if s.CacheEnabled() {
		t.Error("cache still reported enabled after being turned off")
	}
	if got := s.CacheTTLSeconds(); got != 25200 {
		t.Errorf("CacheTTLSeconds = %d, want 25200", got)
	}
	if b, _ := s.Budget("alice"); b.DailyLimitUSD != 12 {
		t.Errorf("alice's limit = %v, want 12", b.DailyLimitUSD)
	}
	if b, ok := s.Budget("bob"); !ok || b.DailyLimitUSD != 3 {
		t.Errorf("bob = %v, %v; want 3, true", b.DailyLimitUSD, ok)
	}
	if got := s.Fallback(); len(got) != 1 || got[0] != "claude-haiku-4-5" {
		t.Errorf("Fallback = %v, want [claude-haiku-4-5]", got)
	}
}

// Fallback's caller appends to the returned slice to build its attempt
// order. Handing back the live backing array would let that append race
// with Apply, or worse, mutate configuration.
func TestSettings_FallbackReturnsACopy(t *testing.T) {
	s := NewSettings("", baseConfig())

	got := s.Fallback()
	got[0] = "mutated"

	if again := s.Fallback(); again[0] != "gpt-4o-mini" {
		t.Errorf("mutating the returned slice changed live config: %v", again)
	}
}

func TestSettings_SnapshotIsDecoupledFromLiveState(t *testing.T) {
	s := NewSettings("", baseConfig())

	snap := s.Snapshot()
	snap.Users["alice"] = 999
	snap.Fallback[0] = "mutated"

	if b, _ := s.Budget("alice"); b.DailyLimitUSD != 5 {
		t.Errorf("mutating a snapshot changed live budgets: %v", b.DailyLimitUSD)
	}
	if got := s.Fallback(); got[0] != "gpt-4o-mini" {
		t.Errorf("mutating a snapshot changed live fallback: %v", got)
	}
}

func TestSettings_RejectsInvalidUpdatesWithoutChangingAnything(t *testing.T) {
	cases := []struct {
		name string
		in   Editable
	}{
		{"zero ttl", Editable{CacheTTLSeconds: 0}},
		{"negative ttl", Editable{CacheTTLSeconds: -1}},
		{"absurd ttl", Editable{CacheTTLSeconds: maxCacheTTLSeconds + 1}},
		{"negative budget", Editable{CacheTTLSeconds: 300, Users: map[string]float64{"alice": -1}}},
		{"empty user id", Editable{CacheTTLSeconds: 300, Users: map[string]float64{"": 5}}},
		{"empty fallback entry", Editable{CacheTTLSeconds: 300, Fallback: []string{""}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSettings("", baseConfig())
			if _, err := s.Apply(tc.in); err == nil {
				t.Fatal("expected the update to be rejected")
			}
			// A rejected update must leave the running config untouched.
			if !s.CacheEnabled() || s.CacheTTLSeconds() != 300 {
				t.Errorf("live config changed despite a rejected update: enabled=%v ttl=%d",
					s.CacheEnabled(), s.CacheTTLSeconds())
			}
			if b, ok := s.Budget("alice"); !ok || b.DailyLimitUSD != 5 {
				t.Errorf("budgets changed despite a rejected update: %v", b.DailyLimitUSD)
			}
		})
	}
}

func TestSettings_ApplyPersistsToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := baseConfig()
	if err := Save(path, cfg); err != nil {
		t.Fatalf("seeding config: %v", err)
	}

	s := NewSettings(path, cfg)
	if _, err := s.Apply(Editable{
		CacheEnabled: true, CacheTTLSeconds: 900,
		Users: map[string]float64{"alice": 7},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if reloaded.Cache.TTL != 900 {
		t.Errorf("persisted ttl = %d, want 900", reloaded.Cache.TTL)
	}
	if reloaded.Users["alice"].DailyLimitUSD != 7 {
		t.Errorf("persisted limit = %v, want 7", reloaded.Users["alice"].DailyLimitUSD)
	}

	// The parts the dashboard must never touch have to survive a write
	// intact — a round-trip that dropped provider keys would lock the
	// gateway out of every provider on the next restart.
	if reloaded.Providers["openai"].APIKey != "sk-real-secret" {
		t.Error("provider api_key was lost or altered by a settings update")
	}
	if reloaded.Keys["sk-guard-abc"] != "alice" {
		t.Error("virtual key mapping was lost by a settings update")
	}
	if reloaded.Dashboard.SessionSecret != "super-secret-signing-key" {
		t.Error("session secret was lost by a settings update")
	}
}

func TestSettings_PersistedFileStaysOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := baseConfig()
	if err := Save(path, cfg); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // simulate a loose file
		t.Fatalf("chmod: %v", err)
	}

	s := NewSettings(path, cfg)
	// users must be carried through: baseConfig has a live key mapped to
	// alice, and dropping her budget is refused by design.
	if _, err := s.Apply(Editable{
		CacheEnabled: true, CacheTTLSeconds: 600,
		Users: map[string]float64{"alice": 5},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("config is %o after a dashboard-driven write; provider keys are readable by other local users", perm)
	}
}

// The reason Settings exists at all. Under -race this fails loudly if the
// request path reads these fields without synchronization.
func TestSettings_ConcurrentReadsAndWritesAreRaceFree(t *testing.T) {
	s := NewSettings("", baseConfig())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.CacheEnabled()
					_ = s.CacheTTLSeconds()
					_ = s.Fallback()
					_, _ = s.Budget("alice")
					_ = s.Snapshot()
				}
			}
		}()
	}

	for i := 0; i < 200; i++ {
		ttl := 60 + i
		if _, err := s.Apply(Editable{
			CacheEnabled:    i%2 == 0,
			CacheTTLSeconds: ttl,
			Fallback:        []string{"gpt-4o-mini", "claude-haiku-4-5"},
			Users:           map[string]float64{"alice": float64(i)},
		}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}

	close(stop)
	wg.Wait()
}

// The sharpest edge in the feature: keys: is terminal-only, budgets are
// dashboard-editable, so removing a user whose key is still live would
// silently uncap that key. One click, no warning, on a tool whose whole
// job is capping spend.
func TestSettings_RefusesToOrphanALiveVirtualKey(t *testing.T) {
	s := NewSettings("", baseConfig()) // keys: sk-guard-abc -> alice

	_, err := s.Apply(Editable{
		CacheEnabled: true, CacheTTLSeconds: 300,
		Users: map[string]float64{}, // alice removed
	})
	if err == nil {
		t.Fatal("expected removing a budgeted user with a live key to be refused")
	}
	if !strings.Contains(err.Error(), "alice") {
		t.Errorf("error should name the affected user, got: %v", err)
	}

	// And the refusal must leave the budget intact.
	if b, ok := s.Budget("alice"); !ok || b.DailyLimitUSD != 5 {
		t.Errorf("alice's budget changed despite the refusal: %v, %v", b.DailyLimitUSD, ok)
	}
}

// Setting 0 is the supported way to make a key unmetered: explicit, and
// visible in the config afterwards.
func TestSettings_AllowsUnmeteringAKeyViaZeroLimit(t *testing.T) {
	s := NewSettings("", baseConfig())

	if _, err := s.Apply(Editable{
		CacheEnabled: true, CacheTTLSeconds: 300,
		Users: map[string]float64{"alice": 0},
	}); err != nil {
		t.Fatalf("setting a 0 limit should be allowed: %v", err)
	}
	if b, ok := s.Budget("alice"); !ok || b.DailyLimitUSD != 0 {
		t.Errorf("expected an explicit 0 limit, got %v, %v", b.DailyLimitUSD, ok)
	}
}

// A user with no key attached is just a stale budget entry, so removing
// it is safe and shouldn't be blocked.
func TestSettings_AllowsRemovingAUserWithNoKeys(t *testing.T) {
	cfg := baseConfig()
	cfg.Users["orphan"] = Budget{DailyLimitUSD: 3} // no key maps here
	s := NewSettings("", cfg)

	if _, err := s.Apply(Editable{
		CacheEnabled: true, CacheTTLSeconds: 300,
		Users: map[string]float64{"alice": 5},
	}); err != nil {
		t.Fatalf("removing a keyless budget should be allowed: %v", err)
	}
	if _, ok := s.Budget("orphan"); ok {
		t.Error("expected the keyless budget to be removed")
	}
}

// NaN compares false against every bound, so a plain "limit < 0" check
// lets it through — and a NaN limit makes every downstream budget
// comparison false, silently disabling enforcement for that user.
func TestValidateEditable_RejectsNonFiniteLimits(t *testing.T) {
	for name, v := range map[string]float64{
		"NaN":  math.NaN(),
		"+Inf": math.Inf(1),
		"-Inf": math.Inf(-1),
	} {
		err := ValidateEditable(Editable{
			CacheTTLSeconds: 300,
			Users:           map[string]float64{"alice": v},
		})
		if err == nil {
			t.Errorf("%s limit was accepted; budget enforcement would silently stop working", name)
		}
	}
}

func TestValidateEditable_RejectsWhitespaceOnlyNames(t *testing.T) {
	if err := ValidateEditable(Editable{
		CacheTTLSeconds: 300,
		Users:           map[string]float64{"   ": 5},
	}); err == nil {
		t.Error("whitespace-only user id was accepted")
	}
	if err := ValidateEditable(Editable{
		CacheTTLSeconds: 300,
		Fallback:        []string{"  "},
	}); err == nil {
		t.Error("whitespace-only fallback model was accepted")
	}
}
