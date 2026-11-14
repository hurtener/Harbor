package protocol_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/publication"
	sqlitedriver "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

type hookCands struct {
	publication.CandidateStore
	onRead func(user string, c publication.Candidate)
}

func (h *hookCands) Read(ctx context.Context, caller identity.Quadruple, id string, p bool) (publication.Candidate, error) {
	c, err := h.CandidateStore.Read(ctx, caller, id, p)
	if h.onRead != nil {
		h.onRead(caller.UserID, c)
	}
	return c, err
}

type hookStore struct {
	publication.Store
	onPublish func(user string)
	onResolve func(user string, res publication.CandidateResolution)
}

func (h *hookStore) Publish(ctx context.Context, c identity.Quadruple, r publication.PublishRequest) (publication.Metadata, publication.Receipt, error) {
	if h.onPublish != nil {
		h.onPublish(c.UserID)
	}
	return h.Store.Publish(ctx, c, r)
}
func (h *hookStore) ResolveCandidate(ctx context.Context, c identity.Quadruple, id string, expected uint64) (publication.CandidateResolution, error) {
	res, err := h.Store.ResolveCandidate(ctx, c, id, expected)
	if h.onResolve != nil {
		h.onResolve(c.UserID, res)
	}
	return res, err
}

func snapFn() protocol.PersonalSkillSnapshotFunc {
	return protocol.PersonalSkillSnapshotFunc(func(_ context.Context, _, name string) (skills.Skill, string, int, error) {
		return skills.Skill{Name: name, Trigger: "when", Steps: []string{"v1"}, Origin: skills.OriginGenerated, Scope: skills.ScopeUser}, "", 0, nil
	})
}

// Review P1-1: a reject that read a stale claim X must not revoke the fence
// of a fresh claim Y installed by a concurrent accept in between.
func TestReview_ResolveRevokesFreshClaimFence(t *testing.T) {
	raw := publication.NewMemoryStore("runtime-a")
	auth0, _ := publication.NewAuthorizedStore(raw, auth.NewAgentReachAuthorizer())
	var old atomic.Bool
	old.Store(true)
	clock := func() time.Time {
		if old.Load() {
			return time.Now().Add(-3 * time.Minute)
		}
		return time.Now()
	}
	cands := &hookCands{CandidateStore: publication.NewMemoryCandidateStoreWithClock("runtime-a", clock)}
	store := &hookStore{Store: auth0}
	surface, err := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: store, Candidates: cands, Snapshots: snapFn()})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID("alice"), AgentID: "agent-a", Name: "triage", IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	id := sub.(*types.SkillPublicationCandidateSubmitResponse).Candidate.CandidateID
	// stale claim X (crashed accept, no fence)
	if _, err := cands.Claim(context.Background(), publicationTestIdentity("adminx"), id, 1, publication.AcceptClaim{Mode: publication.AcceptModeNew}); err != nil {
		t.Fatal(err)
	}
	old.Store(false)
	rejRead := make(chan struct{})
	aFenced := make(chan struct{})
	rejResolved := make(chan struct{})
	var rejOnce, pubOnce, resOnce sync.Once
	cands.onRead = func(user string, c publication.Candidate) {
		if user == "rej" {
			rejOnce.Do(func() { close(rejRead); <-aFenced })
		}
	}
	store.onPublish = func(user string) {
		if user == "adminA" {
			pubOnce.Do(func() { close(aFenced); <-rejResolved })
		}
	}
	store.onResolve = func(user string, res publication.CandidateResolution) {
		if user == "rej" {
			resOnce.Do(func() { close(rejResolved) })
		}
	}
	var wg sync.WaitGroup
	var rejErr, accErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, rejErr = surface.Dispatch(candAdminCtx(t, "rej"), methods.MethodSkillsPublicationsCandidatesReject, &types.SkillPublicationCandidateRejectRequest{Identity: candID("rej"), CandidateID: id, ExpectedGeneration: 1, DecisionNote: "no", IdempotencyKey: "r"})
	}()
	<-rejRead
	go func() {
		defer wg.Done()
		_, accErr = surface.Dispatch(candAdminCtx(t, "adminA"), methods.MethodSkillsPublicationsCandidatesAccept, &types.SkillPublicationCandidateAcceptRequest{Identity: candID("adminA"), CandidateID: id, ExpectedGeneration: 1, Mode: "new", Name: "triage-other", IdempotencyKey: "a"})
	}()
	wg.Wait()
	c, _ := cands.CandidateStore.Read(context.Background(), publicationTestIdentity("x"), id, true)
	pubs, _ := auth0.List(candAdminCtx(t, "x"), publicationTestIdentity("x"))
	t.Logf("reject err=%v ; fresh accept err=%v ; candidate state=%s claim=%v ; publications=%d", rejErr, accErr, c.State, c.Claim, len(pubs))
	if accErr != nil {
		t.Errorf("(d) VIOLATED: fresh non-stale accept was revoked by a reject's stale-claim resolution: %v", accErr)
	}
}

