package stream_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// llmParamsHandler builds an agent-config handler whose service validates a
// pinned model against {model-a, model-b} (WithValidModels) — so the
// unknown-model + out-of-range set-time rejections are reachable over the wire.
func llmParamsHandler(t *testing.T) http.Handler {
	t.Helper()
	ctx := context.Background()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(ctx)
		_ = bus.Close(ctx)
		_ = st.Close(ctx)
	})
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithValidModels([]string{"model-a", "model-b"}),
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return h
}

// TestAgentConfigHandler_SetLLMParams_AdminAllowed proves an admin caller
// pins a valid model (200).
func TestAgentConfigHandler_SetLLMParams_AdminAllowed(t *testing.T) {
	h := llmParamsHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","llm_params":{"model":"model-b"}}`
	code, resp := acReq(t, h, "set_llm_params", body, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("valid model set should be 200; got %d body=%s", code, resp)
	}
}

// TestAgentConfigHandler_SetLLMParams_NonAdminRejected proves the verb stays
// admin-gated.
func TestAgentConfigHandler_SetLLMParams_NonAdminRejected(t *testing.T) {
	h := llmParamsHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","llm_params":{"model":"model-b"}}`
	code, resp := acReq(t, h, "set_llm_params", body, acID(), []auth.Scope{}) // no admin
	if code != http.StatusForbidden {
		t.Fatalf("set_llm_params should reject a non-admin caller; got %d body=%s", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeScopeMismatch {
		t.Fatalf("non-admin error code = %s, want %s", c, protoerrors.CodeScopeMismatch)
	}
}

// TestAgentConfigHandler_SetLLMParams_UnknownModelIs400 proves an unknown
// model is a CLIENT error (400 CodeInvalidRequest) — NOT a 500 server fault —
// and the offending model name survives in the message (the regression the
// adversarial review caught).
func TestAgentConfigHandler_SetLLMParams_UnknownModelIs400(t *testing.T) {
	h := llmParamsHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","llm_params":{"model":"ghost-model"}}`
	code, resp := acReq(t, h, "set_llm_params", body, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown model should be 400, not a server fault; got %d body=%s", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeInvalidRequest {
		t.Fatalf("unknown-model error code = %s, want %s", c, protoerrors.CodeInvalidRequest)
	}
}

// TestAgentConfigHandler_SetLLMParams_OutOfRangeIs400 proves an out-of-range
// sampling value (temperature > 2) is a 400 client error, validated at set
// time (never silently passed through to the provider).
func TestAgentConfigHandler_SetLLMParams_OutOfRangeIs400(t *testing.T) {
	h := llmParamsHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","llm_params":{"model":"model-a","temperature":9.0}}`
	code, resp := acReq(t, h, "set_llm_params", body, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusBadRequest {
		t.Fatalf("out-of-range temperature should be 400; got %d body=%s", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeInvalidRequest {
		t.Fatalf("out-of-range error code = %s, want %s", c, protoerrors.CodeInvalidRequest)
	}
}

// TestAgentConfigHandler_SetRevision_UnknownModelIs400 proves the full-payload
// set_revision path ALSO returns 400 for an unknown pinned model.
func TestAgentConfigHandler_SetRevision_UnknownModelIs400(t *testing.T) {
	h := llmParamsHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","payload":{"llm_params":{"model":"ghost-model"}}}`
	code, resp := acReq(t, h, "set_revision", body, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusBadRequest {
		t.Fatalf("set_revision unknown model should be 400; got %d body=%s", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeInvalidRequest {
		t.Fatalf("set_revision unknown-model error code = %s, want %s", c, protoerrors.CodeInvalidRequest)
	}
}
