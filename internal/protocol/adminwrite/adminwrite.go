// Package adminwrite holds the ONE fail-closed admin-plane write+audit
// posture shared by every admin-scoped Protocol write that must not leave an
// un-auditable mutation observably applied (CLAUDE.md §5, §7 rule 6).
//
// It exists as its own leaf package so both `internal/protocol` (the MCP admin
// verbs) and `internal/runtime/agentcfg/protocol` (the agent-config admin
// verbs, e.g. the discovery-allowance write) reuse the SAME implementation
// rather than each re-deriving the emit/compensate ordering — the correction
// that established one canonical posture instead of the earlier apply-then-emit
// lie.
package adminwrite

import (
	stderrors "errors"
	"fmt"
)

// Apply runs an admin-plane mutation and its audit emit as a single
// fail-closed unit: it applies the mutation, then emits the audit event; if the
// emit fails it COMPENSATES by reverting the mutation, so an un-auditable admin
// write is never left observably applied (CLAUDE.md §5, §7 rule 6). It is the
// ONE admin-write audit posture the MCP admin verbs and the agent-config admin
// writes share.
//
// apply returns a revert closure (restoring the pre-write state) and an apply
// error. The two returned errors are disjoint: a non-nil applyErr means the
// mutation never happened (map it through the caller's domain error mapper); a
// non-nil emitErr means the mutation was applied then reverted (surface it as
// an audit failure). When the compensating revert itself fails, the emitErr
// wraps both so the operator sees the state may be inconsistent.
func Apply(apply func() (revert func() error, err error), emit func() error) (applyErr, emitErr error) {
	revert, err := apply()
	if err != nil {
		return err, nil
	}
	if e := emit(); e != nil {
		if revert != nil {
			if rerr := revert(); rerr != nil {
				return nil, fmt.Errorf("audit emit failed AND compensating revert failed (state may be inconsistent): %w",
					stderrors.Join(e, rerr))
			}
		}
		return nil, fmt.Errorf("audit emit failed (mutation reverted, not applied): %w", e)
	}
	return nil, nil
}
