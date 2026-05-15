package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCE (RFC 7636) — code_verifier and code_challenge generation.
//
// code_verifier: 43-128 chars of [A-Z a-z 0-9 -._~]. We pick 64.
// code_challenge: BASE64URL(SHA256(code_verifier)), without padding.

// PKCEVerifierLen is the byte length of the random verifier
// (URL-safe base64 expands to 4*ceil(N/3) chars; 48 raw bytes →
// 64 base64url chars without padding).
const PKCEVerifierLen = 48

// newPKCEVerifier returns a fresh PKCE code_verifier.
// crypto/rand-backed; no shared mutable state.
func newPKCEVerifier() (string, error) {
	buf := make([]byte, PKCEVerifierLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: pkce verifier rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// pkceChallengeS256 computes BASE64URL(SHA256(verifier)) per RFC 7636.
func pkceChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newState mints a random CSRF / flow-correlation token. URL-safe;
// not a secret (it's a one-time-use nonce) but it MUST be unguessable
// to prevent flow injection.
func newState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: state rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
