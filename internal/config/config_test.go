package config

import (
	"os"
	"path/filepath"
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

func TestValidate_RejectsDashboardUsersWithoutSessionSecret(t *testing.T) {
	cfg := validConfig()
	cfg.Dashboard.Users = []DashboardUser{{Username: "admin", PasswordHash: "$2a$10$..."}}
	cfg.Dashboard.SessionSecret = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for a dashboard login with no session_secret (would sign sessions with an empty HMAC key)")
	}
}

func TestValidate_AllowsDashboardUsersWithSessionSecret(t *testing.T) {
	cfg := validConfig()
	cfg.Dashboard.Users = []DashboardUser{{Username: "admin", PasswordHash: "$2a$10$..."}}
	cfg.Dashboard.SessionSecret = "some-generated-secret"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
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
	cfg.Dashboard.Users = []DashboardUser{{Username: "admin", PasswordHash: "$2a$10$..."}}
	if warnings := cfg.Warnings(); len(warnings) != 0 {
		t.Fatalf("expected no warnings for a fully-specified config, got: %v", warnings)
	}
}

func TestWarnings_FlagsEmptyDashboardUsers(t *testing.T) {
	cfg := validConfig()
	warnings := cfg.Warnings()
	if !anyContains(warnings, "no dashboard users configured") {
		t.Fatalf("expected a warning about the dashboard having no login, got: %v", warnings)
	}
}

func TestWarnings_NoWarningWhenDashboardUserConfigured(t *testing.T) {
	cfg := validConfig()
	cfg.Dashboard.Users = []DashboardUser{{Username: "admin", PasswordHash: "$2a$10$..."}}
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "dashboard") {
			t.Fatalf("did not expect a dashboard warning once a user is configured, got: %v", w)
		}
	}
}

func TestLoad_ExpandsBracedEnvVars(t *testing.T) {
	t.Setenv("FITGUARD_TEST_KEY", "sk-from-env")
	path := writeTempConfig(t, "providers:\n  openai:\n    api_key: ${FITGUARD_TEST_KEY}\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Providers["openai"].APIKey; got != "sk-from-env" {
		t.Fatalf("got api_key %q, want expanded env var value", got)
	}
}

// TestLoad_DirectoryAtConfigPathGivesActionableError is the fix for the
// #1 first-run Docker footgun: `docker compose up` before config.yaml
// exists on the host makes Docker silently bind-mount a directory there
// instead of failing, and the raw os.ReadFile error ("is a directory")
// gives no hint why — this confirms Load names the actual cause instead.
func TestLoad_DirectoryAtConfigPathGivesActionableError(t *testing.T) {
	dir := t.TempDir() + "/config.yaml"
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected an error when the config path is a directory")
	}
	if !strings.Contains(err.Error(), "is a directory") || !strings.Contains(err.Error(), "Docker") {
		t.Fatalf("got %q, want an error naming the directory-at-config-path cause", err.Error())
	}
}

func TestLoad_UsesFITGUARD_CONFIGWithoutReadingPath(t *testing.T) {
	t.Setenv("FITGUARD_CONFIG", "port: 9090\ndata_dir: /var/data\nproviders:\n  openai:\n    api_key: from-env-config\n")

	cfg, err := Load(t.TempDir() + "/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9090 || cfg.DataDir != "/var/data" {
		t.Fatalf("got port=%d data_dir=%q from environment config", cfg.Port, cfg.DataDir)
	}
	if got := cfg.Providers["openai"].APIKey; got != "from-env-config" {
		t.Fatalf("got api_key %q, want value from FITGUARD_CONFIG", got)
	}
}

// A bcrypt hash (dashboard.users[].password_hash) is a bare string like
// $2a$10$..., which happens to look like shell variable syntax. Load must
// not mangle it: only the ${VAR} braced form is env-var expansion, bare
// $VAR is passed through unchanged. This is a regression test for exactly
// that bug — a real bcrypt hash silently corrupted on every config load,
// locking every dashboard password reset out immediately.
func TestLoad_DoesNotMangleBareDollarSignsLikeBcryptHashes(t *testing.T) {
	const hash = "$2a$10$XxDD2tqME/V4A2/IoFKWNezWxOK8QuzEG5mf3y5.qoupJIA/vX2PG"
	path := writeTempConfig(t, "providers:\n  openai:\n    api_key: sk-test\n"+
		"dashboard:\n  session_secret: test-secret\n  users:\n    - username: admin\n      password_hash: \""+hash+"\"\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Dashboard.Users) != 1 || cfg.Dashboard.Users[0].PasswordHash != hash {
		t.Fatalf("got password_hash %q, want unchanged %q", cfg.Dashboard.Users[0].PasswordHash, hash)
	}
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
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

// config.yaml holds provider API keys, the session-signing secret, and
// password hashes, so it must never be group- or world-readable.
func TestSave_CreatesFileOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := Save(path, &Config{Port: 8787}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("new config is %o, want owner-only", perm)
	}
}

// The subtle case: os.WriteFile applies its mode only when it creates the
// file. Rewriting a config.yaml that already existed as 0644 (hand-made,
// restored from backup, COPYd into an image) used to leave it readable by
// every local account, including after `fitguard reset-dashboard-password`
// wrote a fresh secret into it.
func TestSave_TightensPermissionsOnPreExistingLooseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := os.WriteFile(path, []byte("port: 1\n"), 0o644); err != nil {
		t.Fatalf("seeding file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // defeat umask
		t.Fatalf("chmod: %v", err)
	}

	if err := Save(path, &Config{Port: 8787}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("config is still %o after Save; group/other can read provider keys", perm)
	}
}

// Rewriting an already-correct file must not loosen it, and must not
// churn the mode for no reason. (There's no writable mode stricter than
// 0600 to test against: 0400 isn't writable, so Save legitimately fails
// on it.)
func TestSave_LeavesAlreadyRestrictedFileUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := os.WriteFile(path, []byte("port: 1\n"), 0o600); err != nil {
		t.Fatalf("seeding file: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil { // defeat umask
		t.Fatalf("chmod: %v", err)
	}
	if err := Save(path, &Config{Port: 8787}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions changed from 0600 to %o", perm)
	}
}
