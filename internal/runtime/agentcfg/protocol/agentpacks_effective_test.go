package protocol

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type effectivePackBootReader struct {
	mu      sync.RWMutex
	entries map[string][]bootpacks.Entry
}

func (r *effectivePackBootReader) Lookup(tenantID, agentID string) ([]bootpacks.Entry, bool) {
	r.mu.RLock()
	declarations, ok := r.entries[tenantID+"\x00"+agentID]
	out := make([]bootpacks.Entry, len(declarations))
	for i, entry := range declarations {
		out[i] = entry
		out[i].Skill = cloneEffectiveSkill(entry.Skill)
	}
	r.mu.RUnlock()
	return out, ok
}

func cloneEffectiveSkill(skill skills.Skill) skills.Skill {
	skill.Tags = append([]string(nil), skill.Tags...)
	skill.Steps = append([]string(nil), skill.Steps...)
	skill.Preconditions = append([]string(nil), skill.Preconditions...)
	skill.FailureModes = append([]string(nil), skill.FailureModes...)
	skill.RequiredTools = append([]string(nil), skill.RequiredTools...)
	skill.RequiredNS = append([]string(nil), skill.RequiredNS...)
	skill.RequiredTags = append([]string(nil), skill.RequiredTags...)
	if skill.Extra != nil {
		skill.Extra = map[string]any{}
		for key, value := range skill.Extra {
			skill.Extra[key] = value
		}
	}
	return skill
}

func newEffectivePackService(t *testing.T, registry agentcfg.Registry, store state.StateStore, boot BootPackReader) *Service {
	t.Helper()
	service, err := NewService(registry, WithAgentPackCopyState(store), WithBootPackReader(boot))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func newEffectivePackRegistry(t *testing.T) (agentcfg.Registry, state.StateStore) {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("event bus: %v", err)
	}
	registry, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = registry.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	})
	return registry, store
}

func effectivePackContext(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	return ctx
}

func effectivePackItem(name, step, originRef string) skills.AgentPackItem {
	return skills.AgentPackItem{
		Name: name, Title: name, Trigger: "trigger-" + name, Steps: []string{step}, OriginRef: originRef,
	}
}

func effectiveBootEntry(t *testing.T, item skills.AgentPackItem) bootpacks.Entry {
	t.Helper()
	skill, err := item.Skill()
	if err != nil {
		t.Fatalf("pack item skill: %v", err)
	}
	return bootpacks.Entry{Skill: skill, SemanticHash: skills.CanonicalContentHash(skill), Source: "test"}
}

func seedEffectivePack(t *testing.T, registry agentcfg.Registry, id identity.Identity, agentID string, items []skills.AgentPackItem) agentcfg.Revision {
	t.Helper()
	revision, err := registry.SetRevision(context.Background(), identity.Quadruple{Identity: id}, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{AgentPacks: items}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed %s: %v", agentID, err)
	}
	return revision
}

func effectivePackByName(items []AgentPackLayerItem, name string) (AgentPackLayerItem, bool) {
	for _, item := range items {
		if item.PackID == name {
			return item, true
		}
	}
	return AgentPackLayerItem{}, false
}

func effectiveNames(items []AgentPackLayerItem) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, item := range items {
		out[item.PackID] = struct{}{}
	}
	return out
}

