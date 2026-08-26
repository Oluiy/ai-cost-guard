package dashboard

import (
	"crypto/subtle"

	"github.com/gofiber/fiber/v2"

	"github.com/Oluiy/ai-cost-guard/internal/auth"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Login verifies credentials against the configured dashboard account(s)
// and, on success, issues a session cookie.
func (h *Handler) Login(c *fiber.Ctx) error {
	var req loginRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{"message": "invalid request", "type": "invalid_request_error"},
		})
	}

	// Look up the username, then always run one password comparison, so
	// an unknown username isn't distinguishable from a wrong password by
	// response time.
	var (
		hash     string
		username string
		found    bool
	)
	for _, u := range h.Cfg.Users {
		if subtle.ConstantTimeCompare([]byte(u.Username), []byte(req.Username)) == 1 {
			hash, username, found = u.PasswordHash, u.Username, true
		}
	}

	if auth.VerifyPasswordConstantTime(hash, found, req.Password) {
		auth.SetSessionCookie(c, h.Cfg.SessionSecret, username, auth.SessionTTL(h.Cfg.SessionTTLHours))
		return c.JSON(fiber.Map{"ok": true})
	}

	return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
		"error": fiber.Map{"message": "incorrect username or password", "type": "invalid_credentials"},
	})
}

// Logout clears the session cookie.
func (h *Handler) Logout(c *fiber.Ctx) error {
	auth.ClearSessionCookie(c)
	return c.JSON(fiber.Map{"ok": true})
}

// Whoami tells the dashboard UI who's logged in, or that no login is
// configured at all.
func (h *Handler) Whoami(c *fiber.Ctx) error {
	if !h.requiresLogin() {
		return c.JSON(fiber.Map{"auth_enabled": false})
	}
	username, _ := c.Locals("dashboard_user").(string)
	return c.JSON(fiber.Map{"auth_enabled": true, "username": username})
}
