// Package cli implements the fitguard command-line experience.
package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/pterm/pterm"

	"github.com/Oluiy/ai-cost-guard/internal/auth"
	"github.com/Oluiy/ai-cost-guard/internal/config"
	"github.com/Oluiy/ai-cost-guard/internal/cost"
)

// generateKey returns a random gateway-issued virtual API key.
func generateKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating key: %w", err)
	}
	return "sk-guard-" + hex.EncodeToString(b), nil
}

var providerDefaults = map[string]string{
	"openai":    "https://api.openai.com/v1",
	"anthropic": "https://api.anthropic.com/v1",
	"gemini":    "https://generativelanguage.googleapis.com/v1beta",
	"groq":      "https://api.groq.com/openai/v1",
	"together":  "https://api.together.xyz/v1",
}

// supportedProviders is the list offered by the setup wizard and
// `add-provider`, in the order shown.
var supportedProviders = []string{"openai", "anthropic", "gemini", "groq", "together"}

// promptForProviders asks which providers to add and collects an API key
// for each, writing them into cfg. already-configured providers are
// offered but re-entering one overwrites its key, which is how you rotate
// a key without hand-editing the file.
func promptForProviders(cfg *config.Config, options []string) error {
	if len(options) == 0 {
		return fmt.Errorf("every supported provider is already configured")
	}

	selected, err := pterm.DefaultInteractiveMultiselect.
		WithOptions(options).
		WithDefaultText("Which providers do you want to route through fitguard?").
		Show()
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("select at least one provider")
	}

	if cfg.Providers == nil {
		cfg.Providers = map[string]config.Provider{}
	}
	for _, name := range selected {
		key, err := pterm.DefaultInteractiveTextInput.
			WithMask("*").
			Show(fmt.Sprintf("API key for %s", name))
		if err != nil {
			return err
		}
		cfg.Providers[name] = config.Provider{APIKey: key, BaseURL: providerDefaults[name]}
	}
	return nil
}

// RunAddProvider adds one or more providers to an existing config,
// leaving everything else (keys, budgets, dashboard login) untouched.
func RunAddProvider(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("run 'fitguard init' first, or check your config: %w", err)
	}

	var available []string
	var existing []string
	for _, name := range supportedProviders {
		if _, ok := cfg.Providers[name]; ok {
			existing = append(existing, name)
			continue
		}
		available = append(available, name)
	}

	if len(existing) > 0 {
		pterm.Info.Printfln("Already configured: %s", strings.Join(existing, ", "))
	}
	if len(available) == 0 {
		pterm.Info.Println("Every supported provider is already configured. " +
			"To rotate a key, edit `providers:` in your config directly.")
		return nil
	}

	if err := promptForProviders(cfg, available); err != nil {
		return err
	}

	if err := config.Save(configPath, cfg); err != nil {
		return err
	}

	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	pterm.Success.Printfln("Wrote %s", configPath)
	pterm.Info.Printfln("Providers now configured: %s", strings.Join(names, ", "))
	pterm.Info.Printfln("Models you can now use as fallback: %s",
		strings.Join(cost.ChatModelsFor(names), ", "))
	pterm.Info.Println("Restart `fitguard run` for the new provider to take effect.")
	return nil
}

