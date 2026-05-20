package search_test

import (
	"context"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/search"
)

// allowAdmin is the always-true ScopeChecker used by tests that want
// to exercise cross-tenant paths without wiring the full Phase 61
// scope-injection ceremony.
func allowAdmin(_ context.Context) bool { return true }

// denyAdmin is the always-false ScopeChecker used by tests that pin
// the cross-tenant rejection path.
func denyAdmin(_ context.Context) bool { return false }

// testDeps returns a Deps with the production patterns redactor and
// the supplied ScopeChecker. The patterns redactor is the real
// production driver per the §13 "no stub-default" rule — even in
// tests we exercise the same code path.
func testDeps(adminScope search.ScopeChecker) search.Deps {
	return search.Deps{
		Redactor:   patterns.New(),
		AdminScope: adminScope,
	}
}
