package publication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	sqlitedriver "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// candidateEnv is a candidate store and a publication store over one
// StateStore, as serve wires them.
type candidateEnv struct {
	st    state.StateStore
	cands CandidateStore
	pubs  *StateStoreStore
}

func newCandidateEnv(t *testing.T, driver string) candidateEnv {
	t.Helper()
	var (
		st  state.StateStore
		err error
	)
	if driver == "sqlite" {
		st, err = sqlitedriver.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "candidates.sqlite")})
	} else {
		st, err = stateinmem.New(config.StateConfig{})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	cands, err := NewStateStoreCandidateStore(st, "rt")
	if err != nil {
		t.Fatal(err)
	}
	pubs, err := NewStateStore(st, "rt")
	if err != nil {
		t.Fatal(err)
	}
	return candidateEnv{st: st, cands: cands, pubs: pubs}
}

func candidateEnvs(t *testing.T) map[string]candidateEnv {
	return map[string]candidateEnv{"state-inmem": newCandidateEnv(t, "inmem"), "state-sqlite": newCandidateEnv(t, "sqlite")}
}

// adminCtx is a verified context holding the admin scope for caller.
func adminCtx(t *testing.T, caller identity.Quadruple) context.Context {
	return authorizedContext(t, caller.Identity, nil, auth.ScopeAdmin)
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
	for name, env := range candidateEnvs(t) {
		t.Run(name, func(t *testing.T) {
			store := env.cands
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

			// Accept: a stale generation conflicts; another tenant cannot see it.
			if _, err := store.Accept(adminCtx(t, bob), bob, AcceptCandidateRequest{IdempotencyKey: "a0", CandidateID: c.CandidateID, ExpectedGeneration: 7, Mode: AcceptModeNew}); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale accept err=%v", err)
			}
			if _, err := store.Accept(adminCtx(t, other), other, AcceptCandidateRequest{IdempotencyKey: "a0", CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeNew}); !errors.Is(err, ErrCandidateNotFound) {
				t.Fatalf("cross-tenant accept err=%v", err)
			}
			done, err := store.Accept(adminCtx(t, bob), bob, AcceptCandidateRequest{IdempotencyKey: "a1", CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeNew, DecisionNote: "ok"})
			if err != nil || done.Candidate.State != CandidateAccepted || done.Candidate.DecidedBy != "bob" || done.Candidate.DecisionNote != "ok" || done.Candidate.Generation != 2 {
				t.Fatalf("accept=%+v err=%v", done, err)
			}
			if done.Publication.Name != "triage" || done.Candidate.PublicationID != done.Publication.PublicationID || done.Candidate.RevisionID != done.Publication.RevisionID || done.Receipt.Operation != "publish" || done.Receipt.OperationID != "a1" {
				t.Fatalf("accept result=%+v", done)
			}
			// The publication is in the same tenant's catalog, not another's.
			if body, _, err := env.pubs.Read(ctx, bob, ReadRequest{PublicationID: done.Publication.PublicationID}); err != nil || body.Steps[0] != "step for triage" || body.ContentHash != c.ContentHash {
				t.Fatalf("published body=%+v err=%v", body, err)
			}
			if foreign, _ := env.pubs.List(ctx, other); len(foreign) != 0 {
				t.Fatalf("cross-tenant publications=%+v", foreign)
			}
			// After a decision the submitter may submit the same name again.
			submit(t, store, "alice", "k3", "triage")
		})
	}
}

