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

// OperatorTierSource is the provenance of one item of the effective operator
// tier bound to a run skill-reader snapshot: the strict merge of the
// boot-declared operator skill baseline and the selected agent's active
// durable operator-pack revision (HA-66).
type OperatorTierSource string

// Operator-tier provenance markers. The marker is a pure function of the
// strict merge: an item present in only one source keeps that source, and an
// item present in BOTH sources with an identical canonical semantic hash
// dedupes to one item marked both.
const (
	// OperatorTierSourceBoot — the item exists only in the boot-declared
	// baseline.
	OperatorTierSourceBoot OperatorTierSource = "boot"
	// OperatorTierSourceRevision — the item exists only in the active
	// durable operator-pack revision.
	OperatorTierSourceRevision OperatorTierSource = "revision"
	// OperatorTierSourceBoth — the item exists in both sources with an
	// identical canonical attachment-free semantic content hash and
	// deduped to one combined item.
	OperatorTierSourceBoth OperatorTierSource = "both"
)

// RunSkillReaderSnapshot is the immutable run-start binding between an
// already-selected effective agent, the run identity, and its read-only skill
// projection. Agent selection happens before construction; this type does not
// infer authority or selection from tool-invocation provenance.
//
// The value is safe to copy. The bound SkillReader must satisfy SkillReader's
// concurrent-reuse contract and must itself expose an immutable view for the
// lifetime of the run.
//
// The snapshot optionally carries the effective operator-tier provenance
// captured at run start: the deterministic boot baseline set hash, the
// combined operator-tier set hash, and one source marker per canonical
// operator-tier name. These are additive read-only metadata — they never
// alter the identity gate, which still fails closed for any quadruple other
// than the bound one.
type RunSkillReaderSnapshot struct {
	q                identity.Quadruple
	effectiveAgentID string
	reader           SkillReader
	bootPackSetHash  string
	combinedHash     string
	sources          map[string]OperatorTierSource
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

// WithOperatorTier returns a copy of the snapshot bound to the effective
// operator-tier provenance captured at run start: the deterministic boot
// baseline set hash ("" when no boot baseline was declared), the combined
// operator-tier set hash ("" when the tier is empty), and one source marker
// per canonical operator-tier name. The identity gate is untouched — the
// returned snapshot still fails closed for any quadruple other than the bound
// one. The sources map is deep-copied, so later caller mutation cannot alter
// the snapshot; callers must key it by canonical (lowercase, trimmed) name.
func (s RunSkillReaderSnapshot) WithOperatorTier(bootPackSetHash, combinedHash string, sources map[string]OperatorTierSource) RunSkillReaderSnapshot {
	next := s
	next.bootPackSetHash = bootPackSetHash
	next.combinedHash = combinedHash
	if len(sources) > 0 {
		copied := make(map[string]OperatorTierSource, len(sources))
		for name, source := range sources {
			copied[strings.ToLower(strings.TrimSpace(name))] = source
		}
		next.sources = copied
	}
	return next
}

// HasOperatorTier reports whether operator-tier provenance was bound to this
// snapshot.
func (s RunSkillReaderSnapshot) HasOperatorTier() bool {
	return s.bootPackSetHash != "" || s.combinedHash != "" || s.sources != nil
}

// BootPackSetHash returns the deterministic set hash over the normalized
// boot-declared baseline entries bound at run start ("" when no boot baseline
// was declared for the effective agent).
func (s RunSkillReaderSnapshot) BootPackSetHash() string { return s.bootPackSetHash }

// OperatorTierHash returns the deterministic set hash over the combined
// operator-tier items (boot baseline + active durable revision after strict
// dedup) bound at run start ("" when no operator tier was bound).
func (s RunSkillReaderSnapshot) OperatorTierHash() string { return s.combinedHash }

// OperatorTierSource returns the provenance marker of the canonical
// operator-tier name. The bool is false when the name is not an
// operator-tier item.
func (s RunSkillReaderSnapshot) OperatorTierSource(name string) (OperatorTierSource, bool) {
	source, ok := s.sources[strings.ToLower(strings.TrimSpace(name))]
	return source, ok
}

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
