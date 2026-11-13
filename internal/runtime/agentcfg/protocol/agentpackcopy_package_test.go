package protocol_test

// agentpackcopy_package_test.go — a same-runtime pack copy carries
// package-backed items: the target gets the exact PackageHash and its own
// installed package, a support-file-only change propagates, and deselecting
// the item releases the target's package. A missing source package is
// refused before anything is published.

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
)

const copyTargetAgentID = "agent-copy"

func copyCtx(id identity.Identity) context.Context {
	ctx, _ := identity.WithVerified(context.Background(), id)
	return ctx
}

func (fx *packImportFixture) copyPacks(t *testing.T, key string, packIDs []string) (agentcfgprotocol.AgentPackCopyResult, error) {
	t.Helper()
	ctx := copyCtx(importUserA)
	source, err := fx.admin.InspectEffective(ctx, testAgentID)
	if err != nil {
		t.Fatalf("inspect source: %v", err)
	}
	target, err := fx.admin.InspectEffective(ctx, copyTargetAgentID)
	if err != nil {
		t.Fatalf("inspect target: %v", err)
	}
	return fx.admin.CopySelected(ctx, agentcfgprotocol.AgentPackCopyRequest{
		SourceAgentID: testAgentID, TargetAgentID: copyTargetAgentID, PackIDs: packIDs,
		ExpectedSourceCompositionHash: source.CompositionHash,
		ExpectedTargetCompositionHash: target.CompositionHash,
		IdempotencyKey:                key,
	})
}

