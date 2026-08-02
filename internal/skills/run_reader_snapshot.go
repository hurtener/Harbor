package skills

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
)

// ErrInvalidRunSkillReaderSnapshot reports an incomplete or mismatched
// run-scoped skill-reader binding. The binding fails closed: a reader selected
// for one effective agent or identity quadruple is never reused for another.
var ErrInvalidRunSkillReaderSnapshot = errors.New("skills: invalid run skill-reader snapshot")

// RunSkillReaderSnapshot is the immutable run-start binding between an
// already-selected effective agent, the run identity, and its read-only skill
// projection. Agent selection happens before construction; this type does not
// infer authority or selection from tool-invocation provenance.
//
// The value is safe to copy. The bound SkillReader must satisfy SkillReader's
// concurrent-reuse contract and must itself expose an immutable view for the
// lifetime of the run.
type RunSkillReaderSnapshot struct {
	q                identity.Quadruple
	effectiveAgentID string
	reader           SkillReader
}

// NewRunSkillReaderSnapshot validates and binds the effective agent, complete
// run quadruple, and read-only view selected at run start.
func NewRunSkillReaderSnapshot(
	q identity.Quadruple,
	effectiveAgentID string,
	reader SkillReader,
) (RunSkillReaderSnapshot, error) {
	if err := identity.Validate(q.Identity); err != nil {
		return RunSkillReaderSnapshot{}, fmt.Errorf("%w: identity: %w", ErrInvalidRunSkillReaderSnapshot, err)
	}
	if strings.TrimSpace(q.RunID) == "" {
		return RunSkillReaderSnapshot{}, fmt.Errorf("%w: run ID is required", ErrInvalidRunSkillReaderSnapshot)
	}
	if strings.TrimSpace(effectiveAgentID) == "" {
		return RunSkillReaderSnapshot{}, fmt.Errorf("%w: effective agent ID is required", ErrInvalidRunSkillReaderSnapshot)
	}
	if reader == nil {
		return RunSkillReaderSnapshot{}, fmt.Errorf("%w: reader is nil", ErrInvalidRunSkillReaderSnapshot)
	}
	return RunSkillReaderSnapshot{q: q, effectiveAgentID: effectiveAgentID, reader: reader}, nil
}

// EffectiveAgentID returns the agent selected before this snapshot was built.
func (s RunSkillReaderSnapshot) EffectiveAgentID() string { return s.effectiveAgentID }

// Quadruple returns the exact run identity bound to this snapshot.
func (s RunSkillReaderSnapshot) Quadruple() identity.Quadruple { return s.q }

type runSkillReaderSnapshotContextKey struct{}

// WithRunSkillReaderSnapshot attaches a validated immutable run-start reader
// binding. It does not alter or derive the context identity.
func WithRunSkillReaderSnapshot(ctx context.Context, snapshot RunSkillReaderSnapshot) context.Context {
	return context.WithValue(ctx, runSkillReaderSnapshotContextKey{}, snapshot)
}

// ResolveSkillReader returns the run-bound reader when present, otherwise the
// caller's boot-time fallback. A snapshot whose quadruple does not exactly
// match q fails closed instead of falling back to a wider catalog.
func ResolveSkillReader(ctx context.Context, q identity.Quadruple, fallback SkillReader) (SkillReader, error) {
	snapshot, ok := ctx.Value(runSkillReaderSnapshotContextKey{}).(RunSkillReaderSnapshot)
	if !ok {
		if fallback == nil {
			return nil, fmt.Errorf("%w: no run snapshot or fallback reader", ErrInvalidRunSkillReaderSnapshot)
		}
		return fallback, nil
	}
	if snapshot.reader == nil || snapshot.q != q {
		return nil, fmt.Errorf("%w: snapshot identity does not match caller", ErrInvalidRunSkillReaderSnapshot)
	}
	return snapshot.reader, nil
}