func TestCandidateStore_ValidatesInput(t *testing.T) {
	store := newCandidateEnv(t, "inmem").cands
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
	admin := adminCtx(t, id)
	for i, req := range []AcceptCandidateRequest{
		{IdempotencyKey: "k", CandidateID: "missing", ExpectedGeneration: 1, Mode: "bogus"},
		{IdempotencyKey: "k", CandidateID: "missing", ExpectedGeneration: 1, Mode: AcceptModeSuccessor, PublicationID: "p"},
		{IdempotencyKey: "k", CandidateID: "missing", ExpectedGeneration: 1, Mode: AcceptModeNew, PublicationID: "p"},
		{IdempotencyKey: "", CandidateID: "missing", ExpectedGeneration: 1, Mode: AcceptModeNew},
		{IdempotencyKey: "k", CandidateID: "missing", ExpectedGeneration: 1, Mode: AcceptModeNew, DecisionNote: string(long)},
	} {
		if _, err := store.Accept(admin, id, req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("accept case %d err=%v want invalid", i, err)
		}
	}
	if _, err := store.List(ctx, id, ListCandidatesRequest{State: "nope"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("bad state err=%v", err)
	}
}

// Accept writes the organization catalog directly, so it applies the same
// verified-admin gate as AuthorizedStore.
func TestCandidateAccept_RequiresVerifiedAdmin(t *testing.T) {
	env := newCandidateEnv(t, "inmem")
	c := submit(t, env.cands, "alice", "k", "triage")
	admin := publicationIdentity("tenant-a", "admin", "s-admin")
	req := AcceptCandidateRequest{IdempotencyKey: "a", CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeNew}
	if _, err := env.cands.Accept(context.Background(), admin, req); !errors.Is(err, ErrVerifiedIdentityRequired) {
		t.Fatalf("unverified accept err=%v", err)
	}
	if _, err := env.cands.Accept(authorizedContext(t, admin.Identity, nil, auth.ScopeAgentConfigUser), admin, req); !errors.Is(err, ErrAdminRequired) {
		t.Fatalf("non-admin accept err=%v", err)
	}
	other := publicationIdentity("tenant-a", "mallory", "s-m")
	if _, err := env.cands.Accept(adminCtx(t, other), admin, req); !errors.Is(err, ErrVerifiedIdentityRequired) {
		t.Fatalf("mismatched caller accept err=%v", err)
	}
	if pubs, _ := env.pubs.List(context.Background(), admin); len(pubs) != 0 {
		t.Fatalf("denied accept published %+v", pubs)
	}
	if got, _ := env.cands.Read(context.Background(), admin, c.CandidateID, true); got.State != CandidatePending || got.Generation != 1 {
		t.Fatalf("denied accept changed the candidate: %+v", got)
	}
}

func TestCandidateAccept_SuccessorAndFailuresWriteNothing(t *testing.T) {
	for name, env := range candidateEnvs(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			admin := publicationIdentity("tenant-a", "admin", "s-admin")
			actx := adminCtx(t, admin)
			pub, _, err := env.pubs.Publish(ctx, admin, PublishRequest{IdempotencyKey: "p", Name: "triage", Skill: publicationSkill("triage", "v0"), ExpectedAbsent: true})
			if err != nil {
				t.Fatal(err)
			}
			c := submit(t, env.cands, "alice", "k", "my-triage")
			// Name clash, stale predicates and a missing target all conflict
			// without writing anything.
			if _, err := env.cands.Accept(actx, admin, AcceptCandidateRequest{IdempotencyKey: "a1", CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeNew, Name: "triage"}); !errors.Is(err, ErrConflict) {
				t.Fatalf("name clash err=%v", err)
			}
			if _, err := env.cands.Accept(actx, admin, AcceptCandidateRequest{IdempotencyKey: "a2", CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeSuccessor, PublicationID: pub.PublicationID, ExpectedPublicationGeneration: 9, ExpectedPublicationContentHash: pub.ContentHash}); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale successor err=%v", err)
			}
			if _, err := env.cands.Accept(actx, admin, AcceptCandidateRequest{IdempotencyKey: "a3", CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeSuccessor, PublicationID: "pub_missing", ExpectedPublicationGeneration: 1, ExpectedPublicationContentHash: "h"}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing target err=%v", err)
			}
			if got, _ := env.pubs.Get(ctx, admin, pub.PublicationID); got.Generation != 1 {
				t.Fatalf("failed accepts moved the publication: %+v", got)
			}
			if got, _ := env.cands.Read(ctx, admin, c.CandidateID, true); got.State != CandidatePending || got.Generation != 1 {
				t.Fatalf("failed accepts moved the candidate: %+v", got)
			}
			// The successor lands under the publication's own name.
			res, err := env.cands.Accept(actx, admin, AcceptCandidateRequest{IdempotencyKey: "a4", CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeSuccessor, PublicationID: pub.PublicationID, ExpectedPublicationGeneration: 1, ExpectedPublicationContentHash: pub.ContentHash})
			if err != nil || res.Publication.Generation != 2 || res.Publication.Name != "triage" || res.Receipt.Operation != "publish_successor" || res.Candidate.PriorContentHash != pub.ContentHash {
				t.Fatalf("successor accept=%+v err=%v", res, err)
			}
			if body, _, err := env.pubs.Read(ctx, admin, ReadRequest{PublicationID: pub.PublicationID}); err != nil || body.Steps[0] != "step for my-triage" {
				t.Fatalf("successor body=%+v err=%v", body, err)
			}
		})
	}
}

