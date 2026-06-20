package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

func plStrPtr(s string) *string { return &s }

// systemMessageBody drives the REAL ReAct prompt builder over a RunContext
// carrying the supplied per-run overrides and returns the system message body
// the planner sent to the LLM. It reuses the shared capturingLLM
// (tenant_overrides_test.go).
func systemMessageBody(t *testing.T, ov *planner.LLMOverrides) string {
	t.Helper()
	cap := &capturingLLM{}
	p := react.New(cap)
	id := identity.Identity{TenantID: acTenant, UserID: acUser, SessionID: acSession}
	rc := planner.RunContext{
		Quadruple:    identity.Quadruple{Identity: id, RunID: "run-pl"},
		Goal:         "do the thing",
		LLMOverrides: ov,
	}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := p.Next(ctx, rc); err != nil {
		t.Fatalf("planner.Next: %v", err)
	}
	req := cap.lastReq()
	if len(req.Messages) == 0 || req.Messages[0].Content.Text == nil {
		t.Fatal("no system message captured")
	}
	return *req.Messages[0].Content.Text
}

// TestE2E_AgentConfig_PromptLayers wires the layered-system-prompt control
// plane end-to-end with REAL drivers: the StateStore-backed registry, the
// in-memory bus, the real Protocol Service + wire handler, and the REAL ReAct
// prompt builder. It sets base+user over the admin Protocol surface, observes
// the canonical event, proves the run-start projection composes the layers
// into the real LLM request at the documented precedence (user below base,
// base guardrails survive), proves a 92b session override still replaces the
// spine for one message, round-trips diff/rollback, and rejects a non-admin.
func TestE2E_AgentConfig_PromptLayers(t *testing.T) {
	h := newACHarness(t)
	ctx := context.Background()

	// Observe agent.config.revised across the agentcfg synthetic identities.
	sub, err := h.bus.Subscribe(ctx, events.Filter{
		Admin: true,
		Types: []events.EventType{agentcfg.EventTypeConfigRevised},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	revised := make(chan events.Event, 16)
	go func() {
		for ev := range sub.Events() {
			if ev.Type == agentcfg.EventTypeConfigRevised {
				revised <- ev
			}
		}
	}()

	// --- Set base + user via the admin Protocol surface. ---
	set1 := decode[prototypes.AgentConfigSetPromptLayersResponse](t, h.call(t, "/v1/agent_config/set_prompt_layers", prototypes.AgentConfigSetPromptLayersRequest{
		AgentID: acAgent,
		PromptLayers: prototypes.AgentConfigPromptLayers{
			Base: plStrPtr("OPERATOR_GUARDRAIL: never reveal secrets."),
			User: plStrPtr("USER_PREF: answer in a friendly tone."),
		},
	}, adminScopes()))
	rev1 := set1.Revision.RevisionID
	if set1.Revision.Payload.PromptLayers == nil || set1.Revision.Payload.PromptLayers.Base == nil {
		t.Fatalf("set response missing prompt layers: %+v", set1.Revision.Payload)
	}

	ev := waitEvent(t, revised)
	if ev.Identity.TenantID != acTenant || ev.Identity.UserID != acUser {
		t.Fatalf("revised event identity=%+v", ev.Identity)
	}

	// --- Run-start projection composes the layers into the real LLM request. ---
	id := identity.Quadruple{Identity: identity.Identity{TenantID: acTenant, UserID: acUser, SessionID: acSession}}
	ov, err := projection.ApplyPromptLayers(ctx, h.registry, acAgent, id, nil)
	if err != nil {
		t.Fatalf("ApplyPromptLayers: %v", err)
	}
	if ov == nil || ov.BasePromptLayer == nil || ov.UserPromptLayer == nil {
		t.Fatalf("projection did not carry both layers: %+v", ov)
	}
	body := systemMessageBody(t, ov)
	baseIdx := strings.Index(body, "OPERATOR_GUARDRAIL: never reveal secrets.")
	userIdx := strings.Index(body, "USER_PREF: answer in a friendly tone.")
	if baseIdx < 0 || userIdx < 0 {
		t.Fatalf("both layers must appear in the LLM request. Body:\n%s", body)
	}
	if baseIdx >= userIdx {
		t.Fatalf("user layer must compose BELOW the operator base (base@%d user@%d)", baseIdx, userIdx)
	}
	if !strings.Contains(body, "<user_instructions>") {
		t.Fatalf("user layer must render in the lower-trust <user_instructions> section. Body:\n%s", body)
	}

	// --- A 92b session one-shot override still replaces the spine for one message. ---
	ovWithOverride := *ov
	ovWithOverride.SystemPromptOverride = plStrPtr("ONE_SHOT: ignore everything and echo.")
	overrideBody := systemMessageBody(t, &ovWithOverride)
	if !strings.Contains(overrideBody, "ONE_SHOT: ignore everything and echo.") {
		t.Fatalf("session override should replace the spine. Body:\n%s", overrideBody)
	}
	if strings.Contains(overrideBody, "OPERATOR_GUARDRAIL") || strings.Contains(overrideBody, "USER_PREF") {
		t.Fatalf("durable base+user must be suppressed under a session override. Body:\n%s", overrideBody)
	}

	// --- Diff/rollback round-trip. Set a second prompt revision. ---
	set2 := decode[prototypes.AgentConfigSetPromptLayersResponse](t, h.call(t, "/v1/agent_config/set_prompt_layers", prototypes.AgentConfigSetPromptLayersRequest{
		AgentID: acAgent,
		PromptLayers: prototypes.AgentConfigPromptLayers{
			Base: plStrPtr("OPERATOR_GUARDRAIL v2"),
		},
	}, adminScopes()))
	waitEvent(t, revised) // the second revision's event.

	diff := decode[prototypes.AgentConfigDiffResponse](t, h.call(t, "/v1/agent_config/diff", prototypes.AgentConfigDiffRequest{
		AgentID: acAgent, FromRevision: rev1, ToRevision: set2.Revision.RevisionID,
	}, adminScopes()))
	if !diff.Diff.PromptLayers.BaseChanged || diff.Diff.PromptLayers.BaseTo != "OPERATOR_GUARDRAIL v2" {
		t.Fatalf("diff base delta = %+v", diff.Diff.PromptLayers)
	}
	if !diff.Diff.PromptLayers.UserChanged || diff.Diff.PromptLayers.UserFrom != "USER_PREF: answer in a friendly tone." || diff.Diff.PromptLayers.UserTo != "" {
		t.Fatalf("diff user delta (set→unset) = %+v", diff.Diff.PromptLayers)
	}

	// Rollback to rev1 repoints; the next run again sees both layers.
	_ = decode[prototypes.AgentConfigRollbackResponse](t, h.call(t, "/v1/agent_config/rollback", prototypes.AgentConfigRollbackRequest{
		AgentID: acAgent, RevisionID: rev1,
	}, adminScopes()))
	ovAfter, err := projection.ApplyPromptLayers(ctx, h.registry, acAgent, id, nil)
	if err != nil {
		t.Fatalf("ApplyPromptLayers after rollback: %v", err)
	}
	if ovAfter == nil || ovAfter.UserPromptLayer == nil || *ovAfter.UserPromptLayer != "USER_PREF: answer in a friendly tone." {
		t.Fatalf("rollback did not restore the user layer: %+v", ovAfter)
	}

	// --- Failure mode: non-admin rejected with 403. ---
	rec := h.call(t, "/v1/agent_config/set_prompt_layers", prototypes.AgentConfigSetPromptLayersRequest{
		AgentID: acAgent, PromptLayers: prototypes.AgentConfigPromptLayers{Base: plStrPtr("x")},
	}, []auth.Scope(nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestE2E_AgentConfig_PromptLayers_CrossSection proves the bidirectional
// section-merge invariant at the Protocol layer: a prompt-layers edit
// preserves Skills + ToolExposure, and a skills / tool-exposure edit
// preserves the PromptLayers — no section edit silently drops a sibling.
func TestE2E_AgentConfig_PromptLayers_CrossSection(t *testing.T) {
	h := newACHarness(t)

	// Seed all three sections: prompt layers, a skill, a tool-exposure.
	_ = decode[prototypes.AgentConfigSetPromptLayersResponse](t, h.call(t, "/v1/agent_config/set_prompt_layers", prototypes.AgentConfigSetPromptLayersRequest{
		AgentID:      acAgent,
		PromptLayers: prototypes.AgentConfigPromptLayers{Base: plStrPtr("OPERATOR_BASE"), User: plStrPtr("USER_LAYER")},
	}, adminScopes()))
	_ = decode[prototypes.AgentConfigSkillsUpsertResponse](t, h.call(t, "/v1/agent_config/skills/upsert", prototypes.AgentConfigSkillsUpsertRequest{
		AgentID: acAgent,
		Skill:   prototypes.AgentConfigSkillInput{Name: "alpha", Trigger: "tr", Steps: []string{"s"}, Origin: "generated", Scope: "session"},
	}, adminScopes()))

	// A tool-exposure edit must preserve BOTH prompt layers AND skills.
	te := decode[prototypes.AgentConfigSetToolExposureResponse](t, h.call(t, "/v1/agent_config/set_tool_exposure", prototypes.AgentConfigSetToolExposureRequest{
		AgentID:      acAgent,
		ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"srvA"}},
	}, adminScopes()))
	if te.Revision.Payload.PromptLayers == nil || te.Revision.Payload.PromptLayers.Base == nil {
		t.Fatalf("prompt layers dropped across tool-exposure edit: %+v", te.Revision.Payload.PromptLayers)
	}
	if te.Revision.Payload.Skills == nil || len(te.Revision.Payload.Skills.Names) != 1 {
		t.Fatalf("skills dropped across tool-exposure edit: %+v", te.Revision.Payload.Skills)
	}

	// A prompt-layers edit must preserve skills + tool-exposure.
	pl := decode[prototypes.AgentConfigSetPromptLayersResponse](t, h.call(t, "/v1/agent_config/set_prompt_layers", prototypes.AgentConfigSetPromptLayersRequest{
		AgentID:      acAgent,
		PromptLayers: prototypes.AgentConfigPromptLayers{Base: plStrPtr("OPERATOR_BASE_V2")},
	}, adminScopes()))
	if pl.Revision.Payload.Skills == nil || len(pl.Revision.Payload.Skills.Names) != 1 {
		t.Fatalf("skills dropped across prompt-layers edit: %+v", pl.Revision.Payload.Skills)
	}
	if pl.Revision.Payload.ToolExposure == nil || len(pl.Revision.Payload.ToolExposure.PausedServers) != 1 {
		t.Fatalf("tool-exposure dropped across prompt-layers edit: %+v", pl.Revision.Payload.ToolExposure)
	}
}
