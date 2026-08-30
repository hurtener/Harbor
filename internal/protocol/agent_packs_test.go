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
)

const testPackHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type recordingAgentPacksPort struct {
	mu              sync.Mutex
	inspectCalls    int
	copyCalls       int
	inspectRequest  []types.AgentConfigAgentPacksInspectRequest
	copyRequest     []types.AgentConfigAgentPacksCopyRequest
	inspectResponse *types.AgentConfigAgentPacksInspectResponse
	copyErr         error
	copyResponse    *types.AgentConfigAgentPacksCopyResponse
}

func (p *recordingAgentPacksPort) Inspect(_ context.Context, req types.AgentConfigAgentPacksInspectRequest) (types.AgentConfigAgentPacksInspectResponse, error) {
	p.mu.Lock()
	p.inspectCalls++
	p.inspectRequest = append(p.inspectRequest, req)
	response := p.inspectResponse
	p.mu.Unlock()
	if response != nil {
		return *response, nil
	}
	pack := types.AgentConfigAgentPackItem{Name: "alpha", Trigger: "run", Steps: []string{"do"}}
	return types.AgentConfigAgentPacksInspectResponse{
		AgentID: req.AgentID,
		BootPacks: []types.AgentConfigAgentPackInspection{{
			PackID: "alpha", Pack: pack, Source: "boot", SemanticHash: testPackHash, Editable: false,
		}},
		RevisionPacks: []types.AgentConfigAgentPackInspection{{
			PackID: "beta", Pack: types.AgentConfigAgentPackItem{Name: "beta", Trigger: "run", Steps: []string{"do"}}, Source: "revision", SemanticHash: testPackHash, Editable: true,
		}},
		EffectivePacks: []types.AgentConfigAgentPackInspection{{
			PackID: "alpha", Pack: pack, Source: "both", SemanticHash: testPackHash, Editable: false,
		}},
		CompositionHash: testPackHash, BootPackSetHash: testPackHash,
	}, nil
}

type agentPacksTestResolver struct {
	allow func(string) bool
}

func (r agentPacksTestResolver) ResolveAgent(_ context.Context, _ identity.Identity, agentID string) (bool, error) {
	if r.allow != nil {
		return r.allow(agentID), nil
	}
	return true, nil
}

func (p *recordingAgentPacksPort) Copy(_ context.Context, req types.AgentConfigAgentPacksCopyRequest) (types.AgentConfigAgentPacksCopyResponse, error) {
	p.mu.Lock()
	p.copyCalls++
	p.copyRequest = append(p.copyRequest, req)
	err := p.copyErr
	response := p.copyResponse
	p.mu.Unlock()
	if err != nil {
		return types.AgentConfigAgentPacksCopyResponse{}, err
	}
	if response != nil {
		return *response, nil
	}
	outcomes := make([]types.AgentConfigAgentPackCopyOutcome, 0, len(req.PackIDs))
	for _, id := range req.PackIDs {
		outcomes = append(outcomes, types.AgentConfigAgentPackCopyOutcome{PackID: id, Outcome: "copied"})
	}
	return types.AgentConfigAgentPacksCopyResponse{
		SourceAgentID: req.SourceAgentID, TargetAgentID: req.TargetAgentID,
		Outcomes: outcomes, CompositionHash: testPackHash, BootPackSetHash: testPackHash,
	}, nil
}

func verifiedPackContext(t *testing.T, scopes []auth.Scope, reach ...string) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	return auth.WithAgentReach(auth.WithScopes(ctx, scopes), reach)
}

func packIdentity() types.IdentityScope {
	return types.IdentityScope{Tenant: "tenant", User: "user", Session: "session"}
}