// An exact retry returns the original result without publishing again; a
// divergent reuse of the key is an idempotency conflict and a fresh key on a
// decided candidate is an ordinary conflict.
func TestCandidateAccept_IdempotentReplay(t *testing.T) {
	for name, env := range candidateEnvs(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			admin := publicationIdentity("tenant-a", "admin", "s-admin")
			actx := adminCtx(t, admin)
			c := submit(t, env.cands, "alice", "k", "triage")
			req := AcceptCandidateRequest{IdempotencyKey: "acc", CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeNew, DecisionNote: "ok"}
			first, err := env.cands.Accept(actx, admin, req)
			if err != nil {
				t.Fatal(err)
			}
			// The publication advances afterwards; the replay still answers
			// the revision this accept produced.
			if _, _, err := env.pubs.PublishSuccessor(ctx, admin, SuccessorRequest{IdempotencyKey: "s", PublicationID: first.Publication.PublicationID, ExpectedGeneration: 1, ExpectedContentHash: first.Publication.ContentHash, Skill: publicationSkill("triage", "v2")}); err != nil {
				t.Fatal(err)
			}
			replay, err := env.cands.Accept(actx, admin, req)
			if err != nil || replay.Publication != first.Publication || replay.Receipt != first.Receipt || replay.Candidate != first.Candidate {
				t.Fatalf("replay=%+v err=%v want %+v", replay, err, first)
			}
			divergent := req
			divergent.Name = "other"
			if _, err := env.cands.Accept(actx, admin, divergent); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("divergent replay err=%v", err)
			}
			fresh := req
			fresh.IdempotencyKey = "acc-2"
			if _, err := env.cands.Accept(actx, admin, fresh); !errors.Is(err, ErrConflict) {
				t.Fatalf("fresh key on accepted candidate err=%v", err)
			}
			// A key used for another operation never adopts a publication.
			if _, err := env.cands.Accept(actx, admin, AcceptCandidateRequest{IdempotencyKey: "k", CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeNew}); err == nil {
				t.Fatal("submit key reused as an accept key succeeded")
			}
			if pubs, _ := env.pubs.List(ctx, admin); len(pubs) != 1 {
				t.Fatalf("publications=%d want 1", len(pubs))
			}
		})
	}
}