// Review P1-2: a claim whose fence was revoked but not released (crash
// between ResolveCandidate and ReleaseClaim) must not wedge same-target retries.
func TestReview_RevokedRecordedFenceWedgesSameTarget(t *testing.T) {
	raw := publication.NewMemoryStore("runtime-a")
	store, _ := publication.NewAuthorizedStore(raw, auth.NewAgentReachAuthorizer())
	cands := publication.NewMemoryCandidateStoreWithClock("runtime-a", func() time.Time { return time.Now().Add(-3 * time.Minute) })
	surface, _ := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: store, Candidates: cands, Snapshots: snapFn()})
	admin := candAdminCtx(t, "admin")
	me := publicationTestIdentity("admin")
	sub, _ := surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID("alice"), AgentID: "agent-a", Name: "triage", IdempotencyKey: "k"})
	id := sub.(*types.SkillPublicationCandidateSubmitResponse).Candidate.CandidateID
	cl, _ := cands.Claim(admin, me, id, 1, publication.AcceptClaim{Mode: publication.AcceptModeNew})
	tok := cl.Claim.Seq
	if err := store.FenceCandidate(admin, me, id, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveCandidate(admin, me, id, tok); err != nil { // crash before ReleaseClaim
		t.Fatal(err)
	}
	for i := range 3 {
		_, err := surface.Dispatch(admin, methods.MethodSkillsPublicationsCandidatesAccept, &types.SkillPublicationCandidateAcceptRequest{Identity: candID("admin"), CandidateID: id, ExpectedGeneration: 1, Mode: "new", IdempotencyKey: fmt.Sprintf("a%d", i)})
		t.Logf("same-target retry %d: err=%v", i, err)
		if err == nil {
			return
		}
	}
	c, _ := cands.Read(admin, me, id, true)
	t.Logf("after retries: state=%s claim=%+v", c.State, c.Claim)
	_, werr := surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesWithdraw, &types.SkillPublicationCandidateWithdrawRequest{Identity: candID("alice"), CandidateID: id, ExpectedGeneration: 1, IdempotencyKey: "w"})
	t.Logf("submitter withdraw: err=%v", werr)
	_, derr := surface.Dispatch(admin, methods.MethodSkillsPublicationsCandidatesAccept, &types.SkillPublicationCandidateAcceptRequest{Identity: candID("admin"), CandidateID: id, ExpectedGeneration: 1, Mode: "new", Name: "triage-2", IdempotencyKey: "d"})
	t.Logf("different-target accept: err=%v", derr)
	t.Errorf("same-target accept wedged on a revoked recorded fence (escape only via different target/reject)")
}