func TestAgentPacksSurface_InspectAndCopy_EnforcesAdminIdentityAndReach(t *testing.T) {
	port := &recordingAgentPacksPort{}
	surface, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{Port: port, AgentResolver: agentPacksTestResolver{}})
	if err != nil {
		t.Fatalf("NewAgentPacksSurface: %v", err)
	}
	ctx := verifiedPackContext(t, []auth.Scope{auth.ScopeAdmin}, "source", "target")

	inspect, err := surface.Dispatch(ctx, methods.MethodAgentConfigAgentPacksInspect, &types.AgentConfigAgentPacksInspectRequest{
		Identity: packIdentity(), AgentID: "source",
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	gotInspect, ok := inspect.(*types.AgentConfigAgentPacksInspectResponse)
	if !ok || gotInspect == nil || len(gotInspect.BootPacks) != 1 || len(gotInspect.RevisionPacks) != 1 || len(gotInspect.EffectivePacks) != 1 {
		t.Fatalf("inspect response = %#v, want distinct boot/revision/effective views", inspect)
	}
	if gotInspect.EffectivePacks[0].Source != "both" || gotInspect.ProtocolVersion != types.ProtocolVersion {
		t.Fatalf("effective inspect item/version = (%q,%q), want both/%q", gotInspect.EffectivePacks[0].Source, gotInspect.ProtocolVersion, types.ProtocolVersion)
	}

	copyResp, err := surface.Dispatch(ctx, methods.MethodAgentConfigAgentPacksCopy, &types.AgentConfigAgentPacksCopyRequest{
		Identity: packIdentity(), SourceAgentID: "source", TargetAgentID: "target", PackIDs: []string{"alpha"},
		ExpectedSourceCompositionHash: testPackHash, ExpectedTargetCompositionHash: testPackHash,
		IdempotencyKey: "copy-1",
	})
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	gotCopy, ok := copyResp.(*types.AgentConfigAgentPacksCopyResponse)
	if !ok || gotCopy == nil || len(gotCopy.Outcomes) != 1 || gotCopy.Outcomes[0].Outcome != "copied" {
		t.Fatalf("copy response = %#v, want copied outcome", copyResp)
	}
	port.mu.Lock()
	if port.inspectCalls != 1 || port.copyCalls != 1 || port.copyRequest[0].Identity != packIdentity() {
		t.Fatalf("port calls/identity = (%d,%d,%+v), want one each and verified identity", port.inspectCalls, port.copyCalls, port.copyRequest)
	}
	port.mu.Unlock()

	for name, request := range map[string]any{
		"forged identity": &types.AgentConfigAgentPacksInspectRequest{
			Identity: types.IdentityScope{Tenant: "foreign", User: "user", Session: "session"}, AgentID: "source",
		},
		"missing admin":    &types.AgentConfigAgentPacksInspectRequest{Identity: packIdentity(), AgentID: "source"},
		"unreached source": &types.AgentConfigAgentPacksInspectRequest{Identity: packIdentity(), AgentID: "other"},
	} {
		callCtx := ctx
		if name == "missing admin" {
			callCtx = verifiedPackContext(t, nil, "source")
		}
		if name == "unreached source" {
			callCtx = verifiedPackContext(t, []auth.Scope{auth.ScopeAdmin}, "target")
		}
		_, callErr := surface.Dispatch(callCtx, methods.MethodAgentConfigAgentPacksInspect, request)
		if callErr == nil {
			t.Fatalf("%s: expected refusal", name)
		}
		var perr *protoerrors.Error
		if !errors.As(callErr, &perr) {
			t.Fatalf("%s: error = %T %v, want Protocol error", name, callErr, callErr)
		}
		want := protoerrors.CodeScopeMismatch
		if name == "missing admin" {
			want = protoerrors.CodeIdentityScopeRequired
		}
		if perr.Code != want {
			t.Fatalf("%s: code = %s, want %s", name, perr.Code, want)
		}
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.inspectCalls != 1 {
		t.Fatalf("refused calls reached port: %d, want 1 successful call", port.inspectCalls)
	}
}

type codedPackError struct{ code protocol.AgentPacksErrorCode }

func (e codedPackError) Error() string { return "domain detail must not cross the Protocol boundary" }

func (e codedPackError) AgentPacksErrorCode() protocol.AgentPacksErrorCode { return e.code }

func TestAgentPacksSurface_CopyConflictIsClosedAndTyped(t *testing.T) {
	port := &recordingAgentPacksPort{copyErr: codedPackError{code: protocol.AgentPacksErrorConflict}}
	surface, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{Port: port, AgentResolver: agentPacksTestResolver{}})
	if err != nil {
		t.Fatalf("NewAgentPacksSurface: %v", err)
	}
	ctx := verifiedPackContext(t, []auth.Scope{auth.ScopeAdmin}, "source", "target")
	_, err = surface.Dispatch(ctx, methods.MethodAgentConfigAgentPacksCopy, &types.AgentConfigAgentPacksCopyRequest{
		Identity: packIdentity(), SourceAgentID: "source", TargetAgentID: "target", PackIDs: []string{"alpha"},
		ExpectedSourceCompositionHash: testPackHash, ExpectedTargetCompositionHash: testPackHash, IdempotencyKey: "copy-1",
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) || perr.Code != protoerrors.CodeAgentPackCopyConflict {
		t.Fatalf("copy conflict = %T %v, want agent_pack_copy_conflict", err, err)
	}

	port.copyErr = nil
	port.copyResponse = &types.AgentConfigAgentPacksCopyResponse{
		SourceAgentID: "source", TargetAgentID: "target",
		Outcomes: []types.AgentConfigAgentPackCopyOutcome{{PackID: "alpha", Outcome: "conflict"}},
	}
	_, err = surface.Dispatch(ctx, methods.MethodAgentConfigAgentPacksCopy, &types.AgentConfigAgentPacksCopyRequest{
		Identity: packIdentity(), SourceAgentID: "source", TargetAgentID: "target", PackIDs: []string{"alpha"},
		ExpectedSourceCompositionHash: testPackHash, ExpectedTargetCompositionHash: testPackHash, IdempotencyKey: "copy-2",
	})
	if !errors.As(err, &perr) || perr.Code != protoerrors.CodeRuntimeError {
		t.Fatalf("successful conflict outcome = %T %v, want runtime_error", err, err)
	}
}

func TestAgentPacksSurface_RejectsNonCanonicalRuntimeHashes(t *testing.T) {
	ctx := verifiedPackContext(t, []auth.Scope{auth.ScopeAdmin}, "source", "target")
	inspectCases := map[string]func(*types.AgentConfigAgentPacksInspectResponse){
		"missing composition hash": func(resp *types.AgentConfigAgentPacksInspectResponse) {
			resp.CompositionHash = ""
		},
		"missing boot hash": func(resp *types.AgentConfigAgentPacksInspectResponse) {
			resp.BootPackSetHash = ""
		},
		"non-canonical semantic hash": func(resp *types.AgentConfigAgentPacksInspectResponse) {
			resp.EffectivePacks[0].SemanticHash = "not-a-sha256"
		},
	}
	for name, mutate := range inspectCases {
		t.Run("inspect "+name, func(t *testing.T) {
			pack := types.AgentConfigAgentPackItem{Name: "alpha", Trigger: "run", Steps: []string{"do"}}
			response := types.AgentConfigAgentPacksInspectResponse{
				AgentID: "source",
				EffectivePacks: []types.AgentConfigAgentPackInspection{{
					PackID: "alpha", Pack: pack, Source: "both", SemanticHash: testPackHash,
				}},
				CompositionHash: testPackHash, BootPackSetHash: testPackHash,
			}
			mutate(&response)
			port := &recordingAgentPacksPort{inspectResponse: &response}
			surface, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{Port: port, AgentResolver: agentPacksTestResolver{}})
			if err != nil {
				t.Fatalf("NewAgentPacksSurface: %v", err)
			}
			_, err = surface.Dispatch(ctx, methods.MethodAgentConfigAgentPacksInspect, &types.AgentConfigAgentPacksInspectRequest{
				Identity: packIdentity(), AgentID: "source",
			})
			var perr *protoerrors.Error
			if !errors.As(err, &perr) || perr.Code != protoerrors.CodeRuntimeError {
				t.Fatalf("inspect hash validation = %T %v, want runtime_error", err, err)
			}
		})
	}

	port := &recordingAgentPacksPort{copyResponse: &types.AgentConfigAgentPacksCopyResponse{
		SourceAgentID: "source", TargetAgentID: "target",
		Outcomes:        []types.AgentConfigAgentPackCopyOutcome{{PackID: "alpha", Outcome: "noop"}},
		CompositionHash: testPackHash,
	}}
	surface, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{Port: port, AgentResolver: agentPacksTestResolver{}})
	if err != nil {
		t.Fatalf("NewAgentPacksSurface: %v", err)
	}
	req := validCopyRequest()
	_, err = surface.Dispatch(ctx, methods.MethodAgentConfigAgentPacksCopy, &req)
	var perr *protoerrors.Error
	if !errors.As(err, &perr) || perr.Code != protoerrors.CodeRuntimeError {
		t.Fatalf("copy missing boot hash validation = %T %v, want runtime_error", err, err)
	}
}

