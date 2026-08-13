package localdb_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
)

func testSkill(name string) skills.Skill {
	return skills.Skill{
		Name: name, Trigger: "trigger:" + name, Steps: []string{"step"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeUser,
	}
}

func TestSkillStore_AgentBinding_SameNameRemainsSelected(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	first := testSkill("shared-name")
	first.AgentID = "agent-a"
	second := testSkill("shared-name")
	second.AgentID = "agent-b"
	if err := store.Upsert(ctx, fixtureID, first); err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	if err := store.Upsert(ctx, fixtureID, second); err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	rows, err := store.List(ctx, fixtureID, skills.ListFilter{AgentID: "agent-a"})
	if err != nil {
		t.Fatalf("list agent-a: %v", err)
	}
	if len(rows) != 1 || rows[0].AgentID != "agent-a" {
		t.Fatalf("agent-a selection = %#v", rows)
	}
	rows, err = store.List(ctx, fixtureID, skills.ListFilter{AgentID: "agent-b"})
	if err != nil {
		t.Fatalf("list agent-b: %v", err)
	}
	if len(rows) != 1 || rows[0].AgentID != "agent-b" {
		t.Fatalf("agent-b selection = %#v", rows)
	}
}
