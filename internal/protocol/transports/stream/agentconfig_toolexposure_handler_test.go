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

// toolExposureHandler builds a bare agent-config handler (no ModelProfiles
// needed — the loading-mode edge validation is orthogonal to the LLM-params
// gate).
func toolExposureHandler(t *testing.T) http.Handler {
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
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return h
}

// TestAgentConfigHandler_SetToolExposure_LoadingModes_AdminAllowed proves an
// admin caller sets valid loading-mode overrides over the wire (200) and the
// recorded revision carries them (D-281).
func TestAgentConfigHandler_SetToolExposure_LoadingModes_AdminAllowed(t *testing.T) {
	h := toolExposureHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `",` +
		`"tool_exposure":{"server_loading_modes":{"srvA":"always"},"tool_loading_modes":{"srvA_x":"deferred"}}}`
	code, resp := acReq(t, h, "set_tool_exposure", body, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("valid loading-mode overrides should be 200; got %d body=%s", code, resp)
	}
}

// TestAgentConfigHandler_SetToolExposure_UnknownLoadingModeIs400 proves an
// unknown loading-mode value (D-281) is a 400 CLIENT error, not a server
// fault, over the wire — the invalid_request Protocol code.
func TestAgentConfigHandler_SetToolExposure_UnknownLoadingModeIs400(t *testing.T) {
	h := toolExposureHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `",` +
		`"tool_exposure":{"tool_loading_modes":{"srvA_x":"sometimes"}}}`
	code, resp := acReq(t, h, "set_tool_exposure", body, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown loading mode should be 400, not a server fault; got %d body=%s", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeInvalidRequest {
		t.Fatalf("unknown loading-mode error code = %s, want %s", c, protoerrors.CodeInvalidRequest)
	}
}

// TestAgentConfigHandler_SetToolExposure_NonAdminRejected proves the
// loading-mode fields ride the SAME admin-gated verb — no new scope surface.
func TestAgentConfigHandler_SetToolExposure_NonAdminRejected(t *testing.T) {
	h := toolExposureHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `",` +
		`"tool_exposure":{"server_loading_modes":{"srvA":"always"}}}`
	code, resp := acReq(t, h, "set_tool_exposure", body, acID(), []auth.Scope{})
	if code != http.StatusForbidden {
		t.Fatalf("set_tool_exposure should reject a non-admin caller; got %d body=%s", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeScopeMismatch {
		t.Fatalf("non-admin error code = %s, want %s", c, protoerrors.CodeScopeMismatch)
	}
}