func validCopyRequest() types.AgentConfigAgentPacksCopyRequest {
	return types.AgentConfigAgentPacksCopyRequest{
		Identity:                      packIdentity(),
		SourceAgentID:                 "source",
		TargetAgentID:                 "target",
		PackIDs:                       []string{"alpha"},
		ExpectedSourceCompositionHash: testPackHash,
		ExpectedTargetCompositionHash: testPackHash,
		IdempotencyKey:                "copy-1",
	}
}

func TestAgentPacksSurface_CopyValidationIsBoundedAndFailClosed(t *testing.T) {
	port := &recordingAgentPacksPort{}
	surface, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{Port: port, AgentResolver: agentPacksTestResolver{}})
	if err != nil {
		t.Fatalf("NewAgentPacksSurface: %v", err)
	}
	ctx := verifiedPackContext(t, []auth.Scope{auth.ScopeAdmin}, "source", "target")

	cases := map[string]func(*types.AgentConfigAgentPacksCopyRequest){
		"duplicate selected pack": func(req *types.AgentConfigAgentPacksCopyRequest) {
			req.PackIDs = []string{"alpha", "alpha"}
		},
		"non-canonical duplicate selected pack": func(req *types.AgentConfigAgentPacksCopyRequest) {
			req.PackIDs = []string{"alpha", "ALPHA"}
		},
		"too many selected packs": func(req *types.AgentConfigAgentPacksCopyRequest) {
			req.PackIDs = make([]string, 129)
			for i := range req.PackIDs {
				req.PackIDs[i] = "pack-" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
			}
		},
		"missing source CAS": func(req *types.AgentConfigAgentPacksCopyRequest) {
			req.ExpectedSourceCompositionHash = ""
		},
		"missing target CAS": func(req *types.AgentConfigAgentPacksCopyRequest) {
			req.ExpectedTargetCompositionHash = ""
		},
		"missing idempotency": func(req *types.AgentConfigAgentPacksCopyRequest) {
			req.IdempotencyKey = ""
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := validCopyRequest()
			mutate(&req)
			_, err := surface.Dispatch(ctx, methods.MethodAgentConfigAgentPacksCopy, &req)
			var perr *protoerrors.Error
			if !errors.As(err, &perr) || perr.Code != protoerrors.CodeInvalidRequest {
				t.Fatalf("validation error = %T %v, want invalid_request", err, err)
			}
		})
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.copyCalls != 0 {
		t.Fatalf("invalid requests reached runtime port %d times, want 0", port.copyCalls)
	}
}

