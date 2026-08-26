package cli

import "testing"

func TestParseIntInRange_Valid(t *testing.T) {
	v, err := parseIntInRange("8787", 1, 65535)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 8787 {
		t.Fatalf("got %d, want 8787", v)
	}
}

func TestParseIntInRange_TrimsWhitespace(t *testing.T) {
	v, err := parseIntInRange("  8787  ", 1, 65535)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 8787 {
		t.Fatalf("got %d, want 8787", v)
	}
}

func TestParseIntInRange_RejectsNonNumeric(t *testing.T) {
	for _, in := range []string{"abc", "", "80.5", "80a", "-"} {
		if _, err := parseIntInRange(in, 1, 65535); err == nil {
			t.Errorf("input %q: expected error, got none", in)
		}
	}
}

func TestParseIntInRange_RejectsOutOfRange(t *testing.T) {
	if _, err := parseIntInRange("0", 1, 65535); err == nil {
		t.Error("expected error for port 0 (below range)")
	}
	if _, err := parseIntInRange("70000", 1, 65535); err == nil {
		t.Error("expected error for port above 65535")
	}
	if _, err := parseIntInRange("-1", 1, 65535); err == nil {
		t.Error("expected error for negative port")
	}
}

func TestParseNonNegativeUSD_Valid(t *testing.T) {
	for in, want := range map[string]float64{"5": 5, "12.50": 12.5, "0": 0, "0.01": 0.01} {
		got, err := parseNonNegativeUSD(in)
		if err != nil {
			t.Errorf("input %q: unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("input %q: got %v, want %v", in, got, want)
		}
	}
}

func TestParseNonNegativeUSD_StripsDollarSign(t *testing.T) {
	got, err := parseNonNegativeUSD("$5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5 {
		t.Fatalf("got %v, want 5", got)
	}
}

// This is the exact bug this function exists to fix: fmt.Sscanf("abc",
// "%f", &limit) silently left limit at its zero value (0), which the
// budget enforcer treats as "unlimited" — a typo during setup could
// silently grant unlimited spend. parseNonNegativeUSD must error instead
// of returning a fabricated zero.
func TestParseNonNegativeUSD_RejectsMalformedInputInsteadOfSilentlyZeroing(t *testing.T) {
	for _, in := range []string{"abc", "", "five", "5.0.0", "$"} {
		if _, err := parseNonNegativeUSD(in); err == nil {
			t.Errorf("input %q: expected error, got none (this must never silently become 0)", in)
		}
	}
}

func TestParseNonNegativeUSD_RejectsNegative(t *testing.T) {
	if _, err := parseNonNegativeUSD("-3"); err == nil {
		t.Error("expected error for negative budget (would also silently mean 'unlimited' downstream)")
	}
}

func TestValidateNewPassword_Valid(t *testing.T) {
	if err := validateNewPassword("correct-horse-battery", "correct-horse-battery"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateNewPassword_RejectsTooShort(t *testing.T) {
	if err := validateNewPassword("short", "short"); err == nil {
		t.Fatal("expected error for a password under the minimum length")
	}
}

func TestValidateNewPassword_RejectsMismatch(t *testing.T) {
	if err := validateNewPassword("correct-horse-battery", "different-password"); err == nil {
		t.Fatal("expected error when the confirmation doesn't match")
	}
}

// The cache-TTL prompt's bounds. 21600 (6 hours) is the case that
// motivated asking at all: a scheduled job whose interval far exceeds the
// 5-minute default and would otherwise never hit the cache.
func TestParseIntInRange_CacheTTLBounds(t *testing.T) {
	const minTTL, maxTTL = 1, 60 * 60 * 24 * 30

	valid := []string{"1", "300", "21600", "2592000", " 900 "}
	for _, in := range valid {
		if _, err := parseIntInRange(in, minTTL, maxTTL); err != nil {
			t.Errorf("parseIntInRange(%q) rejected a valid TTL: %v", in, err)
		}
	}

	invalid := []string{"0", "-1", "2592001", "", "abc", "5m", "300s"}
	for _, in := range invalid {
		if _, err := parseIntInRange(in, minTTL, maxTTL); err == nil {
			t.Errorf("parseIntInRange(%q) accepted an invalid TTL", in)
		}
	}
}
