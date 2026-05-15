package integration

import (
	"crypto/sha256"
	"encoding/base64"
)

// pkceChallengeImpl is the local-to-this-package implementation of
// RFC 7636 PKCE S256 challenge computation. The auth package's
// version is unexported by design — this is the spec and the
// integration test calls the same algorithm against the test
// authorization server.
func pkceChallengeImpl(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