func TestAgentPacksSurface_RequiresSameRuntimeRegistrationBeforeCopy(t *testing.T) {
	port := &recordingAgentPacksPort{}
	surface, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{
		Port: port, AgentResolver: agentPacksTestResolver{allow: func(agentID string) bool { return agentID == "source" }},
	})
	if err != nil {
		t.Fatalf("NewAgentPacksSurface: %v", err)
	}
	ctx := verifiedPackContext(t, []auth.Scope{auth.ScopeAdmin}, "source", "target")
	_, err = surface.Dispatch(ctx, methods.MethodAgentConfigAgentPacksCopy, &types.AgentConfigAgentPacksCopyRequest{
		Identity: packIdentity(), SourceAgentID: "source", TargetAgentID: "target", PackIDs: []string{"alpha"},
		ExpectedSourceCompositionHash: testPackHash, ExpectedTargetCompositionHash: testPackHash, IdempotencyKey: "unknown-target",
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) || perr.Code != protoerrors.CodeInvalidRequest {
		t.Fatalf("unknown target registration = %T %v, want invalid_request", err, err)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.copyCalls != 0 {
		t.Fatalf("unknown target reached runtime port %d times, want 0", port.copyCalls)
	}
}

func TestAgentPacksSurface_RequiresResolver(t *testing.T) {
	_, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{Port: &recordingAgentPacksPort{}})
	if !errors.Is(err, protocol.ErrAgentPacksMisconfigured) {
		t.Fatalf("missing resolver error = %v, want ErrAgentPacksMisconfigured", err)
	}
}

