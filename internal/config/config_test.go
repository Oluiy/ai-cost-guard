package config

import (
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		Port:      DefaultPort,
		DataDir:   ".",
		Providers: map[string]Provider{"openai": {APIKey: "sk-test"}},
		Cache:     CacheConfig{Backend: "memory"},
		Users:     map[string]Budget{},
		Keys:      map[string]string{},
	}
}

func TestValidate_ValidConfigPasses(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_ZeroPortDefaults(t *testing.T) {
	cfg := validConfig()
	cfg.Port = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != DefaultPort {
		t.Fatalf("got port %d, want default %d", cfg.Port, DefaultPort)
	}
}

func TestValidate_RejectsOutOfRangePort(t *testing.T) {
	for _, port := range []int{-1, 70000, 100000} {
		cfg := validConfig()
		cfg.Port = port
		if err := cfg.Validate(); err == nil {
			t.Errorf("port %d: expected error, got none", port)
		}
	}
}

func TestValidate_RejectsNoProviders(t *testing.T) {
	cfg := validConfig()
	cfg.Providers = nil
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for config with no providers")
	}
}

func TestValidate_RejectsUnknownCacheBackend(t *testing.T) {
	cfg := validConfig()
	cfg.Cache.Backend = "memcached"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unrecognized cache backend")
	}
}

func TestValidate_RejectsRedisEnabledWithoutURL(t *testing.T) {
	cfg := validConfig()
	cfg.Cache.Enabled = true
	cfg.Cache.Backend = "redis"
	cfg.Cache.RedisURL = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for redis backend with no redis_url")
	}
}

func TestValidate_BudgetBackendDefaultsToLocal(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BudgetBackend.Backend != "local" {
		t.Fatalf("got budget backend %q, want \"local\"", cfg.BudgetBackend.Backend)
	}
}

func TestValidate_RejectsUnknownBudgetBackend(t *testing.T) {
	cfg := validConfig()
	cfg.BudgetBackend.Backend = "postgres"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unrecognized budget backend")
	}
}

func TestValidate_RejectsRedisBudgetBackendWithoutURL(t *testing.T) {
	cfg := validConfig()
	cfg.BudgetBackend.Backend = "redis"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for redis budget backend with no redis_url and no cache fallback")
	}
}

func TestValidate_RedisBudgetBackendReusesCacheRedisURL(t *testing.T) {
	cfg := validConfig()
	cfg.Cache.Backend = "redis"
	cfg.Cache.RedisURL = "redis://cache-host:6379/0"
	cfg.BudgetBackend.Backend = "redis"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BudgetBackend.RedisURL != "redis://cache-host:6379/0" {
		t.Fatalf("expected budget.redis_url to fall back to cache.redis_url, got %q", cfg.BudgetBackend.RedisURL)
	}
}

func TestValidate_RedisBudgetBackendExplicitURLNotOverridden(t *testing.T) {
	cfg := validConfig()
	cfg.Cache.Backend = "redis"
	cfg.Cache.RedisURL = "redis://cache-host:6379/0"
	cfg.BudgetBackend.Backend = "redis"
	cfg.BudgetBackend.RedisURL = "redis://budget-host:6379/1"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BudgetBackend.RedisURL != "redis://budget-host:6379/1" {
		t.Fatalf("explicit budget.redis_url was overwritten, got %q", cfg.BudgetBackend.RedisURL)
	}
}

func TestWarnings_FlagsEmptyProviderAPIKey(t *testing.T) {
	cfg := validConfig()
	cfg.Providers["anthropic"] = Provider{APIKey: ""}
	warnings := cfg.Warnings()
	if !anyContains(warnings, "anthropic") {
		t.Fatalf("expected a warning mentioning the empty-key provider, got: %v", warnings)
	}
}

// This is the config-file equivalent of the CLI's silent-zero bug: a typo
// in a key's user_id (or editing config.yaml by hand) silently produces an
// unlimited budget with no error, because Enforcer.Reserve treats "no
// budget entry" as "no limit". Warnings must surface it.
func TestWarnings_FlagsKeyMappedToUndefinedUser(t *testing.T) {
	cfg := validConfig()
	cfg.Keys["sk-guard-abc123456789"] = "user_typo"
	warnings := cfg.Warnings()
	if !anyContains(warnings, "user_typo") {
		t.Fatalf("expected a warning about key mapping to undefined user_id, got: %v", warnings)
	}
	if !anyContains(warnings, "UNLIMITED") {
		t.Fatalf("expected the warning to call out unlimited budget, got: %v", warnings)
	}
}

func TestWarnings_NoWarningWhenKeyMapsToDefinedUser(t *testing.T) {
	cfg := validConfig()
	cfg.Users["user_123"] = Budget{DailyLimitUSD: 5}
	cfg.Keys["sk-guard-abc123456789"] = "user_123"
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "user_123") {
			t.Fatalf("did not expect a warning about correctly-configured user_123, got: %v", w)
		}
	}
}

func TestWarnings_FlagsEmptyKeys(t *testing.T) {
	cfg := validConfig()
	warnings := cfg.Warnings()
	if !anyContains(warnings, "single-tenant") {
		t.Fatalf("expected a single-tenant-mode warning when keys is empty, got: %v", warnings)
	}
}

func TestWarnings_CleanConfigHasNoWarnings(t *testing.T) {
	cfg := validConfig()
	cfg.Users["user_123"] = Budget{DailyLimitUSD: 5}
	cfg.Keys["sk-guard-abc123456789"] = "user_123"
	if warnings := cfg.Warnings(); len(warnings) != 0 {
		t.Fatalf("expected no warnings for a fully-specified config, got: %v", warnings)
	}
}

func TestMaskKey(t *testing.T) {
	if got := maskKey("sk-guard-abc123456789xyz"); got != "sk-guard-abc..." {
		t.Fatalf("got %q", got)
	}
	if got := maskKey("short"); got != "short" {
		t.Fatalf("short keys should be returned as-is, got %q", got)
	}
}

func anyContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
