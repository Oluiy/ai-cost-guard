// Package config loads and validates ai-guard's YAML configuration.
package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration for ai-guard.
type Config struct {
	Port      int                 `yaml:"port"`
	DataDir   string              `yaml:"data_dir"`
	Providers map[string]Provider `yaml:"providers"`
	Cache     CacheConfig         `yaml:"cache"`
	// BudgetBackend selects how daily-budget enforcement is tracked.
	// "local" (default) is correct for a single ai-guard instance only.
	// "redis" shares enforcement state across multiple instances behind
	// a load balancer — required for horizontally-scaled deployments,
	// since "local" tracking is per-process and a user's real budget can
	// otherwise be exceeded by roughly (instance count)×.
	BudgetBackend BudgetBackendConfig `yaml:"budget"`
	Users         map[string]Budget   `yaml:"users"`
	// Keys maps a gateway-issued virtual API key (what callers put in
	// their Authorization header) to the user_id their budget is tracked
	// under. Real provider credentials in Providers are never exposed to
	// callers. If empty, ai-guard runs in single-tenant mode: every
	// request is attributed to the "default" user with no authentication
	// (fine for local/solo use, not for anything multi-caller).
	Keys     map[string]string `yaml:"keys"`
	Fallback []string          `yaml:"fallback"`
	Default  RouteConfig       `yaml:"default"`
	// Dashboard gates the human-facing /dashboard UI behind a login,
	// entirely separate from Keys above (which authenticates API callers,
	// not people looking at the spend dashboard in a browser).
	Dashboard DashboardConfig `yaml:"dashboard"`
}

// Provider holds credentials/config for an upstream LLM provider.
type Provider struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
}

// CacheConfig controls semantic response caching.
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

// BudgetBackendConfig selects and configures where budget-enforcement
// state (in-flight reservations and, for redis, settled spend) lives.
type BudgetBackendConfig struct {
	Backend  string `yaml:"backend"` // "local" or "redis"
	RedisURL string `yaml:"redis_url"`
}

// RouteConfig sets default routing behavior.
type RouteConfig struct {
	Model string `yaml:"model"`
}

// DashboardConfig protects the /dashboard UI with a login. Users is a
// list, not a single username/password pair, so a later move to more
// than one account (e.g. a read-only account for someone who should see
// spend but not change anything) is additive — no config migration needed
// when that gets built. `ai-guard init` only ever creates one entry today.
type DashboardConfig struct {
	// SessionSecret signs session cookies (see internal/auth). Generated
	// once by `ai-guard init` and persisted here so sessions survive
	// `ai-guard run` restarts instead of logging everyone out each time.
	SessionSecret string          `yaml:"session_secret"`
	Users         []DashboardUser `yaml:"users"`
}

// DashboardUser is one dashboard login. Password is never stored — only
// its bcrypt hash (see internal/auth.HashPassword).
type DashboardUser struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

const DefaultPort = 8787

var bracedEnvVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandBracedEnvVars replaces ${VAR} with os.Getenv("VAR") (empty string
// if unset). Unlike os.ExpandEnv, it leaves bare $VAR untouched, so values
// that legitimately contain a literal '$' followed by non-identifier-safe
// text (bcrypt hashes: $2a$10$...) pass through unchanged.
func expandBracedEnvVars(s string) string {
	return bracedEnvVarPattern.ReplaceAllStringFunc(s, func(m string) string {
		name := bracedEnvVarPattern.FindStringSubmatch(m)[1]
		return os.Getenv(name)
	})
}

// Load reads and parses a YAML config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	// Expand ${ENV_VAR} references (e.g. api_key: ${OPENAI_API_KEY}) so
	// secrets don't need to live in the config file itself. Deliberately
	// only the braced form, via our own regexp, not os.ExpandEnv's bare
	// $VAR too: bcrypt hashes (dashboard.users[].password_hash) are bare
	// strings like $2a$10$..., which os.ExpandEnv would silently mangle
	// by treating "2a", "10", etc. as (undefined, so empty) variable
	// names, corrupting every stored password hash on load.
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

// Validate checks that the config is safe to start with, returning an
// error for problems that make that impossible or clearly wrong. It does
// not catch everything — see Warnings for non-fatal footguns that are
// still worth surfacing.
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
		// Convenience: if the cache is already pointed at a Redis, reuse
		// its URL rather than making the operator repeat it.
		if c.Cache.Backend == "redis" && c.Cache.RedisURL != "" {
			c.BudgetBackend.RedisURL = c.Cache.RedisURL
		} else {
			return fmt.Errorf("budget.backend is \"redis\" but budget.redis_url is not set")
		}
	}

	// A dashboard login with no session_secret would still "work" —
	// sessions get signed with an empty HMAC key instead of failing to
	// start — which is exactly the kind of silently-weak state that's
	// worse than an error. `ai-guard init`/`reset-dashboard-password`
	// always generate one together with the account, so this only fires
	// if config.yaml was hand-edited to add a dashboard user directly.
	if len(c.Dashboard.Users) > 0 && c.Dashboard.SessionSecret == "" {
		return fmt.Errorf("dashboard.users is set but dashboard.session_secret is empty — " +
			"run `ai-guard reset-dashboard-password` instead of hand-editing dashboard.users, " +
			"it generates both together")
	}
	return nil
}

// Warnings returns non-fatal configuration problems worth surfacing at
// startup: things that won't stop ai-guard from running, but silently
// undermine what it's supposed to do — e.g. a budget that's effectively
// unlimited because of a typo in a user_id, which is invisible unless
// someone goes looking for it.
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
			"and is visible to anyone who can reach it; run `ai-guard reset-dashboard-password` to set one up")
	}

	return warnings
}

// maskKey shortens a virtual API key for display in warnings, so it's
// identifiable without echoing the whole secret to logs/terminal scrollback.
func maskKey(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12] + "..."
}

// Save writes the config to path as YAML.
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing config %s: %w", path, err)
	}
	return nil
}