// Review: randomized interleavings — at most one publication per candidate,
// none for a rejected or withdrawn one, and nothing left stuck pending — over memory and SQLite (two runtimes on one file).
func TestReview_RandomInterleavings(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var stale atomic.Bool
			clock := func() time.Time {
				if stale.Load() {
					return time.Now().Add(-3 * time.Minute)
				}
				return time.Now()
			}
			var surfaces []*protocol.SkillPublicationsSurface
			var lister publication.Store
			var cs publication.CandidateStore
			if backend == "memory" {
				raw := publication.NewMemoryStore("runtime-a")
				st, _ := publication.NewAuthorizedStore(raw, auth.NewAgentReachAuthorizer())
				cs = publication.NewMemoryCandidateStoreWithClock("runtime-a", clock)
				s, _ := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: st, Candidates: cs, Snapshots: snapFn()})
				surfaces = append(surfaces, s, s)
				lister = st
			} else {
				dsn := filepath.Join(t.TempDir(), "rt.sqlite")
				for range 2 {
					sq, err := sqlitedriver.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = sq.Close(context.Background()) })
					p, _ := publication.NewStateStore(sq, "runtime-a")
					st, _ := publication.NewAuthorizedStore(p, auth.NewAgentReachAuthorizer())
					c, _ := publication.NewStateStoreCandidateStoreWithClock(sq, "runtime-a", clock)
					s, _ := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: st, Candidates: c, Snapshots: snapFn()})
					surfaces = append(surfaces, s)
					lister, cs = st, c
				}
			}
			ids := []string{}
			for i := range 12 {
				sub, err := surfaces[0].Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID("alice"), AgentID: "agent-a", Name: fmt.Sprintf("s%d", i), IdempotencyKey: fmt.Sprintf("k%d", i)})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, sub.(*types.SkillPublicationCandidateSubmitResponse).Candidate.CandidateID)
			}
			stale.Store(true) // every claim looks stale → maximal resolution racing
			var wg sync.WaitGroup
			for i, id := range ids {
				for round := range 3 {
					for w := range 4 {
						wg.Add(1)
						go func(i, w, round int, id string) {
							defer wg.Done()
							s := surfaces[(w+round)%len(surfaces)]
							u := fmt.Sprintf("adm%d", w)
							switch w {
							case 0, 1:
								_, _ = s.Dispatch(candAdminCtx(t, u), methods.MethodSkillsPublicationsCandidatesAccept, &types.SkillPublicationCandidateAcceptRequest{Identity: candID(u), CandidateID: id, ExpectedGeneration: 1, Mode: "new", Name: fmt.Sprintf("s%d-%d", i, w), IdempotencyKey: fmt.Sprintf("a%d%d-%d", w, round, i)})
							case 2:
								if i%3 == 0 {
									_, _ = s.Dispatch(candAdminCtx(t, u), methods.MethodSkillsPublicationsCandidatesReject, &types.SkillPublicationCandidateRejectRequest{Identity: candID(u), CandidateID: id, ExpectedGeneration: 1, DecisionNote: "n", IdempotencyKey: fmt.Sprintf("r%d-%d", i, round)})
								}
							case 3:
								_, _ = s.Dispatch(candAdminCtx(t, u), methods.MethodSkillsPublicationsCandidatesAccept, &types.SkillPublicationCandidateAcceptRequest{Identity: candID(u), CandidateID: id, ExpectedGeneration: 1, Mode: "new", Name: fmt.Sprintf("s%d-0", i), IdempotencyKey: fmt.Sprintf("a3%d-%d", round, i)})
							}
						}(i, w, round, id)
					}
				}
			}
			wg.Wait()
			pubs, err := lister.List(candAdminCtx(t, "x"), publicationTestIdentity("x"))
			if err != nil {
				t.Fatal(err)
			}
			per := map[string]int{}
			for _, p := range pubs {
				sk, _, err := lister.Read(candAdminCtx(t, "x"), publicationTestIdentity("x"), publication.ReadRequest{PublicationID: p.PublicationID, AnyState: true})
				if err != nil {
					t.Fatal(err)
				}
				per[sk.Extra[publication.CandidateMarkerKey].(string)]++
			}
			states := map[publication.CandidateState]int{}
			for _, id := range ids {
				c, err := cs.Read(context.Background(), publicationTestIdentity("x"), id, true)
				if err != nil {
					t.Fatal(err)
				}
				states[c.State]++
				if per[id] > 1 {
					t.Errorf("(a) candidate %s has %d publications", id, per[id])
				}
				if (c.State == publication.CandidateRejected || c.State == publication.CandidateWithdrawn) && per[id] > 0 {
					t.Errorf("(b) %s state=%s but has publication", id, c.State)
				}
				if c.State == publication.CandidateAccepted && per[id] != 1 {
					t.Errorf("accepted %s with %d publications", id, per[id])
				}
				if c.State == publication.CandidatePending {
					// must be resolvable now: an admin reject (claim is stale)
					_, rerr := surfaces[0].Dispatch(candAdminCtx(t, "fin"), methods.MethodSkillsPublicationsCandidatesReject, &types.SkillPublicationCandidateRejectRequest{Identity: candID("fin"), CandidateID: id, ExpectedGeneration: 1, DecisionNote: "n", IdempotencyKey: "fin-" + id})
					c2, _ := cs.Read(context.Background(), publicationTestIdentity("x"), id, true)
					if c2.State == publication.CandidatePending {
						adm := candAdminCtx(t, "dbg")
						res, e1 := lister.ResolveCandidate(adm, publicationTestIdentity("dbg"), id, c2.Claim.Seq)
						e2 := cs.ReleaseClaim(adm, publicationTestIdentity("dbg"), id, c2.Claim.Seq)
						c3, _ := cs.Read(adm, publicationTestIdentity("dbg"), id, true)
						t.Logf("debug %s: resolve=%+v err=%v release err=%v after claim=%v gen=%d", id, res.Applied, e1, e2, c3.Claim, c3.Generation)
						t.Errorf("(c) %s stuck pending: %v claim=%+v pubs=%d", id, rerr, c2.Claim, per[id])
					} else if c2.State == publication.CandidateRejected && per[id] > 0 {
						t.Errorf("(b) %s rejected after finalizer with publication", id)
					}
				}
			}
			t.Logf("%s: states=%v publications=%d", backend, states, len(pubs))
		})
	}
}

var _ = errors.Is
