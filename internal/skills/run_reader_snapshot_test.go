package skills_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

type snapshotTestReader struct{}

type allowlistTestReader struct{}

func (allowlistTestReader) Get(context.Context, identity.Quadruple, string) (skills.Skill, error) {
	return skills.Skill{Name: "allowed"}, nil
}
func (allowlistTestReader) GetScope(context.Context, identity.Quadruple, string, skills.Scope) (skills.Skill, error) {
	return skills.Skill{Name: "allowed"}, nil
}
func (allowlistTestReader) List(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
	return []skills.Skill{{Name: "allowed"}, {Name: "blocked"}}, nil
}
func (allowlistTestReader) Search(context.Context, identity.Quadruple, string, int) ([]skills.RankedSkill, error) {
	return []skills.RankedSkill{{Skill: skills.Skill{Name: "allowed"}}, {Skill: skills.Skill{Name: "blocked"}}}, nil
}

func (snapshotTestReader) Get(context.Context, identity.Quadruple, string) (skills.Skill, error) {
	return skills.Skill{}, skills.ErrSkillNotFound
}
func (snapshotTestReader) GetScope(context.Context, identity.Quadruple, string, skills.Scope) (skills.Skill, error) {
	return skills.Skill{}, skills.ErrSkillNotFound
}
func (snapshotTestReader) List(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
	return nil, nil
}
func (snapshotTestReader) Search(context.Context, identity.Quadruple, string, int) ([]skills.RankedSkill, error) {
	return nil, nil
}

func TestRunSkillReaderSnapshot_ValidationAndIdentityBinding(t *testing.T) {
	t.Parallel()
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"},
		RunID:    "run",
	}
	reader := snapshotTestReader{}

	for name, build := range map[string]func() error{
		"missing identity": func() error {
			_, err := skills.NewRunSkillReaderSnapshot(identity.Quadruple{RunID: "run"}, "agent", reader)
			return err
		},
		"missing run": func() error {
			_, err := skills.NewRunSkillReaderSnapshot(identity.Quadruple{Identity: q.Identity}, "agent", reader)
			return err
		},
		"missing agent": func() error {
			_, err := skills.NewRunSkillReaderSnapshot(q, "", reader)
			return err
		},
		"missing reader": func() error {
			_, err := skills.NewRunSkillReaderSnapshot(q, "agent", nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := build(); !errors.Is(err, skills.ErrInvalidRunSkillReaderSnapshot) {
				t.Fatalf("error = %v, want ErrInvalidRunSkillReaderSnapshot", err)
			}
		})
	}

	snapshot, err := skills.NewRunSkillReaderSnapshot(q, "agent", reader)
	if err != nil {
		t.Fatalf("NewRunSkillReaderSnapshot: %v", err)
	}
	ctx := skills.WithRunSkillReaderSnapshot(context.Background(), snapshot)
	if _, err := skills.ResolveSkillReader(ctx, q, nil); err != nil {
		t.Fatalf("ResolveSkillReader matching q: %v", err)
	}
	mismatch := q
	mismatch.RunID = "other-run"
	if _, err := skills.ResolveSkillReader(ctx, mismatch, reader); !errors.Is(err, skills.ErrInvalidRunSkillReaderSnapshot) {
		t.Fatalf("ResolveSkillReader mismatched q error = %v, want ErrInvalidRunSkillReaderSnapshot", err)
	}
}