func TestAgentPackInspectEffective_PreservesLayersAndDedupes(t *testing.T) {
	registry, store := newEffectivePackRegistry(t)
	id := identity.Identity{TenantID: "tenant-a", UserID: "operator", SessionID: "session-a"}
	sharedBoot := effectivePackItem("shared", "same body", "boot-authored")
	bootOnly := effectivePackItem("boot-only", "boot body", "boot-only-authored")
	sharedRevision := effectivePackItem("shared", "same body", "revision-authored")
	revisionOnly := effectivePackItem("revision-only", "revision body", "revision-only-authored")
	seedEffectivePack(t, registry, id, "source", []skills.AgentPackItem{sharedRevision, revisionOnly})
	boot := &effectivePackBootReader{entries: map[string][]bootpacks.Entry{
		"tenant-a\x00source": {effectiveBootEntry(t, sharedBoot), effectiveBootEntry(t, bootOnly)},
	}}
	service := newEffectivePackService(t, registry, store, boot)
	view, err := service.InspectEffective(effectivePackContext(t, id), "source")
	if err != nil {
		t.Fatalf("InspectEffective: %v", err)
	}
	if len(view.BootItems) != 2 || len(view.RevisionItems) != 2 || len(view.Items) != 3 {
		t.Fatalf("layer/effective counts = boot %d revision %d effective %d", len(view.BootItems), len(view.RevisionItems), len(view.Items))
	}
	bootShared, ok := effectivePackByName(view.BootItems, "shared")
	if !ok || bootShared.Item.OriginRef != "boot-authored" {
		t.Fatalf("boot shared body not preserved: %+v", bootShared)
	}
	revisionShared, ok := effectivePackByName(view.RevisionItems, "shared")
	if !ok || revisionShared.Item.OriginRef != "revision-authored" {
		t.Fatalf("revision shared body not preserved: %+v", revisionShared)
	}
	var effectiveShared AgentPackEffectiveItem
	for _, item := range view.Items {
		if item.PackID == "shared" {
			effectiveShared = item
		}
	}
	if effectiveShared.Source != skills.OperatorTierSourceBoth || effectiveShared.Editable || effectiveShared.Item.OriginRef != "boot-authored" {
		t.Fatalf("effective shared = %+v, want boot-authored both/read-only", effectiveShared)
	}
	if view.BootPackSetHash == "" || view.CompositionHash == "" || view.RevisionHash == "" {
		t.Fatal("inspection omitted one or more composition hashes")
	}
}

