package auth

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !VerifyPassword(hash, "correct-horse-battery-staple") {
		t.Fatal("expected the correct password to verify")
	}
	if VerifyPassword(hash, "wrong-password") {
		t.Fatal("expected an incorrect password to fail verification")
	}
}

func TestHashPassword_NeverStoresPlaintext(t *testing.T) {
	hash, err := HashPassword("hunter2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash == "hunter2" {
		t.Fatal("HashPassword must not return the raw password")
	}
}

func TestVerifyPassword_EmptyHashFailsClosed(t *testing.T) {
	// An empty/malformed hash (e.g. the dashboard was never set up) must
	// reject every password, not accept everything.
	if VerifyPassword("", "anything") {
		t.Fatal("expected an empty hash to fail verification, not accept any password")
	}
}

func TestGenerateSecret_ProducesDistinctValues(t *testing.T) {
	a, err := GenerateSecret()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := GenerateSecret()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == b {
		t.Fatal("expected two independently generated secrets to differ")
	}
	if len(a) < 32 {
		t.Fatalf("secret looks too short to resist guessing: %d chars", len(a))
	}
}

func TestSessionToken_ValidRoundTrip(t *testing.T) {
	secret := "test-secret"
	token := NewSessionToken(secret, "admin", DefaultSessionTTL)

	username, ok := VerifySessionToken(secret, token)
	if !ok {
		t.Fatal("expected a freshly issued token to verify")
	}
	if username != "admin" {
		t.Fatalf("got username %q, want admin", username)
	}
}

func TestSessionToken_WrongSecretRejected(t *testing.T) {
	token := NewSessionToken("secret-a", "admin", DefaultSessionTTL)
	if _, ok := VerifySessionToken("secret-b", token); ok {
		t.Fatal("expected a token signed with a different secret to be rejected")
	}
}

func TestSessionToken_TamperedPayloadRejected(t *testing.T) {
	secret := "test-secret"
	token := NewSessionToken(secret, "admin", DefaultSessionTTL)

	// Swap the payload for one claiming a different (e.g. higher-
	// privileged, in a future multi-account world) username, keeping the
	// original signature — this must not verify, or the signature isn't
	// doing anything.
	forged := signToken(secret, "someone-else", time.Now().Add(DefaultSessionTTL).Unix())
	payload, _, _ := strings.Cut(forged, ".")
	_, sig, _ := strings.Cut(token, ".")
	tampered := payload + "." + sig

	if _, ok := VerifySessionToken(secret, tampered); ok {
		t.Fatal("expected a token with a mismatched signature to be rejected")
	}
}

func TestSessionToken_ExpiredRejected(t *testing.T) {
	secret := "test-secret"
	expired := signToken(secret, "admin", time.Now().Add(-time.Minute).Unix())

	if _, ok := VerifySessionToken(secret, expired); ok {
		t.Fatal("expected an expired token to be rejected")
	}
}

func TestSessionToken_MalformedInputRejected(t *testing.T) {
	secret := "test-secret"
	for _, bad := range []string{"", "no-dot-separator", "..", "not-base64!!.also-not-base64!!"} {
		if _, ok := VerifySessionToken(secret, bad); ok {
			t.Errorf("expected malformed token %q to be rejected", bad)
		}
	}
}

// Usernames containing a dot used to produce a token that signed fine but
// never verified, because the payload was split at the first dot rather
// than the last, leaving "lovelace.<expiry>" to be parsed as an expiry.
func TestSessionToken_UsernameContainingDotsRoundTrips(t *testing.T) {
	secret := "test-secret"
	for _, name := range []string{"ada.lovelace", "a.b.c.d", "first.last@example.com"} {
		token := NewSessionToken(secret, name, DefaultSessionTTL)
		got, ok := VerifySessionToken(secret, token)
		if !ok {
			t.Errorf("username %q: expected token to verify", name)
			continue
		}
		if got != name {
			t.Errorf("username %q: round-tripped as %q", name, got)
		}
	}
}

// A dotted username must not let anyone forge a *different* identity by
// crafting the name so the payload re-splits somewhere else.
func TestSessionToken_DottedUsernameStillTamperProof(t *testing.T) {
	secret := "test-secret"
	token := NewSessionToken(secret, "ada.lovelace", DefaultSessionTTL)
	if _, ok := VerifySessionToken("different-secret", token); ok {
		t.Fatal("expected a token signed with another secret to be rejected")
	}
}

func TestVerifyPasswordConstantTime_CorrectPasswordAccepted(t *testing.T) {
	hash, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	if !VerifyPasswordConstantTime(hash, true, "correct-horse") {
		t.Fatal("expected the correct password to verify")
	}
	if VerifyPasswordConstantTime(hash, true, "wrong") {
		t.Fatal("expected a wrong password to be rejected")
	}
}

func TestVerifyPasswordConstantTime_UnknownUserAlwaysFails(t *testing.T) {
	// found=false must reject regardless of what's passed as the hash.
	if VerifyPasswordConstantTime("", false, "anything") {
		t.Fatal("expected an unknown user to be rejected")
	}
}

// The whole point of the decoy: a miss must cost roughly what a hit
// costs, or response latency leaks which usernames exist. Compares
// against a generous lower bound rather than the hit's exact duration,
// since wall-clock timing under test parallelism is noisy.
func TestVerifyPasswordConstantTime_UnknownUserStillSpendsBcryptTime(t *testing.T) {
	hash, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}

	start := time.Now()
	VerifyPasswordConstantTime(hash, true, "wrong-password")
	hit := time.Since(start)

	start = time.Now()
	VerifyPasswordConstantTime("", false, "wrong-password")
	miss := time.Since(start)

	// Without the decoy the miss path returned in microseconds; anything
	// near the hit's cost means bcrypt actually ran.
	if miss < hit/2 {
		t.Fatalf("unknown-user path returned too fast (%v vs %v for a known user): "+
			"username enumeration is observable from timing", miss, hit)
	}
}

// A malformed decoy would be rejected by bcrypt immediately, which would
// silently reintroduce the timing gap the decoy exists to close.
func TestDecoyHash_IsWellFormedBcrypt(t *testing.T) {
	cost, err := bcrypt.Cost([]byte(decoyHash))
	if err != nil {
		t.Fatalf("decoy hash is not a valid bcrypt hash: %v", err)
	}
	if cost != bcrypt.DefaultCost {
		t.Errorf("decoy cost is %d, want %d to match real hashes", cost, bcrypt.DefaultCost)
	}
}

func TestSessionTTL_ResolvesFromConfig(t *testing.T) {
	if got := SessionTTL(24); got != 24*time.Hour {
		t.Errorf("SessionTTL(24) = %v, want 24h", got)
	}
	// Unset or nonsensical values fall back rather than producing a
	// zero-length session that expires the instant it's issued.
	for _, hours := range []int{0, -1} {
		if got := SessionTTL(hours); got != DefaultSessionTTL {
			t.Errorf("SessionTTL(%d) = %v, want the default %v", hours, got, DefaultSessionTTL)
		}
	}
}

// A shorter configured TTL has to actually shorten the token's validity,
// not just the cookie's Expires attribute — the cookie is a client-side
// hint, the signed expiry is what the server enforces.
func TestSessionToken_ShortTTLExpiresOnSchedule(t *testing.T) {
	secret := "test-secret"
	token := NewSessionToken(secret, "admin", -time.Second)
	if _, ok := VerifySessionToken(secret, token); ok {
		t.Fatal("a token issued with an already-elapsed TTL must not verify")
	}
}
