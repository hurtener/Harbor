package integration

import (
	cryptorand "crypto/rand"
	"encoding/base64"
)

// phase30Rand is a thin alias around crypto/rand.Read so the
// Phase 30 test file can stay short on top-level imports.
func phase30Rand(b []byte) (int, error) { return cryptorand.Read(b) }

// base64URLEncode is the URL-safe base64 encoding used for fake
// codes / tokens in the integration test's authorization server.
func base64URLEncode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
