package cli

import (
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// maskRedisURL exists specifically to keep a Redis password out of logs —
// this is the regression test for that, not just a general sanity check.
func TestMaskRedisURL_StripsCredentials(t *testing.T) {
	got := maskRedisURL("redis://default:supersecretpassword@my-redis-host:6379/0")
	if strings.Contains(got, "supersecretpassword") {
		t.Fatalf("password leaked into masked URL: %q", got)
	}
	for _, want := range []string{"my-redis-host", "6379"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected masked URL to still contain %q, got %q", want, got)
		}
	}
}

func TestMaskRedisURL_NoCredentialsUnchanged(t *testing.T) {
	got := maskRedisURL("redis://my-redis-host:6379/0")
	if !strings.Contains(got, "my-redis-host") || !strings.Contains(got, "6379") {
		t.Errorf("expected host/port preserved, got %q", got)
	}
}

func TestMaskRedisURL_MalformedURLDoesNotLeakRawInput(t *testing.T) {
	raw := "redis://default:supersecretpassword@[invalid"
	got := maskRedisURL(raw)
	if got == raw {
		t.Fatal("malformed URL was returned unmasked")
	}
	if strings.Contains(got, "supersecretpassword") {
		t.Errorf("password leaked from malformed URL: %q", got)
	}
}

// The rate limiter keys on c.IP(), and Fiber only reads a forwarded
// header when ProxyHeader is non-empty. Leaving it empty unless the
// operator named their proxies is what stops a caller from claiming a
// fresh source address per login attempt and sailing past the limiter.
func TestProxyHeader_EmptyWithoutTrustedProxies(t *testing.T) {
	for _, trusted := range [][]string{nil, {}} {
		if got := proxyHeader(trusted); got != "" {
			t.Errorf("proxyHeader(%v) = %q, want \"\": honoring a forwarded "+
				"header with no trusted proxies lets any caller spoof its IP "+
				"and bypass the login rate limiter", trusted, got)
		}
	}
}

// Behind a configured proxy the opposite failure applies: without a
// forwarded header every request looks like it came from the proxy, so
// one attacker's attempts exhaust the shared bucket and lock out real users.
func TestProxyHeader_SetWhenTrustedProxiesConfigured(t *testing.T) {
	got := proxyHeader([]string{"127.0.0.1"})
	if got != fiber.HeaderXForwardedFor {
		t.Errorf("proxyHeader = %q, want %q", got, fiber.HeaderXForwardedFor)
	}
}
