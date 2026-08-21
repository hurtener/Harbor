package serve

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/publication"
)

func runPublicationSkill(name, step string) skills.Skill {
	return skills.Skill{Name: name, Trigger: "when " + name, Steps: []string{step}, Origin: skills.OriginGenerated, Scope: skills.ScopeUser}
}

func publicationRunContext(t *testing.T, q identity.Quadruple, agentID string) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), q.Identity)
	if err != nil {
		t.Fatal(err)
	}
	return auth.WithAgentReach(ctx, []string{agentID})
}

func TestRunLoopDriver_PublicationSnapshotUsesOnePinnedReader(t *testing.T) {
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "publication-t", UserID: "publication-u", SessionID: "publication-s"}, RunID: "publication-run"}
	st := runSnapshotState(t)
	activateRunSnapshotAgent(t, st, q, "agent-a")
	driver, _ := newRunSnapshotDriver(t, nil, &runSnapshotReader{}, st)
	store := publication.NewMemoryStore("runtime-a")
	caller := identity.Quadruple{Identity: q.Identity}
	pub, _, err := store.Publish(context.Background(), caller, publication.PublishRequest{IdempotencyKey: "publish", Name: "published", Skill: runPublicationSkill("published", "do published"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Install(context.Background(), caller, publication.InstallRequest{IdempotencyKey: "install", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatal(err)
	}
	driver.publicationStore = store
	driver.publicationRuntimeID = "runtime-a"
	snapshot, ok, err := driver.captureRunSkillSnapshot(publicationRunContext(t, q, "agent-a"), "agent-a", q, nil)
	if err != nil || !ok {
		t.Fatalf("capture publication snapshot ok=%t err=%v", ok, err)
	}
	if source, found := snapshot.OperatorTierSource("published"); !found || source != skills.OperatorTierSourceRevision {
		t.Fatalf("publication provenance source=%q found=%t", source, found)
	}
	ctx := withRunSnapshot(t, q, snapshot)
	reader, err := skills.ResolveSkillReader(ctx, q, &runSnapshotReader{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Get(ctx, q, "published")
	if err != nil {
		t.Fatalf("snapshot get: %v", err)
	}
	if got.Steps[0] != "do published" || got.Origin != skills.OriginPack || got.Scope != skills.ScopeTenant || !strings.HasPrefix(got.OriginRef, "skills.publications.origin.v1.") {
		t.Fatalf("resolved publication body=%+v", got)
	}
	listed, err := reader.List(ctx, q, skills.ListFilter{})
	if err != nil {
		t.Fatalf("snapshot list: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "published" {
		t.Fatalf("snapshot list=%+v", listed)
	}
	searched, err := reader.Search(ctx, q, "published", 20)
	if err != nil {
		t.Fatalf("snapshot search: %v", err)
	}
	if len(searched) != 1 || searched[0].Skill.Name != "published" {
		t.Fatalf("snapshot search=%+v", searched)
	}

	// A runtime-binding change after publication must fail at the run-start
	// resolution gate; the resolver is never built from a foreign body.
	driver.publicationRuntimeID = "runtime-b"
	if _, _, err := driver.captureRunSkillSnapshot(publicationRunContext(t, q, "agent-a"), "agent-a", q, nil); !errors.Is(err, publication.ErrRuntimeMismatch) {
		t.Fatalf("foreign runtime capture=%v want mismatch", err)
	}
}

func TestRunLoopDriver_PublicationSnapshot_FailsClosedOnRetireAndAllowsMissingReference(t *testing.T) {
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "publication-t", UserID: "publication-u", SessionID: "publication-s"}, RunID: "publication-run"}
	st := runSnapshotState(t)
	activateRunSnapshotAgent(t, st, q, "agent-without-reference")
	activateRunSnapshotAgent(t, st, q, "agent-retiring")
	driver, _ := newRunSnapshotDriver(t, nil, &runSnapshotReader{}, st)
	store := publication.NewMemoryStore("runtime-a")
	driver.publicationStore = store
	driver.publicationRuntimeID = "runtime-a"
	missing, ok, err := driver.captureRunSkillSnapshot(publicationRunContext(t, q, "agent-without-reference"), "agent-without-reference", q, nil)
	if err != nil || !ok || missing.HasOperatorTier() {
		t.Fatalf("missing reference capture snapshot=%+v ok=%t err=%v", missing, ok, err)
	}
	caller := identity.Quadruple{Identity: q.Identity}
	pub, _, err := store.Publish(context.Background(), caller, publication.PublishRequest{IdempotencyKey: "publish", Name: "retiring", Skill: runPublicationSkill("retiring", "do retiring"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Install(context.Background(), caller, publication.InstallRequest{IdempotencyKey: "install", AgentID: "agent-retiring", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Retire(context.Background(), caller, publication.RetireRequest{IdempotencyKey: "retire", PublicationID: pub.PublicationID, ExpectedGeneration: pub.Generation, ExpectedContentHash: pub.ContentHash}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := driver.captureRunSkillSnapshot(publicationRunContext(t, q, "agent-retiring"), "agent-retiring", q, nil); !errors.Is(err, publication.ErrRetired) {
		t.Fatalf("retired capture=%v want retired", err)
	}
}

func TestRunLoopDriver_PublicationSnapshot_ConcurrentTupleIsolationN128(t *testing.T) {
	const runs = 128
	base := &runSnapshotReader{}
	st := runSnapshotState(t)
	driver, _ := newRunSnapshotDriver(t, nil, base, st)
	store := publication.NewMemoryStore("runtime-a")
	owner := identity.Quadruple{Identity: identity.Identity{TenantID: "publication-isolation", UserID: "publication-user", SessionID: "owner"}}
	pub, _, err := store.Publish(context.Background(), owner, publication.PublishRequest{IdempotencyKey: "publish", Name: "shared", Skill: runPublicationSkill("shared", "same body"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Install(context.Background(), owner, publication.InstallRequest{IdempotencyKey: "install", AgentID: "agent-shared", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatal(err)
	}
	driver.publicationStore = store
	driver.publicationRuntimeID = "runtime-a"
	errCh := make(chan error, runs)
	var wg sync.WaitGroup
	for i := range runs {
		q := identity.Quadruple{Identity: identity.Identity{TenantID: owner.TenantID, UserID: owner.UserID, SessionID: "session-" + string(rune('A'+i))}, RunID: "run-" + string(rune('A'+i))}
		agentID := "agent-shared-" + string(rune('A'+i))
		activateRunSnapshotAgent(t, st, q, agentID)
		if _, _, err := store.Install(context.Background(), owner, publication.InstallRequest{IdempotencyKey: "install-" + agentID, AgentID: agentID, PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(q identity.Quadruple, agentID string) {
			defer wg.Done()
			snapshot, ok, captureErr := driver.captureRunSkillSnapshot(publicationRunContext(t, q, agentID), agentID, q, nil)
			if captureErr != nil || !ok {
				errCh <- captureErr
				if captureErr == nil {
					errCh <- errors.New("publication snapshot missing")
				}
				return
			}
			reader, resolveErr := skills.ResolveSkillReader(withRunSnapshot(t, q, snapshot), q, base)
			if resolveErr != nil {
				errCh <- resolveErr
				return
			}
			body, getErr := reader.Get(context.Background(), q, "shared")
			if getErr != nil {
				errCh <- getErr
				return
			}
			if body.Steps[0] != "same body" {
				errCh <- errors.New("publication body changed across run tuples")
			}
		}(q, agentID)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}
