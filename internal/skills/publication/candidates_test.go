package publication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hurtener/Harbor/internal/state"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/skills"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	sqlitedriver "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func candidateStores(t *testing.T) map[string]CandidateStore {
	t.Helper()
	raw, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close(context.Background()) })
	inmemDurable, err := NewStateStoreCandidateStore(raw, "rt")
	if err != nil {
		t.Fatal(err)
	}
	sq, err := sqlitedriver.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "candidates.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sq.Close(context.Background()) })
	sqliteDurable, err := NewStateStoreCandidateStore(sq, "rt")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]CandidateStore{"memory": NewMemoryCandidateStore("rt"), "state-inmem": inmemDurable, "state-sqlite": sqliteDurable}
}

func submit(t *testing.T, s CandidateStore, user, key, name string) CandidateMetadata {
	t.Helper()
	sk := publicationSkill(name, "step for "+name)
	sk.Title, sk.Description = "Title "+name, "Describes "+name
	got, err := s.Submit(context.Background(), publicationIdentity("tenant-a", user, "s-"+user), SubmitCandidateRequest{IdempotencyKey: key, AgentID: "agent-a", Skill: sk, PackageHash: "v1:pkg-" + name, Note: "please"})
	if err != nil {
		t.Fatalf("submit %s: %v", name, err)
	}
	return got
}

