package auth

import (
	"time"

	"github.com/gofiber/fiber/v2"
)

// CookieName is the session cookie the dashboard reads/writes.
const CookieName = "fitguard_session"

// secureCookies reports whether the session cookie should carry Secure.
// c.Protocol() reflects X-Forwarded-Proto behind a trusted proxy, so a
// TLS-terminating proxy still yields "https" even though fitguard itself
// speaks plain HTTP. Without EnableTrustedProxyCheck, any caller can
// claim https for itself — harmless here, since that only makes its own
// cookie more restrictive.
func secureCookies(c *fiber.Ctx) bool {
	return c.Protocol() == "https"
}

// SetSessionCookie logs username in on the response. ttl controls how
// long the session lasts; use SessionTTL to resolve it from config.
func SetSessionCookie(c *fiber.Ctx, secret, username string, ttl time.Duration) {
	c.Cookie(&fiber.Cookie{
		Name:     CookieName,
		Value:    NewSessionToken(secret, username, ttl),
		Expires:  time.Now().Add(ttl),
		HTTPOnly: true,
		Secure:   secureCookies(c),
		SameSite: fiber.CookieSameSiteLaxMode,
		Path:     "/dashboard",
	})
}

// ClearSessionCookie logs the caller out. The attributes have to match
// the ones used when setting it, or the browser treats this as a
// different cookie and the original survives the logout.
func ClearSessionCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     CookieName,
		Value:    "",
		Expires:  time.Now().Add(-time.Hour),
		HTTPOnly: true,
		Secure:   secureCookies(c),
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
