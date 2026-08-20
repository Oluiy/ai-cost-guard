// Package cli implements the ai-guard command-line experience.
package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/pterm/pterm"

	"github.com/aicostguard/ai-cost-guard/internal/config"
)

// generateKey returns a random gateway-issued virtual API key. Callers
// authenticate to ai-guard with this instead of a real provider key, which
// keeps provider credentials out of client code entirely.
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
	"groq":      "https://api.groq.com/openai/v1",
	"together":  "https://api.together.xyz/v1",
}

// RunInit walks the user through an interactive setup and writes configPath.
func RunInit(configPath string) error {
	pterm.DefaultBigText.WithLetters(pterm.NewLettersFromStringWithStyle("AI Guard", pterm.NewStyle(pterm.FgCyan))).Render()
	pterm.Info.Println("Let's set up your cost-protected AI gateway.")
	pterm.Println()

	if _, err := os.Stat(configPath); err == nil {
		overwrite, err := pterm.DefaultInteractiveConfirm.
			WithDefaultValue(false).
			Show(fmt.Sprintf("%s already exists and will be overwritten, including any virtual API keys "+
				"in it — those aren't stored anywhere else, so copy them out first if you still need them. Continue?", configPath))
		if err != nil {
			return err
		}
		if !overwrite {
			pterm.Info.Println("Cancelled — existing config left untouched.")
			return nil
		}
	}

	selected, err := pterm.DefaultInteractiveMultiselect.
		WithOptions([]string{"openai", "anthropic", "groq", "together"}).
		WithDefaultText("Which providers do you want to route through ai-guard?").
		Show()
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("select at least one provider")
	}

	cfg := &config.Config{
		Port:      config.DefaultPort,
		DataDir:   ".",
		Providers: map[string]config.Provider{},
		Users:     map[string]config.Budget{},
		Keys:      map[string]string{},
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

	port, err := promptValidatedInt("Port to run ai-guard on", config.DefaultPort, 1, 65535)
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
		"and a virtual API key — callers authenticate to ai-guard with that key (never your real " +
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
		pterm.Warning.Println("No budgeted users configured — ai-guard will run in single-tenant mode: " +
			"every caller shares one \"default\" identity with no authentication and no budget limit. " +
			"Fine for local/solo use; not for anything with multiple callers.")
	}

	if len(selected) > 1 {
		pterm.Info.Println("You can configure automatic fallback models in config.yaml under `fallback:` " +
			"(e.g. try gpt-4o-mini or claude-3-haiku if your primary model fails or rate-limits).")
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

	nextSteps := "ai-guard run\n\nThen point your app at:\n" + fmt.Sprintf("  http://localhost:%d/v1", cfg.Port)
	if len(issued) > 0 {
		nextSteps += "\n\nAuthenticate with the issued key instead of your real provider key:\n" +
			fmt.Sprintf("  Authorization: Bearer %s", issued[0].key)
	}
	pterm.DefaultBox.WithTitle("Next steps").Println(nextSteps)
	return nil
}
