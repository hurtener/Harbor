package protocol_test

import (
	"context"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

type recordingPreparer struct {
	mu       sync.Mutex
	calls    []protocol.SessionPrepareRequest
	reach    [][]string
	subjects []tasks.AgentReachAdmissionSubject
	state    types.PrepareState
	err      error
}

func (r *recordingPreparer) PrepareSession(ctx context.Context, req protocol.SessionPrepareRequest) (types.PrepareState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reach, _ := auth.AgentReachFrom(ctx)
	r.calls = append(r.calls, req)
	r.reach = append(r.reach, reach)
	subject, _ := tasks.AgentReachAdmissionSubjectFrom(ctx)
	r.subjects = append(r.subjects, subject)
	if r.err != nil {
		return "", r.err
	}
	return r.state, nil
}

func newPrepareSurface(t *testing.T, preparer protocol.SessionPreparer) (*protocol.ControlSurface, *agentFixture) {
	t.Helper()
	return newPrepareSurfaceWith(t, preparer, true)
}

func newPrepareSurfaceWith(t *testing.T, preparer protocol.SessionPreparer, admissions bool) (*protocol.ControlSurface, *agentFixture) {
	t.Helper()
	f := newAgentFixture(t, true)
	opts := []protocol.Option{protocol.WithAgentResolver(serve.NewAgentResolverAdapter(f.cfg, bootAgentID))}
	if admissions {
		opts = append(opts, protocol.WithAgentReachAdmissionAuthority(f.authority))
	}
	if preparer != nil {
		opts = append(opts, protocol.WithSessionPreparer(preparer))
	}
	surface, err := protocol.NewControlSurface(f.tasks, steering.NewRegistry(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return surface, f
}

func prepareReq(id identity.Identity, agentID string) *types.PrepareRequest {
	return &types.PrepareRequest{
		Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		AgentID:  agentID,
	}
}

func TestDispatchPrepare_AdmitsLikeStartAndNarrowsReach(t *testing.T) {
	rec := &recordingPreparer{state: types.PrepareStateScheduled}
	surface, f := newPrepareSurface(t, rec)
	caller := agentIdent("tenant-a", "user-1", "home")
	f.writeRevision(t, caller, "configured-agent")
	ctx := auth.WithAgentReach(authCtx(t, caller), []string{"configured-agent", "other-agent"})
	req := prepareReq(caller, "configured-agent")
	req.ProviderRoute = &types.LLMProviderRouteSelector{RouteID: "r", RouteGeneration: 1, ProviderConnectionID: "c",
		ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "planner"}
	resp, err := surface.Dispatch(ctx, methods.MethodPrepare, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	pr := resp.(*types.PrepareResponse)
	if pr.State != types.PrepareStateScheduled || pr.ProtocolVersion != types.ProtocolVersion {
		t.Fatalf("response = %+v", pr)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("preparer calls = %d", len(rec.calls))
	}
	got := rec.calls[0]
	if got.Identity != caller || got.EffectiveAgentID != "configured-agent" || got.ProviderRoute == nil || got.ProviderRoute.ModelSelector != "planner" {
		t.Fatalf("admitted request = %+v", got)
	}
	if len(rec.reach[0]) != 1 || rec.reach[0][0] != "configured-agent" {
		t.Fatalf("work ctx reach = %v, want narrowed to the admitted agent", rec.reach[0])
	}
	if sub := rec.subjects[0]; sub.TenantID != caller.TenantID || sub.UserID != caller.UserID || sub.SessionID != caller.SessionID || sub.EffectiveAgentID != "configured-agent" {
		t.Fatalf("work ctx admission subject = %+v, want the admitted caller and agent", sub)
	}
	// Omitted agent selects the default, exactly like start.
	if _, err := surface.Dispatch(auth.WithAgentReach(authCtx(t, caller), []string{bootAgentID}), methods.MethodPrepare, prepareReq(caller, "")); err != nil {
		t.Fatalf("default-agent prepare: %v", err)
	}
	if rec.calls[1].EffectiveAgentID != bootAgentID {
		t.Fatalf("default agent = %q", rec.calls[1].EffectiveAgentID)
	}
	// Nothing was materialized.
	if n := countTasks(t, f.tasks, caller); n != 0 {
		t.Fatalf("prepare created %d tasks", n)
	}
}

func TestDispatchPrepare_RefusesWhatStartRefuses(t *testing.T) {
	rec := &recordingPreparer{state: types.PrepareStateScheduled}
	surface, f := newPrepareSurface(t, rec)
	caller := agentIdent("tenant-a", "user-1", "home")
	f.writeRevision(t, caller, "configured-agent")
	f.writeRevision(t, agentIdent("tenant-b", "user-9", "s"), "foreign-agent")
	reach := func(agents ...string) context.Context { return auth.WithAgentReach(authCtx(t, caller), agents) }

	cases := []struct {
		name string
		ctx  context.Context
		req  *types.PrepareRequest
		want protoerrors.Code
	}{
		{"no verified identity", auth.WithAgentReach(context.Background(), []string{"configured-agent"}), prepareReq(caller, "configured-agent"), protoerrors.CodeIdentityRequired},
		{"incomplete identity", reach("configured-agent"), prepareReq(identity.Identity{TenantID: "tenant-a", UserID: "user-1"}, "configured-agent"), protoerrors.CodeIdentityRequired},
		{"body identity differs from verified", reach("configured-agent"), prepareReq(agentIdent("tenant-a", "user-2", "home"), "configured-agent"), protoerrors.CodeScopeMismatch},
		{"no signed reach", reach("other-agent"), prepareReq(caller, "configured-agent"), protoerrors.CodeScopeMismatch},
		{"foreign tenant agent", reach("foreign-agent"), prepareReq(caller, "foreign-agent"), protoerrors.CodeInvalidRequest},
		{"impersonation", reach("configured-agent"), func() *types.PrepareRequest {
			r := prepareReq(caller, "configured-agent")
			r.Identity.Impersonating = &types.IdentityScope{Tenant: "tenant-a", User: "user-1", Session: "home"}
			return r
		}(), protoerrors.CodeInvalidRequest},
		{"partial provider route", reach("configured-agent"), func() *types.PrepareRequest {
			r := prepareReq(caller, "configured-agent")
			r.ProviderRoute = &types.LLMProviderRouteSelector{RouteID: "r"}
			return r
		}(), protoerrors.CodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := surface.Dispatch(tc.ctx, methods.MethodPrepare, tc.req)
			if got := codeOf(t, err); got != tc.want {
				t.Fatalf("code = %q, want %q (err %v)", got, tc.want, err)
			}
		})
	}
	if len(rec.calls) != 0 {
		t.Fatalf("a refused prepare reached the preparer %d times", len(rec.calls))
	}
}

func TestDispatchPrepare_UnwiredAndUnavailableReadAsAbsent(t *testing.T) {
	caller := agentIdent("tenant-a", "user-1", "home")
	ctx := auth.WithAgentReach(authCtx(t, caller), []string{bootAgentID})
	surface, _ := newPrepareSurface(t, nil)
	if _, err := surface.Dispatch(ctx, methods.MethodPrepare, prepareReq(caller, "")); codeOf(t, err) != protoerrors.CodeUnknownMethod {
		t.Fatalf("unwired prepare err = %v", err)
	}
	unbound := serve.NewSessionPrepareBridge()
	surface, _ = newPrepareSurface(t, unbound)
	if _, err := surface.Dispatch(ctx, methods.MethodPrepare, prepareReq(caller, "")); codeOf(t, err) != protoerrors.CodeUnknownMethod {
		t.Fatalf("unbound prepare err = %v", err)
	}
	// Without the admission authority no record could ever be consumed; the
	// method reads as absent rather than preparing unbound state.
	noAdmissions, _ := newPrepareSurfaceWith(t, &recordingPreparer{state: types.PrepareStateScheduled}, false)
	if _, err := noAdmissions.Dispatch(ctx, methods.MethodPrepare, prepareReq(caller, "")); codeOf(t, err) != protoerrors.CodeUnknownMethod {
		t.Fatalf("prepare without an admission authority err = %v", err)
	}
	if _, err := surface.Dispatch(ctx, methods.MethodPrepare, &types.StartRequest{}); codeOf(t, err) != protoerrors.CodeInvalidRequest {
		t.Fatalf("wrong request type err = %v", err)
	}
}
