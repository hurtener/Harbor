package publication

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func publicationIdentity(tenant, user, session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session}}
}

func publicationSkill(name, step string) skills.Skill {
	return skills.Skill{Name: name, Trigger: "when " + name, Steps: []string{step}, Origin: skills.OriginGenerated, Scope: skills.ScopeUser}
}

func TestMemoryStore_PublicationLifecycleAndContentFreeProjections(t *testing.T) {
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "user-a", "session-a")
	store := NewMemoryStore(NewRuntimeID("deployment-a"))
	pub, receipt, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "publish-1", Name: " Playbook ", Skill: publicationSkill("playbook", "do it"), ExpectedAbsent: true})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.Name != "playbook" || pub.State != StateActive || pub.Generation != 1 || receipt.AfterHash == "" {
		t.Fatalf("unexpected publication: %+v receipt=%+v", pub, receipt)
	}
	// Metadata and receipts must never grow a body-shaped field.
	encoded, err := json.Marshal(struct {
		Metadata Metadata
		Receipt  Receipt
	}{pub, receipt})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || containsJSONBody(string(encoded)) {
		t.Fatalf("content leaked into metadata/receipt: %s", encoded)
	}
	list, err := store.List(ctx, id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].PublicationID != pub.PublicationID {
		t.Fatalf("list=%+v", list)
	}
	if _, _, err := store.Install(ctx, id, InstallRequest{IdempotencyKey: "install-1", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatalf("install: %v", err)
	}
	body, resolved, err := store.Resolve(ctx, id, "agent-a")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if body.Name != "playbook" || resolved.ContentHash != pub.ContentHash {
		t.Fatalf("resolved body/meta=%+v/%+v", body, resolved)
	}
	if _, _, err := store.Install(ctx, id, InstallRequest{IdempotencyKey: "install-2", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second install error=%v want conflict", err)
	}
}

func containsJSONBody(s string) bool {
	for _, field := range []string{"\"trigger\"", "\"steps\"", "\"description\""} {
		if len(s) >= len(field) && indexJSON(s, field) >= 0 {
			return true
		}
	}
	return false
}

func indexJSON(s, needle string) int {
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestMemoryStore_SuccessorRetireCASAndResponseLossReplay(t *testing.T) {
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "user-a", "session-a")
	store := NewMemoryStore("runtime-a")
	first, firstReceipt, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "first"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	firstAgain, replay, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "first"), ExpectedAbsent: true})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if firstAgain != first || replay != firstReceipt {
		t.Fatalf("replay changed result: first=%+v again=%+v", first, firstAgain)
	}
	second, _, err := store.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "s", PublicationID: first.PublicationID, ExpectedGeneration: first.Generation, ExpectedContentHash: first.ContentHash, Skill: publicationSkill("ops", "second")})
	if err != nil {
		t.Fatalf("successor: %v", err)
	}
	if second.Generation != 2 || second.RevisionID == first.RevisionID || second.ContentHash == first.ContentHash {
		t.Fatalf("successor not immutable: %+v", second)
	}
	if _, _, err := store.Install(ctx, id, InstallRequest{IdempotencyKey: "install-old", AgentID: "agent-old", PublicationID: first.PublicationID, RevisionID: first.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatalf("install immutable predecessor: %v", err)
	}
	oldBody, oldMeta, err := store.Resolve(ctx, id, "agent-old")
	if err != nil {
		t.Fatalf("resolve immutable predecessor: %v", err)
	}
	if oldBody.Steps[0] != "first" || oldMeta.RevisionID != first.RevisionID {
		t.Fatalf("predecessor mutated: body=%+v meta=%+v", oldBody, oldMeta)
	}
	updated, updateReceipt, err := store.Update(ctx, id, UpdateRequest{IdempotencyKey: "update-old", AgentID: "agent-old", PublicationID: first.PublicationID, RevisionID: second.RevisionID, ExpectedGeneration: first.Generation, ExpectedContentHash: first.ContentHash})
	if err != nil {
		t.Fatalf("update to exact successor: %v", err)
	}
	if updated.RevisionID != second.RevisionID || updateReceipt.RevisionID != second.RevisionID {
		t.Fatalf("successor update ref=%+v receipt=%+v", updated, updateReceipt)
	}
	if _, _, err := store.Update(ctx, id, UpdateRequest{IdempotencyKey: "update-predecessor", AgentID: "agent-old", PublicationID: first.PublicationID, RevisionID: first.RevisionID, ExpectedGeneration: updated.Generation, ExpectedContentHash: updated.ContentHash}); !errors.Is(err, ErrConflict) {
		t.Fatalf("update to predecessor=%v want conflict", err)
	}
	if _, _, err := store.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "stale", PublicationID: first.PublicationID, ExpectedGeneration: first.Generation, ExpectedContentHash: first.ContentHash, Skill: publicationSkill("ops", "third")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale successor=%v want conflict", err)
	}
	if _, _, err := store.Retire(ctx, id, RetireRequest{IdempotencyKey: "r", PublicationID: first.PublicationID, ExpectedGeneration: second.Generation, ExpectedContentHash: second.ContentHash}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, replay, err := store.Retire(ctx, id, RetireRequest{IdempotencyKey: "r", PublicationID: first.PublicationID, ExpectedGeneration: second.Generation, ExpectedContentHash: second.ContentHash}); err != nil || replay.State != StateRetired {
		t.Fatalf("retire replay=%+v err=%v want retired replay", replay, err)
	}
}

