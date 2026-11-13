package protocol_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/publication"
)

type candidateFixture struct {
	surface *protocol.SkillPublicationsSurface
	store   publication.Store
	// supports maps skill name → support-file count reported by the snapshotter.
	supports map[string]int
}

func newCandidateFixture(t *testing.T) *candidateFixture {
	t.Helper()
	raw := publication.NewMemoryStore("runtime-a")
	store, err := publication.NewAuthorizedStore(raw, auth.NewAgentReachAuthorizer())
	if err != nil {
		t.Fatal(err)
	}
	f := &candidateFixture{store: store, supports: map[string]int{}}
	snap := protocol.PersonalSkillSnapshotFunc(func(_ context.Context, agentID, name string) (skills.Skill, string, int, error) {
		if name == "missing" {
			return skills.Skill{}, "", 0, skills.ErrSkillNotFound
		}
		return skills.Skill{Name: name, Title: "T " + name, Description: "**" + name + "** helps", Trigger: "when " + name, Steps: []string{"do " + name + " on " + agentID}, Origin: skills.OriginGenerated, Scope: skills.ScopeUser, Extra: map[string]any{"_importer.frontmatter_raw": "x"}}, "v1:" + name, f.supports[name], nil
	})
	f.surface, err = protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: store, Candidates: publication.NewMemoryCandidateStore("runtime-a"), Snapshots: snap})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func candUserCtx(t *testing.T, user string) context.Context {
	return publicationTestContext(t, user, "s-"+user, nil, []string{"agent-a"})
}

func candAdminCtx(t *testing.T, user string) context.Context {
	return publicationTestContext(t, user, "s-"+user, []auth.Scope{auth.ScopeAdmin}, nil)
}

func candID(user string) types.IdentityScope {
	return types.IdentityScope{Tenant: "tenant-a", User: user, Session: "s-" + user}
}

func candCode(err error) protoerrors.Code {
	var perr *protoerrors.Error
	if errors.As(err, &perr) {
		return perr.Code
	}
	return ""
}

func (f *candidateFixture) submit(t *testing.T, user, name, key string) types.SkillPublicationCandidate {
	t.Helper()
	resp, err := f.surface.Dispatch(candUserCtx(t, user), methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID(user), AgentID: "agent-a", Name: name, Note: "useful", IdempotencyKey: key})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return resp.(*types.SkillPublicationCandidateSubmitResponse).Candidate
}