// RunInit walks the user through an interactive setup and writes configPath.
func RunInit(configPath string) error {
	pterm.DefaultBigText.WithLetters(pterm.NewLettersFromStringWithStyle("FitGuard", pterm.NewStyle(pterm.FgCyan))).Render()
	pterm.Info.Println("Let's set up your cost-protected AI gateway.")
	pterm.Println()

	if _, err := os.Stat(configPath); err == nil {
		overwrite, err := pterm.DefaultInteractiveConfirm.
			WithDefaultValue(false).
			Show(fmt.Sprintf("%s already exists and will be overwritten, including any virtual API keys "+
				"in it, note that those aren't stored anywhere else, so copy them out first if you still need them. Continue?", configPath))
		if err != nil {
			return err
		}
		if !overwrite {
			pterm.Info.Println("Cancelled — existing config left untouched.")
			return nil
		}
	}

	cfg := &config.Config{
		Port:      config.DefaultPort,
		DataDir:   ".",
		Providers: map[string]config.Provider{},
		Users:     map[string]config.Budget{},
		Keys:      map[string]string{},
	}

	if err := promptForProviders(cfg, supportedProviders); err != nil {
		return err
	}

	port, err := promptValidatedInt("Port to run fitguard on", config.DefaultPort, 1, 65535)
	if err != nil {
		return err
	}
	cfg.Port = port

	enableCache, err := pterm.DefaultInteractiveConfirm.
		WithDefaultValue(true).
		Show("Enable response caching (saves ~40% on repeated prompts)?")
	if err != nil {
		return err
	}
	cfg.Cache.Enabled = enableCache
	cfg.Cache.Backend = "memory"
	cfg.Cache.TTL = 300

	if enableCache {
		// Explained via Info rather than in the question itself — every
		// other prompt here fits one line, and a scheduled job needs a
		// TTL that covers its actual interval.
		pterm.Info.Println("Cached responses expire after a set time. The default 300s (5 min) suits " +
			"interactive apps; a scheduled job that repeats the same prompt hours apart needs a TTL " +
			"at least as long as its interval (6 hours = 21600) or it will never hit the cache.")
		ttl, err := promptValidatedInt("Cache TTL in seconds", 300, 1, 60*60*24*30)
		if err != nil {
			return err
		}
		cfg.Cache.TTL = ttl

		useRedis, err := pterm.DefaultInteractiveConfirm.
			WithDefaultValue(false).
			Show("Use Redis for the cache (recommended for multi-instance deployments)?")
		if err != nil {
			return err
		}
		if useRedis {
			redisURL, err := pterm.DefaultInteractiveTextInput.
				WithDefaultValue("redis://localhost:6379/0").
				Show("Redis URL")
			if err != nil {
				return err
			}
			cfg.Cache.Backend = "redis"
			cfg.Cache.RedisURL = redisURL
		}
	}

	pterm.Println()
	pterm.Info.Println("Now let's add budgeted users. Each one gets their own daily spend limit " +
		"and a virtual API key — callers authenticate to fitguard with that key (never your real " +
		"provider keys), so budgets can't be evaded just by sending a different name in a header.")

	type issuedKey struct {
		userID   string
		key      string
		dailyUSD float64
	}
	var issued []issuedKey

	for {
		addUser, err := pterm.DefaultInteractiveConfirm.
			WithDefaultValue(len(issued) == 0).
			Show("Add a budgeted user?")
		if err != nil {
			return err
		}
		if !addUser {
			break
		}

		userID, err := pterm.DefaultInteractiveTextInput.
			WithDefaultValue(fmt.Sprintf("user_%d", len(issued)+1)).
			Show("User id (for your own reference, e.g. a customer or service name)")
		if err != nil {
			return err
		}

		limit, err := promptNonNegativeUSD(fmt.Sprintf("Daily budget in USD for %q (0 = unlimited)", userID), 5)
		if err != nil {
			return err
		}
		if limit == 0 {
			confirmUnlimited, err := pterm.DefaultInteractiveConfirm.
				WithDefaultValue(false).
				Show(fmt.Sprintf("You entered 0 — %q will have an UNLIMITED daily budget. Is that what you want?", userID))
			if err != nil {
				return err
			}
			if !confirmUnlimited {
				pterm.Info.Println("Skipped — try again with a non-zero amount.")
				continue
			}
		}

		key, err := generateKey()
		if err != nil {
			return err
		}

		cfg.Users[userID] = config.Budget{DailyLimitUSD: limit}
		cfg.Keys[key] = userID
		issued = append(issued, issuedKey{userID: userID, key: key, dailyUSD: limit})
	}

	if len(issued) == 0 {
		pterm.Warning.Println("No budgeted users configured — fitguard will run in single-tenant mode: " +
			"every caller shares one \"default\" identity with no authentication and no budget limit. " +
			"Fine for local/solo use; not for anything with multiple callers.")
	}

	if len(cfg.Providers) > 1 {
		pterm.Info.Println("You can configure automatic fallback models in config.yaml under `fallback:` " +
			"(e.g. try gpt-4o-mini or claude-haiku-4-5 if your primary model fails or rate-limits).")
	}

	pterm.Println()
	dashboardConfigured, err := setUpDashboardLogin(cfg)
	if err != nil {
		return err
	}

	if err := config.Save(configPath, cfg); err != nil {
		return err
	}

	pterm.Success.Printfln("Wrote %s", configPath)
	pterm.Println()

	if len(issued) > 0 {
		pterm.Warning.Println("These virtual API keys won't be shown again — copy them now:")
		tableData := pterm.TableData{{"User", "Daily budget", "API key"}}
		for _, k := range issued {
			budgetLabel := fmt.Sprintf("$%.2f", k.dailyUSD)
			if k.dailyUSD == 0 {
				budgetLabel = "unlimited"
			}
			tableData = append(tableData, []string{k.userID, budgetLabel, k.key})
		}
		pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()
		pterm.Println()
	}

	nextSteps := "fitguard run\n\nThen point your app at:\n" + fmt.Sprintf("  http://localhost:%d/v1", cfg.Port)
	if len(issued) > 0 {
		nextSteps += "\n\nAuthenticate with the issued key instead of your real provider key:\n" +
			fmt.Sprintf("  Authorization: Bearer %s", issued[0].key)
	}
	if dashboardConfigured {
		nextSteps += fmt.Sprintf("\n\nDashboard: http://localhost:%d/dashboard (log in with the account you just created)", cfg.Port)
	}
	pterm.DefaultBox.WithTitle("Next steps").Println(nextSteps)
	return nil
}

// setUpDashboardLogin optionally creates the one dashboard admin account.
// Skipping it leaves /dashboard reachable with no login, warned about at
// every `fitguard run`. Returns whether an account was created.
func setUpDashboardLogin(cfg *config.Config) (bool, error) {
	pterm.Info.Println("Last step: protect the dashboard with a login, so spend and usage data " +
		"isn't visible to anyone who can reach the port — the same thing tools like Grafana and Coolify do.")

	setUp, err := pterm.DefaultInteractiveConfirm.
		WithDefaultValue(true).
		Show("Set up a dashboard login?")
	if err != nil {
		return false, err
	}
	if !setUp {
		pterm.Warning.Println("Skipped — the dashboard will be reachable by anyone with no login. " +
			"Run `fitguard reset-dashboard-password` any time to add one.")
		return false, nil
	}

	username, err := pterm.DefaultInteractiveTextInput.
		WithDefaultValue("admin").
		Show("Dashboard username")
	if err != nil {
		return false, err
	}

	password, err := promptNewPassword(fmt.Sprintf("Password for %q", username))
	if err != nil {
		return false, err
	}

	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return false, err
	}
	sessionSecret, err := auth.GenerateSecret()
	if err != nil {
		return false, err
	}

	cfg.Dashboard = config.DashboardConfig{
		SessionSecret: sessionSecret,
		Users:         []config.DashboardUser{{Username: username, PasswordHash: passwordHash}},
	}
	return true, nil
}
