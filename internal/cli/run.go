package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/pterm/pterm"

	"github.com/Oluiy/ai-cost-guard/internal/budget"
	"github.com/Oluiy/ai-cost-guard/internal/cache"
	"github.com/Oluiy/ai-cost-guard/internal/config"
	"github.com/Oluiy/ai-cost-guard/internal/cost"
	"github.com/Oluiy/ai-cost-guard/internal/dashboard"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
	"github.com/Oluiy/ai-cost-guard/internal/proxy"
)

// RunServer loads configPath and starts the fitguard proxy server. It
// blocks until the server stops (either it fails to start, or it's shut
// down cleanly via SIGINT/SIGTERM).
func RunServer(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("run 'fitguard init' first, or check your config: %w", err)
	}

	for _, w := range cfg.Warnings() {
		pterm.Warning.Println(w)
	}

	if len(cfg.Pricing) > 0 {
		overrides := make(map[string]cost.Price, len(cfg.Pricing))
		for model, o := range cfg.Pricing {
			overrides[model] = cost.Price{
				InputPer1K: o.InputPer1K, OutputPer1K: o.OutputPer1K,
				Provider: o.Provider, Embedding: o.Embedding,
			}
		}
		cost.ApplyOverrides(overrides)
		pterm.Info.Printfln("pricing: %d model price override(s) applied from config", len(cfg.Pricing))
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data_dir %q: %w", cfg.DataDir, err)
	}

	dbPath := filepath.Join(cfg.DataDir, "fitguard.db")
	store, err := logging.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening request log %q: %w", dbPath, err)
	}
	defer store.Close()

	var c cache.Cache
	if cfg.Cache.Enabled {
		switch cfg.Cache.Backend {
		case "redis":
			rc, err := cache.NewRedisCache(cfg.Cache.RedisURL)
			if err != nil {
				return fmt.Errorf("connecting to redis at %s: %w (is Redis running and reachable?)", maskRedisURL(cfg.Cache.RedisURL), err)
			}
			c = rc
			pterm.Info.Printfln("cache: redis @ %s", maskRedisURL(cfg.Cache.RedisURL))
		default:
			mc := cache.NewMemoryCache()
			defer mc.Close()
			c = mc
			pterm.Info.Println("cache: in-memory")
		}
	}

	// Shared by the proxy (reads), the budget enforcer (reads), and the
	// dashboard (writes), so a settings change is visible immediately.
	settings := config.NewSettings(configPath, cfg)

	var enforcer budget.Enforcer
	switch cfg.BudgetBackend.Backend {
	case "redis":
		re, err := budget.NewRedisEnforcer(cfg.BudgetBackend.RedisURL, settings)
		if err != nil {
			return fmt.Errorf("connecting budget backend to redis at %s: %w (is Redis running and reachable?)",
				maskRedisURL(cfg.BudgetBackend.RedisURL), err)
		}
		re.SetFailClosed(cfg.BudgetBackend.FailClosed)
		enforcer = re
		pterm.Info.Printfln("budget backend: redis @ %s (correct across multiple fitguard instances)", maskRedisURL(cfg.BudgetBackend.RedisURL))
	default:
		enforcer = budget.NewFailClosed(store, settings, cfg.BudgetBackend.FailClosed)
		pterm.Info.Println("budget backend: local (correct for a single fitguard instance only)")
	}
	if cfg.BudgetBackend.FailClosed {
		pterm.Info.Println("budget enforcement: fail-closed (a key with no configured budget is refused)")
	}
	handler := proxy.New(cfg, settings, c, enforcer, store)

	app := fiber.New(fiber.Config{
		AppName:               "fitguard",
		DisableStartupMessage: true,
		// Only honor X-Forwarded-For/-Proto from an explicitly trusted
		// proxy; otherwise a caller could spoof its own address and
		// defeat the login rate limiter.
		EnableTrustedProxyCheck: len(cfg.TrustedProxies) > 0,
		TrustedProxies:          cfg.TrustedProxies,
		ProxyHeader:             proxyHeader(cfg.TrustedProxies),
		// Fiber returns the forwarded header even if empty/invalid unless
		// this is on; without it, a direct request (no proxy) keys the
		// rate limiter on an empty string and shares one bucket.
		EnableIPValidation: true,
		// Guards against slowloris-style connection exhaustion.
		// WriteTimeout is intentionally unset: a streamed completion can
		// legitimately take minutes.
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second,
	})
	// Recovers a panic in any handler to a 500 for that request, instead
	// of crashing the whole process.
	app.Use(recover.New())

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Post("/v1/chat/completions", handler.ChatCompletions)
	app.Post("/v1/embeddings", handler.Embeddings)
	dashboard.New(store, cfg.Dashboard).WithSettings(settings).Register(app)

	printBanner(cfg)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- app.Listen(fmt.Sprintf(":%d", cfg.Port))
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("starting server on port %d: %w (is another process already using this port? "+
				"set a different `port:` in %s)", cfg.Port, err, configPath)
		}
		return nil
	case sig := <-sigCh:
		pterm.Println()
		pterm.Info.Printfln("received %s, shutting down (waiting up to 10s for in-flight requests)...", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.ShutdownWithContext(ctx); err != nil {
			pterm.Warning.Printfln("error during shutdown: %v", err)
		}
		pterm.Success.Println("stopped")
		return nil
	}
}

// proxyHeader returns the header Fiber reads a client's real address
// from, or "" to use the raw peer address when no proxy is trusted.
func proxyHeader(trustedProxies []string) string {
	if len(trustedProxies) == 0 {
		return ""
	}
	return fiber.HeaderXForwardedFor
}

// maskRedisURL strips embedded credentials before a Redis URL is logged.
// Falls back to a placeholder if the URL doesn't parse, so a malformed
// URL can't bypass the masking.
func maskRedisURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[redis url]"
	}
	u.User = nil
	return u.String()
}

func printBanner(cfg *config.Config) {
	pterm.DefaultBigText.WithLetters(pterm.NewLettersFromStringWithStyle("FitGuard", pterm.NewStyle(pterm.FgCyan))).Render()
	providers := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		providers = append(providers, name)
	}
	pterm.Info.Printfln("providers: %v", providers)
	pterm.Info.Printfln("cache: enabled=%v backend=%s", cfg.Cache.Enabled, cfg.Cache.Backend)
	if len(cfg.Users) > 0 {
		pterm.Info.Printfln("budgets configured for %d user(s)", len(cfg.Users))
	}
	pterm.Success.Printfln("listening on http://localhost:%d/v1", cfg.Port)
	pterm.Success.Printfln("dashboard at http://localhost:%d/dashboard", cfg.Port)
	pterm.Println()
}
