package skills

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
)

// AllowlistReader is an immutable run-bound SkillReader projection. A non-nil
// allowlist is authoritative: an empty list denies every skill. It is applied
// at the reader boundary so all skill discovery tools share the same gate.
type AllowlistReader struct {
	inner   SkillReader
	allowed map[string]struct{}
}

// NewAllowlistReader binds a reader to names. The names slice is copied and a
// non-nil empty slice remains deny-all.
func NewAllowlistReader(inner SkillReader, names []string) (SkillReader, error) {
	if inner == nil {
		return nil, fmt.Errorf("%w: reader is nil", ErrInvalidRunSkillReaderSnapshot)
	}
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			allowed[name] = struct{}{}
		}
	}
	return AllowlistReader{inner: inner, allowed: allowed}, nil
}

func (r AllowlistReader) permits(name string) bool {
	_, ok := r.allowed[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func (r AllowlistReader) Get(ctx context.Context, id identity.Quadruple, name string) (Skill, error) {
	if !r.permits(name) {
		return Skill{}, ErrSkillNotFound
	}
	return r.inner.Get(ctx, id, name)
}

func (r AllowlistReader) GetScope(ctx context.Context, id identity.Quadruple, name string, scope Scope) (Skill, error) {
	if !r.permits(name) {
		return Skill{}, ErrSkillNotFound
	}
	return r.inner.GetScope(ctx, id, name, scope)
}

func (r AllowlistReader) List(ctx context.Context, id identity.Quadruple, filter ListFilter) ([]Skill, error) {
	items, err := r.inner.List(ctx, id, filter)
	if err != nil {
		return nil, err
	}
	out := make([]Skill, 0, len(items))
	for _, item := range items {
		if r.permits(item.Name) {
			out = append(out, item)
		}
	}
	return out, nil
}

func (r AllowlistReader) Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]RankedSkill, error) {
	items, err := r.inner.Search(ctx, id, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]RankedSkill, 0, len(items))
	for _, item := range items {
		if r.permits(item.Skill.Name) {
			out = append(out, item)
		}
	}
	return out, nil
}

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
