package stream_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

const agentPacksHandlerHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type handlerAgentPacksPort struct{}

type handlerAgentPacksResolver struct{}

func (handlerAgentPacksResolver) ResolveAgent(_ context.Context, _ identity.Identity, agentID string) (bool, error) {
	return agentID != "", nil
}

func (handlerAgentPacksPort) Inspect(_ context.Context, req types.AgentConfigAgentPacksInspectRequest) (types.AgentConfigAgentPacksInspectResponse, error) {
	pack := types.AgentConfigAgentPackItem{Name: "alpha", Trigger: "run", Steps: []string{"do"}}
	item := types.AgentConfigAgentPackInspection{PackID: "alpha", Pack: pack, Source: "both", SemanticHash: agentPacksHandlerHash, Editable: false}
	return types.AgentConfigAgentPacksInspectResponse{
		AgentID: req.AgentID, EffectivePacks: []types.AgentConfigAgentPackInspection{item},
		CompositionHash: agentPacksHandlerHash, BootPackSetHash: agentPacksHandlerHash,
	}, nil
}

func (handlerAgentPacksPort) Copy(_ context.Context, req types.AgentConfigAgentPacksCopyRequest) (types.AgentConfigAgentPacksCopyResponse, error) {
	outcomes := make([]types.AgentConfigAgentPackCopyOutcome, 0, len(req.PackIDs))
	for _, packID := range req.PackIDs {
		outcomes = append(outcomes, types.AgentConfigAgentPackCopyOutcome{PackID: packID, Outcome: "noop"})
	}
	return types.AgentConfigAgentPacksCopyResponse{
		SourceAgentID: req.SourceAgentID, TargetAgentID: req.TargetAgentID,
		Outcomes: outcomes, CompositionHash: agentPacksHandlerHash, BootPackSetHash: agentPacksHandlerHash,
	}, nil
}

func TestAgentConfigHandler_AgentPacks_UsesTypedRoutesAndGates(t *testing.T) {
	surface, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{Port: handlerAgentPacksPort{}, AgentResolver: handlerAgentPacksResolver{}})
	if err != nil {
		t.Fatalf("NewAgentPacksSurface: %v", err)
	}
	fixture := newSessionHandlerFixture(t, stream.WithAgentPacksSurface(surface))

	inspectBody := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x"}`
	code, response := acReq(t, fixture.handler, "agent_packs/inspect", inspectBody, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != 200 {
		t.Fatalf("inspect status=%d body=%s", code, response)
	}
	var inspect types.AgentConfigAgentPacksInspectResponse
	if err := json.Unmarshal(response, &inspect); err != nil {
		t.Fatalf("decode inspect: %v body=%s", err, response)
	}
	if inspect.AgentID != acAgent || len(inspect.EffectivePacks) != 1 || inspect.ProtocolVersion != types.ProtocolVersion {
		t.Fatalf("inspect response=%+v, want typed effective pack response", inspect)
	}

	copyBody := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"source_agent_id":"agent-x","target_agent_id":"agent-y","pack_ids":["alpha"],"expected_source_composition_hash":"` + agentPacksHandlerHash + `","expected_target_composition_hash":"` + agentPacksHandlerHash + `","idempotency_key":"copy-1"}`
	code, response = acReqReach(t, fixture.handler, "agent_packs/copy", copyBody, acID(), []auth.Scope{auth.ScopeAdmin}, []string{"agent-x", "agent-y"})
	if code != 200 {
		t.Fatalf("copy status=%d body=%s", code, response)
	}
	var copied types.AgentConfigAgentPacksCopyResponse
	if err := json.Unmarshal(response, &copied); err != nil {
		t.Fatalf("decode copy: %v body=%s", err, response)
	}
	if copied.TargetAgentID != "agent-y" || len(copied.Outcomes) != 1 || copied.Outcomes[0].Outcome != "noop" {
		t.Fatalf("copy response=%+v, want noop response", copied)
	}

	code, response = acReq(t, fixture.handler, "agent_packs/inspect", inspectBody, acID(), nil)
	if code != 403 || errCode(t, response) != protoerrors.CodeScopeMismatch {
		t.Fatalf("non-admin inspect=(%d,%s), want 403 scope_mismatch", code, response)
	}
	// The admin gate runs before decoding or reach checks: a caller cannot use
	// malformed or agent-specific body values as an oracle on an admin route.
	code, response = acReqReach(t, fixture.handler, "agent_packs/inspect", `{"identity":{"tenant":"foreign","user":"u1","session":"s1"},"agent_id":"unreached"}`, acID(), nil, nil)
	if code != 403 || errCode(t, response) != protoerrors.CodeScopeMismatch {
		t.Fatalf("non-admin forged/unreached inspect=(%d,%s), want 403 scope_mismatch before body/reach", code, response)
	}
	forged := `{"identity":{"tenant":"foreign","user":"u1","session":"s1"},"agent_id":"agent-x"}`
	code, response = acReq(t, fixture.handler, "agent_packs/inspect", forged, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != 401 || errCode(t, response) != protoerrors.CodeIdentityRequired {
		t.Fatalf("forged inspect=(%d,%s), want 401 identity_required", code, response)
	}
}

func TestAgentConfigHandler_AgentPacks_AbsentSurfaceFailsLoud(t *testing.T) {
	code, response := acReq(t, sessionHandler(t), "agent_packs/inspect", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x"}`, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != 500 || errCode(t, response) != protoerrors.CodeRuntimeError {
		t.Fatalf("absent surface=(%d,%s), want 500 runtime_error", code, response)
	}
}