func TestCandidates_SubmitReadListAndRefusals(t *testing.T) {
	f := newCandidateFixture(t)
	c := f.submit(t, "alice", "triage", "k1")
	if c.State != "pending" || c.SubmitterUser != "alice" || c.PackageHash != "v1:triage" || c.Note != "useful" || c.CreatedAt == "" {
		t.Fatalf("candidate=%+v", c)
	}

	// Support files are refused before anything is written.
	f.supports["bundle"] = 2
	_, err := f.surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID("alice"), AgentID: "agent-a", Name: "bundle", IdempotencyKey: "k2"})
	if candCode(err) != protoerrors.CodeCandidateSupportFilesUnsupported {
		t.Fatalf("support files err=%v", err)
	}
	// A second pending candidate for the same skill is refused.
	_, err = f.surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID("alice"), AgentID: "agent-a", Name: "triage", IdempotencyKey: "k3"})
	if candCode(err) != protoerrors.CodeCandidatePendingExists {
		t.Fatalf("pending exists err=%v", err)
	}
	// Signed reach is required for the source Agent.
	noReach := publicationTestContext(t, "alice", "s-alice", nil, []string{"other"})
	if _, err := f.surface.Dispatch(noReach, methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID("alice"), AgentID: "agent-a", Name: "x", IdempotencyKey: "k4"}); candCode(err) != protoerrors.CodeIdentityScopeRequired {
		t.Fatalf("no reach err=%v", err)
	}
	if _, err := f.surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID("alice"), AgentID: "agent-a", Name: "missing", IdempotencyKey: "k5"}); candCode(err) != protoerrors.CodeNotFound {
		t.Fatalf("missing skill err=%v", err)
	}
	// A forged body identity never selects another owner.
	if _, err := f.surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID("bob"), AgentID: "agent-a", Name: "y", IdempotencyKey: "k6"}); candCode(err) != protoerrors.CodeScopeMismatch {
		t.Fatalf("forged identity err=%v", err)
	}

	f.submit(t, "bob", "notes", "k1")
	list := func(ctx context.Context, user string) []types.SkillPublicationCandidate {
		resp, err := f.surface.Dispatch(ctx, methods.MethodSkillsPublicationsCandidatesList, &types.SkillPublicationCandidateListRequest{Identity: candID(user)})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return resp.(*types.SkillPublicationCandidateListResponse).Candidates
	}
	if got := list(candUserCtx(t, "alice"), "alice"); len(got) != 1 || got[0].Name != "triage" {
		t.Fatalf("alice list=%+v", got)
	}
	if got := list(candAdminCtx(t, "admin"), "admin"); len(got) != 2 {
		t.Fatalf("admin list=%+v", got)
	}

	read := func(ctx context.Context, user, id string) (*types.SkillPublicationCandidateReadResponse, error) {
		resp, err := f.surface.Dispatch(ctx, methods.MethodSkillsPublicationsCandidatesRead, &types.SkillPublicationCandidateReadRequest{Identity: candID(user), CandidateID: id})
		if err != nil {
			return nil, err
		}
		return resp.(*types.SkillPublicationCandidateReadResponse), nil
	}
	own, err := read(candUserCtx(t, "alice"), "alice", c.CandidateID)
	if err != nil || !strings.Contains(own.Document, "do triage on agent-a") || own.Skill.Steps[0] != "do triage on agent-a" {
		t.Fatalf("own read=%+v err=%v", own, err)
	}
	if _, ok := own.Skill.Extra["_importer.frontmatter_raw"]; ok {
		t.Fatalf("importer bookkeeping leaked: %+v", own.Skill.Extra)
	}
	if _, err := read(candUserCtx(t, "bob"), "bob", c.CandidateID); candCode(err) != protoerrors.CodeNotFound {
		t.Fatalf("other user read err=%v", err)
	}
	if _, err := read(candAdminCtx(t, "admin"), "admin", c.CandidateID); err != nil {
		t.Fatalf("admin read err=%v", err)
	}
	// Accept and reject are admin-only.
	if _, err := f.surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesAccept, &types.SkillPublicationCandidateAcceptRequest{Identity: candID("alice"), CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: "new", IdempotencyKey: "a"}); candCode(err) != protoerrors.CodeIdentityScopeRequired {
		t.Fatalf("non-admin accept err=%v", err)
	}
	if _, err := f.surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesReject, &types.SkillPublicationCandidateRejectRequest{Identity: candID("alice"), CandidateID: c.CandidateID, ExpectedGeneration: 1, IdempotencyKey: "r"}); candCode(err) != protoerrors.CodeIdentityScopeRequired {
		t.Fatalf("non-admin reject err=%v", err)
	}
}