// Concurrent decisions on one candidate through two store instances over one
// StateStore (so no in-process mutex serializes them): exactly one wins, and
// the end state is consistent — accepted with exactly one publication, or
// rejected with none. Never both.
func TestCandidateAccept_ConcurrentAcceptVersusReject(t *testing.T) {
	for name, env := range candidateEnvs(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			second, err := NewStateStoreCandidateStore(env.st, "rt")
			if err != nil {
				t.Fatal(err)
			}
			admin := publicationIdentity("tenant-a", "admin", "s-admin")
			actx := adminCtx(t, admin)
			accepted, rejected := 0, 0
			for i := range 20 {
				skill := fmt.Sprintf("race-%d", i)
				c := submit(t, env.cands, "alice", "k-"+skill, skill)
				var acceptErr, rejectErr error
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					_, acceptErr = env.cands.Accept(actx, admin, AcceptCandidateRequest{IdempotencyKey: "a-" + skill, CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeNew})
				}()
				go func() {
					defer wg.Done()
					_, rejectErr = second.Decide(ctx, admin, DecideCandidateRequest{IdempotencyKey: "r-" + skill, CandidateID: c.CandidateID, ExpectedGeneration: 1, Outcome: CandidateRejected}, true)
				}()
				wg.Wait()
				if (acceptErr == nil) == (rejectErr == nil) {
					t.Fatalf("round %d: accept err=%v reject err=%v, want exactly one winner", i, acceptErr, rejectErr)
				}
				for _, err := range []error{acceptErr, rejectErr} {
					if err != nil && !errors.Is(err, ErrConflict) {
						t.Fatalf("round %d: loser err=%v want conflict", i, err)
					}
				}
				got, _ := env.cands.Read(ctx, admin, c.CandidateID, true)
				published := 0
				pubs, _ := env.pubs.List(ctx, admin)
				for _, p := range pubs {
					if p.Name == skill {
						published++
					}
				}
				switch {
				case acceptErr == nil && got.State == CandidateAccepted && published == 1:
					accepted++
				case rejectErr == nil && got.State == CandidateRejected && published == 0:
					rejected++
				default:
					t.Fatalf("round %d: state=%s publications=%d", i, got.State, published)
				}
			}
			t.Logf("accepted=%d rejected=%d", accepted, rejected)
		})
	}
}

// hookedState runs before once, ahead of the first conditional write of the
// hooked kind, so another decision deterministically commits between this
// store's load and its save.
type hookedState struct {
	state.StateStore
	batch  bool // hook SaveBatchIf (an accept) or SaveIf (a reject)
	before func()
	once   sync.Once
}

func (h *hookedState) SaveBatchIf(ctx context.Context, expect []state.SlotExpectation, writes []state.StateRecord) error {
	if h.batch {
		h.once.Do(h.before)
	}
	return h.StateStore.SaveBatchIf(ctx, expect, writes)
}

func (h *hookedState) SaveIf(ctx context.Context, expect []state.SlotExpectation, next state.StateRecord) error {
	if !h.batch && next.Kind == candidateKind {
		h.once.Do(h.before)
	}
	return h.StateStore.SaveIf(ctx, expect, next)
}