func TestAgentPackCopySelected_CASIdempotencyReconciliationAndCollision(t *testing.T) {
	registry, store := newEffectivePackRegistry(t)
	id := identity.Identity{TenantID: "tenant-a", UserID: "operator", SessionID: "session-a"}
	ctx := effectivePackContext(t, id)
	alpha := effectivePackItem("alpha", "alpha-v1", "")
	beta := effectivePackItem("beta", "beta-v1", "")
	seedEffectivePack(t, registry, id, "source", []skills.AgentPackItem{alpha, beta})
	keep := effectivePackItem("keep", "independent", "independent-target")
	seedEffectivePack(t, registry, id, "target", []skills.AgentPackItem{keep})
	service := newEffectivePackService(t, registry, store, nil)
	sourceView, err := service.InspectEffective(ctx, "source")
	if err != nil {
		t.Fatalf("inspect source: %v", err)
	}
	targetView, err := service.InspectEffective(ctx, "target")
	if err != nil {
		t.Fatalf("inspect target: %v", err)
	}
	request := AgentPackCopyRequest{
		SourceAgentID: "source", TargetAgentID: "target", PackIDs: []string{"beta", "alpha"},
		ExpectedSourceCompositionHash: sourceView.CompositionHash,
		ExpectedTargetCompositionHash: targetView.CompositionHash,
		ExpectedTargetContentHash:     targetView.ContentHash,
		IdempotencyKey:                "copy-v1",
	}
	first, err := service.CopySelected(ctx, request)
	if err != nil {
		t.Fatalf("first copy: %v", err)
	}
	if !first.Changed || first.Replayed {
		t.Fatalf("first copy result = %+v", first)
	}
	if first.SourceRevisionID != sourceView.RevisionID || first.SourceContentHash != sourceView.ContentHash || first.SourceCompositionHash != sourceView.CompositionHash {
		t.Fatalf("source snapshot fields = %+v, source = %+v", first, sourceView)
	}
	if first.TargetRevisionID != first.Target.RevisionID || first.TargetContentHash != first.Target.ContentHash || first.TargetCompositionHash != first.Target.CompositionHash {
		t.Fatalf("target snapshot fields = %+v", first)
	}
	if len(first.Outcomes) != 2 || first.Outcomes[0] != (AgentPackCopyItemResult{PackID: "alpha", Outcome: AgentPackCopyOutcomeCopied}) || first.Outcomes[1] != (AgentPackCopyItemResult{PackID: "beta", Outcome: AgentPackCopyOutcomeCopied}) {
		t.Fatalf("first copy outcomes = %+v", first.Outcomes)
	}
	if got := effectiveNames(first.Target.RevisionItems); len(got) != 3 {
		t.Fatalf("copied target names = %v", got)
	}
	alphaLayer, ok := effectivePackByName(first.Target.RevisionItems, "alpha")
	if !ok || !stringsHasPrefix(alphaLayer.Item.OriginRef, copiedOriginPrefix) {
		t.Fatalf("alpha was not server-stamped: %+v", alphaLayer)
	}
	second, err := service.CopySelected(ctx, request)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if !second.Replayed || second.Target.RevisionID != first.Target.RevisionID {
		t.Fatalf("replay result = %+v, first revision %q", second, first.Target.RevisionID)
	}
	if len(second.Outcomes) != 2 || second.Outcomes[0] != (AgentPackCopyItemResult{PackID: "alpha", Outcome: AgentPackCopyOutcomeCopied}) || second.Outcomes[1] != (AgentPackCopyItemResult{PackID: "beta", Outcome: AgentPackCopyOutcomeCopied}) {
		t.Fatalf("replay outcomes = %+v", second.Outcomes)
	}

	sourceBefore, _, err := registry.Active(context.Background(), identity.Quadruple{Identity: id}, "source", agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("source active: %v", err)
	}
	alphaV2 := effectivePackItem("alpha", "alpha-v2", "")
	sourceV2Payload := sourceBefore.Payload
	sourceV2Payload.AgentPacks = []skills.AgentPackItem{alphaV2, beta}
	seedExpectedEffectivePack(t, registry, id, "source", sourceV2Payload, sourceBefore.ContentHash)
	sourceV2View, err := service.InspectEffective(ctx, "source")
	if err != nil {
		t.Fatalf("inspect source v2: %v", err)
	}
	targetAfterFirst, err := service.InspectEffective(ctx, "target")
	if err != nil {
		t.Fatalf("inspect target after first: %v", err)
	}
	update, err := service.CopySelected(ctx, AgentPackCopyRequest{
		SourceAgentID: "source", TargetAgentID: "target", PackIDs: []string{"alpha"},
		ExpectedSourceCompositionHash: sourceV2View.CompositionHash,
		ExpectedTargetCompositionHash: targetAfterFirst.CompositionHash,
		ExpectedTargetContentHash:     targetAfterFirst.ContentHash,
		IdempotencyKey:                "copy-v2",
	})
	if err != nil {
		t.Fatalf("reconciled update: %v", err)
	}
	if !update.Changed || update.Target.RevisionID == first.Target.RevisionID {
		t.Fatalf("reconciled update result = %+v", update)
	}
	if len(update.Outcomes) != 1 || update.Outcomes[0] != (AgentPackCopyItemResult{PackID: "alpha", Outcome: AgentPackCopyOutcomeCopied}) {
		t.Fatalf("reconciled update outcomes = %+v", update.Outcomes)
	}
	if _, ok := effectivePackByName(update.Target.RevisionItems, "beta"); ok {
		t.Fatal("omitted copied beta survived reconciliation")
	}
	if got, ok := effectivePackByName(update.Target.RevisionItems, "alpha"); !ok || got.Item.Steps[0] != "alpha-v2" {
		t.Fatalf("updated alpha = %+v", got)
	}

	independent := effectivePackItem("alpha", "independent-alpha", "operator-authored")
	targetBeforeCollision, _, err := registry.Active(context.Background(), identity.Quadruple{Identity: id}, "target", agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("target before collision: %v", err)
	}
	collisionPayload := targetBeforeCollision.Payload
	collisionPayload.AgentPacks = []skills.AgentPackItem{keep, independent}
	seedExpectedEffectivePack(t, registry, id, "target", collisionPayload, targetBeforeCollision.ContentHash)
	sourceBeforeV3, _, err := registry.Active(context.Background(), identity.Quadruple{Identity: id}, "source", agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("source before v3: %v", err)
	}
	alphaV3 := effectivePackItem("alpha", "alpha-v3", "")
	sourceV3Payload := sourceBeforeV3.Payload
	sourceV3Payload.AgentPacks = []skills.AgentPackItem{alphaV3, beta}
	seedExpectedEffectivePack(t, registry, id, "source", sourceV3Payload, sourceBeforeV3.ContentHash)
	sourceV3View, err := service.InspectEffective(ctx, "source")
	if err != nil {
		t.Fatalf("inspect source v3: %v", err)
	}
	targetCollisionView, err := service.InspectEffective(ctx, "target")
	if err != nil {
		t.Fatalf("inspect target collision: %v", err)
	}
	_, err = service.CopySelected(ctx, AgentPackCopyRequest{
		SourceAgentID: "source", TargetAgentID: "target", PackIDs: []string{"alpha", "beta"},
		ExpectedSourceCompositionHash: sourceV3View.CompositionHash,
		ExpectedTargetCompositionHash: targetCollisionView.CompositionHash,
		ExpectedTargetContentHash:     targetCollisionView.ContentHash,
		IdempotencyKey:                "copy-v3",
	})
	if !errors.Is(err, ErrAgentPackCopyCollision) {
		t.Fatalf("independent collision error = %v, want ErrAgentPackCopyCollision", err)
	}
	afterCollision, err := service.InspectEffective(ctx, "target")
	if err != nil {
		t.Fatalf("inspect target after collision: %v", err)
	}
	if afterCollision.ContentHash != targetCollisionView.ContentHash {
		t.Fatal("collision changed target despite fail-closed whole operation")
	}
	if _, ok := effectivePackByName(afterCollision.RevisionItems, "beta"); ok {
		t.Fatal("plural collision partially copied beta")
	}

	equalTarget := "equal-target"
	equalInitial := seedEffectivePack(t, registry, id, equalTarget, []skills.AgentPackItem{alphaV3})
	equalView, err := service.InspectEffective(ctx, equalTarget)
	if err != nil {
		t.Fatalf("inspect equal target: %v", err)
	}
	equalResult, err := service.CopySelected(ctx, AgentPackCopyRequest{
		SourceAgentID: "source", TargetAgentID: equalTarget, PackIDs: []string{"alpha"},
		ExpectedSourceCompositionHash: sourceV3View.CompositionHash,
		ExpectedTargetCompositionHash: equalView.CompositionHash,
		ExpectedTargetContentHash:     equalView.ContentHash,
		IdempotencyKey:                "copy-equal",
	})
	if err != nil {
		t.Fatalf("equal content copy: %v", err)
	}
	if equalResult.Changed || equalResult.Target.RevisionID != equalInitial.RevisionID {
		t.Fatalf("equal content should be no-op: %+v", equalResult)
	}
	if len(equalResult.Outcomes) != 1 || equalResult.Outcomes[0] != (AgentPackCopyItemResult{PackID: "alpha", Outcome: AgentPackCopyOutcomeNoop}) {
		t.Fatalf("equal content outcome = %+v", equalResult.Outcomes)
	}

	mixedTarget := "mixed-target"
	mixedInitial := seedEffectivePack(t, registry, id, mixedTarget, []skills.AgentPackItem{alphaV3})
	mixedView, err := service.InspectEffective(ctx, mixedTarget)
	if err != nil {
		t.Fatalf("inspect mixed target: %v", err)
	}
	mixedResult, err := service.CopySelected(ctx, AgentPackCopyRequest{
		SourceAgentID: "source", TargetAgentID: mixedTarget, PackIDs: []string{"beta", "alpha"},
		ExpectedSourceCompositionHash: sourceV3View.CompositionHash,
		ExpectedTargetCompositionHash: mixedView.CompositionHash,
		ExpectedTargetContentHash:     mixedView.ContentHash,
		IdempotencyKey:                "copy-mixed",
	})
	if err != nil {
		t.Fatalf("mixed copy: %v", err)
	}
	if !mixedResult.Changed || mixedResult.Target.RevisionID == mixedInitial.RevisionID {
		t.Fatalf("mixed copy should publish one new pack: %+v", mixedResult)
	}
	if len(mixedResult.Outcomes) != 2 || mixedResult.Outcomes[0] != (AgentPackCopyItemResult{PackID: "alpha", Outcome: AgentPackCopyOutcomeNoop}) || mixedResult.Outcomes[1] != (AgentPackCopyItemResult{PackID: "beta", Outcome: AgentPackCopyOutcomeCopied}) {
		t.Fatalf("mixed copy outcomes = %+v", mixedResult.Outcomes)
	}

}

