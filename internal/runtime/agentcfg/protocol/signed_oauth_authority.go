package protocol

import (
	"crypto"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/agentcfg"
)

// SignedOAuthMCPKeySet resolves a JWT kid only from a boot-configured trust
// anchor. It is deliberately the narrow subset of the Protocol auth key-set
// seam so the registration service never discovers a verifier from the wire.
type SignedOAuthMCPKeySet interface {
	KeyByID(kid string) (crypto.PublicKey, string, error)
}

// SignedOAuthMCPCapabilityAuthority is one immutable boot-declared D-401
// trust anchor. Broker is the request-visible selector, but all authority is
// fixed by this construction-time value.
type SignedOAuthMCPCapabilityAuthority struct {
	Broker               string
	Issuer               string
	Keys                 SignedOAuthMCPKeySet
	ScopeCeiling         []string
	MaxAuthorityLifetime time.Duration
}

// Verify validates a signed envelope against this fixed trust anchor. The
// unverified parse reads only kid/alg to select a key from the already-pinned
// KeySet; all claims and the signature are then verified by agentcfg's
// asymmetric, exact-binding verifier.
func (a SignedOAuthMCPCapabilityAuthority) Verify(raw string, now time.Time, binding agentcfg.SignedOAuthMCPBinding) (agentcfg.SignedOAuthMCPAuthorityClaims, error) {
	if strings.TrimSpace(a.Broker) == "" || strings.TrimSpace(a.Issuer) == "" || a.Keys == nil || a.MaxAuthorityLifetime <= 0 {
		return agentcfg.SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: boot signed capability authority incomplete", agentcfg.ErrSignedCapabilityAuthority)
	}
	if binding.Broker != a.Broker {
		return agentcfg.SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: request broker %q does not select trust anchor %q", agentcfg.ErrSignedCapabilityBinding, binding.Broker, a.Broker)
	}
	token, _, err := jwt.NewParser().ParseUnverified(raw, jwt.MapClaims{})
	if err != nil || token == nil {
		return agentcfg.SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: malformed authority envelope", agentcfg.ErrSignedCapabilityAuthority)
	}
	kid, ok := token.Header["kid"].(string)
	if !ok || strings.TrimSpace(kid) == "" {
		return agentcfg.SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: authority envelope has no key id", agentcfg.ErrSignedCapabilityAuthority)
	}
	alg, ok := token.Header["alg"].(string)
	if !ok || strings.TrimSpace(alg) == "" {
		return agentcfg.SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: authority envelope has no algorithm", agentcfg.ErrSignedCapabilityAuthority)
	}
	key, trustedAlg, err := a.Keys.KeyByID(kid)
	if err != nil || key == nil {
		return agentcfg.SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: resolve configured key: %w", agentcfg.ErrSignedCapabilityAuthority, err)
	}
	if alg != trustedAlg {
		return agentcfg.SignedOAuthMCPAuthorityClaims{}, fmt.Errorf("%w: authority algorithm does not match configured key", agentcfg.ErrSignedCapabilityAuthority)
	}
	return agentcfg.VerifySignedOAuthMCPAuthorityBounded(raw, a.Issuer, kid, key, now, binding, a.ScopeCeiling, a.MaxAuthorityLifetime)
}
