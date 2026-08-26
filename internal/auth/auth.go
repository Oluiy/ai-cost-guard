// Package auth handles authentication: password hashing and
// signed-cookie sessions.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword returns a bcrypt hash of password.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword reports whether password matches the given bcrypt hash.
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// decoyHash is a real bcrypt hash of a random string nobody holds, used
// to keep a login for a nonexistent username as slow as a real one —
// without it, a missing user returns in microseconds while a real
// mismatch takes bcrypt's ~100ms, letting an attacker enumerate valid
// usernames by timing.
const decoyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// VerifyPasswordConstantTime checks password against hash, taking the
// same time whether or not the user exists (found).
func VerifyPasswordConstantTime(hash string, found bool, password string) bool {
	if !found {
		_ = bcrypt.CompareHashAndPassword([]byte(decoyHash), []byte(password))
		return false
	}
	return VerifyPassword(hash, password)
}

// GenerateSecret returns a random secret for signing session tokens.
func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating session secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DefaultSessionTTL is how long a dashboard login lasts when
// session_ttl_hours is unset.
const DefaultSessionTTL = 7 * 24 * time.Hour

// SessionTTL resolves the configured lifetime, falling back to
// DefaultSessionTTL for zero or negative values.
func SessionTTL(hours int) time.Duration {
	if hours <= 0 {
		return DefaultSessionTTL
	}
	return time.Duration(hours) * time.Hour
}

// NewSessionToken returns a signed session token for username, valid for ttl.
func NewSessionToken(secret, username string, ttl time.Duration) string {
	expiry := time.Now().Add(ttl).Unix()
	return signToken(secret, username, expiry)
}

func signToken(secret, username string, expiry int64) string {
	payload := username + "." + strconv.FormatInt(expiry, 10)
	sig := signPayload(secret, payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
}

func signPayload(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifySessionToken verifies token against secret and returns the
// username if the signature is valid and unexpired.
func VerifySessionToken(secret, token string) (username string, ok bool) {
	payloadB64, sig, found := strings.Cut(token, ".")
	if !found {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return "", false
	}

	expectedSig := signPayload(secret, string(payload))
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return "", false
	}

	// Split on the last dot: the payload is "<username>.<expiry>" and a
	// username may itself contain dots.
	dot := strings.LastIndex(string(payload), ".")
	if dot < 0 {
		return "", false
	}
	name, expiryStr := string(payload)[:dot], string(payload)[dot+1:]
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return "", false
	}
	if time.Now().Unix() > expiry {
		return "", false
	}
	return name, true
}
