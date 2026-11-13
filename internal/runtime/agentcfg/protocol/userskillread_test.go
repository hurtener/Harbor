package protocol_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
)

func TestUserSkillRead_DocumentManifestAndSupportFiles(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	md := "---\nname: demo-skill\ndescription: 'Reference guide'\n---\n# Overview\nRead [reference](docs/reference.md) and ![image](assets/icon.svg).\n"
	entries := map[string]string{
		"SKILL.md":          md,
		"docs/reference.md": "# Reference\nContent that survives installation.\n",
		"assets/icon.svg":   "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"1\" height=\"1\"></svg>",
	}
	review, _, err := fx.validateCommit(t, importUserA, entries, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	before := skillExportMutationCounts(fx)
	got, err := fx.svc.Read(ctx, prototypes.AgentConfigUserSkillsReadRequest{AgentID: testAgentID, Name: "Demo-Skill"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Name != "demo-skill" || got.PackageHash != review.PackageHash || !strings.Contains(got.Document, "# Overview") || !strings.Contains(got.Document, "docs/reference.md") {
		t.Fatalf("read=%+v", got)
	}
	if got.Skill.Scope != "user" || got.Skill.OriginRef != "" {
		t.Fatalf("skill projection=%+v", got.Skill)
	}
	for key := range got.Skill.Extra {
		if strings.HasPrefix(key, "_") {
			t.Fatalf("private extra key %q leaked", key)
		}
	}
	if len(got.SupportFiles) != 2 || got.SupportFile != nil {
		t.Fatalf("manifest=%+v file=%+v", got.SupportFiles, got.SupportFile)
	}
	if skillExportMutationCounts(fx) != before {
		t.Fatal("read mutated state")
	}

	ref, err := fx.svc.Read(ctx, prototypes.AgentConfigUserSkillsReadRequest{AgentID: testAgentID, Name: "demo-skill", SupportPath: "docs/reference.md"})
	if err != nil || ref.SupportFile == nil || ref.SupportFile.Binary || !strings.Contains(ref.SupportFile.Text, "survives installation") {
		t.Fatalf("support read=%+v err=%v", ref.SupportFile, err)
	}
	svg, err := fx.svc.Read(ctx, prototypes.AgentConfigUserSkillsReadRequest{AgentID: testAgentID, Name: "demo-skill", SupportPath: "assets/icon.svg"})
	if err != nil || svg.SupportFile == nil || !svg.SupportFile.Binary || svg.SupportFile.Text != "" {
		t.Fatalf("binary support=%+v err=%v", svg.SupportFile, err)
	}
	if _, err := fx.svc.Read(ctx, prototypes.AgentConfigUserSkillsReadRequest{AgentID: testAgentID, Name: "demo-skill", SupportPath: "../SKILL.md"}); !errors.Is(err, agentcfgprotocol.ErrUserSkillSupportFileNotFound) {
		t.Fatalf("unknown path err=%v", err)
	}

	// Another user in the same tenant cannot read it, and a forged wire
	// identity is refused.
	if _, err := fx.svc.Read(importCtx(identity.Identity{TenantID: "t", UserID: "other-user", SessionID: "s2"}, testAgentID), prototypes.AgentConfigUserSkillsReadRequest{AgentID: testAgentID, Name: "demo-skill"}); err == nil {
		t.Fatal("other user read succeeded")
	}
	if _, err := fx.svc.Read(ctx, prototypes.AgentConfigUserSkillsReadRequest{Identity: prototypes.IdentityScope{Tenant: "other"}, AgentID: testAgentID, Name: "demo-skill"}); err == nil {
		t.Fatal("forged identity read succeeded")
	}

	snap, err := fx.svc.SnapshotPersonalSkill(ctx, testAgentID, "demo-skill")
	if err != nil || snap.SupportFiles != 2 || snap.PackageHash != review.PackageHash || snap.Skill.Name != "demo-skill" {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	if _, err := fx.svc.SnapshotPersonalSkill(ctx, testAgentID, "absent"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("absent snapshot err=%v", err)
	}
}