func TestCandidateStore_Lifecycle(t *testing.T) {
	for name, store := range candidateStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			alice := publicationIdentity("tenant-a", "alice", "s-alice")
			bob := publicationIdentity("tenant-a", "bob", "s-bob")
			other := publicationIdentity("tenant-b", "alice", "s-alice")

			c := submit(t, store, "alice", "k1", "triage")
			if c.State != CandidatePending || c.Generation != 1 || c.SubmitterUser != "alice" || c.PackageHash != "v1:pkg-triage" || c.ContentHash == "" || c.Title != "Title triage" {
				t.Fatalf("candidate=%+v", c)
			}
			// Idempotent replay returns the same candidate; a divergent reuse conflicts.
			again := submit(t, store, "alice", "k1", "triage")
			if again.CandidateID != c.CandidateID {
				t.Fatalf("replay produced %s, want %s", again.CandidateID, c.CandidateID)
			}
			if _, err := store.Submit(ctx, alice, SubmitCandidateRequest{IdempotencyKey: "k1", AgentID: "agent-a", Skill: publicationSkill("triage", "different")}); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("divergent reuse err=%v", err)
			}
			// One pending candidate per submitter and name.
			if _, err := store.Submit(ctx, alice, SubmitCandidateRequest{IdempotencyKey: "k2", AgentID: "agent-a", Skill: publicationSkill("triage", "v2")}); !errors.Is(err, ErrCandidatePendingExists) {
				t.Fatalf("second pending err=%v", err)
			}
			// Another person may submit the same name.
			bc := submit(t, store, "bob", "k1", "triage")

			// Visibility: submitters see their own; the admin view sees all; other tenants see nothing.
			if mine, _ := store.List(ctx, alice, ListCandidatesRequest{}); len(mine) != 1 || mine[0].CandidateID != c.CandidateID {
				t.Fatalf("alice list=%+v", mine)
			}
			if all, _ := store.List(ctx, alice, ListCandidatesRequest{All: true}); len(all) != 2 {
				t.Fatalf("admin list=%+v", all)
			}
			if foreign, _ := store.List(ctx, other, ListCandidatesRequest{All: true}); len(foreign) != 0 {
				t.Fatalf("cross-tenant list=%+v", foreign)
			}
			if _, err := store.Read(ctx, bob, c.CandidateID, false); !errors.Is(err, ErrCandidateNotFound) {
				t.Fatalf("other user read err=%v", err)
			}
			if _, err := store.Read(ctx, other, c.CandidateID, true); !errors.Is(err, ErrCandidateNotFound) {
				t.Fatalf("cross-tenant read err=%v", err)
			}
			full, err := store.Read(ctx, alice, c.CandidateID, false)
			if err != nil || full.Skill.Steps[0] != "step for triage" {
				t.Fatalf("own read=%+v err=%v", full, err)
			}

			// Withdraw is the submitter's; reject is the admin path.
			if _, err := store.Decide(ctx, bob, DecideCandidateRequest{IdempotencyKey: "w", CandidateID: c.CandidateID, ExpectedGeneration: 1, Outcome: CandidateWithdrawn}, false); !errors.Is(err, ErrCandidateNotFound) {
				t.Fatalf("foreign withdraw err=%v", err)
			}
			rej, err := store.Decide(ctx, alice, DecideCandidateRequest{IdempotencyKey: "r", CandidateID: bc.CandidateID, ExpectedGeneration: 1, Outcome: CandidateRejected, DecisionNote: "duplicate"}, true)
			if err != nil || rej.State != CandidateRejected || rej.DecidedBy != "alice" || rej.Generation != 2 {
				t.Fatalf("reject=%+v err=%v", rej, err)
			}
			if again, err := store.Decide(ctx, alice, DecideCandidateRequest{IdempotencyKey: "r", CandidateID: bc.CandidateID, ExpectedGeneration: 1, Outcome: CandidateRejected, DecisionNote: "duplicate"}, true); err != nil || again.Generation != 2 {
				t.Fatalf("reject replay=%+v err=%v", again, err)
			}

			// Claim → finalize; stale generation and divergent claims conflict.
			if _, err := store.Claim(ctx, alice, c.CandidateID, 7, AcceptClaim{Mode: AcceptModeNew}); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale claim err=%v", err)
			}
			claimed, err := store.Claim(ctx, alice, c.CandidateID, 1, AcceptClaim{Mode: AcceptModeNew, DecisionNote: "ok"})
			if err != nil || claimed.Claim == nil || claimed.Claim.Name != "triage" {
				t.Fatalf("claim=%+v err=%v", claimed, err)
			}
			if _, err := store.Claim(ctx, bob, c.CandidateID, 1, AcceptClaim{Mode: AcceptModeNew, Name: "other"}); !errors.Is(err, ErrConflict) {
				t.Fatalf("divergent claim err=%v", err)
			}
			if resumed, err := store.Claim(ctx, bob, c.CandidateID, 1, AcceptClaim{Mode: AcceptModeNew, Name: "triage", DecisionNote: "ok"}); err != nil || resumed.Claim == nil {
				t.Fatalf("resume claim=%+v err=%v", resumed, err)
			}
			if _, err := store.Decide(ctx, alice, DecideCandidateRequest{IdempotencyKey: "r2", CandidateID: c.CandidateID, ExpectedGeneration: 1, Outcome: CandidateRejected}, true); !errors.Is(err, ErrConflict) {
				t.Fatalf("reject while claimed err=%v", err)
			}
			done, err := store.FinalizeAccept(ctx, bob, c.CandidateID, AcceptResult{PublicationID: "pub-1", RevisionID: "rev-1", Receipt: Receipt{Operation: "publish"}})
			if err != nil || done.State != CandidateAccepted || done.PublicationID != "pub-1" || done.DecidedBy != "bob" || done.DecisionNote != "ok" || done.Generation != 2 {
				t.Fatalf("finalize=%+v err=%v", done, err)
			}
			if replay, err := store.Claim(ctx, alice, c.CandidateID, 1, AcceptClaim{Mode: AcceptModeNew, Name: "triage", DecisionNote: "ok"}); err != nil || replay.State != CandidateAccepted {
				t.Fatalf("accepted replay=%+v err=%v", replay, err)
			}
			// After a decision the submitter may submit the same name again.
			submit(t, store, "alice", "k3", "triage")
		})
	}
}

