// cmd/harbor/tokenclaims.go — the shared Harbor JWT claim shape.
//
// Two CLI signers emit Harbor JWTs: the in-memory ephemeral signer that
// `harbor dev` mounts for first-clone convenience (devauth.go), and the
// operator-managed, persistable signer behind `harbor token` for the
// bring-your-own-issuer on-ramp (token_crypto.go). They share NOTHING
// except this file: the canonical claim set the Protocol's JWT verifier
// (internal/protocol/auth) checks at the transport edge. Keeping that
// shape in one place is what stops the two signers from drifting — a
// change to the claim contract (a new mandatory claim, a renamed key)
// lands here once and both signers inherit it.
//
// The claim set maps onto identity.Identity by name:
//
//   - tenant / user / session — the mandatory isolation triple; the
//     verifier rejects any token missing one of them.
//   - iss / aud — matched against the operator's configured issuer /
//     audience; a mismatch is a hard rejection.
//   - exp — mandatory (a token with no expiry is treated as expired).
//   - nbf / iat — stamped for completeness.
//   - sub — the subject, audited; set to the user component.
//   - scopes — optional; an empty scope set is authenticated but
//     entitled to nothing elevated (least privilege).

package main

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// harborClaims builds the canonical Harbor JWT claim set both CLI
// signers stamp. The issuer / audience / ttl / scopes are parameters,
// not hardcoded constants, so the operator-managed signer can mint a
// token whose iss / aud match the operator's identity config (the dev
// signer passes its own fixed defaults). An empty scopes slice omits the
// `scopes` claim entirely — the least-privilege default.
//
// The token is signed by the caller (the signing key and algorithm
// differ between the two signers); this function shapes only the claims.
func harborClaims(now time.Time, ttl time.Duration, issuer, audience, tenant, user, session string, scopes []string) jwt.MapClaims {
	claims := jwt.MapClaims{
		"iss":     issuer,
		"sub":     user,
		"aud":     audience,
		"exp":     now.Add(ttl).Unix(),
		"nbf":     now.Add(-1 * time.Minute).Unix(),
		"iat":     now.Unix(),
		"tenant":  tenant,
		"user":    user,
		"session": session,
	}
	if len(scopes) > 0 {
		claims["scopes"] = scopes
	}
	return claims
}
