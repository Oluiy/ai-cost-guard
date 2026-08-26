package auth

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func newLoginApp(trustedProxies []string) *fiber.App {
	app := fiber.New(fiber.Config{
		EnableTrustedProxyCheck: len(trustedProxies) > 0,
		TrustedProxies:          trustedProxies,
	})
	app.Post("/dashboard/login", func(c *fiber.Ctx) error {
		SetSessionCookie(c, "test-secret", "admin", DefaultSessionTTL)
		return c.SendStatus(fiber.StatusOK)
	})
	return app
}

// Over plain HTTP the cookie must NOT be Secure, or the browser refuses
// to send it back and local development silently fails to stay logged in.
func TestSetSessionCookie_PlainHTTPOmitsSecure(t *testing.T) {
	app := newLoginApp(nil)

	req := httptest.NewRequest("POST", "http://localhost:8787/dashboard/login", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	cookie := resp.Header.Get("Set-Cookie")
	if cookie == "" {
		t.Fatal("expected a Set-Cookie header")
	}
	if strings.Contains(cookie, "secure") || strings.Contains(cookie, "Secure") {
		t.Errorf("plain HTTP should not set Secure, got: %s", cookie)
	}
	if !strings.Contains(strings.ToLower(cookie), "httponly") {
		t.Errorf("expected HttpOnly regardless of scheme, got: %s", cookie)
	}
}

// Behind a TLS-terminating proxy the connection to fitguard is plain
// HTTP, but the client used HTTPS. The cookie must be Secure in that
// case, which is the whole production deployment story.
func TestSetSessionCookie_ForwardedHTTPSSetsSecure(t *testing.T) {
	// 0.0.0.0 is the peer address Fiber's test harness reports.
	app := newLoginApp([]string{"0.0.0.0/0"})

	req := httptest.NewRequest("POST", "http://ai.example.com/dashboard/login", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	cookie := resp.Header.Get("Set-Cookie")
	if cookie == "" {
		t.Fatal("expected a Set-Cookie header")
	}
	if !strings.Contains(strings.ToLower(cookie), "secure") {
		t.Errorf("HTTPS via a trusted proxy should set Secure, got: %s", cookie)
	}
}

// Documents a Fiber behavior that is easy to assume otherwise: with no
// trusted proxies configured, IsProxyTrusted() returns true, so
// X-Forwarded-Proto is honored from any caller and the cookie comes back
// Secure.
//
// This is pinned as a test because the safety argument depends on it and
// a future Fiber change would be worth noticing. It is not a
// vulnerability: asserting the header only makes the caller's *own*
// cookie more restrictive, and it cannot be injected into someone else's
// request (browsers don't send X-Forwarded-Proto). Contrast c.IP(), which
// also needs a non-empty ProxyHeader — the reason run.go leaves that
// unset without trusted proxies, keeping the rate limiter unspoofable.
func TestSetSessionCookie_ForwardedProtoHonoredEvenWithoutTrustedProxies(t *testing.T) {
	app := newLoginApp(nil) // no trusted proxies configured

	req := httptest.NewRequest("POST", "http://ai.example.com/dashboard/login", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	cookie := strings.ToLower(resp.Header.Get("Set-Cookie"))
	if !strings.Contains(cookie, "secure") {
		t.Errorf("Fiber's documented default trusts X-Forwarded-Proto when "+
			"EnableTrustedProxyCheck is off; if this now fails, that default "+
			"changed and secureCookies' comment needs revisiting. Got: %s", cookie)
	}
}

// Logout has to clear the cookie with matching attributes, or the browser
// treats it as a different cookie and the session survives.
func TestClearSessionCookie_MatchesSetAttributes(t *testing.T) {
	app := fiber.New()
	app.Post("/dashboard/logout", func(c *fiber.Ctx) error {
		ClearSessionCookie(c)
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest("POST", "http://localhost:8787/dashboard/logout", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	cookie := strings.ToLower(resp.Header.Get("Set-Cookie"))
	if !strings.Contains(cookie, "httponly") {
		t.Errorf("expected HttpOnly on the cleared cookie, got: %s", cookie)
	}
	if !strings.Contains(cookie, "path=/dashboard") {
		t.Errorf("expected the same Path as when set, got: %s", cookie)
	}
}