// The interleavings the free-running race may not hit, forced: a reject
// that commits after an accept loaded wins and the accept publishes nothing;
// an accept that commits after a reject loaded wins and the reject conflicts.
func TestCandidateAccept_InterleavedDecisionsExactlyOneWins(t *testing.T) {
	for name, env := range candidateEnvs(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			admin := publicationIdentity("tenant-a", "admin", "s-admin")
			actx := adminCtx(t, admin)
			plain, err := NewStateStoreCandidateStore(env.st, "rt")
			if err != nil {
				t.Fatal(err)
			}
			hooked := func(batch bool, before func()) CandidateStore {
				s, err := NewStateStoreCandidateStore(&hookedState{StateStore: env.st, batch: batch, before: before}, "rt")
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			countPubs := func(skill string) int {
				pubs, _ := env.pubs.List(ctx, admin)
				n := 0
				for _, p := range pubs {
					if p.Name == skill {
						n++
					}
				}
				return n
			}

			c := submit(t, plain, "alice", "k1", "reject-wins")
			var rejectErr error
			acceptor := hooked(true, func() {
				_, rejectErr = plain.Decide(ctx, admin, DecideCandidateRequest{IdempotencyKey: "r1", CandidateID: c.CandidateID, ExpectedGeneration: 1, Outcome: CandidateRejected}, true)
			})
			if _, err := acceptor.Accept(actx, admin, AcceptCandidateRequest{IdempotencyKey: "a1", CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeNew}); !errors.Is(err, ErrConflict) || rejectErr != nil {
				t.Fatalf("accept err=%v reject err=%v, want conflict/nil", err, rejectErr)
			}
			if got, _ := plain.Read(ctx, admin, c.CandidateID, true); got.State != CandidateRejected || countPubs("reject-wins") != 0 {
				t.Fatalf("reject-wins state=%s publications=%d", got.State, countPubs("reject-wins"))
			}

			c2 := submit(t, plain, "alice", "k2", "accept-wins")
			var acceptErr error
			rejecter := hooked(false, func() {
				_, acceptErr = plain.Accept(actx, admin, AcceptCandidateRequest{IdempotencyKey: "a2", CandidateID: c2.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeNew})
			})
			if _, err := rejecter.Decide(ctx, admin, DecideCandidateRequest{IdempotencyKey: "r2", CandidateID: c2.CandidateID, ExpectedGeneration: 1, Outcome: CandidateRejected}, true); !errors.Is(err, ErrConflict) || acceptErr != nil {
				t.Fatalf("reject err=%v accept err=%v, want conflict/nil", err, acceptErr)
			}
			if got, _ := plain.Read(ctx, admin, c2.CandidateID, true); got.State != CandidateAccepted || countPubs("accept-wins") != 1 {
				t.Fatalf("accept-wins state=%s publications=%d", got.State, countPubs("accept-wins"))
			}
		})
	}
}

// Of N concurrent accepts with distinct targets, through two store instances,
// exactly one wins and exactly one publication exists.
func TestCandidateAccept_ConcurrentAcceptsPublishOnce(t *testing.T) {
	for name, env := range candidateEnvs(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			second, err := NewStateStoreCandidateStore(env.st, "rt")
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for i := range 40 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					user := fmt.Sprintf("u%d", i%10)
					_, _ = env.cands.Submit(ctx, publicationIdentity("tenant-a", user, "s"), SubmitCandidateRequest{IdempotencyKey: fmt.Sprintf("k%d", i), AgentID: "a", Skill: publicationSkill(fmt.Sprintf("s%d", i), "x")})
				}(i)
			}
			wg.Wait()
			admin := publicationIdentity("tenant-a", "admin", "s")
			all, err := env.cands.List(ctx, admin, ListCandidatesRequest{All: true})
			if err != nil || len(all) != 40 {
				t.Fatalf("listed %d err=%v", len(all), err)
			}
			target := all[0]
			actx := adminCtx(t, admin)
			var wins atomic.Int32
			for i := range 20 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					store := env.cands
					if i%2 == 1 {
						store = second
					}
					if _, err := store.Accept(actx, admin, AcceptCandidateRequest{IdempotencyKey: fmt.Sprintf("a%d", i), CandidateID: target.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeNew, Name: fmt.Sprintf("n%d", i)}); err == nil {
						wins.Add(1)
					} else if !errors.Is(err, ErrConflict) {
						t.Errorf("loser err=%v", err)
					}
				}(i)
			}
			wg.Wait()
			pubs, _ := env.pubs.List(ctx, admin)
			if wins.Load() != 1 || len(pubs) != 1 {
				t.Fatalf("winners=%d publications=%d want 1/1", wins.Load(), len(pubs))
			}
		})
	}
}

