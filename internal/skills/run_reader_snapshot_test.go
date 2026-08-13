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
