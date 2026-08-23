package auth

import (
	"strings"
	"testing"
	"time"
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
	token := NewSessionToken(secret, "admin")

	username, ok := VerifySessionToken(secret, token)
	if !ok {
		t.Fatal("expected a freshly issued token to verify")
	}
	if username != "admin" {
		t.Fatalf("got username %q, want admin", username)
	}
}

func TestSessionToken_WrongSecretRejected(t *testing.T) {
	token := NewSessionToken("secret-a", "admin")
	if _, ok := VerifySessionToken("secret-b", token); ok {
		t.Fatal("expected a token signed with a different secret to be rejected")
	}
}

func TestSessionToken_TamperedPayloadRejected(t *testing.T) {
	secret := "test-secret"
	token := NewSessionToken(secret, "admin")

	// Swap the payload for one claiming a different (e.g. higher-
	// privileged, in a future multi-account world) username, keeping the
	// original signature — this must not verify, or the signature isn't
	// doing anything.
	forged := signToken(secret, "someone-else", time.Now().Add(SessionTTL).Unix())
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
