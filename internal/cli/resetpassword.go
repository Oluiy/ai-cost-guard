package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pterm/pterm"

	"github.com/Oluiy/ai-cost-guard/internal/auth"
	"github.com/Oluiy/ai-cost-guard/internal/config"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

// RunResetDashboardPassword sets or resets the dashboard admin account in
// configPath, or in the database when settings live there (the postgres
// backend, or storage.state: database), where a config file can't be written
// back. Doesn't require the current password or a running server;
// also works as first-time setup if init skipped the dashboard login.
func RunResetDashboardPassword(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("run 'fitguard init' first, or check your config: %w", err)
	}

	defaultUsername := "admin"
	if len(cfg.Dashboard.Users) > 0 {
		defaultUsername = cfg.Dashboard.Users[0].Username
	}

	username, err := pterm.DefaultInteractiveTextInput.
		WithDefaultValue(defaultUsername).
		Show("Dashboard username")
	if err != nil {
		return err
	}

	password, err := promptNewPassword(fmt.Sprintf("New password for %q", username))
	if err != nil {
		return err
	}

	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	// Rotated on every reset so a password change also signs out any
	// existing session.
	sessionSecret, err := auth.GenerateSecret()
	if err != nil {
		return err
	}

	cfg.Dashboard = config.DashboardConfig{
		SessionSecret:   sessionSecret,
		SessionTTLHours: cfg.Dashboard.SessionTTLHours,
		Users:           []config.DashboardUser{{Username: username, PasswordHash: passwordHash}},
	}

	if cfg.Storage.StateInDatabase() {
		if err := saveDashboardToDatabase(configPath, cfg); err != nil {
			return err
		}
	} else if err := config.Save(configPath, cfg); err != nil {
		return err
	}

	pterm.Success.Printfln("Dashboard password set for %q. Any previous password no longer works.", username)
	return nil
}

// saveDashboardToDatabase writes the new login into the stored settings. A
// running server picks it up on its next reload (within 30 seconds).
func saveDashboardToDatabase(configPath string, cfg *config.Config) error {
	var store *logging.Store
	var err error
	if cfg.Storage.IsPostgres() {
		store, err = logging.OpenPostgres(cfg.Storage.PostgresURL)
		if err != nil {
			return fmt.Errorf("connecting to postgres at %s: %w", maskDBURL(cfg.Storage.PostgresURL), err)
		}
	} else {
		if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
			return fmt.Errorf("creating data_dir %q: %w", cfg.DataDir, err)
		}
		store, err = logging.Open(filepath.Join(cfg.DataDir, "fitguard.db"))
		if err != nil {
			return err
		}
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Attach first so the rest of the stored settings (budgets, keys) are
	// loaded rather than overwritten with the config file's copy. The
	// login was already put on cfg above, so re-apply it after the attach.
	want := cfg.Dashboard
	settings := config.NewSettings(configPath, cfg)
	if _, err := settings.AttachStore(ctx, store); err != nil {
		return err
	}
	return settings.ReplaceDashboard(want)
}