func TestCandidates_AcceptNewSuccessorRejectWithdraw(t *testing.T) {
	f := newCandidateFixture(t)
	c := f.submit(t, "alice", "triage", "k1")
	accept := func(ctx context.Context, admin string, req types.SkillPublicationCandidateAcceptRequest) (*types.SkillPublicationCandidateAcceptResponse, error) {
		req.Identity = candID(admin)
		resp, err := f.surface.Dispatch(ctx, methods.MethodSkillsPublicationsCandidatesAccept, &req)
		if err != nil {
			return nil, err
		}
		return resp.(*types.SkillPublicationCandidateAcceptResponse), nil
	}
	got, err := accept(candAdminCtx(t, "admin"), "admin", types.SkillPublicationCandidateAcceptRequest{CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: "new", DecisionNote: "great", IdempotencyKey: "a1"})
	if err != nil {
		t.Fatalf("accept new: %v", err)
	}
	if got.Candidate.State != "accepted" || got.Candidate.PublicationID == "" || got.Publication.Name != "triage" || got.Candidate.DecidedBy != "admin" || got.Candidate.DecisionNote != "great" {
		t.Fatalf("accepted=%+v", got)
	}
	// Retried by the same or another admin: no second publication.
	again, err := accept(candAdminCtx(t, "admin2"), "admin2", types.SkillPublicationCandidateAcceptRequest{CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: "new", DecisionNote: "great", IdempotencyKey: "a2"})
	if err != nil || again.Publication.PublicationID != got.Publication.PublicationID {
		t.Fatalf("retry=%+v err=%v", again, err)
	}
	pubs, _ := f.store.List(candAdminCtx(t, "admin"), publicationTestIdentity("admin"))
	if len(pubs) != 1 {
		t.Fatalf("publications=%d want 1", len(pubs))
	}

	// The published body is readable by any verified caller in the tenant.
	resp, err := f.surface.Dispatch(candUserCtx(t, "carol"), methods.MethodSkillsPublicationsRead, &types.SkillPublicationReadRequest{Identity: candID("carol"), PublicationID: got.Publication.PublicationID})
	if err != nil || !strings.Contains(resp.(*types.SkillPublicationReadResponse).Document, "do triage") {
		t.Fatalf("publication read=%+v err=%v", resp, err)
	}

	// A later version from the same person becomes a successor revision.
	c2 := f.submit(t, "alice", "triage", "k2")
	succ, err := accept(candAdminCtx(t, "admin"), "admin", types.SkillPublicationCandidateAcceptRequest{CandidateID: c2.CandidateID, ExpectedGeneration: 1, Mode: "successor", PublicationID: got.Publication.PublicationID, ExpectedPublicationGeneration: got.Publication.Generation, ExpectedPublicationContentHash: got.Publication.ContentHash, IdempotencyKey: "a3"})
	if err == nil {
		// Identical content: the store records a successor with the same hash.
		if succ.Candidate.PriorContentHash != got.Publication.ContentHash || succ.Publication.Generation != 2 {
			t.Fatalf("successor=%+v", succ)
		}
	} else if candCode(err) != protoerrors.CodeSkillPublicationConflict {
		t.Fatalf("successor err=%v", err)
	}
	// A stale publication precondition conflicts and leaves the candidate pending.
	c3 := f.submit(t, "bob", "triage", "k1")
	if _, err := accept(candAdminCtx(t, "admin"), "admin", types.SkillPublicationCandidateAcceptRequest{CandidateID: c3.CandidateID, ExpectedGeneration: 1, Mode: "successor", PublicationID: got.Publication.PublicationID, ExpectedPublicationGeneration: 1, ExpectedPublicationContentHash: "stale", IdempotencyKey: "a4"}); candCode(err) == "" {
		t.Fatal("stale successor accepted")
	}

	// Reject and withdraw.
	c4 := f.submit(t, "dave", "notes", "k1")
	rej, err := f.surface.Dispatch(candAdminCtx(t, "admin"), methods.MethodSkillsPublicationsCandidatesReject, &types.SkillPublicationCandidateRejectRequest{Identity: candID("admin"), CandidateID: c4.CandidateID, ExpectedGeneration: 1, DecisionNote: "not now", IdempotencyKey: "r"})
	if err != nil || rej.(*types.SkillPublicationCandidateRejectResponse).Candidate.State != "rejected" {
		t.Fatalf("reject=%+v err=%v", rej, err)
	}
	c5 := f.submit(t, "erin", "notes", "k1")
	wd, err := f.surface.Dispatch(candUserCtx(t, "erin"), methods.MethodSkillsPublicationsCandidatesWithdraw, &types.SkillPublicationCandidateWithdrawRequest{Identity: candID("erin"), CandidateID: c5.CandidateID, ExpectedGeneration: 1, IdempotencyKey: "w"})
	if err != nil || wd.(*types.SkillPublicationCandidateWithdrawResponse).Candidate.State != "withdrawn" {
		t.Fatalf("withdraw=%+v err=%v", wd, err)
	}
}

func TestCandidates_ConcurrentAcceptPublishesOnce(t *testing.T) {
	f := newCandidateFixture(t)
	c := f.submit(t, "alice", "triage", "k1")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			admin := "admin" + string(rune('a'+i))
			_, _ = f.surface.Dispatch(candAdminCtx(t, admin), methods.MethodSkillsPublicationsCandidatesAccept, &types.SkillPublicationCandidateAcceptRequest{Identity: candID(admin), CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: "new", IdempotencyKey: "k"})
		}(i)
	}
	wg.Wait()
	pubs, err := f.store.List(candAdminCtx(t, "admin"), publicationTestIdentity("admin"))
	if err != nil || len(pubs) != 1 {
		t.Fatalf("publications=%d err=%v want exactly 1", len(pubs), err)
	}
}

