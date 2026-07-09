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

// TestSetToolExposure_PreservesPinnedHook is the D-283 headline regression at
// the Protocol service layer: set_revision pins a Hooks.RunCompletion section,
// then set_tool_exposure flips a loading-mode override (a section-scoped edit
// that owns ONLY ToolExposure) — a subsequent get must still show the hook
// intact. Before this phase, SetToolExposure rebuilt the envelope without
// carrying Hooks forward, silently erasing the pinned hook (CLAUDE.md §13).
func TestSetToolExposure_PreservesPinnedHook(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Hooks: &prototypes.AgentConfigHooks{
				RunCompletion: &prototypes.AgentConfigRunCompletionHook{Tool: "run-transcript-sink", TimeoutMS: 4000},
			},
		},
	}); err != nil {
		t.Fatalf("seed hooks: %v", err)
	}
	if _, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{
			ToolLoadingModes: map[string]string{"some_tool": "deferred"},
		},
	}); err != nil {
		t.Fatalf("set_tool_exposure: %v", err)
	}
	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Revision == nil || got.Revision.Payload.Hooks == nil || got.Revision.Payload.Hooks.RunCompletion == nil {
		t.Fatalf("hooks section dropped by set_tool_exposure: %+v", got.Revision)
	}
	if hook := got.Revision.Payload.Hooks.RunCompletion; hook.Tool != "run-transcript-sink" || hook.TimeoutMS != 4000 {
		t.Fatalf("hooks section altered by set_tool_exposure: %+v", hook)
	}
	// The owned section did apply — confirms the setter actually ran, not a
	// no-op that trivially preserved everything.
	if got.Revision.Payload.ToolExposure == nil || got.Revision.Payload.ToolExposure.ToolLoadingModes["some_tool"] != "deferred" {
		t.Fatalf("tool-exposure edit did not apply: %+v", got.Revision.Payload.ToolExposure)
	}
}

// TestAddMCPConnection_PreservesPinnedHook is the D-283 headline regression
// for add_mcp_connection: set_revision pins a Hooks.RunCompletion section,
// then add_mcp_connection attaches a NEW server (a section-scoped edit that
// owns ONLY Connections) — a subsequent get must still show the hook intact.
func TestAddMCPConnection_PreservesPinnedHook(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	attacher := &fakeAttacher{result: nil} // nil = online
	s, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithConnectionAttacher(attacher))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Hooks: &prototypes.AgentConfigHooks{
				RunCompletion: &prototypes.AgentConfigRunCompletionHook{Tool: "run-transcript-sink", TimeoutMS: 4000},
			},
		},
	}); err != nil {
		t.Fatalf("seed hooks: %v", err)
	}
	if _, err := s.AddMCPConnection(ctx, prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID,
		Connection: prototypes.AgentConfigMCPConnectionDescriptor{
			Name: "new-server", Transport: "http", URL: "https://example.invalid/new-server",
		},
	}); err != nil {
		t.Fatalf("add_mcp_connection: %v", err)
	}
	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Revision == nil || got.Revision.Payload.Hooks == nil || got.Revision.Payload.Hooks.RunCompletion == nil {
		t.Fatalf("hooks section dropped by add_mcp_connection: %+v", got.Revision)
	}
	if hook := got.Revision.Payload.Hooks.RunCompletion; hook.Tool != "run-transcript-sink" || hook.TimeoutMS != 4000 {
		t.Fatalf("hooks section altered by add_mcp_connection: %+v", hook)
	}
	if got.Revision.Payload.Connections == nil || len(got.Revision.Payload.Connections.Servers) != 1 ||
		got.Revision.Payload.Connections.Servers[0].Name != "new-server" {
		t.Fatalf("connection add did not apply: %+v", got.Revision.Payload.Connections)
	}
}

// TestDiff_Hooks_ExplicitNoHookVisible proves `agent_config.diff` is NOT
// blind to the explicit no-hook revision at the wire level (mirrors
// TestDiff_Naming_BareOptOutVisible): a revision with NO hooks section
// diffed against one carrying a bare `{}` hooks section registers
// `section_present_changed` with "" → "present" — while tool and timeout are
// empty on both sides, so WITHOUT the presence dimension the per-agent
// no-hook opt-out would render as "no change in any section" in the Console
// diff view (a phantom revision).
func TestDiff_Hooks_ExplicitNoHookVisible(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Revision 1: a payload with NO hooks section (an unrelated section so
	// the envelope is non-empty).
	r1, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"s1"}},
		},
	})
	if err != nil {
		t.Fatalf("rev1: %v", err)
	}
	// Revision 2: same envelope + the bare `{}` hooks section (the explicit
	// per-agent no-hook).
	r2, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"s1"}},
			Hooks:  &prototypes.AgentConfigHooks{},
		},
	})
	if err != nil {
		t.Fatalf("rev2 (bare no-hook): %v", err)
	}
	if r1.Revision.RevisionID == r2.Revision.RevisionID {
		t.Fatal("the bare no-hook did not produce a new revision (idempotent re-set) — presence is not being preserved")
	}

	// absent → bare no-hook.
	d, err := s.Diff(ctx, prototypes.AgentConfigDiffRequest{
		Identity: scope(), AgentID: testAgentID,
		FromRevision: r1.Revision.RevisionID, ToRevision: r2.Revision.RevisionID,
	})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	h := d.Diff.Hooks
	if !h.SectionPresentChanged || h.SectionPresentFrom != "" || h.SectionPresentTo != "present" {
		t.Fatalf("absent→noHook wire diff = %+v, want section_present_changed with \"\"→\"present\"", h)
	}
	if h.RunCompletionToolChanged || h.RunCompletionTimeoutChanged {
		t.Fatalf("absent→noHook wire diff = %+v, want tool/timeout unchanged (both empty) — presence is the only signal", h)
	}

	// bare no-hook → absent (the rollback direction).
	d2, err := s.Diff(ctx, prototypes.AgentConfigDiffRequest{
		Identity: scope(), AgentID: testAgentID,
		FromRevision: r2.Revision.RevisionID, ToRevision: r1.Revision.RevisionID,
	})
	if err != nil {
		t.Fatalf("reverse diff: %v", err)
	}
	h2 := d2.Diff.Hooks
	if !h2.SectionPresentChanged || h2.SectionPresentFrom != "present" || h2.SectionPresentTo != "" {
		t.Fatalf("noHook→absent wire diff = %+v, want section_present_changed with \"present\"→\"\"", h2)
	}
}