func TestCandidateStore_ValidatesInput(t *testing.T) {
	store := NewMemoryCandidateStore("rt")
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "alice", "s")
	long := make([]rune, maxCandidateNoteRunes+1)
	for i := range long {
		long[i] = 'x'
	}
	cases := []SubmitCandidateRequest{
		{IdempotencyKey: "", AgentID: "a", Skill: publicationSkill("x", "y")},
		{IdempotencyKey: "k", AgentID: "", Skill: publicationSkill("x", "y")},
		{IdempotencyKey: "k", AgentID: "a", Skill: skills.Skill{Name: "x"}},
		{IdempotencyKey: "k", AgentID: "a", Skill: publicationSkill("x", "y"), Note: string(long)},
	}
	for i, req := range cases {
		if _, err := store.Submit(ctx, id, req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("case %d err=%v want invalid", i, err)
		}
	}
	if _, err := store.Claim(ctx, id, "missing", 1, AcceptClaim{Mode: "bogus"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("bad mode err=%v", err)
	}
	if _, err := store.Claim(ctx, id, "missing", 1, AcceptClaim{Mode: AcceptModeSuccessor, PublicationID: "p"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("incomplete successor err=%v", err)
	}
	if _, err := store.List(ctx, id, ListCandidatesRequest{State: "nope"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("bad state err=%v", err)
	}
}

func TestCandidateStore_ConcurrentSubmitsAndClaims(t *testing.T) {
	for name, store := range candidateStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var wg sync.WaitGroup
			for i := 0; i < 100; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					user := fmt.Sprintf("u%d", i%10)
					_, _ = store.Submit(ctx, publicationIdentity("tenant-a", user, "s"), SubmitCandidateRequest{IdempotencyKey: fmt.Sprintf("k%d", i), AgentID: "a", Skill: publicationSkill(fmt.Sprintf("s%d", i), "x")})
				}(i)
			}
			wg.Wait()
			all, err := store.List(ctx, publicationIdentity("tenant-a", "admin", "s"), ListCandidatesRequest{All: true})
			if err != nil || len(all) != 100 {
				t.Fatalf("listed %d err=%v", len(all), err)
			}
			// Exactly one of N concurrent distinct claims wins.
			target := all[0]
			var wins atomic.Int32
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if _, err := store.Claim(ctx, publicationIdentity("tenant-a", "admin", "s"), target.CandidateID, 1, AcceptClaim{Mode: AcceptModeNew, Name: fmt.Sprintf("n%d", i)}); err == nil {
						wins.Add(1)
					}
				}(i)
			}
			wg.Wait()
			if wins.Load() != 1 {
				t.Fatalf("claim winners=%d want 1", wins.Load())
			}
		})
	}
}

func TestStore_ReadVisibility(t *testing.T) {
	ctx := context.Background()
	id := publicationIdentity("tenant-a", "admin", "s")
	store := NewMemoryStore("rt")
	pub, _, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "p", Name: "guide", Skill: publicationSkill("guide", "v1"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := store.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "s", PublicationID: pub.PublicationID, ExpectedGeneration: 1, ExpectedContentHash: pub.ContentHash, Skill: publicationSkill("guide", "v2")})
	if err != nil {
		t.Fatal(err)
	}
	body, meta, err := store.Read(ctx, id, ReadRequest{PublicationID: pub.PublicationID})
	if err != nil || body.Steps[0] != "v2" || meta.RevisionID != next.RevisionID {
		t.Fatalf("current read=%+v %+v err=%v", body, meta, err)
	}
	if _, _, err := store.Read(ctx, id, ReadRequest{PublicationID: pub.PublicationID, RevisionID: pub.RevisionID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-admin earlier revision err=%v", err)
	}
	old, _, err := store.Read(ctx, id, ReadRequest{PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, AnyState: true})
	if err != nil || old.Steps[0] != "v1" {
		t.Fatalf("admin earlier revision=%+v err=%v", old, err)
	}
	if _, _, err := store.Retire(ctx, id, RetireRequest{IdempotencyKey: "r", PublicationID: pub.PublicationID, ExpectedGeneration: next.Generation, ExpectedContentHash: next.ContentHash}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Read(ctx, id, ReadRequest{PublicationID: pub.PublicationID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-admin retired err=%v", err)
	}
	if _, _, err := store.Read(ctx, id, ReadRequest{PublicationID: pub.PublicationID, AnyState: true}); err != nil {
		t.Fatalf("admin retired err=%v", err)
	}
	if _, _, err := store.Read(ctx, publicationIdentity("tenant-b", "admin", "s"), ReadRequest{PublicationID: pub.PublicationID, AnyState: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant err=%v", err)
	}
}

func TestCandidateClaimReleaseAndOutstanding(t *testing.T) {
	for name, store := range candidateStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			admin := publicationIdentity("tenant-a", "admin", "s-admin")
			meta := submit(t, store, "alice", "k1", "triage")
			first, err := store.Claim(ctx, admin, meta.CandidateID, 1, AcceptClaim{Mode: AcceptModeNew})
			if err != nil || first.Claim == nil || first.Claim.ClaimedAt.IsZero() || first.Claim.Seq != 1 {
				t.Fatalf("claim=%+v err=%v", first, err)
			}
			// A different target while the claim is outstanding is refused as such.
			if _, err := store.Claim(ctx, admin, meta.CandidateID, 1, AcceptClaim{Mode: AcceptModeNew, Name: "other"}); !errors.Is(err, ErrClaimOutstanding) || !errors.Is(err, ErrConflict) {
				t.Fatalf("outstanding err=%v", err)
			}
			// The same target resumes the original claim even with a new note.
			again, err := store.Claim(ctx, admin, meta.CandidateID, 1, AcceptClaim{Mode: AcceptModeNew, DecisionNote: "x"})
			if err != nil || again.Claim.Seq != first.Claim.Seq || again.Claim.DecisionNote != "" {
				t.Fatalf("resume=%+v err=%v", again.Claim, err)
			}
			// Releasing another claim time is a no-op; releasing the recorded one clears it.
			if err := store.ReleaseClaim(ctx, admin, meta.CandidateID, first.Claim.Seq+1); err != nil {
				t.Fatal(err)
			}
			if got, _ := store.Read(ctx, admin, meta.CandidateID, true); got.Claim == nil {
				t.Fatal("mismatched release cleared the claim")
			}
			if err := store.ReleaseClaim(ctx, admin, meta.CandidateID, first.Claim.Seq); err != nil {
				t.Fatal(err)
			}
			got, _ := store.Read(ctx, admin, meta.CandidateID, true)
			if got.Claim != nil || got.State != CandidatePending || got.Generation != 1 {
				t.Fatalf("released=%+v", got)
			}
			if _, err := store.Decide(ctx, admin, DecideCandidateRequest{IdempotencyKey: "r", CandidateID: meta.CandidateID, ExpectedGeneration: 1, Outcome: CandidateRejected}, true); err != nil {
				t.Fatalf("reject after release: %v", err)
			}
		})
	}
}

