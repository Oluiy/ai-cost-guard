package config

import (
	"context"
	"fmt"
	"strings"
)

// StorageConfig says where FitGuard keeps its data.
//
// Two separate things live in "storage", and they're easy to conflate:
//
//   - Backend: where the request log (what the dashboard charts) is kept.
//     "sqlite" is a file next to the binary; "postgres" is a database that
//     survives redeploys on hosts with no persistent disk and is shared by
//     every instance.
//   - State: where the settings that change at runtime live (dashboard
//     logins, virtual keys, budgets, the fallback list, cache settings).
//     Those used to live only in config.yaml, which a PaaS can't write:
//     a password reset or a budget change would silently vanish on the next
//     deploy. "database" keeps them in the same database as the request
//     log, so they survive and are shared; config.yaml then only seeds them
//     on first boot.
type StorageConfig struct {
	Backend     string `yaml:"backend,omitempty"`      // "sqlite" (default) or "postgres"
	PostgresURL string `yaml:"postgres_url,omitempty"` // postgres://user:pass@host/db; reference it as ${DATABASE_URL}
	State       string `yaml:"state,omitempty"`        // "config" (default with sqlite) or "database" (always with postgres)
}

const (
	BackendSQLite   = "sqlite"
	BackendPostgres = "postgres"
	StateConfig     = "config"
	StateDatabase   = "database"
)

// IsPostgres reports whether the request log lives in Postgres.
func (s StorageConfig) IsPostgres() bool { return s.Backend == BackendPostgres }

// StateInDatabase reports whether runtime-mutable settings live in the
// database rather than config.yaml. Postgres implies it: a config file
// can't be shared between instances or written on a disk-less host, which
// is the whole reason to be on Postgres.
func (s StorageConfig) StateInDatabase() bool {
	return s.IsPostgres() || s.State == StateDatabase
}

func (s StorageConfig) validate() error {
	switch s.Backend {
	case "", BackendSQLite, BackendPostgres:
	default:
		return fmt.Errorf("storage.backend must be %q or %q, got %q", BackendSQLite, BackendPostgres, s.Backend)
	}
	switch s.State {
	case "", StateConfig, StateDatabase:
	default:
		return fmt.Errorf("storage.state must be %q or %q, got %q", StateConfig, StateDatabase, s.State)
	}
	if s.IsPostgres() {
		if strings.TrimSpace(s.PostgresURL) == "" {
			return fmt.Errorf("storage.backend is postgres but storage.postgres_url is empty; set it to ${DATABASE_URL} and provide that environment variable")
		}
		if s.State == StateConfig {
			return fmt.Errorf("storage.state: config can't be used with the postgres backend: a config file can't be shared between instances or written on a host with no disk. Remove the line (postgres keeps this state in the database)")
		}
	}
	return nil
}

// State is every setting that can change while FitGuard runs, as one
// value. It's what gets stored in the database, and what config.yaml seeds
// it from on first boot.
type State struct {
	Dashboard       DashboardConfig   `json:"dashboard"`
	Users           map[string]Budget `json:"users"`
	Keys            map[string]string `json:"keys"`
	Fallback        []string          `json:"fallback"`
	CacheEnabled    bool              `json:"cache_enabled"`
	CacheTTLSeconds int               `json:"cache_ttl_seconds"`
}

// StateStore persists State somewhere that outlives a redeploy.
// LoadState reports ok=false when nothing has been stored yet.
type StateStore interface {
	LoadState(ctx context.Context) (st State, ok bool, err error)
	SaveState(ctx context.Context, st State) error
}

// State returns a deep copy of c's runtime-mutable settings.
func (c *Config) State() State {
	st := State{
		Dashboard:       c.Dashboard,
		Users:           make(map[string]Budget, len(c.Users)),
		Keys:            make(map[string]string, len(c.Keys)),
		Fallback:        append([]string(nil), c.Fallback...),
		CacheEnabled:    c.Cache.Enabled,
		CacheTTLSeconds: c.Cache.TTL,
	}
	st.Dashboard.Users = append([]DashboardUser(nil), c.Dashboard.Users...)
	for k, v := range c.Users {
		st.Users[k] = v
	}
	for k, v := range c.Keys {
		st.Keys[k] = v
	}
	return st
}

// applyState overwrites c's runtime-mutable settings with st.
func (c *Config) applyState(st State) {
	c.Dashboard = st.Dashboard
	c.Users = st.Users
	c.Keys = st.Keys
	c.Fallback = st.Fallback
	c.Cache.Enabled = st.CacheEnabled
	c.Cache.TTL = st.CacheTTLSeconds
	if c.Users == nil {
		c.Users = map[string]Budget{}
	}
	if c.Keys == nil {
		c.Keys = map[string]string{}
	}
}
