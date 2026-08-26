package cli

import (
	"fmt"

	"github.com/pterm/pterm"

	"github.com/Oluiy/ai-cost-guard/internal/auth"
	"github.com/Oluiy/ai-cost-guard/internal/config"
)

// RunResetDashboardPassword sets or resets the dashboard admin account in
// configPath. Doesn't require the current password or a running server;
// also works as first-time setup if init skipped the dashboard login.
func RunResetDashboardPassword(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("run 'ai-guard init' first, or check your config: %w", err)
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
		SessionSecret: sessionSecret,
		Users:         []config.DashboardUser{{Username: username, PasswordHash: passwordHash}},
	}

	if err := config.Save(configPath, cfg); err != nil {
		return err
	}

	pterm.Success.Printfln("Dashboard password set for %q. Any previous password no longer works.", username)
	return nil
}
