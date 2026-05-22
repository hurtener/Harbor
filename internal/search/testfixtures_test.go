package search_test

import "context"

// allowAdmin is the always-true ScopeChecker used by tests that want
// to exercise cross-tenant paths without wiring the full Phase 61
// scope-injection ceremony.
func allowAdmin(_ context.Context) bool { return true }

// denyAdmin is the always-false ScopeChecker used by tests that pin
// the cross-tenant rejection path.
func denyAdmin(_ context.Context) bool { return false }