func TestMemoryStore_RuntimeAndTenantIsolationFailClosed(t *testing.T) {
	ctx := context.Background()
	owner := publicationIdentity("tenant-a", "user-a", "session-a")
	otherTenant := publicationIdentity("tenant-b", "user-a", "session-a")
	store := NewMemoryStore("runtime-a")
	pub, _, err := store.Publish(ctx, owner, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, otherTenant, pub.PublicationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get=%v want not found", err)
	}
	if _, _, err := store.Install(ctx, otherTenant, InstallRequest{IdempotencyKey: "i", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant install=%v want not found", err)
	}
	runtimeBound := NewMemoryStore("runtime-b")
	other, _, err := runtimeBound.Publish(ctx, owner, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtimeBound.Install(ctx, owner, InstallRequest{IdempotencyKey: "i", AgentID: "agent-a", PublicationID: other.PublicationID, RevisionID: other.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtimeBound.Resolve(WithRuntimeID(ctx, "runtime-a"), owner, "agent-a"); !errors.Is(err, ErrRuntimeMismatch) {
		t.Fatalf("runtime mismatch=%v want mismatch", err)
	}
}

func TestMemoryStore_ConcurrentResolveN128(t *testing.T) {
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "user-a", "session-a")
	store := NewMemoryStore("runtime-a")
	pub, _, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Install(ctx, id, InstallRequest{IdempotencyKey: "i", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 128)
	for range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, meta, err := store.Resolve(ctx, id, "agent-a")
			if err != nil {
				errs <- err
				return
			}
			if body.ContentHash != meta.ContentHash {
				errs <- ErrContentHashMismatch
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestStateStoreStore_RestartAndCAS(t *testing.T) {
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "user-a", "session-a")
	raw, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStateStore(raw, "runtime-a")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Install(ctx, id, InstallRequest{IdempotencyKey: "i", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatal(err)
	}
	// Re-open a new wrapper over the same StateStore to model a runtime restart.
	restarted, err := NewStateStore(raw, "runtime-a")
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := restarted.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "successor", PublicationID: pub.PublicationID, ExpectedGeneration: pub.Generation, ExpectedContentHash: pub.ContentHash, Skill: publicationSkill("ops", "new")})
	if err != nil {
		t.Fatalf("successor after restart: %v", err)
	}
	updated, updateReceipt, err := restarted.Update(ctx, id, UpdateRequest{IdempotencyKey: "update", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: next.RevisionID, ExpectedGeneration: pub.Generation, ExpectedContentHash: pub.ContentHash})
	if err != nil {
		t.Fatalf("update after restart: %v", err)
	}
	if updated.RevisionID != next.RevisionID || updateReceipt.PublicationID != pub.PublicationID || updateReceipt.RevisionID != next.RevisionID {
		t.Fatalf("durable update ref=%+v receipt=%+v", updated, updateReceipt)
	}
	body, _, err := restarted.Resolve(ctx, id, "agent-a")
	if err != nil {
		t.Fatalf("resolve after restart: %v", err)
	}
	if body.Name != "ops" || body.Steps[0] != "new" {
		t.Fatalf("body after restart=%+v", body)
	}
	if _, _, err := restarted.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "stale", PublicationID: pub.PublicationID, ExpectedGeneration: 99, ExpectedContentHash: pub.ContentHash, Skill: publicationSkill("ops", "new")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale CAS=%v want conflict", err)
	}
	_ = restarted.Close(ctx)
	_ = store.Close(ctx)
	_ = raw.Close(ctx)
}

func TestStateStoreStore_PublishRejectsEmptyOrMismatchedName(t *testing.T) {
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "user-a", "session-a")
	raw, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStateStore(raw, "runtime-a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = store.Close(ctx)
		_ = raw.Close(ctx)
	}()

	for _, tc := range []struct {
		name  string
		skill string
	}{
		{name: "", skill: "ops"},
		{name: "other", skill: "ops"},
	} {
		_, _, err := store.Publish(ctx, id, PublishRequest{
			IdempotencyKey: "publish-" + tc.name,
			Name:           tc.name,
			Skill:          publicationSkill(tc.skill, "do"),
			ExpectedAbsent: true,
		})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("publish name=%q skill=%q error=%v, want ErrInvalidRequest", tc.name, tc.skill, err)
		}
	}
	items, err := store.List(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("invalid publishes persisted %d publications", len(items))
	}
}

func TestPublicationStore_SuccessorsKeepOneImmutableRevisionEach(t *testing.T) {
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "user-a", "session-a")

	memory := NewMemoryStore("runtime-a")
	first, _, err := memory.Publish(ctx, id, PublishRequest{IdempotencyKey: "memory-p", Name: "ops", Skill: publicationSkill("ops", "one"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := memory.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "memory-s1", PublicationID: first.PublicationID, ExpectedGeneration: first.Generation, ExpectedContentHash: first.ContentHash, Skill: publicationSkill("ops", "two")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := memory.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "memory-s2", PublicationID: first.PublicationID, ExpectedGeneration: second.Generation, ExpectedContentHash: second.ContentHash, Skill: publicationSkill("ops", "three")}); err != nil {
		t.Fatal(err)
	}
	if got := len(memory.pubs[id.TenantID][0].Revisions); got != 3 {
		t.Fatalf("memory revision count=%d, want 3", got)
	}
	_, old, err := memory.findRevision(id, first.PublicationID, first.RevisionID)
	if err != nil || old.Skill.Steps[0] != "one" {
		t.Fatalf("memory predecessor=%+v err=%v", old, err)
	}

	raw, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	durable, err := NewStateStore(raw, "runtime-a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = durable.Close(ctx); _ = raw.Close(ctx) }()
	first, _, err = durable.Publish(ctx, id, PublishRequest{IdempotencyKey: "state-p", Name: "ops", Skill: publicationSkill("ops", "one"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err = durable.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "state-s1", PublicationID: first.PublicationID, ExpectedGeneration: first.Generation, ExpectedContentHash: first.ContentHash, Skill: publicationSkill("ops", "two")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := durable.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "state-s2", PublicationID: first.PublicationID, ExpectedGeneration: second.Generation, ExpectedContentHash: second.ContentHash, Skill: publicationSkill("ops", "three")}); err != nil {
		t.Fatal(err)
	}
	record, err := raw.Load(ctx, orgIdentity(id.TenantID), publicationOrgKind)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := decodeAggregate(record)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(aggregate.Publications[0].Revisions); got != 3 {
		t.Fatalf("state revision count=%d, want 3", got)
	}
}

func TestStateStoreStore_RemoveThenInstallAndConcurrentResolveN128(t *testing.T) {
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "user-a", "session-a")
	raw, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStateStore(raw, "runtime-a")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	installed, _, err := store.Install(ctx, id, InstallRequest{IdempotencyKey: "i", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove(ctx, id, RemoveRequest{IdempotencyKey: "remove", AgentID: installed.AgentID, ExpectedGeneration: installed.Generation, ExpectedContentHash: installed.ContentHash}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove(ctx, id, RemoveRequest{IdempotencyKey: "remove-again", AgentID: installed.AgentID, ExpectedGeneration: installed.Generation, ExpectedContentHash: installed.ContentHash}); !errors.Is(err, ErrReferenceNotFound) {
		t.Fatalf("remove after remove=%v want reference not found", err)
	}
	reinstalled, _, err := store.Install(ctx, id, InstallRequest{IdempotencyKey: "reinstall", AgentID: installed.AgentID, PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true})
	if err != nil {
		t.Fatalf("reinstall after remove: %v", err)
	}
	if reinstalled.RevisionID != installed.RevisionID {
		t.Fatalf("reinstalled reference=%+v", reinstalled)
	}
	errCh := make(chan error, 128)
	var wg sync.WaitGroup
	for range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, meta, resolveErr := store.Resolve(ctx, id, installed.AgentID)
			if resolveErr != nil {
				errCh <- resolveErr
				return
			}
			if body.ContentHash != meta.ContentHash {
				errCh <- ErrContentHashMismatch
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	_ = store.Close(ctx)
	_ = raw.Close(ctx)
}

func TestStateStoreStore_ResolveRejectsTamperedRevisionBody(t *testing.T) {
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "user-a", "session-a")
	raw, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStateStore(raw, "runtime-a")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Install(ctx, id, InstallRequest{IdempotencyKey: "i", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatal(err)
	}
	record, err := raw.Load(ctx, orgIdentity(id.TenantID), publicationOrgKind)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := decodeAggregate(record)
	if err != nil {
		t.Fatal(err)
	}
	aggregate.Publications[0].Skill.Steps[0] = "tampered"
	bytes, err := encode(aggregate)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.SaveIf(ctx, []state.SlotExpectation{{Identity: record.Identity, Kind: record.Kind, ExpectedEventID: record.ID}}, state.StateRecord{ID: state.NewEventID(), Identity: record.Identity, Kind: record.Kind, Bytes: bytes}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Resolve(ctx, id, "agent-a"); !errors.Is(err, ErrContentHashMismatch) {
		t.Fatalf("tampered resolve=%v want content hash mismatch", err)
	}
	_ = store.Close(ctx)
	_ = raw.Close(ctx)
}

func TestPublicationValidation_IdentityAndRequiredCAS(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore("runtime-a")
	if _, _, err := store.Publish(ctx, identity.Quadruple{}, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true}); !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("missing identity=%v", err)
	}
	id := publicationIdentity("t", "u", "s")
	if _, _, err := store.Publish(ctx, id, PublishRequest{Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing idempotency=%v", err)
	}
	if _, _, err := store.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "s", PublicationID: "x", Skill: publicationSkill("x", "do")}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing successor CAS=%v", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationValidation_CanonicalNameExtraAndOriginRef(t *testing.T) {
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "user-a", "session-a")
	store := NewMemoryStore("runtime-a")
	skill := publicationSkill("ops", "do")
	skill.OriginRef = "caller-controlled"
	pub, _, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "p", Name: "OPS", Skill: skill, ExpectedAbsent: true})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	body, _, err := store.Resolve(ctx, id, "missing")
	if !errors.Is(err, ErrReferenceNotFound) || body.Name != "" {
		t.Fatalf("unexpected missing reference result: body=%+v err=%v", body, err)
	}
	// Origin references are minted by the publication store and cannot be
	// supplied by a caller as an authority-bearing value.
	stored, _, err := store.findRevision(id, pub.PublicationID, pub.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Skill.OriginRef == skill.OriginRef || stored.Skill.OriginRef == "" || !strings.HasPrefix(stored.Skill.OriginRef, publicationOriginPrefix) {
		t.Fatalf("origin ref was not store-minted: %q", stored.Skill.OriginRef)
	}
	if _, _, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "bad-name", Name: "other", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("name mismatch=%v want invalid request", err)
	}
	invalidExtra := publicationSkill("extra", "do")
	invalidExtra.Extra = map[string]any{"count": 1}
	if _, _, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "bad-extra", Name: "extra", Skill: invalidExtra, ExpectedAbsent: true}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("non-string extra=%v want invalid request", err)
	}
}