func TestPublicationRead_AdminVersusCaller(t *testing.T) {
	f := newCandidateFixture(t)
	ctx := candAdminCtx(t, "admin")
	pubResp, err := f.surface.Dispatch(ctx, methods.MethodSkillsPublicationsPublish, &types.SkillPublicationPublishRequest{Identity: candID("admin"), Name: "ops", Skill: publicationWireSkill(), IdempotencyKey: "p", ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	pub := pubResp.(*types.SkillPublicationPublishResponse).Publication
	if _, err := f.surface.Dispatch(ctx, methods.MethodSkillsPublicationsRetire, &types.SkillPublicationRetireRequest{Identity: candID("admin"), PublicationID: pub.PublicationID, ExpectedGeneration: pub.Generation, ExpectedContentHash: pub.ContentHash, IdempotencyKey: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.surface.Dispatch(candUserCtx(t, "carol"), methods.MethodSkillsPublicationsRead, &types.SkillPublicationReadRequest{Identity: candID("carol"), PublicationID: pub.PublicationID}); candCode(err) != protoerrors.CodeSkillPublicationNotFound {
		t.Fatalf("caller retired read err=%v", err)
	}
	resp, err := f.surface.Dispatch(ctx, methods.MethodSkillsPublicationsRead, &types.SkillPublicationReadRequest{Identity: candID("admin"), PublicationID: pub.PublicationID})
	if err != nil || resp.(*types.SkillPublicationReadResponse).Skill.Steps[0] != "run ops" {
		t.Fatalf("admin retired read=%+v err=%v", resp, err)
	}
	if _, err := f.surface.Dispatch(ctx, methods.MethodSkillsPublicationsRead, &types.SkillPublicationReadRequest{Identity: candID("admin")}); candCode(err) != protoerrors.CodeInvalidRequest {
		t.Fatalf("missing id err=%v", err)
	}
}

func TestCandidates_UnwiredSurfaceAnswersUnknownMethod(t *testing.T) {
	surface, err := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: publication.NewMemoryStore("rt")})
	if err != nil {
		t.Fatal(err)
	}
	if surface.CandidatesAvailable() {
		t.Fatal("candidates advertised without a store")
	}
	_, err = surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesList, &types.SkillPublicationCandidateListRequest{Identity: candID("alice")})
	if candCode(err) != protoerrors.CodeUnknownMethod {
		t.Fatalf("err=%v", err)
	}
}

func publicationTestIdentity(user string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: user, SessionID: "s-" + user}}
}

// flakyFinalize fails the first FinalizeAccept, modelling a crash between
// the publish and the candidate update.
type flakyFinalize struct {
	publication.CandidateStore
	failed bool
}

func (f *flakyFinalize) FinalizeAccept(ctx context.Context, caller identity.Quadruple, id string, result publication.AcceptResult) (publication.CandidateMetadata, error) {
	if !f.failed {
		f.failed = true
		return publication.CandidateMetadata{}, errors.New("simulated crash")
	}
	return f.CandidateStore.FinalizeAccept(ctx, caller, id, result)
}

