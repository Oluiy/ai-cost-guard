package cli

import (
	"strings"
	"testing"
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