func TestMemoryStore_IdempotencyReplayPreservesOriginalResult(t *testing.T) {
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "user-a", "session-a")
	store := NewMemoryStore("runtime-a")
	first, _, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "first"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "s", PublicationID: first.PublicationID, ExpectedGeneration: first.Generation, ExpectedContentHash: first.ContentHash, Skill: publicationSkill("ops", "second")})
	if err != nil {
		t.Fatal(err)
	}
	replayed, _, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "first"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.RevisionID != first.RevisionID || replayed.Generation != first.Generation || replayed.ContentHash != first.ContentHash {
		t.Fatalf("publish replay returned current result: first=%+v current=%+v replay=%+v", first, second, replayed)
	}
	installed, _, err := store.Install(ctx, id, InstallRequest{IdempotencyKey: "i", AgentID: "agent-a", PublicationID: first.PublicationID, RevisionID: first.RevisionID, ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	updated, updateReceipt, err := store.Update(ctx, id, UpdateRequest{IdempotencyKey: "u", AgentID: "agent-a", PublicationID: second.PublicationID, RevisionID: second.RevisionID, ExpectedGeneration: installed.Generation, ExpectedContentHash: installed.ContentHash})
	if err != nil {
		t.Fatal(err)
	}
	if updateReceipt.PublicationID != updated.PublicationID || updateReceipt.RevisionID != updated.RevisionID {
		t.Fatalf("update receipt omitted exact target ids: ref=%+v receipt=%+v", updated, updateReceipt)
	}
	replayedRef, _, err := store.Install(ctx, id, InstallRequest{IdempotencyKey: "i", AgentID: "agent-a", PublicationID: first.PublicationID, RevisionID: first.RevisionID, ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if replayedRef.RevisionID != installed.RevisionID || replayedRef.ContentHash != installed.ContentHash {
		t.Fatalf("install replay returned current reference: installed=%+v replay=%+v", installed, replayedRef)
	}
}

type commitThenErrorStore struct {
	state.StateStore
	failNext atomic.Bool
}

func (s *commitThenErrorStore) SaveIf(ctx context.Context, expect []state.SlotExpectation, next state.StateRecord) error {
	if err := s.StateStore.SaveIf(ctx, expect, next); err != nil {
		return err
	}
	if s.failNext.CompareAndSwap(true, false) {
		return errors.New("simulated response loss")
	}
	return nil
}

func TestStateStoreStore_ResponseLossAndUserScopedReferences(t *testing.T) {
	ctx := context.Background()
	firstSession := publicationIdentity("tenant-a", "user-a", "session-a")
	secondSession := publicationIdentity("tenant-a", "user-a", "session-b")
	raw, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &commitThenErrorStore{StateStore: raw}
	store, err := NewStateStore(wrapped, "runtime-a")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := store.Publish(ctx, firstSession, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	wrapped.failNext.Store(true)
	req := InstallRequest{IdempotencyKey: "i", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}
	if _, _, err := store.Install(ctx, firstSession, req); err == nil || !strings.Contains(err.Error(), "response loss") {
		t.Fatalf("install response-loss error=%v", err)
	}
	restarted, err := NewStateStore(raw, "runtime-a")
	if err != nil {
		t.Fatal(err)
	}
	replayed, _, err := restarted.Install(ctx, firstSession, req)
	if err != nil {
		t.Fatalf("reconcile install: %v", err)
	}
	if replayed.PublicationID != pub.PublicationID || replayed.RevisionID != pub.RevisionID {
		t.Fatalf("reconciled reference=%+v", replayed)
	}
	refs, err := restarted.ListReferences(ctx, secondSession)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].AgentID != "agent-a" {
		t.Fatalf("user-scoped references=%+v", refs)
	}
	if _, _, err := restarted.Resolve(ctx, secondSession, "agent-a"); err != nil {
		t.Fatalf("resolve from second session: %v", err)
	}
	_ = restarted.Close(ctx)
	_ = store.Close(ctx)
	_ = raw.Close(ctx)
}