func seedExpectedEffectivePack(t *testing.T, registry agentcfg.Registry, id identity.Identity, agentID string, payload agentcfg.ConfigPayload, expected string) agentcfg.Revision {
	t.Helper()
	revision, err := registry.SetRevision(context.Background(), identity.Quadruple{Identity: id}, agentID, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{ExpectedContentHash: expected})
	if err != nil {
		t.Fatalf("seed expected %s: %v", agentID, err)
	}
	return revision
}

func stringsHasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}

func TestAgentPackCopySelected_BootTargetReadOnlyAndVerifiedTenant(t *testing.T) {
	registry, store := newEffectivePackRegistry(t)
	id := identity.Identity{TenantID: "tenant-a", UserID: "operator", SessionID: "session-a"}
	ctx := effectivePackContext(t, id)
	seedEffectivePack(t, registry, id, "source", []skills.AgentPackItem{effectivePackItem("owned", "source", "")})
	boot := &effectivePackBootReader{entries: map[string][]bootpacks.Entry{
		"tenant-a\x00target": {effectiveBootEntry(t, effectivePackItem("owned", "boot", "boot-owned"))},
	}}
	service := newEffectivePackService(t, registry, store, boot)
	source, err := service.InspectEffective(ctx, "source")
	if err != nil {
		t.Fatalf("source inspect: %v", err)
	}
	target, err := service.InspectEffective(ctx, "target")
	if err != nil {
		t.Fatalf("target inspect: %v", err)
	}
	_, err = service.CopySelected(ctx, AgentPackCopyRequest{
		SourceAgentID: "source", TargetAgentID: "target", PackIDs: []string{"owned"},
		ExpectedSourceCompositionHash: source.CompositionHash,
		ExpectedTargetCompositionHash: target.CompositionHash,
		ExpectedTargetContentHash:     agentcfg.ExpectNoActiveRevision,
		IdempotencyKey:                "boot-copy",
	})
	if !errors.Is(err, ErrBootPackOwned) {
		t.Fatalf("boot target copy error = %v, want ErrBootPackOwned", err)
	}
	if _, err := service.InspectEffective(context.Background(), "source"); !errors.Is(err, ErrAgentPackInspectionIdentityRequired) {
		t.Fatalf("missing verified identity error = %v", err)
	}
	foreign := effectivePackContext(t, identity.Identity{TenantID: "tenant-b", UserID: "operator", SessionID: "session-b"})
	foreignView, err := service.InspectEffective(foreign, "source")
	if err != nil {
		t.Fatalf("foreign tenant inspect should be an empty local view: %v", err)
	}
	if len(foreignView.Items) != 0 {
		t.Fatalf("foreign tenant saw source pack: %+v", foreignView.Items)
	}
}