func TestCandidates_ResumeAfterPartialAcceptAdoptsWithoutRepublishing(t *testing.T) {
	for _, mode := range []string{"new", "successor"} {
		t.Run(mode, func(t *testing.T) {
			raw := publication.NewMemoryStore("runtime-a")
			store, err := publication.NewAuthorizedStore(raw, auth.NewAgentReachAuthorizer())
			if err != nil {
				t.Fatal(err)
			}
			candidates := &flakyFinalize{CandidateStore: publication.NewMemoryCandidateStore("runtime-a")}
			snap := protocol.PersonalSkillSnapshotFunc(func(_ context.Context, _, name string) (skills.Skill, string, int, error) {
				return skills.Skill{Name: name, Trigger: "when", Steps: []string{"v2"}, Origin: skills.OriginGenerated, Scope: skills.ScopeUser}, "", 0, nil
			})
			surface, err := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: store, Candidates: candidates, Snapshots: snap})
			if err != nil {
				t.Fatal(err)
			}
			req := types.SkillPublicationCandidateAcceptRequest{ExpectedGeneration: 1, Mode: mode, IdempotencyKey: "a"}
			if mode == "successor" {
				pubResp, err := surface.Dispatch(candAdminCtx(t, "admin1"), methods.MethodSkillsPublicationsPublish, &types.SkillPublicationPublishRequest{Identity: candID("admin1"), Name: "triage", Skill: types.SkillPublicationSkill{Name: "triage", Trigger: "when", Steps: []string{"v1"}}, IdempotencyKey: "p", ExpectedAbsent: true})
				if err != nil {
					t.Fatal(err)
				}
				pub := pubResp.(*types.SkillPublicationPublishResponse).Publication
				req.PublicationID, req.ExpectedPublicationGeneration, req.ExpectedPublicationContentHash = pub.PublicationID, pub.Generation, pub.ContentHash
			}
			subResp, err := surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID("alice"), AgentID: "agent-a", Name: "triage", IdempotencyKey: "k"})
			if err != nil {
				t.Fatal(err)
			}
			req.CandidateID = subResp.(*types.SkillPublicationCandidateSubmitResponse).Candidate.CandidateID
			req.Identity = candID("admin1")
			if _, err := surface.Dispatch(candAdminCtx(t, "admin1"), methods.MethodSkillsPublicationsCandidatesAccept, &req); err == nil {
				t.Fatal("first accept should fail at finalize")
			}
			req.Identity = candID("admin2")
			resp, err := surface.Dispatch(candAdminCtx(t, "admin2"), methods.MethodSkillsPublicationsCandidatesAccept, &req)
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			got := resp.(*types.SkillPublicationCandidateAcceptResponse)
			if got.Candidate.State != "accepted" || got.Candidate.DecidedBy != "admin2" {
				t.Fatalf("resumed=%+v", got.Candidate)
			}
			pubs, _ := store.List(candAdminCtx(t, "admin2"), publicationTestIdentity("admin2"))
			wantGen := uint64(1)
			if mode == "successor" {
				wantGen = 2
			}
			if len(pubs) != 1 || pubs[0].Generation != wantGen {
				t.Fatalf("publications=%+v want one at generation %d", pubs, wantGen)
			}
			// The candidate marker never reaches the wire.
			read, err := surface.Dispatch(candUserCtx(t, "carol"), methods.MethodSkillsPublicationsRead, &types.SkillPublicationReadRequest{Identity: candID("carol"), PublicationID: pubs[0].PublicationID})
			if err != nil || len(read.(*types.SkillPublicationReadResponse).Skill.Extra) != 0 {
				t.Fatalf("read=%+v err=%v", read, err)
			}
		})
	}
}

// A name clash on a new acceptance must not wedge the candidate: the claim
// is released, so the admin can accept under another name or reject it.
func TestCandidates_NameClashReleasesTheClaim(t *testing.T) {
	f := newCandidateFixture(t)
	admin := candAdminCtx(t, "admin")
	if _, err := f.surface.Dispatch(admin, methods.MethodSkillsPublicationsPublish, &types.SkillPublicationPublishRequest{Identity: candID("admin"), Name: "weekly-report", Skill: types.SkillPublicationSkill{Name: "weekly-report", Trigger: "when", Steps: []string{"org"}}, IdempotencyKey: "p", ExpectedAbsent: true}); err != nil {
		t.Fatal(err)
	}
	c := f.submit(t, "bruno", "weekly-report", "k1")
	accept := func(name, key string) error {
		_, err := f.surface.Dispatch(admin, methods.MethodSkillsPublicationsCandidatesAccept, &types.SkillPublicationCandidateAcceptRequest{Identity: candID("admin"), CandidateID: c.CandidateID, ExpectedGeneration: 1, Mode: "new", Name: name, IdempotencyKey: key})
		return err
	}
	if err := accept("", "a1"); candCode(err) != protoerrors.CodeSkillPublicationConflict {
		t.Fatalf("clash err=%v", err)
	}
	// Retrying the same clash still conflicts (never "in progress").
	if err := accept("", "a2"); candCode(err) != protoerrors.CodeSkillPublicationConflict {
		t.Fatalf("clash retry err=%v", err)
	}
	// Another name succeeds.
	if err := accept("weekly-report-bruno", "a3"); err != nil {
		t.Fatalf("accept under another name: %v", err)
	}
	pubs, _ := f.store.List(admin, publicationTestIdentity("admin"))
	if len(pubs) != 2 {
		t.Fatalf("publications=%d want 2", len(pubs))
	}

	// And a clashing candidate can be rejected after the failed accept.
	c2 := f.submit(t, "carla", "weekly-report", "k1")
	if _, err := f.surface.Dispatch(admin, methods.MethodSkillsPublicationsCandidatesAccept, &types.SkillPublicationCandidateAcceptRequest{Identity: candID("admin"), CandidateID: c2.CandidateID, ExpectedGeneration: 1, Mode: "new", IdempotencyKey: "b1"}); candCode(err) != protoerrors.CodeSkillPublicationConflict {
		t.Fatalf("clash err=%v", err)
	}
	if _, err := f.surface.Dispatch(admin, methods.MethodSkillsPublicationsCandidatesReject, &types.SkillPublicationCandidateRejectRequest{Identity: candID("admin"), CandidateID: c2.CandidateID, ExpectedGeneration: 1, DecisionNote: "duplicate", IdempotencyKey: "r1"}); err != nil {
		t.Fatalf("reject after clash: %v", err)
	}
	// The submitter can withdraw after a failed accept too.
	c3 := f.submit(t, "dina", "weekly-report", "k1")
	_, _ = f.surface.Dispatch(admin, methods.MethodSkillsPublicationsCandidatesAccept, &types.SkillPublicationCandidateAcceptRequest{Identity: candID("admin"), CandidateID: c3.CandidateID, ExpectedGeneration: 1, Mode: "new", IdempotencyKey: "c1"})
	if _, err := f.surface.Dispatch(candUserCtx(t, "dina"), methods.MethodSkillsPublicationsCandidatesWithdraw, &types.SkillPublicationCandidateWithdrawRequest{Identity: candID("dina"), CandidateID: c3.CandidateID, ExpectedGeneration: 1, IdempotencyKey: "w1"}); err != nil {
		t.Fatalf("withdraw after clash: %v", err)
	}
}

