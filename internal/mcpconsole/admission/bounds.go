package admission

import "time"

// Package-level bounds for the render-admission claims envelope. These
// are the explicit UTF-8 / NUL / rune / byte / cardinality / time /
// token bounds the authority enforces at BOTH mint and verify, so a
// sealed admission and its verifying expectation can never diverge in
// shape between the two edges of the same authority.
const (
	// Schema is the claim-family identifier bound into every admission.
	// Verify rejects any other claim family.
	Schema = "harbor.render.admission"

	// SchemaVersion is the claims schema version. Bumped only when the
	// claims shape changes in a backwards-incompatible way; verify
	// rejects any other version (unknown version fails closed).
	SchemaVersion = 1

	// DefaultTTL is the admission lifetime applied when no WithTTL
	// option is supplied: 15 minutes.
	DefaultTTL = 15 * time.Minute

	// MaxTTL is the hard ceiling for the WithTTL option AND for any
	// sealed admission's issued→expiry span at verify. No minting
	// authority may ever produce a longer-lived admission, so a longer
	// span in a sealed claim is structurally invalid.
	MaxTTL = 24 * time.Hour

	// MaxClockSkew is the bounded clock-skew tolerance applied at verify
	// to the issued-at (future issuance) and expires-at (expiry) checks.
	// It is symmetric leeway: an admission issued at most MaxClockSkew
	// ahead of the verifier's clock is not rejected as future, and an
	// admission whose expiry is within MaxClockSkew of the verifier's
	// clock is not yet expired.
	MaxClockSkew = 5 * time.Minute

	// NonceSize is the crypto-random claim nonce size in bytes (128
	// bits). Every admission carries a fresh 128-bit nonce so two mints
	// of the identical tuple produce distinct sealed tokens.
	NonceSize = 16

	// MaxClaimStringBytes bounds every string claim field in bytes.
	// Mint rejects inputs over the bound; verify rejects sealed claims
	// over the bound.
	MaxClaimStringBytes = 512

	// MaxClaimStringRunes bounds every string claim field in runes.
	// Kept alongside the byte bound because UTF-8 multi-byte runes
	// defeat a byte-only ceiling's intent (a byte cap alone does not
	// bound cardinality of characters).
	MaxClaimStringRunes = 256

	// MaxClaimJSONBytes is the ceiling for the decoded claims plaintext
	// BEFORE strict decode. It is a defence-in-depth constant: the
	// per-field input bounds make it unreachable for any admission this
	// authority mints (the worst legal claim set is far smaller), so a
	// plaintext exceeding it can only be an authenticated-but-bloated
	// forgery from a compromised sealing key.
	MaxClaimJSONBytes = 12288

	// MaxTokenBytes is the ceiling for the opaque base64url token string
	// accepted at verify. Like MaxClaimJSONBytes it is unreachable for
	// tokens this authority mints (the sealed envelope of the largest
	// legal claim set stays well under it); it exists so a verify path
	// never allocates on an unbounded token.
	MaxTokenBytes = 16384

	// resourceScheme is the reserved URI scheme a render resource must
	// carry. It mirrors the host's `ui://` recognition for MCP App
	// documents; the check is kept local so this package stays
	// dependency-light and does not import the MCP driver.
	resourceScheme = "ui://"
)