// Decided candidates never count toward the tenant quota, and only one
// person's open requests can't fill it.
func TestCandidateQuotaCountsOnlyPendingAndPrunesHistory(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryCandidateStore("rt")
	alice := publicationIdentity("tenant-a", "alice", "s-alice")
	seen := map[string]bool{}
	for i := 0; i < maxRetainedTerminal+50; i++ {
		c := submit(t, store, "alice", fmt.Sprintf("k-%d", i), "triage")
		if seen[c.CandidateID] {
			t.Fatalf("candidate id reused after pruning: %s", c.CandidateID)
		}
		seen[c.CandidateID] = true
		if _, err := store.Decide(ctx, alice, DecideCandidateRequest{IdempotencyKey: fmt.Sprintf("w-%d", i), CandidateID: c.CandidateID, ExpectedGeneration: 1, Outcome: CandidateWithdrawn}, false); err != nil {
			t.Fatalf("withdraw %d: %v", i, err)
		}
	}
	// Another person can still submit.
	submit(t, store, "bob", "k", "notes")
	all, err := store.List(ctx, publicationIdentity("tenant-a", "admin", "s-admin"), ListCandidatesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) > maxRetainedTerminal+1 {
		t.Fatalf("history not pruned: %d candidates", len(all))
	}
	// One person's open requests are capped.
	for i := 0; i < maxPendingPerUser; i++ {
		submit(t, store, "carol", fmt.Sprintf("p-%d", i), fmt.Sprintf("skill-%d", i))
	}
	sk := publicationSkill("one-more", "step")
	if _, err := store.Submit(ctx, publicationIdentity("tenant-a", "carol", "s-carol"), SubmitCandidateRequest{IdempotencyKey: "over", AgentID: "agent-a", Skill: sk}); !errors.Is(err, ErrConflict) {
		t.Fatalf("per-user cap err=%v", err)
	}
	submit(t, store, "dave", "k", "notes")
}

