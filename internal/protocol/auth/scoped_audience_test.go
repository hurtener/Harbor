package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
)

func TestNewValidator_ScopedAudienceRequiresDistinctLegacyAudience(t *testing.T) {
	_, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("RS256", pub)
	for _, tc := range []struct {
		legacy string
		scoped string
	}{
		{"", scopedTestAudience},
		{" ", scopedTestAudience},
		{scopedTestAudience, scopedTestAudience},
		{legacyTestAudience, " "},
		{legacyTestAudience, "scoped\nvalue"},
	} {
		if _, err := auth.NewValidator(keys, withTestRedactor(), auth.WithAudience(tc.legacy), auth.WithScopedTokenAudience(tc.scoped)); !errors.Is(err, auth.ErrMisconfigured) {
			t.Fatalf("legacy=%q scoped=%q: %v", tc.legacy, tc.scoped, err)
		}
	}
}

func TestValidator_ScopedAudience_ExactSingletonAndExplicitOptIn(t *testing.T) {
	v, priv := newScopedRSValidator(t)
	legacy, _ := newRSValidator(t, fixedNow)
	for _, tc := range []struct {
		name string
		aud  any
		good bool
	}{
		{"scalar", scopedTestAudience, true},
		{"singleton", []string{scopedTestAudience}, true},
		{"null", nil, false},
		{"empty", []string{}, false},
		{"legacy", legacyTestAudience, false},
		{"both audiences", []string{scopedTestAudience, legacyTestAudience}, false},
		{"duplicate scoped audience", []string{scopedTestAudience, scopedTestAudience}, false},
		{"extra unknown audience", []string{scopedTestAudience, "other"}, false},
		{"number", 1, false},
		{"object", map[string]string{"aud": scopedTestAudience}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims(fixedNow)
			claims[auth.MethodReachClaim] = []string{string(methods.MethodStart)}
			claims["aud"] = tc.aud
			token := signRS256(t, priv, claims, "k1")
			_, err := v.Validate(context.Background(), token)
			if tc.good && err != nil || !tc.good && !errors.Is(err, auth.ErrAudienceMismatch) {
				t.Fatalf("audience validation = %v, good %v", err, tc.good)
			}
			// A validator with no explicit scoped-audience configuration
			// rejects all restricted tokens, even when legacy aud is optional.
			if _, err := legacy.Validate(context.Background(), token); !errors.Is(err, auth.ErrAudienceMismatch) {
				t.Fatalf("restricted token accepted without opt-in: %v", err)
			}
		})
	}
	for _, claimName := range []string{auth.SessionAdmissionEpochClaim, auth.SessionAdmissionCoordinatorClaim} {
		claims := validClaims(fixedNow)
		claims[claimName] = nil
		claims["aud"] = legacyTestAudience
		if _, err := v.Validate(context.Background(), signRS256(t, priv, claims, "k1")); !errors.Is(err, auth.ErrAudienceMismatch) {
			t.Fatalf("partial admission claim %q escaped scoped audience: %v", claimName, err)
		}
	}
}

func TestValidator_ScopedTokenRejectedByLegacyAudienceEnforcement(t *testing.T) {
	v, priv := newScopedRSValidator(t)
	claims := admissionClaims()
	token := signRS256(t, priv, claims, "k1")
	if _, err := v.Validate(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	// This verifier knows nothing about Harbor's new restriction claims: it
	// represents the existing asymmetric signature/time/legacy-audience
	// posture, with precisely the same public key and token. Dedicated aud,
	// rather than claim presence or JWT typ, is why it rejects the token.
	legacyParser := jwt.NewParser(jwt.WithValidMethods(auth.AllowedAlgorithms),
		jwt.WithAudience(legacyTestAudience), jwt.WithTimeFunc(func() time.Time { return fixedNow }))
	keyfunc := func(_ *jwt.Token) (any, error) { return &priv.PublicKey, nil }
	if _, err := legacyParser.ParseWithClaims(token, jwt.MapClaims{}, keyfunc); !errors.Is(err, jwt.ErrTokenInvalidAudience) {
		t.Fatalf("legacy audience verifier accepted restricted token: %v", err)
	}
	// The rollout prerequisite is essential: an old audience-less verifier
	// accepts that exact token while silently ignoring all new authority.
	unsafeParser := jwt.NewParser(jwt.WithValidMethods(auth.AllowedAlgorithms),
		jwt.WithTimeFunc(func() time.Time { return fixedNow }))
	if _, err := unsafeParser.ParseWithClaims(token, jwt.MapClaims{}, keyfunc); err != nil {
		t.Fatalf("fixture must expose audience-less legacy hazard: %v", err)
	}
	broad := validClaims(fixedNow)
	broad["aud"] = legacyTestAudience
	broadToken := signRS256(t, priv, broad, "k1")
	if _, err := legacyParser.ParseWithClaims(broadToken, jwt.MapClaims{}, keyfunc); err != nil {
		t.Fatalf("legacy broad token rejected: %v", err)
	}
	if _, err := v.Validate(context.Background(), broadToken); err != nil {
		t.Fatalf("new validator must preserve broad token: %v", err)
	}
}
