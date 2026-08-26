package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

// generateKey returns a random gateway-issued virtual API key.
func generateKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating key: %w", err)
	}
	return "sk-guard-" + hex.EncodeToString(b), nil
}

// Settings holds the subset of Config the dashboard can change while
// ai-guard is running: cache on/off, cache TTL, fallback list, and
// per-user budgets. Everything else (provider credentials, virtual keys,
// session secret, port, data_dir) stays on Config and is terminal-only.
//
// Every field here is read by request goroutines and written by the
// dashboard handler. Use the accessor methods only — reading
// cfg.Cache.TTL directly while Apply runs is a data race.
type Settings struct {
	mu   sync.RWMutex
	path string
	cfg  *Config
}

// NewSettings wraps cfg for live updates, persisting to path on change.
// An empty path applies changes in memory only.
func NewSettings(path string, cfg *Config) *Settings {
	return &Settings{path: path, cfg: cfg}
}

// CacheEnabled reports whether response caching is currently on.
func (s *Settings) CacheEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Cache.Enabled
}

// CacheTTLSeconds is how long a cached response stays valid.
func (s *Settings) CacheTTLSeconds() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Cache.TTL
}

// Fallback returns a copy of the fallback model list.
func (s *Settings) Fallback() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.cfg.Fallback))
	copy(out, s.cfg.Fallback)
	return out
}

// Budget returns the daily limit for userID and whether one is
// configured at all; "no entry" and "entry with a zero limit" are
// different states to the budget enforcer.
func (s *Settings) Budget(userID string) (Budget, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.cfg.Users[userID]
	return b, ok
}

// Editable is the wire shape of everything the dashboard may read or
// write. Kept separate from Config so the API surface can't accidentally
// grow to include credentials.
type Editable struct {
	CacheEnabled    bool               `json:"cache_enabled"`
	CacheTTLSeconds int                `json:"cache_ttl_seconds"`
	Fallback        []string           `json:"fallback"`
	Users           map[string]float64 `json:"users"` // user_id -> daily_limit_usd
}

// Snapshot returns the current editable settings, deep-copied.
func (s *Settings) Snapshot() Editable {
	s.mu.RLock()
	defer s.mu.RUnlock()

	users := make(map[string]float64, len(s.cfg.Users))
	for id, b := range s.cfg.Users {
		users[id] = b.DailyLimitUSD
	}
	fallback := make([]string, len(s.cfg.Fallback))
	copy(fallback, s.cfg.Fallback)

	return Editable{
		CacheEnabled:    s.cfg.Cache.Enabled,
		CacheTTLSeconds: s.cfg.Cache.TTL,
		Fallback:        fallback,
		Users:           users,
	}
}

// maxCacheTTLSeconds caps a cache entry at 30 days, to guard against a
// mistyped value pinning entries in memory indefinitely.
const maxCacheTTLSeconds = 60 * 60 * 24 * 30

// ValidateEditable checks e without applying it.
func ValidateEditable(e Editable) error {
	if e.CacheTTLSeconds < 1 || e.CacheTTLSeconds > maxCacheTTLSeconds {
		return fmt.Errorf("cache_ttl_seconds must be between 1 and %d (30 days), got %d",
			maxCacheTTLSeconds, e.CacheTTLSeconds)
	}
	for id, limit := range e.Users {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("user id cannot be empty or whitespace")
		}
		// limit < 0 alone doesn't catch NaN/Inf, which compare false
		// against every bound and would silently disable enforcement.
		if math.IsNaN(limit) || math.IsInf(limit, 0) {
			return fmt.Errorf("daily limit for %q must be a real number", id)
		}
		if limit < 0 {
			return fmt.Errorf("daily limit for %q cannot be negative, got %v", id, limit)
		}
	}
	for i, model := range e.Fallback {
		if strings.TrimSpace(model) == "" {
			return fmt.Errorf("fallback[%d] cannot be empty or whitespace", i)
		}
	}
	return nil
}

// checkKeysStillBudgeted rejects an update that would drop the budget for
// a user_id a virtual key still maps to, since an absent budget entry
// means unlimited spend. Set the limit to 0 instead to make a key
// unmetered explicitly.
func (s *Settings) checkKeysStillBudgeted(next map[string]float64) error {
	orphaned := map[string]bool{}
	for _, userID := range s.cfg.Keys {
		if _, ok := next[userID]; !ok {
			orphaned[userID] = true
		}
	}
	if len(orphaned) == 0 {
		return nil
	}

	names := make([]string, 0, len(orphaned))
	for id := range orphaned {
		names = append(names, id)
	}
	sort.Strings(names)

	return fmt.Errorf("cannot remove the budget for %s: a virtual key still maps to %s, "+
		"and removing the limit would let that key spend without a cap. "+
		"Set the limit to 0 if you intend it to be unmetered, or remove the key from `keys:` first",
		strings.Join(names, ", "), pluralSubject(len(names)))
}

func pluralSubject(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// Apply validates e, swaps it into live state, and persists it. A
// rejected update leaves the running config untouched; a failed write
// rolls back the in-memory change too, so the process and the file never
// disagree.
// Apply returns the virtual keys newly issued for any user_id in e.Users
// that had no existing key, keyed by user_id, so the caller can show them
// once. A user added only through Apply otherwise has a budget but no
// way to authenticate as it.
func (s *Settings) Apply(e Editable) (issuedKeys map[string]string, err error) {
	if err := ValidateEditable(e); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkKeysStillBudgeted(e.Users); err != nil {
		return nil, err
	}
	// Needs the lock: reads s.cfg.Providers, which ValidateEditable
	// (deliberately pure) can't see.
	if err := ValidateFallback(e.Fallback, s.cfg.Providers); err != nil {
		return nil, err
	}

	prevCache := s.cfg.Cache
	prevUsers := s.cfg.Users
	prevFallback := s.cfg.Fallback
	prevKeys := s.cfg.Keys

	hasKey := make(map[string]bool, len(s.cfg.Keys))
	for _, userID := range s.cfg.Keys {
		hasKey[userID] = true
	}

	users := make(map[string]Budget, len(e.Users))
	keys := make(map[string]string, len(s.cfg.Keys))
	for k, v := range s.cfg.Keys {
		keys[k] = v
	}
	issued := map[string]string{}
	for id, limit := range e.Users {
		users[id] = Budget{DailyLimitUSD: limit}
		if !hasKey[id] {
			key, kerr := generateKey()
			if kerr != nil {
				return nil, fmt.Errorf("issuing key for %q: %w", id, kerr)
			}
			keys[key] = id
			issued[id] = key
		}
	}
	fallback := make([]string, len(e.Fallback))
	copy(fallback, e.Fallback)

	s.cfg.Cache.Enabled = e.CacheEnabled
	s.cfg.Cache.TTL = e.CacheTTLSeconds
	s.cfg.Users = users
	s.cfg.Fallback = fallback
	s.cfg.Keys = keys

	if s.path != "" {
		if err := Save(s.path, s.cfg); err != nil {
			s.cfg.Cache = prevCache
			s.cfg.Users = prevUsers
			s.cfg.Fallback = prevFallback
			s.cfg.Keys = prevKeys
			return nil, fmt.Errorf("persisting settings: %w", err)
		}
	}
	return issued, nil
}
