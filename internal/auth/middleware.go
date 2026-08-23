package auth

import (
	"time"

	"github.com/gofiber/fiber/v2"
)

// CookieName is the session cookie the dashboard reads/writes.
const CookieName = "ai_guard_session"

// SetSessionCookie logs username in on the response.
// Set a secure, HTTP-only session cookie with the user's username, where the cookie is done by the user's browser.
func SetSessionCookie(c *fiber.Ctx, secret, username string) {
	c.Cookie(&fiber.Cookie{
		Name:     CookieName,
		Value:    NewSessionToken(secret, username),
		Expires:  time.Now().Add(SessionTTL),
		HTTPOnly: true,
		SameSite: fiber.CookieSameSiteLaxMode,
		Path:     "/dashboard",
	})
}

// ClearSessionCookie logs the caller out.
func ClearSessionCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     CookieName,
		Value:    "",
		Expires:  time.Now().Add(-time.Hour),
		HTTPOnly: true,
		SameSite: fiber.CookieSameSiteLaxMode,
		Path:     "/dashboard",
	})
}

// Authenticated reports whether the request carries a valid, unexpired session cookie for one of validUsernames.
func Authenticated(c *fiber.Ctx, secret string, validUsernames map[string]bool) bool {
	username, ok := VerifySessionToken(secret, c.Cookies(CookieName))
	return ok && validUsernames[username]
}

// RequireJSON is for JSON/SSE data routes: an unauthenticated request gets a plain 401.
func RequireJSON(secret string, validUsernames map[string]bool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if !Authenticated(c, secret, validUsernames) {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": fiber.Map{"message": "login required", "type": "unauthorized"},
			})
		}
		return c.Next()
	}
}
