package cli

import (
	"fmt"

	"github.com/pterm/pterm"

	"github.com/Oluiy/ai-cost-guard/internal/auth"
	"github.com/Oluiy/ai-cost-guard/internal/config"
)

// RunResetDashboardPassword sets (or resets) the one dashboard admin
// account in configPath. It's the recovery path every comparable
// self-hosted tool provides for a forgotten password (there's no email
// system here to send a reset link to) — deliberately doesn't require
// knowing the current password or the server to be running, and doubles
// as first-time setup if the dashboard login was skipped during
// `ai-guard init`.
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

	// Rotated on every reset, not just the first time: this command exists
	// for disaster recovery, and a password change should invalidate any
	// existing session, not leave one quietly valid on some other device
	// (the same default GitHub/Google use — changing your password signs
	// out everywhere else, not just where you changed it).
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
