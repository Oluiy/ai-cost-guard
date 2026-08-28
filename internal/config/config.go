// Package config loads and validates fitguard's YAML configuration.
package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration for fitguard.
type Config struct {
	Port      int                 `yaml:"port"`
	DataDir   string              `yaml:"data_dir"`
	Providers map[string]Provider `yaml:"providers"`
	Cache     CacheConfig         `yaml:"cache"`
	// BudgetBackend selects where budget state is tracked: "local"
	// (default, single instance only) or "redis" (shared across instances).
	BudgetBackend BudgetBackendConfig `yaml:"budget"`
	Users         map[string]Budget   `yaml:"users"`
	// Keys maps a virtual API key to the user_id its budget is tracked
	// under. Empty means single-tenant mode: every request is "default"
	// with no auth.
	Keys     map[string]string `yaml:"keys"`
	Fallback []string          `yaml:"fallback"`
	Default  RouteConfig       `yaml:"default"`
	// Dashboard gates /dashboard behind a login, separate from Keys.
	Dashboard DashboardConfig `yaml:"dashboard"`
	// TrustedProxies lists reverse proxies allowed to set
	// X-Forwarded-For/-Proto. Empty (default) uses the real peer address;
	// set it when running behind nginx/Caddy/a load balancer.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// Pricing corrects or extends internal/cost's built-in price table,
	// keyed by model name. The built-in table is a manually maintained
	// snapshot of each provider's published pricing and drifts whenever a
	// provider changes prices; this is the fix for that drift — an
	// operator can correct a stale price (or add a model the table
	// doesn't know about yet) here, without waiting on a fitguard release.
	Pricing map[string]PricingOverride `yaml:"pricing"`
}

// PricingOverride replaces (for a model already in the built-in table) or
// defines (for one that isn't) a single model's pricing.
type PricingOverride struct {
	InputPer1K  float64 `yaml:"input_per_1k"`
	OutputPer1K float64 `yaml:"output_per_1k"`
	// Provider and Embedding are only required when adding a model the
	// built-in table doesn't already have; correcting an existing
	// entry's price only needs the two fields above.
	Provider  string `yaml:"provider,omitempty"`
	Embedding bool   `yaml:"embedding,omitempty"`
}

// Provider holds credentials/config for an upstream LLM provider.
type Provider struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
}

// CacheConfig controls response caching.
type CacheConfig struct {
	Enabled  bool   `yaml:"enabled"`
	TTL      int    `yaml:"ttl_seconds"`
	Backend  string `yaml:"backend"` // "memory" or "redis"
	RedisURL string `yaml:"redis_url"`
}

// Budget defines a per-user spending limit.
type Budget struct {
	DailyLimitUSD float64 `yaml:"daily_limit_usd"`
}

// BudgetBackendConfig selects where budget-enforcement state lives.
type BudgetBackendConfig struct {
	Backend  string `yaml:"backend"` // "local" or "redis"
	RedisURL string `yaml:"redis_url"`
	// FailClosed rejects a key whose user_id has no `users:` entry
	// instead of treating it as unlimited. Off by default. An explicit
	// daily_limit_usd: 0 still means unlimited either way.
	FailClosed bool `yaml:"fail_closed"`
}

// RouteConfig sets default routing behavior.
type RouteConfig struct {
	Model string `yaml:"model"`
}

// DashboardConfig protects the /dashboard UI with a login. Users is a
// list so more than one account can be added later without a config
// migration; `fitguard init` only creates one today.
type DashboardConfig struct {
	// SessionSecret signs session cookies. Generated once by `fitguard
	// init` so sessions survive restarts.
	SessionSecret string          `yaml:"session_secret"`
	Users         []DashboardUser `yaml:"users"`
	// SessionTTLHours is how long a login stays valid. Defaults to 168
	// (7 days). Sessions can't be revoked individually; rotating
	// SessionSecret via `fitguard reset-dashboard-password` invalidates
	// all of them at once.
	SessionTTLHours int `yaml:"session_ttl_hours"`
}

// DashboardUser is one dashboard login. Only the bcrypt hash is stored,
// never the password itself.
type DashboardUser struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

const DefaultPort = 8787

var bracedEnvVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandBracedEnvVars replaces ${VAR} with os.Getenv("VAR"). Unlike
// os.ExpandEnv, bare $VAR is left untouched so bcrypt hashes
// ($2a$10$...) survive unmangled.
func expandBracedEnvVars(s string) string {
	return bracedEnvVarPattern.ReplaceAllStringFunc(s, func(m string) string {
		name := bracedEnvVarPattern.FindStringSubmatch(m)[1]
		return os.Getenv(name)
	})
}