// A publish whose acceptance was revoked (its claim resolved as stale) can
// never land, even if it reaches the store after a newer acceptance.
func TestCandidateFenceRejectsARevokedPublish(t *testing.T) {
	raw, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close(context.Background()) })
	durable, err := NewStateStore(raw, "rt")
	if err != nil {
		t.Fatal(err)
	}
	for name, store := range map[string]Store{"memory": NewMemoryStore("rt"), "state": durable} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			admin := publicationIdentity("tenant-a", "admin", "s-admin")
			t1, err := store.FenceCandidate(ctx, admin, "cand-1", 0)
			if err != nil || t1 != 1 {
				t.Fatalf("first fence=%d err=%v", t1, err)
			}
			if _, err := store.FenceCandidate(ctx, admin, "cand-1", 0); !errors.Is(err, ErrConflict) {
				t.Fatalf("second live fence err=%v", err)
			}
			res, err := store.ResolveCandidate(ctx, admin, "cand-1")
			if err != nil || res.Applied {
				t.Fatalf("resolve=%+v err=%v", res, err)
			}
			t2, err := store.FenceCandidate(ctx, admin, "cand-1", 0)
			if err != nil || t2 != 2 {
				t.Fatalf("new fence after revoke=%d err=%v", t2, err)
			}
			sk := publicationSkill("triage", "v1")
			sk.Extra = map[string]any{CandidateMarkerKey: "cand-1"}
			// The old acceptance's publish arrives late: refused at commit.
			if _, _, err := store.Publish(ctx, admin, PublishRequest{IdempotencyKey: "old", Name: "triage", Skill: sk, ExpectedAbsent: true, Candidate: &CandidateFence{CandidateID: "cand-1", Token: t1}}); !errors.Is(err, ErrCandidateFenced) {
				t.Fatalf("revoked publish err=%v", err)
			}
			if pubs, _ := store.List(ctx, admin); len(pubs) != 0 {
				t.Fatalf("revoked publish landed: %+v", pubs)
			}
			// The live acceptance publishes once; a repeat by another caller is
			// the same revision, never a second publication.
			first, _, err := store.Publish(ctx, admin, PublishRequest{IdempotencyKey: "new", Name: "triage-2", Skill: withName(sk, "triage-2"), ExpectedAbsent: true, Candidate: &CandidateFence{CandidateID: "cand-1", Token: t2}})
			if err != nil {
				t.Fatal(err)
			}
			other := publicationIdentity("tenant-a", "admin2", "s-admin2")
			again, _, err := store.Publish(ctx, other, PublishRequest{IdempotencyKey: "new", Name: "triage-2", Skill: withName(sk, "triage-2"), ExpectedAbsent: true, Candidate: &CandidateFence{CandidateID: "cand-1", Token: t2}})
			if err != nil || again.PublicationID != first.PublicationID {
				t.Fatalf("repeat=%+v err=%v", again, err)
			}
			// Once applied, resolution reports it and never revokes.
			res, err = store.ResolveCandidate(ctx, admin, "cand-1")
			if err != nil || !res.Applied || res.Publication.PublicationID != first.PublicationID {
				t.Fatalf("applied resolve=%+v err=%v", res, err)
			}
			if pubs, _ := store.List(ctx, admin); len(pubs) != 1 {
				t.Fatalf("publications=%+v", pubs)
			}
		})
	}
}

func withName(sk skills.Skill, name string) skills.Skill {
	sk.Name = name
	return sk
}

// An ordinary publish cannot plant the candidate marker (which would make a
// pending candidate adopt another body), and a candidate publish may carry
// only its own marker.
func TestPublishRefusesReservedMarker(t *testing.T) {
	raw, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close(context.Background()) })
	durable, err := NewStateStore(raw, "rt")
	if err != nil {
		t.Fatal(err)
	}
	for name, store := range map[string]Store{"memory": NewMemoryStore("rt"), "state": durable} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			admin := publicationIdentity("tenant-a", "admin", "s-admin")
			planted := publicationSkill("triage", "v1")
			planted.Extra = map[string]any{CandidateMarkerKey: "cand-1"}
			if _, _, err := store.Publish(ctx, admin, PublishRequest{IdempotencyKey: "p", Name: "triage", Skill: planted, ExpectedAbsent: true}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("planted marker on publish err=%v", err)
			}
			// Only the marker is reserved: other underscore keys earlier
			// releases accepted keep working.
			other := publicationSkill("triage", "v1")
			other.Extra = map[string]any{"_publication.source": "existing metadata"}
			clean, _, err := store.Publish(ctx, admin, PublishRequest{IdempotencyKey: "p3", Name: "triage", Skill: other, ExpectedAbsent: true})
			if err != nil {
				t.Fatalf("legacy underscore key on publish: %v", err)
			}
			if _, _, err := store.PublishSuccessor(ctx, admin, SuccessorRequest{IdempotencyKey: "s", PublicationID: clean.PublicationID, ExpectedGeneration: clean.Generation, ExpectedContentHash: clean.ContentHash, Skill: planted}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("planted marker on successor err=%v", err)
			}
			// A candidate publish naming another candidate is refused too.
			tok, err := store.FenceCandidate(ctx, admin, "cand-2", 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Publish(ctx, admin, PublishRequest{IdempotencyKey: "c", Name: "other", Skill: withName(planted, "other"), ExpectedAbsent: true, Candidate: &CandidateFence{CandidateID: "cand-2", Token: tok}}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("foreign marker on candidate publish err=%v", err)
			}
			// Nothing planted means nothing to adopt.
			if res, err := store.ResolveCandidate(ctx, admin, "cand-1"); err != nil || res.Applied {
				t.Fatalf("resolve=%+v err=%v", res, err)
			}
		})
	}
}

