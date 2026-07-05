// Package integration — Phase 152 (D-283): the section-setter Hooks
// carry-forward regression at the integration seam (real Protocol Service +
// real wire handler + real StateStore-backed registry + the run-start
// projection). Every section-scoped agent-config setter rebuilds the config
// envelope by hand, and none of the five carried the Hooks section (D-280)
// forward — a section-scoped edit silently erased a pinned run-completion
// hook. This test proves the fix holds at the seam a Console-driven
// coordinator control plane actually exercises: set_revision pins a hook,
// an INTERLEAVED section-scoped edit fires through the real wire handler,
// and the SAME run-start projection helper the production run loop calls
// (projection.ActiveRunCompletionHook, CLAUDE.md §17.4 — no test-local copy)
// must still resolve the pinned hook. The §17.3 failure mode is covered too:
// a REJECTED sibling edit (an invalid loading mode → 400 invalid_request)
// records no revision and never corrupts the pinned hook.
package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

// TestE2E_AgentConfig_HooksSurviveInterleavedSectionEdit is the D-283
// integration-level regression.
func TestE2E_AgentConfig_HooksSurviveInterleavedSectionEdit(t *testing.T) {
	h := newACHarness(t)
	ctx := context.Background()

	// Pin a run-completion hook via set_revision.
	_ = decode[prototypes.AgentConfigSetRevisionResponse](t, h.call(t, "/v1/agent_config/set_revision", prototypes.AgentConfigSetRevisionRequest{
		AgentID: acAgent,
		Payload: prototypes.AgentConfigPayload{
			Hooks: &prototypes.AgentConfigHooks{
				RunCompletion: &prototypes.AgentConfigRunCompletionHook{Tool: "run-transcript-sink", TimeoutMS: 6000},
			},
		},
	}, adminScopes()))

	// An INTERLEAVED section-scoped edit that owns only the tool-exposure
	// section — before this phase, this silently wiped the hook above.
	rec := h.call(t, "/v1/agent_config/set_tool_exposure", prototypes.AgentConfigSetToolExposureRequest{
		AgentID:      acAgent,
		ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"interleaved-srv"}},
	}, adminScopes())
	if rec.Code != http.StatusOK {
		t.Fatalf("set_tool_exposure status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The run-start projection — the SAME helper the production run loop and
	// the devstack twin call — must still resolve the pinned hook.
	id := identity.Quadruple{Identity: identity.Identity{TenantID: acTenant, UserID: acUser, SessionID: acSession}}
	spec, ok, err := projection.ActiveRunCompletionHook(ctx, h.registry, acAgent, id, nil)
	if err != nil {
		t.Fatalf("projection.ActiveRunCompletionHook: %v", err)
	}
	if !ok || spec == nil {
		t.Fatal("run-completion hook did not resolve after the interleaved tool-exposure edit (D-283 regression)")
	}
	if spec.Tool != "run-transcript-sink" || spec.Timeout != 6*time.Second {
		t.Fatalf("resolved hook = %+v, want tool=run-transcript-sink timeout=6s", spec)
	}
	if spec.AgentID != acAgent {
		t.Errorf("resolved hook AgentID = %q, want %q", spec.AgentID, acAgent)
	}

	// The tool-exposure edit itself DID apply — confirms this is a real
	// section-scoped write, not a no-op that trivially preserved the hook.
	get := decode[prototypes.AgentConfigGetResponse](t, h.call(t, "/v1/agent_config/get", prototypes.AgentConfigGetRequest{AgentID: acAgent}, adminScopes()))
	if get.Revision == nil || get.Revision.Payload.ToolExposure == nil ||
		len(get.Revision.Payload.ToolExposure.PausedServers) != 1 ||
		get.Revision.Payload.ToolExposure.PausedServers[0] != "interleaved-srv" {
		t.Fatalf("interleaved tool-exposure edit did not apply: %+v", get.Revision)
	}
	goodRevisionID := get.Revision.RevisionID

	// --- Failure mode (§17.3): a REJECTED sibling edit never corrupts the
	// hook. An invalid loading-mode value fails loud at the wire edge (400
	// invalid_request), records NO revision, and the pinned hook still
	// resolves through the same projection helper afterwards.
	badRec := h.call(t, "/v1/agent_config/set_tool_exposure", prototypes.AgentConfigSetToolExposureRequest{
		AgentID: acAgent,
		ToolExposure: prototypes.AgentConfigToolExposure{
			ToolLoadingModes: map[string]string{"some_tool": "sometimes"}, // not always|deferred
		},
	}, adminScopes())
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("invalid loading mode status=%d body=%s, want 400", badRec.Code, badRec.Body.String())
	}
	if c := errCode(t, badRec); c != protoerrors.CodeInvalidRequest {
		t.Fatalf("invalid loading mode error code = %q, want %q", c, protoerrors.CodeInvalidRequest)
	}
	// No revision was recorded by the rejected edit…
	getAfter := decode[prototypes.AgentConfigGetResponse](t, h.call(t, "/v1/agent_config/get", prototypes.AgentConfigGetRequest{AgentID: acAgent}, adminScopes()))
	if getAfter.Revision == nil || getAfter.Revision.RevisionID != goodRevisionID {
		t.Fatalf("a rejected sibling edit recorded a revision: before=%s after=%+v", goodRevisionID, getAfter.Revision)
	}
	// …and the hook still resolves, unchanged.
	spec, ok, err = projection.ActiveRunCompletionHook(ctx, h.registry, acAgent, id, nil)
	if err != nil {
		t.Fatalf("projection.ActiveRunCompletionHook after rejected edit: %v", err)
	}
	if !ok || spec == nil || spec.Tool != "run-transcript-sink" || spec.Timeout != 6*time.Second {
		t.Fatalf("hook corrupted by a REJECTED sibling edit: ok=%v spec=%+v", ok, spec)
	}
}