func TestAgentPackCopySelected_PreparedReplayAcrossServiceAndSession(t *testing.T) {
	registry, store := newEffectivePackRegistry(t)
	id := identity.Identity{TenantID: "tenant-a", UserID: "operator", SessionID: "session-a"}
	ctx := effectivePackContext(t, id)
	seedEffectivePack(t, registry, id, "source", []skills.AgentPackItem{effectivePackItem("alpha", "v1", "")})
	seedEffectivePack(t, registry, id, "target", []skills.AgentPackItem{effectivePackItem("keep", "keep", "independent")})
	base := newEffectivePackService(t, registry, store, nil)
	source, err := base.InspectEffective(ctx, "source")
	if err != nil {
		t.Fatalf("source inspect: %v", err)
	}
	target, err := base.InspectEffective(ctx, "target")
	if err != nil {
		t.Fatalf("target inspect: %v", err)
	}
	req := AgentPackCopyRequest{
		SourceAgentID: "source", TargetAgentID: "target", PackIDs: []string{"alpha"},
		ExpectedSourceCompositionHash: source.CompositionHash,
		ExpectedTargetCompositionHash: target.CompositionHash,
		ExpectedTargetContentHash:     target.ContentHash,
		IdempotencyKey:                "prepared-replay",
	}
	var failed atomic.Bool
	losing := &responseLossRegistry{Registry: registry, failOnce: &failed}
	first := newEffectivePackService(t, losing, store, nil)
	if _, err := first.CopySelected(ctx, req); err == nil {
		t.Fatal("response-loss copy unexpectedly succeeded")
	}
	secondCtx := effectivePackContext(t, identity.Identity{TenantID: "tenant-a", UserID: "another-operator", SessionID: "session-b"})
	second := newEffectivePackService(t, registry, store, nil)
	replayed, err := second.CopySelected(secondCtx, req)
	if err != nil {
		t.Fatalf("prepared replay: %v", err)
	}
	if !replayed.Replayed || !replayed.Changed || replayed.Target.RevisionID == "" {
		t.Fatalf("prepared replay result = %+v", replayed)
	}
	thirdCtx := effectivePackContext(t, identity.Identity{TenantID: "tenant-a", UserID: "third-operator", SessionID: "session-c"})
	third := newEffectivePackService(t, registry, store, nil)
	committedReplay, err := third.CopySelected(thirdCtx, req)
	if err != nil {
		t.Fatalf("committed replay across service: %v", err)
	}
	if !committedReplay.Replayed || !committedReplay.Changed || committedReplay.TargetRevisionID != replayed.TargetRevisionID || len(committedReplay.Outcomes) != 1 || committedReplay.Outcomes[0].Outcome != AgentPackCopyOutcomeCopied {
		t.Fatalf("committed replay result = %+v", committedReplay)
	}
	original := committedReplay
	editedPayload := agentcfg.ConfigPayload{AgentPacks: []skills.AgentPackItem{
		effectivePackItem("alpha", "edited", "later-operator"),
		effectivePackItem("keep", "edited", "later-operator"),
	}}
	if _, err := registry.SetRevision(context.Background(), identity.Quadruple{Identity: id}, "target", agentcfg.ConfigScopeAgent, editedPayload, agentcfg.SetOptions{ExpectedContentHash: original.TargetContentHash}); err != nil {
		t.Fatalf("legitimate target edit: %v", err)
	}
	fourth := newEffectivePackService(t, registry, store, nil)
	afterEdit, err := fourth.CopySelected(effectivePackContext(t, identity.Identity{TenantID: "tenant-a", UserID: "final-operator", SessionID: "session-d"}), req)
	if err != nil {
		t.Fatalf("committed replay after target edit: %v", err)
	}
	if !afterEdit.Replayed || afterEdit.TargetRevisionID != original.TargetRevisionID || afterEdit.TargetContentHash != original.TargetContentHash || afterEdit.TargetCompositionHash != original.TargetCompositionHash || afterEdit.Target.BootPackSetHash != original.Target.BootPackSetHash || afterEdit.Target.CompositionHash != original.Target.CompositionHash {
		t.Fatalf("replay receipt changed after target edit: got=%+v original=%+v", afterEdit, original)
	}
	if len(afterEdit.Outcomes) != len(original.Outcomes) || afterEdit.Outcomes[0] != original.Outcomes[0] {
		t.Fatalf("replay outcomes changed after target edit: got=%+v original=%+v", afterEdit.Outcomes, original.Outcomes)
	}
}

