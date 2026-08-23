// Command ai-guard runs a self-hostable, cost-protected AI gateway that is
// drop-in compatible with the OpenAI chat completions API.
package main

import (
	"os"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"

	"github.com/Oluiy/ai-cost-guard/internal/cli"
)

var version = "dev"

func main() {
	var configPath string

	root := &cobra.Command{
		Use:     "ai-guard",
		Short:   "Self-hostable AI gateway that prevents runaway LLM bills.",
		Version: version,
		Long: "ai-guard is a self-hostable, OpenAI-compatible proxy that sits between your app and " +
			"OpenAI/Anthropic/Groq/Together, adding response caching, per-user daily budgets, " +
			"automatic fallback, and cost logging. Point your OpenAI SDK's baseURL at ai-guard " +
			"instead of the provider directly; nothing else about your client code changes.\n\n" +
			"Run `ai-guard init` first to generate a config.yaml, then `ai-guard run` to start it.\n\n" +
			"Full documentation: https://github.com/Oluiy/ai-cost-guard/blob/main/DOCS.md",
		// Runtime failures (bad config, port in use, provider unreachable)
		// aren't usage mistakes — don't dump command help/usage for them.
		// Errors are printed once, below, with consistent styling.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "config.yaml", "path to config file")

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Interactively create a config.yaml",
		Long: "Walks you through setting up ai-guard: which providers to route through it, real " +
			"provider API keys, cache settings, and one or more budgeted users. Each budgeted user " +
			"gets a generated virtual API key (config.yaml's `keys:` section) — that's what your " +
			"app authenticates to ai-guard with, never your real provider key.\n\n" +
			"Safe to re-run: if config.yaml already exists, you'll be asked to confirm before it's " +
			"overwritten (existing virtual keys aren't recoverable once that happens).",
		Example: "  ai-guard init\n  ai-guard init --config prod.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cli.RunInit(configPath)
		},
	}

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Start the ai-guard proxy server",
		Long: "Loads config.yaml (or the path given by --config), starts the HTTP proxy, and blocks " +
			"until it's stopped. Ctrl+C (SIGINT) or SIGTERM triggers a graceful shutdown: ai-guard " +
			"stops accepting new connections, waits up to 10s for in-flight requests to finish, " +
			"then exits.\n\n" +
			"The dashboard (live spend, cache hit rate, top expensive requests) is served alongside " +
			"the proxy at /dashboard.",
		Example: "  ai-guard run\n  ai-guard run --config prod.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cli.RunServer(configPath)
		},
	}

	resetPasswordCmd := &cobra.Command{
		Use:   "reset-dashboard-password",
		Short: "Set or reset the dashboard login",
		Long: "Sets the single dashboard admin account's username/password in config.yaml. Works " +
			"whether or not one already exists — use it to set up the dashboard login for the first " +
			"time if you skipped it during `ai-guard init`, or to recover if you've forgotten the " +
			"password. Doesn't require ai-guard to be running, or the current password to be known; " +
			"that's the point, it's the recovery path. Invalidates any existing dashboard session.",
		Example: "  ai-guard reset-dashboard-password\n  ai-guard reset-dashboard-password --config prod.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cli.RunResetDashboardPassword(configPath)
		},
	}

	root.AddCommand(initCmd, runCmd, resetPasswordCmd)

	if err := root.Execute(); err != nil {
		pterm.Error.Println(err)
		os.Exit(1)
	}
}