// Paused-accept regression at the store: integer tokens; a revoked token is
// permanent, a resume must present the exact live token, and a new
// acceptance gets max(live, revoked)+1 (memory and durable stores).
func TestCandidateFenceRevokedTokenIsPermanent(t *testing.T) {
	for name, store := range fenceStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			admin := publicationIdentity("tenant-a", "admin", "s-admin")
			t1, err := store.FenceCandidate(ctx, admin, "cand-1", 0)
			if err != nil || t1 != 1 {
				t.Fatalf("t1=%d err=%v", t1, err)
			}
			if again, err := store.FenceCandidate(ctx, admin, "cand-1", t1); err != nil || again != t1 {
				t.Fatalf("resume=%d err=%v", again, err)
			}
			// A rejection resolves it before the paused accept resumes.
			if res, err := store.ResolveCandidate(ctx, admin, "cand-1"); err != nil || res.Applied {
				t.Fatalf("resolve=%+v err=%v", res, err)
			}
			if _, err := store.FenceCandidate(ctx, admin, "cand-1", t1); !errors.Is(err, ErrCandidateFenced) {
				t.Fatalf("revoked token resumed: %v", err)
			}
			t2, err := store.FenceCandidate(ctx, admin, "cand-1", 0)
			if err != nil || t2 != t1+1 {
				t.Fatalf("t2=%d err=%v", t2, err)
			}
			if _, err := store.FenceCandidate(ctx, admin, "cand-1", t1); !errors.Is(err, ErrCandidateFenced) {
				t.Fatalf("older token after newer: %v", err)
			}
		})
	}
}

func fenceStores(t *testing.T) map[string]Store {
	t.Helper()
	raw, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close(context.Background()) })
	durable, err := NewStateStore(raw, "rt")
	if err != nil {
		t.Fatal(err)
	}
	sq, err := sqlitedriver.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "fences.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sq.Close(context.Background()) })
	sqlite, err := NewStateStore(sq, "rt")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Store{"memory": NewMemoryStore("rt"), "state-inmem": durable, "state-sqlite": sqlite}
}

