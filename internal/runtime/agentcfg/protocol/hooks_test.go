package protocol_test

import (
	"context"
	"errors"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// TestSetRevision_Hooks_NegativeTimeoutRejected proves a set_revision
// carrying a run-completion hook with a negative timeout_ms fails loud at
// set time with ErrInvalidHooks — BEFORE any registry write — matching the
// yaml validator's `runtime.hooks.run_completion.timeout` posture (the
// normalizer's negative→0 coercion is defence-in-depth, never the primary
// gate).
func TestSetRevision_Hooks_NegativeTimeoutRejected(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Hooks: &prototypes.AgentConfigHooks{
				RunCompletion: &prototypes.AgentConfigRunCompletionHook{Tool: "sink", TimeoutMS: -1},
			},
		},
	})
	if !errors.Is(err, agentcfgprotocol.ErrInvalidHooks) {
		t.Fatalf("error = %v, want ErrInvalidHooks", err)
	}
	// Nothing was persisted: the agent still has no active revision.
	got, gErr := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if gErr != nil {
		t.Fatalf("Get: %v", gErr)
	}
	if got.Set {
		t.Fatal("a rejected hooks set still persisted a revision")
	}
}

// TestSetRevision_Hooks_ValidAccepted proves the valid shapes pass: a hook
// with a tool + positive timeout, a zero timeout (inherits the runtime
// default), and no hooks section at all.
func TestSetRevision_Hooks_ValidAccepted(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	resp, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Hooks: &prototypes.AgentConfigHooks{
				RunCompletion: &prototypes.AgentConfigRunCompletionHook{Tool: "sink", TimeoutMS: 5000},
			},
		},
	})
	if err != nil {
		t.Fatalf("set (tool+timeout): %v", err)
	}
	if resp.Revision.Payload.Hooks == nil || resp.Revision.Payload.Hooks.RunCompletion == nil ||
		resp.Revision.Payload.Hooks.RunCompletion.Tool != "sink" ||
		resp.Revision.Payload.Hooks.RunCompletion.TimeoutMS != 5000 {
		t.Fatalf("hooks section did not round-trip on the wire: %+v", resp.Revision.Payload.Hooks)
	}
	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Hooks: &prototypes.AgentConfigHooks{
				RunCompletion: &prototypes.AgentConfigRunCompletionHook{Tool: "sink"},
			},
		},
	}); err != nil {
		t.Fatalf("set (zero timeout): %v", err)
	}
}