type responseLossRegistry struct {
	agentcfg.Registry
	failOnce *atomic.Bool
}

func (r *responseLossRegistry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	revision, err := r.Registry.SetRevision(ctx, id, agentID, scope, payload, opts)
	if err == nil && r.failOnce.CompareAndSwap(false, true) {
		return revision, fmt.Errorf("simulated response loss")
	}
	return revision, err
}

type staleCASRegistry struct {
	agentcfg.Registry
	once        atomic.Bool
	bumpID      identity.Quadruple
	bumpAgentID string
	bumpPayload agentcfg.ConfigPayload
}

func (r *staleCASRegistry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	if agentID == r.bumpAgentID && r.once.CompareAndSwap(false, true) {
		if _, err := r.Registry.SetRevision(ctx, r.bumpID, agentID, scope, clonePayload(r.bumpPayload), agentcfg.SetOptions{ExpectedContentHash: opts.ExpectedContentHash}); err != nil {
			return agentcfg.Revision{}, fmt.Errorf("stale CAS setup: %w", err)
		}
	}
	return r.Registry.SetRevision(ctx, id, agentID, scope, payload, opts)
}

func TestAgentPackCopySelected_RejectsStaleTargetCASWithoutCopy(t *testing.T) {
	registry, store := newEffectivePackRegistry(t)
	id := identity.Identity{TenantID: "tenant-a", UserID: "operator", SessionID: "session-a"}
	ctx := effectivePackContext(t, id)
	seedEffectivePack(t, registry, id, "source", []skills.AgentPackItem{effectivePackItem("alpha", "source", "")})
	initial := seedEffectivePack(t, registry, id, "target", []skills.AgentPackItem{effectivePackItem("keep", "target", "independent")})
	bumpPayload := initial.Payload
	bumpPayload.AgentPacks = []skills.AgentPackItem{effectivePackItem("keep", "target-race", "independent")}
	racing := &staleCASRegistry{
		Registry:    registry,
		bumpID:      identity.Quadruple{Identity: id},
		bumpAgentID: "target",
		bumpPayload: bumpPayload,
	}
	service := newEffectivePackService(t, racing, store, nil)
	source, err := service.InspectEffective(ctx, "source")
	if err != nil {
		t.Fatalf("source inspect: %v", err)
	}
	target, err := service.InspectEffective(ctx, "target")
	if err != nil {
		t.Fatalf("target inspect: %v", err)
	}
	_, err = service.CopySelected(ctx, AgentPackCopyRequest{
		SourceAgentID: "source", TargetAgentID: "target", PackIDs: []string{"alpha"},
		ExpectedSourceCompositionHash: source.CompositionHash,
		ExpectedTargetCompositionHash: target.CompositionHash,
		ExpectedTargetContentHash:     target.ContentHash,
		IdempotencyKey:                "stale-cas",
	})
	if !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("stale target CAS error = %v, want ErrRevisionConflict", err)
	}
	after, err := service.InspectEffective(ctx, "target")
	if err != nil {
		t.Fatalf("target inspect after stale CAS: %v", err)
	}
	if after.ContentHash == target.ContentHash || after.RevisionID == target.RevisionID {
		t.Fatalf("stale CAS setup did not publish its competing revision: before=%+v after=%+v", target, after)
	}
	if _, ok := effectivePackByName(after.RevisionItems, "alpha"); ok {
		t.Fatal("stale CAS published the copy despite the competing target revision")
	}
}