// seedCopyTarget gives the target one independently authored item, so it has
// a revision and a non-empty composition to anchor the copy CAS on.
func (fx *packImportFixture) seedCopyTarget(t *testing.T) {
	t.Helper()
	keep := skills.AgentPackItem{Name: "keep", Title: "keep", Trigger: "trigger-keep", Steps: []string{"independent"}, OriginRef: "independent-target"}
	if _, err := fx.reg.SetRevision(context.Background(), importQuad(importUserA), copyTargetAgentID, agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{AgentPacks: []skills.AgentPackItem{keep}}, agentcfg.SetOptions{ExpectedContentHash: agentcfg.ExpectNoActiveRevision}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
}

func (fx *packImportFixture) targetPackage(t *testing.T) (skills.InstalledPackage, bool) {
	t.Helper()
	q, err := agentcfg.AgentScope("t", copyTargetAgentID)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := fx.store.GetInstalledPackage(context.Background(), q, copyTargetAgentID, "shared-guide")
	if errors.Is(err, skills.ErrInstalledPackageNotFound) {
		return skills.InstalledPackage{}, false
	}
	if err != nil {
		t.Fatalf("GetInstalledPackage: %v", err)
	}
	return pkg, true
}

func TestAgentPackCopy_PackageBackedItemTravelsWithItsPackage(t *testing.T) {
	fx := newPackImportFixture(t)
	importCtxA := importCtx(importUserA, testAgentID)
	v1 := fx.validate(t, importUserA, packEntries("guide v1"))
	if _, err := fx.admin.AgentPacksImportCommit(importCtxA, packCommitReq(importUserA, testAgentID, v1, false)); err != nil {
		t.Fatalf("import v1: %v", err)
	}
	// A body-only target revision so the copy has a CAS anchor.
	fx.seedCopyTarget(t)

	first, err := fx.copyPacks(t, "copy-1", []string{"shared-guide"})
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if len(first.Outcomes) != 1 || first.Outcomes[0].Outcome != agentcfgprotocol.AgentPackCopyOutcomeCopied {
		t.Fatalf("outcomes=%+v", first.Outcomes)
	}
	pkg, ok := fx.targetPackage(t)
	if !ok || pkg.PackageHash != v1.PackageHash || pkg.Skill.AgentID != copyTargetAgentID {
		t.Fatalf("target package=%+v ok=%v", pkg, ok)
	}
	other := identity.Identity{TenantID: "t", UserID: "u2", SessionID: "s2"}
	f, err := fx.svc.ResolveAgentPackSupport(importCtx(other, copyTargetAgentID), copyTargetAgentID, "shared-guide", guideURI(t, v1.PackageHash))
	if err != nil || string(f.Data) != "guide v1" {
		t.Fatalf("target support resolve: %q err=%v", f.Data, err)
	}
	// Copying the same package again is a no-op that leaves it in place.
	if again, err := fx.copyPacks(t, "copy-1b", []string{"shared-guide"}); err != nil || again.Changed || again.Outcomes[0].Outcome != agentcfgprotocol.AgentPackCopyOutcomeNoop {
		t.Fatalf("repeat copy=%+v err=%v", again, err)
	}
	if pkg, ok := fx.targetPackage(t); !ok || pkg.PackageHash != v1.PackageHash {
		t.Fatalf("repeat copy moved the target package: %+v", pkg)
	}

	// A support-file-only change (same SKILL.md) is a real change.
	v2 := fx.validate(t, importUserA, packEntries("guide v2"))
	if _, err := fx.admin.AgentPacksImportCommit(importCtxA, packCommitReq(importUserA, testAgentID, v2, true)); err != nil {
		t.Fatalf("import v2: %v", err)
	}
	second, err := fx.copyPacks(t, "copy-2", []string{"shared-guide"})
	if err != nil {
		t.Fatalf("copy v2: %v", err)
	}
	if second.Outcomes[0].Outcome != agentcfgprotocol.AgentPackCopyOutcomeCopied {
		t.Fatalf("support-only change reported %+v", second.Outcomes)
	}
	if pkg, ok := fx.targetPackage(t); !ok || pkg.PackageHash != v2.PackageHash {
		t.Fatalf("target package after v2=%s", pkg.PackageHash)
	}
	f, err = fx.svc.ResolveAgentPackSupport(importCtx(other, copyTargetAgentID), copyTargetAgentID, "shared-guide", guideURI(t, v2.PackageHash))
	if err != nil || string(f.Data) != "guide v2" {
		t.Fatalf("target v2 support: %q err=%v", f.Data, err)
	}
	if _, err := fx.svc.ResolveAgentPackSupport(importCtx(other, copyTargetAgentID), copyTargetAgentID, "shared-guide", guideURI(t, v1.PackageHash)); !errors.Is(err, skills.ErrSupportNotFound) {
		t.Fatalf("v1 support still resolves on the target: %v", err)
	}

	// Deselecting the copied item removes it and releases its package.
	if _, err := fx.copyPacks(t, "copy-3", []string{}); err != nil {
		t.Fatalf("deselect copy: %v", err)
	}
	if _, ok := fx.targetPackage(t); ok {
		t.Fatalf("deselected item left its package on the target")
	}
	// The source keeps its own package.
	if pkg, ok := fx.installed(t); !ok || pkg.PackageHash != v2.PackageHash {
		t.Fatalf("source package changed: %+v", pkg)
	}
}

func TestAgentPackCopy_PackageBackedSourceWithoutPackageIsRefused(t *testing.T) {
	fx := newPackImportFixture(t)
	importCtxA := importCtx(importUserA, testAgentID)
	v1 := fx.validate(t, importUserA, packEntries("guide v1"))
	if _, err := fx.admin.AgentPacksImportCommit(importCtxA, packCommitReq(importUserA, testAgentID, v1, false)); err != nil {
		t.Fatalf("import: %v", err)
	}
	// Drop the source package so the published reference dangles.
	q, _ := agentcfg.AgentScope("t", testAgentID)
	winner, ok := fx.installed(t)
	if !ok {
		t.Fatal("source package missing")
	}
	if _, err := fx.store.DeleteInstalledPackage(context.Background(), q, testAgentID, "shared-guide", skills.InstalledPackageReceipt{
		TenantID: q.TenantID, UserID: q.UserID, AgentID: testAgentID, Name: "shared-guide", WrittenHash: winner.PackageHash, WrittenVersion: winner.Package.Version,
	}); err != nil {
		t.Fatalf("delete source package: %v", err)
	}
	fx.seedCopyTarget(t)
	if _, err := fx.copyPacks(t, "copy-x", []string{"shared-guide"}); !errors.Is(err, agentcfgprotocol.ErrAgentPackCopyInvalid) {
		t.Fatalf("copy err=%v, want ErrAgentPackCopyInvalid", err)
	}
	if _, ok := fx.targetPackage(t); ok {
		t.Fatal("refused copy installed a package")
	}
	ctx := copyCtx(importUserA)
	target, err := fx.admin.InspectEffective(ctx, copyTargetAgentID)
	if err != nil || len(target.RevisionItems) != 1 {
		t.Fatalf("refused copy published items: %+v err=%v", target.RevisionItems, err)
	}
}