func TestAllowlistReader_EnforcesNonNilEmptyDenyAll(t *testing.T) {
	reader, err := skills.NewAllowlistReader(allowlistTestReader{}, []string{})
	if err != nil {
		t.Fatalf("NewAllowlistReader: %v", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"}
	if _, err := reader.Get(context.Background(), q, "allowed"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("Get error = %v, want ErrSkillNotFound", err)
	}
	listed, err := reader.List(context.Background(), q, skills.ListFilter{})
	if err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("List = %#v, err=%v, want non-nil empty result", listed, err)
	}
	searched, err := reader.Search(context.Background(), q, "query", 10)
	if err != nil || searched == nil || len(searched) != 0 {
		t.Fatalf("Search = %#v, err=%v, want non-nil empty result", searched, err)
	}
}

func TestRunSkillReaderSnapshot_OperatorTierProvenanceBinding(t *testing.T) {
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"},
		RunID:    "run",
	}
	snapshot, err := skills.NewRunSkillReaderSnapshot(q, "agent", snapshotTestReader{})
	if err != nil {
		t.Fatalf("NewRunSkillReaderSnapshot: %v", err)
	}
	if snapshot.HasOperatorTier() {
		t.Fatal("a bare snapshot must not claim operator-tier provenance")
	}
	if snapshot.BootPackSetHash() != "" || snapshot.OperatorTierHash() != "" {
		t.Fatalf("bare snapshot hashes = (%q, %q), want absent", snapshot.BootPackSetHash(), snapshot.OperatorTierHash())
	}
	if _, ok := snapshot.OperatorTierSource("alpha"); ok {
		t.Fatal("bare snapshot OperatorTierSource = present")
	}

	bootHash := "boot-set-hash-1"
	combinedHash := "operator-tier-hash-1"
	sources := map[string]skills.OperatorTierSource{
		"alpha": skills.OperatorTierSourceBoth,
		"beta":  skills.OperatorTierSourceBoot,
		"gamma": skills.OperatorTierSourceRevision,
	}
	bound := snapshot.WithOperatorTier(bootHash, combinedHash, sources)
	if !bound.HasOperatorTier() {
		t.Fatal("bound snapshot must report operator-tier provenance")
	}
	if bound.BootPackSetHash() != bootHash || bound.OperatorTierHash() != combinedHash {
		t.Fatalf("bound hashes = (%q, %q), want (%q, %q)", bound.BootPackSetHash(), bound.OperatorTierHash(), bootHash, combinedHash)
	}
	for name, want := range sources {
		got, ok := bound.OperatorTierSource(name)
		if !ok || got != want {
			t.Fatalf("OperatorTierSource(%q) = (%q, %v), want %q", name, got, ok, want)
		}
	}
	if got, ok := bound.OperatorTierSource("gamma "); !ok || got != skills.OperatorTierSourceRevision {
		t.Fatalf("canonical lookup = (%q, %v), want revision", got, ok)
	}
	if _, ok := bound.OperatorTierSource("missing"); ok {
		t.Fatal("OperatorTierSource(missing) = present")
	}

	// The identity gate is untouched: a mismatched quadruple still fails closed
	// even when operator-tier provenance is bound.
	ctx := skills.WithRunSkillReaderSnapshot(context.Background(), bound)
	if _, err := skills.ResolveSkillReader(ctx, q, nil); err != nil {
		t.Fatalf("ResolveSkillReader matching q with bound provenance: %v", err)
	}
	mismatch := q
	mismatch.RunID = "other-run"
	if _, err := skills.ResolveSkillReader(ctx, mismatch, snapshotTestReader{}); !errors.Is(err, skills.ErrInvalidRunSkillReaderSnapshot) {
		t.Fatalf("ResolveSkillReader mismatched q with bound provenance error = %v, want ErrInvalidRunSkillReaderSnapshot", err)
	}

	// The sources map is deep-copied: later caller mutation cannot alter the
	// snapshot.
	sources["alpha"] = skills.OperatorTierSourceBoot
	delete(sources, "beta")
	if got, ok := bound.OperatorTierSource("alpha"); !ok || got != skills.OperatorTierSourceBoth {
		t.Fatalf("input mutation reached the snapshot: alpha = (%q, %v)", got, ok)
	}
	if got, ok := bound.OperatorTierSource("beta"); !ok || got != skills.OperatorTierSourceBoot {
		t.Fatalf("deleted input key vanished from the snapshot: beta = (%q, %v)", got, ok)
	}

	// Binding is additive and value-safe: the original snapshot is untouched
	// and the original quadruple binding is preserved.
	if snapshot.HasOperatorTier() || bound.Quadruple() != q || bound.EffectiveAgentID() != "agent" {
		t.Fatal("WithOperatorTier mutated the receiver or dropped the binding")
	}
}
