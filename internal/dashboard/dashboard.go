// Package dashboard serves the embedded fitguard cost dashboard.
package dashboard

import (
	"embed"
	"io/fs"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"

	"github.com/Oluiy/ai-cost-guard/internal/auth"
	"github.com/Oluiy/ai-cost-guard/internal/config"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

//go:embed static
var staticFS embed.FS

// Handler serves the dashboard UI and its JSON/SSE data feeds.
type Handler struct {
	Store *logging.Store
	// Cfg is the dashboard login as it was at startup. It's only the
	// fallback: when Live is set, dash() reads the live copy instead.
	Cfg config.DashboardConfig
	// Live enables the settings endpoints when set; nil answers 501.
	Live *config.Settings
}

func New(store *logging.Store, cfg config.DashboardConfig) *Handler {
	return &Handler{Store: store, Cfg: cfg}
}

// dash returns the current dashboard login settings. With settings wired
// in (the normal case) that's the live copy, so a password reset or a new
// operator, made here or through another instance, takes effect without a
// restart. Reading the startup snapshot instead is what made a reset
// invisible until the next boot.
func (h *Handler) dash() config.DashboardConfig {
	if h.Live != nil {
		return h.Live.Dashboard()
	}
	return h.Cfg
}

func usernameSet(d config.DashboardConfig) map[string]bool {
	valid := make(map[string]bool, len(d.Users))
	for _, u := range d.Users {
		valid[u.Username] = true
	}
	return valid
}

// WithSettings enables the live-settings endpoints on h.
func (h *Handler) WithSettings(live *config.Settings) *Handler {
	h.Live = live
	return h
}

// requiresLogin reports whether a dashboard login has been set up. If
// not, the dashboard is reachable with no login.
func (h *Handler) requiresLogin() bool {
	return len(h.dash().Users) > 0
}

// Register mounts the dashboard routes on app.
func (h *Handler) Register(app *fiber.App) {
	app.Get("/dashboard", h.Index)
	app.Get("/dashboard/api/data", h.requireAuth, h.Data)
	app.Get("/dashboard/api/requests", h.requireAuth, h.Requests)
	app.Get("/dashboard/api/report", h.requireAuth, h.Report)
	app.Get("/dashboard/api/whoami", h.requireAuth, h.Whoami)
	app.Get("/dashboard/api/settings", h.requireAuth, h.Settings)
	app.Put("/dashboard/api/settings", h.requireAuth, h.UpdateSettings)
	app.Get("/dashboard/events", h.requireAuth, h.Events)

	// Rate-limited so a login page reachable by anyone isn't also a free
	// password-guessing endpoint. 5 attempts/minute per source IP.
	app.Post("/dashboard/login", limiter.New(limiter.Config{
		Max:        5,
		Expiration: time.Minute,
	}), h.Login)
	app.Post("/dashboard/logout", h.Logout)

	assets, err := fs.Sub(staticFS, "static")
	if err == nil {
		app.Get("/dashboard/css/*", staticHandler(assets, "css"))
		app.Get("/dashboard/js/*", staticHandler(assets, "js"))
	}
}

// requireAuth gates the JSON/SSE data routes with a 401, not a redirect.
// Stashes the authenticated username in c.Locals for handlers like Whoami.
func (h *Handler) requireAuth(c *fiber.Ctx) error {
	if !h.requiresLogin() {
		return c.Next()
	}
	d := h.dash()
	username, ok := auth.VerifySessionToken(d.SessionSecret, c.Cookies(auth.CookieName))
	if !ok || !usernameSet(d)[username] {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": fiber.Map{"message": "login required", "type": "unauthorized"},
		})
	}
	c.Locals("dashboard_user", username)
	return c.Next()
}

// staticHandler serves one subdirectory of embedded assets under
// /dashboard/<dir>/. Always public: no secrets in CSS/JS, and the login
// page needs to load its own styles before authenticating.
func staticHandler(assets fs.FS, dir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		name := dir + "/" + c.Params("*")
		b, err := fs.ReadFile(assets, name)
		if err != nil {
			return c.SendStatus(fiber.StatusNotFound)
		}
		switch {
		case dir == "css":
			c.Set("Content-Type", "text/css; charset=utf-8")
		case dir == "js":
			c.Set("Content-Type", "text/javascript; charset=utf-8")
		}
		return c.Send(b)
	}
}

// Index serves the login page if a dashboard login is configured and the
// caller isn't authenticated, the real dashboard otherwise.
func (h *Handler) Index(c *fiber.Ctx) error {
	page := "static/index.html"
	if d := h.dash(); len(d.Users) > 0 && !auth.Authenticated(c, d.SessionSecret, usernameSet(d)) {
		page = "static/login.html"
	}
	b, err := staticFS.ReadFile(page)
	if err != nil {
		return err
	}
	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.Send(b)
}

// snapshotQueryTimeout bounds each dashboard query so a stuck disk can't
// hang a request (Data) or wedge the streaming loop open forever (Events).
const snapshotQueryTimeout = 5 * time.Second