// A successor retry that re-read the target after the first attempt already
// advanced it resumes the recorded claim instead of conflicting.
func TestCandidates_SuccessorRetryWithFreshPredicatesResumes(t *testing.T) {
	raw := publication.NewMemoryStore("runtime-a")
	store, err := publication.NewAuthorizedStore(raw, auth.NewAgentReachAuthorizer())
	if err != nil {
		t.Fatal(err)
	}
	candidates := &flakyFinalize{CandidateStore: publication.NewMemoryCandidateStore("runtime-a")}
	snap := protocol.PersonalSkillSnapshotFunc(func(_ context.Context, _, name string) (skills.Skill, string, int, error) {
		return skills.Skill{Name: name, Trigger: "when", Steps: []string{"v2"}, Origin: skills.OriginGenerated, Scope: skills.ScopeUser}, "", 0, nil
	})
	surface, err := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: store, Candidates: candidates, Snapshots: snap})
	if err != nil {
		t.Fatal(err)
	}
	admin := candAdminCtx(t, "admin")
	pubResp, err := surface.Dispatch(admin, methods.MethodSkillsPublicationsPublish, &types.SkillPublicationPublishRequest{Identity: candID("admin"), Name: "triage", Skill: types.SkillPublicationSkill{Name: "triage", Trigger: "when", Steps: []string{"v1"}}, IdempotencyKey: "p", ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	pub := pubResp.(*types.SkillPublicationPublishResponse).Publication
	sub, err := surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID("alice"), AgentID: "agent-a", Name: "triage", IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	id := sub.(*types.SkillPublicationCandidateSubmitResponse).Candidate.CandidateID
	req := types.SkillPublicationCandidateAcceptRequest{Identity: candID("admin"), CandidateID: id, ExpectedGeneration: 1, Mode: "successor", PublicationID: pub.PublicationID, ExpectedPublicationGeneration: pub.Generation, ExpectedPublicationContentHash: pub.ContentHash, IdempotencyKey: "a"}
	if _, err := surface.Dispatch(admin, methods.MethodSkillsPublicationsCandidatesAccept, &req); err == nil {
		t.Fatal("first accept should fail at finalize")
	}
	current, err := store.Get(admin, publicationTestIdentity("admin"), pub.PublicationID)
	if err != nil || current.Generation != 2 {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	// The retry carries the re-read (advanced) predicates.
	req.ExpectedPublicationGeneration, req.ExpectedPublicationContentHash = current.Generation, current.ContentHash
	resp, err := surface.Dispatch(admin, methods.MethodSkillsPublicationsCandidatesAccept, &req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	got := resp.(*types.SkillPublicationCandidateAcceptResponse)
	if got.Candidate.State != "accepted" || got.Publication.Generation != 2 || got.Candidate.PriorContentHash != pub.ContentHash {
		t.Fatalf("resumed=%+v", got)
	}
}

// A committed successor whose finalize failed is recovered by a different
// admin even after another successor advanced the publication: the durable
// marker, not a caller-scoped receipt or the current generation, proves it.
func TestCandidates_RecoveryAcrossAdminsAfterPublicationAdvanced(t *testing.T) {
	raw := publication.NewMemoryStore("runtime-a")
	store, err := publication.NewAuthorizedStore(raw, auth.NewAgentReachAuthorizer())
	if err != nil {
		t.Fatal(err)
	}
	candidates := &flakyFinalize{CandidateStore: publication.NewMemoryCandidateStore("runtime-a")}
	snap := protocol.PersonalSkillSnapshotFunc(func(_ context.Context, _, name string) (skills.Skill, string, int, error) {
		return skills.Skill{Name: name, Trigger: "when", Steps: []string{"v2"}, Origin: skills.OriginGenerated, Scope: skills.ScopeUser}, "", 0, nil
	})
	surface, err := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: store, Candidates: candidates, Snapshots: snap})
	if err != nil {
		t.Fatal(err)
	}
	a1, a2 := candAdminCtx(t, "admin1"), candAdminCtx(t, "admin2")
	pubResp, err := surface.Dispatch(a1, methods.MethodSkillsPublicationsPublish, &types.SkillPublicationPublishRequest{Identity: candID("admin1"), Name: "triage", Skill: types.SkillPublicationSkill{Name: "triage", Trigger: "when", Steps: []string{"v1"}}, IdempotencyKey: "p", ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	pub := pubResp.(*types.SkillPublicationPublishResponse).Publication
	sub, err := surface.Dispatch(candUserCtx(t, "alice"), methods.MethodSkillsPublicationsCandidatesSubmit, &types.SkillPublicationCandidateSubmitRequest{Identity: candID("alice"), AgentID: "agent-a", Name: "triage", IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	id := sub.(*types.SkillPublicationCandidateSubmitResponse).Candidate.CandidateID
	req := types.SkillPublicationCandidateAcceptRequest{Identity: candID("admin1"), CandidateID: id, ExpectedGeneration: 1, Mode: "successor", PublicationID: pub.PublicationID, ExpectedPublicationGeneration: pub.Generation, ExpectedPublicationContentHash: pub.ContentHash, IdempotencyKey: "a"}
	if _, err := surface.Dispatch(a1, methods.MethodSkillsPublicationsCandidatesAccept, &req); err == nil {
		t.Fatal("first accept should fail at finalize")
	}
	// Someone else publishes another successor on top.
	cur, _ := store.Get(a1, publicationTestIdentity("admin1"), pub.PublicationID)
	if _, err := surface.Dispatch(a1, methods.MethodSkillsPublicationsSuccessor, &types.SkillPublicationSuccessorRequest{Identity: candID("admin1"), PublicationID: pub.PublicationID, ExpectedGeneration: cur.Generation, ExpectedContentHash: cur.ContentHash, Skill: types.SkillPublicationSkill{Name: "triage", Trigger: "when", Steps: []string{"v3"}}, IdempotencyKey: "s"}); err != nil {
		t.Fatal(err)
	}
	// A different admin retries with freshly read predicates.
	cur, _ = store.Get(a2, publicationTestIdentity("admin2"), pub.PublicationID)
	req.Identity, req.ExpectedPublicationGeneration, req.ExpectedPublicationContentHash = candID("admin2"), cur.Generation, cur.ContentHash
	resp, err := surface.Dispatch(a2, methods.MethodSkillsPublicationsCandidatesAccept, &req)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	got := resp.(*types.SkillPublicationCandidateAcceptResponse)
	if got.Candidate.State != "accepted" || got.Candidate.RevisionID == cur.RevisionID || got.Candidate.PriorContentHash != pub.ContentHash {
		t.Fatalf("recovered=%+v (current %+v)", got.Candidate, cur)
	}
	revs, _ := store.Revisions(a2, publicationTestIdentity("admin2"), pub.PublicationID)
	if len(revs) != 3 {
		t.Fatalf("revisions=%d want 3 (no republish)", len(revs))
	}
}

// A claim is only resolved as stale after any publish it started must have
// ended: the publish deadline is shorter than the stale window.
func TestCandidates_PublishDeadlineIsShorterThanStaleWindow(t *testing.T) {
	if protocol.AcceptPublishDeadline <= 0 || protocol.AcceptPublishDeadline >= protocol.StaleClaimAfter {
		t.Fatalf("publish deadline %v must be positive and shorter than the stale window %v", protocol.AcceptPublishDeadline, protocol.StaleClaimAfter)
	}
}
