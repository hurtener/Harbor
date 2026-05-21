package types

import "time"

// Phase 73m (D-129) — the `auth.rotate_token` wire types.
//
// `auth.rotate_token` is the ONE net-new Protocol method Phase 73m
// ships (the Console Settings page is otherwise a pure consumer of the
// 72f / 72g posture surfaces and the 72h Console DB). It rotates the
// operator's current Protocol-auth token: the Runtime re-mints a JWT
// for the caller's already-verified `(tenant, user, session)` identity
// and returns it once, one-time-reveal. The encrypted persistence — the
// operator re-saving the new token into the 72h `auth_profiles` table —
// is the Console's job, not the Runtime's.
//
// The method is ADMIN-gated: it requires the verified `auth.ScopeAdmin`
// claim on the caller's JWT (D-079 closed two-scope set — no new scope
// is minted). A request without the claim is rejected 403 with the
// canonical `CodeIdentityScopeRequired` Code. Every successful rotation
// emits a redacted `audit.admin_scope_used` event through the shipped
// audit.Redactor.
//
// Both types are flat, Protocol-owned structs (RFC §5.1) — never a
// re-export of an internal Runtime Go type.

// AuthRotateTokenRequest is the `auth.rotate_token` request body.
//
// Token rotation requires no body fields beyond the identity scope —
// the caller's identity is carried by the verified JWT, and the Runtime
// re-mints for exactly that identity. The Identity field is the
// standard flat IdentityScope every Protocol request carries; the
// Runtime asserts it against the verified JWT (defence-in-depth — a
// caller cannot present a valid token for one identity and a body
// claiming another).
type AuthRotateTokenRequest struct {
	// Identity is the caller's identity scope. The auth surface asserts
	// the triple against the verified JWT before re-minting.
	Identity IdentityScope `json:"identity"`
}

// AuthRotateTokenResponse is the `auth.rotate_token` response body — the
// re-minted token. It is one-time-revealed: the Console copies
// `NewToken` once into its encrypted 72h `auth_profiles` blob and never
// displays the raw token again.
type AuthRotateTokenResponse struct {
	// NewToken is the re-minted Protocol-auth JWT, Bearer-shaped. The
	// operator copies this exactly once.
	NewToken string `json:"new_token"`
	// ExpiresAt is the new token's expiry, UTC.
	ExpiresAt time.Time `json:"expires_at"`
}
