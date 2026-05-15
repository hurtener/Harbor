package auth_test

import (
	"context"

	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// testNoopRedactor is the in-test pass-through audit.Redactor used by
// the auth package's test suite. It is deliberately defined in a
// *_test.go file so the production path of `auth.NewValidator` cannot
// resolve to a permissive stub — operator-facing seams demand a real
// Redactor at boot (CLAUDE.md §13 "Test stubs as production defaults
// on operator-facing seams", CLAUDE.md §7 rule 6).
//
// Tests that want to assert the redactor was *invoked* construct a
// real `audit/drivers/patterns.New()` instead; this stub is sufficient
// for the rejection-shape tests that only need a non-nil Redactor to
// build a Validator.
type testNoopRedactor struct{}

func (testNoopRedactor) Redact(_ context.Context, payload any) (any, error) {
	return payload, nil
}

// withTestRedactor is the canonical "give me a Validator option that
// supplies a Redactor" helper for the package's tests. New tests
// added under `package auth_test` should call this rather than
// reaching for `auth.WithRedactor(testNoopRedactor{})` directly.
func withTestRedactor() auth.Option {
	return auth.WithRedactor(testNoopRedactor{})
}