// Records written by releases before 1.55 decode: a candidate's claim and
// claim_seq and the publication aggregate's candidate_fence_tokens are
// ignored, so a pending candidate an older runtime had claimed can still be
// accepted (or rejected), and the next write drops the old fields.
func TestCandidateAccept_DecodesPreAtomicRecords(t *testing.T) {
	env := newCandidateEnv(t, "sqlite")
	ctx := context.Background()
	admin := publicationIdentity("tenant-a", "admin", "s-admin")
	sk := publicationSkill("triage", "v1")
	sk.ContentHash = skills.CanonicalContentHash(sk)
	legacyCand := map[string]any{
		"candidates": []map[string]any{{
			"candidate_id": "cand_legacy", "name": "triage", "submitter_user": "alice", "agent_id": "agent-a",
			"content_hash": sk.ContentHash, "state": "pending", "generation": 1, "runtime_id": "rt", "skill": sk,
			"claim":     map[string]any{"mode": "new", "name": "triage", "claimed_by": "admin", "claimed_at": "2026-11-13T10:00:00Z", "seq": 3},
			"claim_seq": 3,
		}},
		"seq": 1,
	}
	legacyPubs := map[string]any{"publications": []Publication{}, "candidate_fence_tokens": map[string]any{"cand_legacy": map[string]uint64{"live": 3}}}
	for kind, v := range map[string]any{candidateKind: legacyCand, publicationOrgKind: legacyPubs} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.st.SaveIf(ctx, []state.SlotExpectation{{Identity: orgIdentity("tenant-a"), Kind: kind}}, state.StateRecord{ID: state.NewEventID(), Identity: orgIdentity("tenant-a"), Kind: kind, Bytes: b}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := env.cands.Accept(adminCtx(t, admin), admin, AcceptCandidateRequest{IdempotencyKey: "a", CandidateID: "cand_legacy", ExpectedGeneration: 1, Mode: AcceptModeNew})
	if err != nil || res.Candidate.State != CandidateAccepted {
		t.Fatalf("accept of a legacy-claimed candidate=%+v err=%v", res, err)
	}
	for _, kind := range []string{candidateKind, publicationOrgKind} {
		r, err := env.st.Load(ctx, orgIdentity("tenant-a"), kind)
		if err != nil {
			t.Fatal(err)
		}
		if raw := string(r.Bytes); strings.Contains(raw, `"claim"`) || strings.Contains(raw, "claim_seq") || strings.Contains(raw, "candidate_fence_tokens") {
			t.Fatalf("%s still carries pre-atomic fields: %s", kind, raw)
		}
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

// Decided candidates never count toward the tenant quota, and only one
// person's open requests can't fill it.
func TestCandidateQuotaCountsOnlyPendingAndPrunesHistory(t *testing.T) {
	ctx := context.Background()
	store := newCandidateEnv(t, "inmem").cands
	alice := publicationIdentity("tenant-a", "alice", "s-alice")
	seen := map[string]bool{}
	for i := range maxRetainedTerminal + 50 {
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
	for i := range maxPendingPerUser {
		submit(t, store, "carol", fmt.Sprintf("p-%d", i), fmt.Sprintf("skill-%d", i))
	}
	sk := publicationSkill("one-more", "step")
	if _, err := store.Submit(ctx, publicationIdentity("tenant-a", "carol", "s-carol"), SubmitCandidateRequest{IdempotencyKey: "over", AgentID: "agent-a", Skill: sk}); !errors.Is(err, ErrConflict) {
		t.Fatalf("per-user cap err=%v", err)
	}
	submit(t, store, "dave", "k", "notes")
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
	// A candidate whose snapshot carries the legacy key is accepted onto it.
	cands, err := NewStateStoreCandidateStore(sq, "rt")
	if err != nil {
		t.Fatal(err)
	}
	cand := publicationSkill("my-weekly", "v3")
	cand.Extra = map[string]any{"_publication.source": "existing metadata"}
	c, err := cands.Submit(ctx, publicationIdentity("tenant-a", "alice", "s-alice"), SubmitCandidateRequest{IdempotencyKey: "k", AgentID: "agent-a", Skill: cand})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cands.Accept(adminCtx(t, admin), admin, AcceptCandidateRequest{IdempotencyKey: "a", CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: AcceptModeSuccessor, PublicationID: succ.PublicationID, ExpectedPublicationGeneration: succ.Generation, ExpectedPublicationContentHash: succ.ContentHash}); err != nil {
		t.Fatalf("candidate successor with legacy key: %v", err)
	}
}
