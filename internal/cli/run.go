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
	"github.com/Oluiy/ai-cost-guard/internal/dashboard"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
	"github.com/Oluiy/ai-cost-guard/internal/proxy"
)

// RunServer loads configPath and starts the ai-guard proxy server. It
// blocks until the server stops (either it fails to start, or it's shut
// down cleanly via SIGINT/SIGTERM).
func RunServer(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("run 'ai-guard init' first, or check your config: %w", err)
	}

	for _, w := range cfg.Warnings() {
		pterm.Warning.Println(w)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data_dir %q: %w", cfg.DataDir, err)
	}

	dbPath := filepath.Join(cfg.DataDir, "aiguard.db")
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

	var enforcer budget.Enforcer
	switch cfg.BudgetBackend.Backend {
	case "redis":
		re, err := budget.NewRedisEnforcer(cfg.BudgetBackend.RedisURL, cfg.Users)
		if err != nil {
			return fmt.Errorf("connecting budget backend to redis at %s: %w (is Redis running and reachable?)",
				maskRedisURL(cfg.BudgetBackend.RedisURL), err)
		}
		enforcer = re
		pterm.Info.Printfln("budget backend: redis @ %s (correct across multiple ai-guard instances)", maskRedisURL(cfg.BudgetBackend.RedisURL))
	default:
		enforcer = budget.New(store, cfg.Users)
		pterm.Info.Println("budget backend: local (correct for a single ai-guard instance only)")
	}
	handler := proxy.New(cfg, c, enforcer, store)

	app := fiber.New(fiber.Config{
		AppName:               "ai-guard",
		DisableStartupMessage: true,
		// ReadTimeout/IdleTimeout close connections that open but never
		// finish sending a request (or sit idle between keep-alive
		// requests) — standard hardening against slowloris-style
		// connection exhaustion, which is otherwise unbounded (fasthttp
		// defaults to no timeout at all). WriteTimeout is deliberately
		// left unset: a streamed chat completion can legitimately take
		// minutes to finish writing, and capping that risks cutting off
		// a real in-progress response rather than an attack.
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second,
	})
	// A panic in any handler (a malformed upstream response, a nil map
	// access, anything unanticipated) would otherwise crash the entire
	// process — every tenant, the dashboard, everything — since Go
	// terminates the whole program on an unrecovered panic in any
	// goroutine, not just the one it happened in. This converts that into
	// a single 500 for the request that triggered it.
	app.Use(recover.New())

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Post("/v1/chat/completions", handler.ChatCompletions)
	app.Post("/v1/embeddings", handler.Embeddings)
	dashboard.New(store, cfg.Dashboard).Register(app)

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

// maskRedisURL strips embedded credentials before a Redis URL is logged.
// Managed Redis (DigitalOcean, Upstash, Redis Cloud, ...) commonly embeds
// a password directly in the URL (redis://default:PASSWORD@host:port) —
// printing that verbatim on every startup, and in the error path if the
// connection fails, would leak it into terminal scrollback, systemd
// journal logs, and Docker logs. Falls back to a generic placeholder
// rather than the raw string if the URL doesn't parse, so a malformed
// URL can't accidentally bypass the masking.
func maskRedisURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[redis url]"
	}
	u.User = nil
	return u.String()
}

func printBanner(cfg *config.Config) {
	pterm.DefaultBigText.WithLetters(pterm.NewLettersFromStringWithStyle("AI Guard", pterm.NewStyle(pterm.FgCyan))).Render()
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
