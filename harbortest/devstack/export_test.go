// Internal-package surface exposed only to the unit tests. The
// error-returning core of Assemble is package-private to keep the
// public surface tight; the export file is `_test.go` so the alias
// never reaches a production build.
package devstack

import "github.com/hurtener/Harbor/internal/config"

// TryAssemble re-exports the package-private tryAssemble so unit
// tests can drive the error paths directly. The contract matches
// the production Assemble: a non-nil error leaves the returned
// DevStack with whatever subsystems had already opened — the
// caller's deferred Close drains them cleanly.
//
// Phase 110c (D-196): the former PlannerConfigFromConfig re-export is
// gone — the package-private projection it aliased was deleted when
// cmd + devstack converged on the exported `planner.ConfigFromOperator`.
// Its parity gate now lives at the owning package
// (`internal/planner/from_config_test.go::TestConfigFromOperator_FieldParity`),
// which is strictly stronger: reflection over EVERY config field, not
// a hand-listed golden.
func TryAssemble(cfg *config.Config, opts AssembleOpts) (*DevStack, error) {
	return tryAssemble(cfg, opts)
}