// Load reads and parses a YAML config. If FITGUARD_CONFIG is set, its
// value is used as the config content directly instead of reading path —
// this is what lets platforms that build from git with no local disk to
// mount a file from (Render, Railway, Heroku) run fitguard: the whole
// config.yaml goes into one environment variable instead.
func Load(path string) (*Config, error) {
	var data []byte
	if env := os.Getenv("FITGUARD_CONFIG"); env != "" {
		data = []byte(env)
	} else {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config %s: %w", path, err)
		}
	}

	data = []byte(expandBracedEnvVars(string(data)))

	cfg := &Config{
		Port:    DefaultPort,
		DataDir: ".",
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate rejects configs that are unsafe or broken to start with. See
// Warnings for problems that don't block startup.
func (c *Config) Validate() error {
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", c.Port)
	}
	if c.DataDir == "" {
		c.DataDir = "."
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("config must define at least one provider under `providers:`")
	}
	if c.Cache.TTL <= 0 {
		c.Cache.TTL = 300
	}
	if c.Cache.Backend == "" {
		c.Cache.Backend = "memory"
	}
	if c.Cache.Backend != "memory" && c.Cache.Backend != "redis" {
		return fmt.Errorf(`cache.backend must be "memory" or "redis", got %q`, c.Cache.Backend)
	}
	if c.Cache.Enabled && c.Cache.Backend == "redis" && c.Cache.RedisURL == "" {
		return fmt.Errorf("cache.backend is \"redis\" but cache.redis_url is not set")
	}

	if c.BudgetBackend.Backend == "" {
		c.BudgetBackend.Backend = "local"
	}
	if c.BudgetBackend.Backend != "local" && c.BudgetBackend.Backend != "redis" {
		return fmt.Errorf(`budget.backend must be "local" or "redis", got %q`, c.BudgetBackend.Backend)
	}
	if c.BudgetBackend.Backend == "redis" && c.BudgetBackend.RedisURL == "" {
		// Reuse the cache's Redis URL if one is set, rather than requiring it twice.
		if c.Cache.Backend == "redis" && c.Cache.RedisURL != "" {
			c.BudgetBackend.RedisURL = c.Cache.RedisURL
		} else {
			return fmt.Errorf("budget.backend is \"redis\" but budget.redis_url is not set")
		}
	}

	if err := ValidateFallback(c.Fallback, c.Providers); err != nil {
		return err
	}

	// A dashboard user with no session_secret would sign cookies with an
	// empty key. init/reset-dashboard-password always write both
	// together, so this only fires on a hand-edited config.
	if len(c.Dashboard.Users) > 0 && c.Dashboard.SessionSecret == "" {
		return fmt.Errorf("dashboard.users is set but dashboard.session_secret is empty — " +
			"run `fitguard reset-dashboard-password` instead of hand-editing dashboard.users, " +
			"it generates both together")
	}
	return nil
}

// Warnings returns non-fatal problems worth surfacing at startup, such as
// a budget that's silently unlimited due to a typo.
func (c *Config) Warnings() []string {
	var warnings []string

	for name, p := range c.Providers {
		if p.APIKey == "" {
			warnings = append(warnings, fmt.Sprintf(
				"provider %q has no api_key set — requests routed to it will fail upstream", name))
		}
	}

	for token, userID := range c.Keys {
		if _, ok := c.Users[userID]; !ok {
			warnings = append(warnings, fmt.Sprintf(
				"key %s maps to user_id %q, which has no entry under `users:` — that key has an UNLIMITED daily budget",
				maskKey(token), userID))
		}
	}

	if len(c.Keys) == 0 {
		warnings = append(warnings, "no keys configured — running in single-tenant mode: "+
			"every request is unauthenticated and shares one \"default\" budget")
	}

	if len(c.Dashboard.Users) == 0 {
		warnings = append(warnings, "no dashboard users configured — the /dashboard UI has no login "+
			"and is visible to anyone who can reach it; run `fitguard reset-dashboard-password` to set one up")
	}

	return warnings
}

// maskKey shortens a virtual API key for display in warnings, so it stays
// identifiable without leaking the full secret to logs.
func maskKey(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12] + "..."
}

// Save writes the config to path as YAML, restricting file permissions to
// owner-only regardless of how the file existed before.
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing config %s: %w", path, err)
	}
	// os.WriteFile only applies its mode on creation; an existing file
	// keeps its prior permissions. Tighten explicitly so a config that
	// arrived as 0644 doesn't stay world-readable.
	if err := restrictPermissions(path); err != nil {
		return err
	}
	return nil
}

// restrictPermissions clears group/other access on path. No-op if
// already stricter.
func restrictPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("checking permissions on %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		if err := os.Chmod(path, perm&^0o077); err != nil {
			return fmt.Errorf("restricting permissions on %s: %w", path, err)
		}
	}
	return nil
}
