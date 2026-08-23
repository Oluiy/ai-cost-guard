package dashboard

import (
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

	for _, u := range h.Cfg.Users {
		if u.Username == req.Username && auth.VerifyPassword(u.PasswordHash, req.Password) {
			auth.SetSessionCookie(c, h.Cfg.SessionSecret, u.Username)
			return c.JSON(fiber.Map{"ok": true})
		}
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

// Whoami tells the dashboard's own UI who's logged in, so it can show an
// account/logout affordance — or, if no dashboard login is configured at
// all, know to hide that affordance rather than showing an empty one.
func (h *Handler) Whoami(c *fiber.Ctx) error {
	if !h.requiresLogin() {
		return c.JSON(fiber.Map{"auth_enabled": false})
	}
	username, _ := c.Locals("dashboard_user").(string)
	return c.JSON(fiber.Map{"auth_enabled": true, "username": username})
}