func TestAgentPacksSurface_CopyRequiresReachToBothAgents(t *testing.T) {
	port := &recordingAgentPacksPort{}
	surface, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{Port: port, AgentResolver: agentPacksTestResolver{}})
	if err != nil {
		t.Fatalf("NewAgentPacksSurface: %v", err)
	}
	req := validCopyRequest()
	for name, reach := range map[string][]string{
		"source only": {"source"},
		"target only": {"target"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := verifiedPackContext(t, []auth.Scope{auth.ScopeAdmin}, reach...)
			_, err := surface.Dispatch(ctx, methods.MethodAgentConfigAgentPacksCopy, &req)
			var perr *protoerrors.Error
			if !errors.As(err, &perr) || perr.Code != protoerrors.CodeScopeMismatch {
				t.Fatalf("reach error = %T %v, want scope_mismatch", err, err)
			}
		})
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.copyCalls != 0 {
		t.Fatalf("unreached copy reached runtime port %d times, want 0", port.copyCalls)
	}
}

func TestAgentPacksSurface_MapsDomainErrorsWithoutLeakingDetails(t *testing.T) {
	cases := []struct {
		name string
		code protocol.AgentPacksErrorCode
		want protoerrors.Code
	}{
		{name: "invalid", code: protocol.AgentPacksErrorInvalid, want: protoerrors.CodeInvalidRequest},
		{name: "not found", code: protocol.AgentPacksErrorNotFound, want: protoerrors.CodeNotFound},
		{name: "stale", code: protocol.AgentPacksErrorStale, want: protoerrors.CodeRevisionConflict},
		{name: "conflict", code: protocol.AgentPacksErrorConflict, want: protoerrors.CodeAgentPackCopyConflict},
		{name: "idempotency conflict", code: protocol.AgentPacksErrorIdempotencyConflict, want: protoerrors.CodeAgentPackCopyIdempotencyConflict},
		{name: "unavailable", code: protocol.AgentPacksErrorUnavailable, want: protoerrors.CodeRuntimeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port := &recordingAgentPacksPort{copyErr: codedPackError{code: tc.code}}
			surface, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{
				Port:          port,
				AgentResolver: agentPacksTestResolver{},
				ClassifyError: func(err error) protocol.AgentPacksErrorCode {
					if coded, ok := err.(codedPackError); ok {
						return coded.code
					}
					return ""
				},
			})
			if err != nil {
				t.Fatalf("NewAgentPacksSurface: %v", err)
			}
			ctx := verifiedPackContext(t, []auth.Scope{auth.ScopeAdmin}, "source", "target")
			req := validCopyRequest()
			_, err = surface.Dispatch(ctx, methods.MethodAgentConfigAgentPacksCopy, &req)
			var perr *protoerrors.Error
			if !errors.As(err, &perr) || perr.Code != tc.want {
				t.Fatalf("mapped error = %T %v, want %s", err, err, tc.want)
			}
			if strings.Contains(perr.Message, "domain detail") {
				t.Fatalf("mapped Protocol message leaked domain detail: %q", perr.Message)
			}
		})
	}
}

func TestAgentPacksSurface_ConcurrentReuse_Isolated(t *testing.T) {
	port := &recordingAgentPacksPort{}
	surface, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{Port: port, AgentResolver: agentPacksTestResolver{}})
	if err != nil {
		t.Fatalf("NewAgentPacksSurface: %v", err)
	}
	const n = 120
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			agentID := "agent-" + strings.TrimSpace(string(rune('a'+i%26))) + strings.Repeat("x", i/26)
			ctx, verifyErr := identity.WithVerified(context.Background(), identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session-" + agentID})
			if verifyErr != nil {
				errCh <- verifyErr
				return
			}
			ctx = auth.WithAgentReach(auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin}), []string{agentID})
			resp, callErr := surface.Dispatch(ctx, methods.MethodAgentConfigAgentPacksInspect, &types.AgentConfigAgentPacksInspectRequest{
				Identity: types.IdentityScope{Tenant: "tenant", User: "user", Session: "session-" + agentID}, AgentID: agentID,
			})
			if callErr != nil {
				errCh <- callErr
				return
			}
			got, ok := resp.(*types.AgentConfigAgentPacksInspectResponse)
			if !ok || got.AgentID != agentID {
				errCh <- errors.New("concurrent inspect response crossed agent identity")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent inspect: %v", err)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.inspectCalls != n {
		t.Fatalf("inspect calls = %d, want %d", port.inspectCalls, n)
	}
}