// Upgrade: a publication aggregate persisted by an earlier release (no fence
// map, an underscore Extra key such earlier releases accepted from callers)
// keeps working: reads, successors and a candidate acceptance of it.
func TestPublicationUpgradeFromLegacyRecord(t *testing.T) {
	sq, err := sqlitedriver.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "legacy.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sq.Close(context.Background()) })
	ctx := context.Background()
	admin := publicationIdentity("tenant-a", "admin", "s-admin")
	legacy := publicationSkill("weekly", "v1")
	legacy.Extra = map[string]any{"_publication.source": "existing metadata"}
	legacy.ContentHash = skills.CanonicalContentHash(legacy)
	// A record shaped exactly as earlier releases persisted it: no
	// candidate_fences, no candidate marker.
	p := Publication{PublicationID: "pub-legacy", RevisionID: "rev-legacy", Name: "weekly", ContentHash: legacy.ContentHash, State: StateActive, Generation: 1, RuntimeID: "rt", Skill: legacy}
	p.Revisions = []Revision{currentRevision(p)}
	b, err := json.Marshal(map[string]any{"publications": []Publication{p}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sq.SaveIf(ctx, []state.SlotExpectation{{Identity: orgIdentity("tenant-a"), Kind: publicationOrgKind}}, state.StateRecord{ID: state.NewEventID(), Identity: orgIdentity("tenant-a"), Kind: publicationOrgKind, Bytes: b}); err != nil {
		t.Fatal(err)
	}
	store, err := NewStateStore(sq, "rt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, admin, "pub-legacy")
	if err != nil {
		t.Fatal(err)
	}
	next := publicationSkill("weekly", "v2")
	next.Extra = map[string]any{"_publication.source": "existing metadata"}
	succ, _, err := store.PublishSuccessor(ctx, admin, SuccessorRequest{IdempotencyKey: "s", PublicationID: got.PublicationID, ExpectedGeneration: got.Generation, ExpectedContentHash: got.ContentHash, Skill: next})
	if err != nil || succ.Generation != 2 {
		t.Fatalf("successor on legacy record=%+v err=%v", succ, err)
	}
	// A candidate whose snapshot carries the legacy key publishes too.
	tok, err := store.FenceCandidate(ctx, admin, "cand-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	cand := publicationSkill("weekly", "v3")
	cand.Extra = map[string]any{"_publication.source": "existing metadata", CandidateMarkerKey: "cand-1"}
	if _, _, err := store.PublishSuccessor(ctx, admin, SuccessorRequest{IdempotencyKey: "c", PublicationID: succ.PublicationID, ExpectedGeneration: succ.Generation, ExpectedContentHash: succ.ContentHash, Skill: cand, Candidate: &CandidateFence{CandidateID: "cand-1", Token: tok}}); err != nil {
		t.Fatalf("candidate successor with legacy key: %v", err)
	}
}

// Fences survive a SQLite reopen mid-claim: the resume presents its exact
// integer token and resolution still revokes it.
func TestCandidateFenceSurvivesSQLiteReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "reopen.sqlite")
	admin := publicationIdentity("tenant-a", "admin", "s-admin")
	open := func() (*StateStoreStore, func()) {
		sq, err := sqlitedriver.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		st, err := NewStateStore(sq, "rt")
		if err != nil {
			t.Fatal(err)
		}
		return st, func() { _ = sq.Close(ctx) }
	}
	first, closeFirst := open()
	tok, err := first.FenceCandidate(ctx, admin, "cand-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	closeFirst()
	second, closeSecond := open()
	defer closeSecond()
	if again, err := second.FenceCandidate(ctx, admin, "cand-1", tok); err != nil || again != tok {
		t.Fatalf("resume after reopen=%d err=%v", again, err)
	}
	sk := publicationSkill("triage", "v1")
	sk.Extra = map[string]any{CandidateMarkerKey: "cand-1"}
	if _, _, err := second.Publish(ctx, admin, PublishRequest{IdempotencyKey: "p", Name: "triage", Skill: sk, ExpectedAbsent: true, Candidate: &CandidateFence{CandidateID: "cand-1", Token: tok}}); err != nil {
		t.Fatalf("publish after reopen: %v", err)
	}
	if res, err := second.ResolveCandidate(ctx, admin, "cand-1"); err != nil || !res.Applied {
		t.Fatalf("resolve after reopen=%+v err=%v", res, err)
	}
}

// An interim build's string fence is legacy: ignored, so the candidate is
// unfenced — revoked and resolvable — and a new acceptance fences normally.
func TestCandidateFenceLegacyStringFenceIsResolvable(t *testing.T) {
	sq, err := sqlitedriver.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "legacy-fence.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sq.Close(context.Background()) })
	ctx := context.Background()
	admin := publicationIdentity("tenant-a", "admin", "s-admin")
	b, err := json.Marshal(map[string]any{"publications": []Publication{}, "candidate_fences": map[string]string{"cand-1": "2026-11-13T10:00:00.1Z"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sq.SaveIf(ctx, []state.SlotExpectation{{Identity: orgIdentity("tenant-a"), Kind: publicationOrgKind}}, state.StateRecord{ID: state.NewEventID(), Identity: orgIdentity("tenant-a"), Kind: publicationOrgKind, Bytes: b}); err != nil {
		t.Fatal(err)
	}
	store, err := NewStateStore(sq, "rt")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := store.ResolveCandidate(ctx, admin, "cand-1"); err != nil || res.Applied {
		t.Fatalf("legacy resolve=%+v err=%v", res, err)
	}
	if tok, err := store.FenceCandidate(ctx, admin, "cand-1", 0); err != nil || tok == 0 {
		t.Fatalf("fence after legacy=%d err=%v", tok, err)
	}
}