func TestAgentPackService_ConcurrentInspectReuse(t *testing.T) {
	registry, store := newEffectivePackRegistry(t)
	id := identity.Identity{TenantID: "tenant-a", UserID: "operator", SessionID: "session-a"}
	seedEffectivePack(t, registry, id, "source", []skills.AgentPackItem{effectivePackItem("alpha", "v1", "")})
	service := newEffectivePackService(t, registry, store, nil)
	ctx := effectivePackContext(t, id)
	const callers = 100
	errs := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			view, err := service.InspectEffective(ctx, "source")
			if err != nil {
				errs <- err
				return
			}
			if len(view.Items) != 1 || view.CompositionHash == "" {
				errs <- fmt.Errorf("unexpected concurrent view: %+v", view)
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestAgentPackService_ConcurrentInspectCopyReuse exercises the real shared
// service rather than a recording port. Every invocation has its own full
// identity and source/target pair, while the compiled Service and StateStore
// are reused by all callers. The cancelled invocation must not publish a
// revision or affect any peer; successful callers must be safely replayable
// without advancing their target revision a second time.
func TestAgentPackService_ConcurrentInspectCopyReuse(t *testing.T) {
	registry, store := newEffectivePackRegistry(t)
	service := newEffectivePackService(t, registry, store, nil)

	type scenario struct {
		id           identity.Identity
		ctx          context.Context
		sourceID     string
		targetID     string
		sourcePack   string
		sourceStep   string
		metadataTool string
		request      AgentPackCopyRequest
		source       AgentPackInspection
		target       AgentPackInspection
		cancelled    bool
	}

	const callers = 128
	scenarios := make([]scenario, 0, callers)
	for i := range callers {
		id := identity.Identity{
			TenantID:  fmt.Sprintf("tenant-concurrent-%03d", i),
			UserID:    fmt.Sprintf("operator-%03d", i),
			SessionID: fmt.Sprintf("session-%03d", i),
		}
		ctx := effectivePackContext(t, id)
		sourceID := fmt.Sprintf("source-%03d", i)
		targetID := fmt.Sprintf("target-%03d", i)
		sourcePack := fmt.Sprintf("pack-%03d", i)
		sourceStep := fmt.Sprintf("source-step-%03d", i)
		sourceItem := effectivePackItem(sourcePack, sourceStep, "")
		sourceItem.RequiredTools = []string{fmt.Sprintf("metadata-tool-%03d", i)}
		seedEffectivePack(t, registry, id, sourceID, []skills.AgentPackItem{sourceItem})
		keepItem := effectivePackItem(fmt.Sprintf("keep-%03d", i), fmt.Sprintf("keep-step-%03d", i), "operator-authored")
		seedEffectivePack(t, registry, id, targetID, []skills.AgentPackItem{keepItem})

		source, err := service.InspectEffective(ctx, sourceID)
		if err != nil {
			t.Fatalf("inspect source %03d: %v", i, err)
		}
		target, err := service.InspectEffective(ctx, targetID)
		if err != nil {
			t.Fatalf("inspect target %03d: %v", i, err)
		}
		scenarios = append(scenarios, scenario{
			id: id, ctx: ctx, sourceID: sourceID, targetID: targetID,
			sourcePack: sourcePack, sourceStep: sourceStep,
			metadataTool: fmt.Sprintf("metadata-tool-%03d", i),
			request: AgentPackCopyRequest{
				SourceAgentID: sourceID, TargetAgentID: targetID,
				PackIDs:                       []string{sourcePack},
				ExpectedSourceCompositionHash: source.CompositionHash,
				ExpectedTargetCompositionHash: target.CompositionHash,
				ExpectedTargetContentHash:     target.ContentHash,
				IdempotencyKey:                fmt.Sprintf("concurrent-copy-%03d", i),
			},
			source: source, target: target, cancelled: i == 0,
		})
	}

	baseline := runtime.NumGoroutine()
	errs := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for _, sc := range scenarios {
		go func() {
			defer group.Done()
			if sc.cancelled {
				cancelled, cancel := context.WithCancel(sc.ctx)
				cancel()
				if _, err := service.CopySelected(cancelled, sc.request); !errors.Is(err, context.Canceled) {
					errs <- fmt.Errorf("cancelled copy error = %w, want context.Canceled", err)
					return
				}
				view, err := service.InspectEffective(sc.ctx, sc.targetID)
				if err != nil {
					errs <- fmt.Errorf("cancelled target inspect: %w", err)
					return
				}
				if view.RevisionID != sc.target.RevisionID || view.ContentHash != sc.target.ContentHash {
					errs <- fmt.Errorf("cancelled copy changed target: before=%+v after=%+v", sc.target, view)
					return
				}
				if _, ok := effectivePackByName(view.RevisionItems, sc.sourcePack); ok {
					errs <- fmt.Errorf("cancelled copy leaked %q into target", sc.sourcePack)
				}
				return
			}

			first, err := service.CopySelected(sc.ctx, sc.request)
			if err != nil {
				errs <- fmt.Errorf("copy %s/%s: %w", sc.id.TenantID, sc.targetID, err)
				return
			}
			if !first.Changed || first.Replayed {
				errs <- fmt.Errorf("first copy %s result = %+v", sc.targetID, first)
				return
			}
			if first.Target.RevisionID == sc.target.RevisionID || first.Target.ContentHash == sc.target.ContentHash {
				errs <- fmt.Errorf("first copy %s did not publish a new target revision", sc.targetID)
				return
			}
			copied, ok := effectivePackByName(first.Target.RevisionItems, sc.sourcePack)
			if !ok || copied.Item.Steps[0] != sc.sourceStep || !stringsHasPrefix(copied.Item.OriginRef, copiedOriginPrefix) {
				errs <- fmt.Errorf("copy %s body/provenance = %+v", sc.targetID, copied)
				return
			}
			if len(first.Target.RevisionItems) != 2 {
				errs <- fmt.Errorf("copy %s revision items = %d, want own keep+selected", sc.targetID, len(first.Target.RevisionItems))
				return
			}

			second, err := service.CopySelected(sc.ctx, sc.request)
			if err != nil {
				errs <- fmt.Errorf("replay %s: %w", sc.targetID, err)
				return
			}
			if !second.Replayed || second.Target.RevisionID != first.Target.RevisionID || second.Target.ContentHash != first.Target.ContentHash {
				errs <- fmt.Errorf("replay %s advanced or was not marked replayed: first=%+v second=%+v", sc.targetID, first, second)
				return
			}
			view, err := service.InspectEffective(sc.ctx, sc.targetID)
			if err != nil {
				errs <- fmt.Errorf("post-replay inspect %s: %w", sc.targetID, err)
				return
			}
			if view.RevisionID != first.Target.RevisionID || view.ContentHash != first.Target.ContentHash {
				errs <- fmt.Errorf("post-replay target %s changed: first=%+v view=%+v", sc.targetID, first.Target, view)
				return
			}
			got, ok := effectivePackByName(view.RevisionItems, sc.sourcePack)
			if !ok || len(got.Item.RequiredTools) != 1 || got.Item.RequiredTools[0] != sc.metadataTool {
				errs <- fmt.Errorf("target %s lost source metadata: %+v", sc.targetID, view.RevisionItems)
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	// A caller from one tenant must not be able to observe another tenant's
	// agent state even when it guesses the other agent key. This explicit
	// cross-tenant read complements the per-invocation body checks above.
	for i := 1; i < len(scenarios); i++ {
		foreign, err := service.InspectEffective(scenarios[0].ctx, scenarios[i].targetID)
		if err != nil {
			t.Fatalf("cross-tenant inspect of %s: %v", scenarios[i].targetID, err)
		}
		if len(foreign.Items) != 0 || len(foreign.RevisionItems) != 0 {
			t.Fatalf("cross-tenant state leaked for %s: %+v", scenarios[i].targetID, foreign)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+16 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+16 {
		t.Fatalf("goroutine count did not return near baseline: before=%d after=%d", baseline, got)
	}
}
