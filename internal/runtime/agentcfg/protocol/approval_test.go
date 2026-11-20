package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

func TestApprovalRevision_WireRoundTripOmissionAndRemoval(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t), agentcfgprotocol.WithDynamicApprovalRules(true))
	if err != nil {
		t.Fatal(err)
	}
	var payload prototypes.AgentConfigPayload
	raw := []byte(`{"approval":{"rules":[
		{"tools":["crm_delete_*","crm_update_*"],"sources":["crm"],"annotations":["destructive","not_read_only"],"approval":{"policy":"tagged","reason":"Review CRM writes","require_tags":["write"]}},
		{"tools":["crm_*"],"approval":{"policy":"deny-all"}}
	]}}`)
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	first, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{Identity: scope(), AgentID: testAgentID, Payload: payload})
	if err != nil || !reflect.DeepEqual(payload.Approval, first.Revision.Payload.Approval) {
		t.Fatalf("set revision lost approval rules: %+v, %v", first.Revision.Payload.Approval, err)
	}
	first.Revision.Payload.Approval.Rules[0].Approval.RequireTags[0] = "modified-response"
	read := prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID}
	got, err := s.Get(ctx, read)
	if err != nil || got.Revision == nil || !reflect.DeepEqual(payload.Approval, got.Revision.Payload.Approval) {
		t.Fatalf("get revision lost approval rules: %+v, %v", got.Revision, err)
	}
	// A pre-approval client can still write a whole revision without a hash or
	// an approval section. The other edits land with the current policy intact.
	currentHash := first.Revision.ContentHash
	for _, raw := range []string{
		`{"prompt_layers":{"base":"legacy client edit"}}`,
		`{"prompt_layers":{"base":"empty object edit"},"approval":{}}`,
		`{"prompt_layers":{"base":"null rules edit"},"approval":{"rules":null}}`,
	} {
		var edit prototypes.AgentConfigPayload
		if err := json.Unmarshal([]byte(raw), &edit); err != nil {
			t.Fatal(err)
		}
		saved, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
			Identity: scope(), AgentID: testAgentID, Payload: edit,
		})
		if err != nil || saved.Revision.ContentHash == currentHash ||
			!reflect.DeepEqual(payload.Approval, saved.Revision.Payload.Approval) {
			t.Fatalf("omitting approval in %s lost rules or the prompt edit: %+v, %v", raw, saved.Revision, err)
		}
		currentHash = saved.Revision.ContentHash
	}
	var clear prototypes.AgentConfigPayload
	if err := json.Unmarshal([]byte(`{"approval":{"rules":[]}}`), &clear); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID, ExpectedContentHash: currentHash,
		Payload: clear,
	})
	if err != nil || cleared.Revision.Payload.Approval == nil || len(cleared.Revision.Payload.Approval.Rules) != 0 {
		t.Fatalf("empty approval section did not clear rules: %+v, %v", cleared.Revision.Payload.Approval, err)
	}
	_, err = s.Rollback(ctx, prototypes.AgentConfigRollbackRequest{Identity: scope(), AgentID: testAgentID, RevisionID: first.Revision.RevisionID})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, read)
	if err != nil || got.Revision == nil || !reflect.DeepEqual(payload.Approval, got.Revision.Payload.Approval) {
		t.Fatalf("rollback did not restore approval rules: %+v, %v", got.Revision, err)
	}
}

func TestApprovalRevision_RequiresEnforcementForNonemptyRules(t *testing.T) {
	ctx := t.Context()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	req := prototypes.AgentConfigSetRevisionRequest{Identity: scope(), AgentID: testAgentID}
	seed, err := s.SetRevision(ctx, req)
	if err != nil || seed.Revision.Payload.Approval != nil {
		t.Fatalf("omitted approval: %+v, %v", seed.Revision.Payload.Approval, err)
	}
	req.Payload.Approval = &prototypes.AgentConfigApproval{Rules: []prototypes.AgentConfigApprovalRule{{
		Tools: []string{"crm_*"}, Approval: prototypes.AgentConfigApprovalPolicy{Policy: "deny-all"},
	}}}
	if _, err := s.SetRevision(ctx, req); !errors.Is(err, agentcfgprotocol.ErrDynamicApprovalRulesUnavailable) {
		t.Fatalf("error = %v, want ErrDynamicApprovalRulesUnavailable", err)
	}
	history, err := s.ListRevisions(ctx, prototypes.AgentConfigListRevisionsRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || len(history.Revisions) != 1 || history.Revisions[0].RevisionID != seed.Revision.RevisionID {
		t.Fatalf("unsupported write changed revision history: %+v, %v", history, err)
	}
	req.Payload.Approval = &prototypes.AgentConfigApproval{Rules: []prototypes.AgentConfigApprovalRule{}}
	if _, err := s.SetRevision(ctx, req); err != nil {
		t.Fatalf("explicit clear without enforcement failed: %v", err)
	}
}

func TestApprovalRevision_ValidatesAtTheWireAndRegistryBoundaries(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	s, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithDynamicApprovalRules(true))
	if err != nil {
		t.Fatal(err)
	}
	policy := prototypes.AgentConfigApprovalPolicy{Policy: "deny-all"}
	valid := prototypes.AgentConfigApprovalRule{Tools: []string{"crm_*"}, Approval: policy}
	for _, tc := range []struct {
		name, field string
		rules       []prototypes.AgentConfigApprovalRule
	}{
		{"too many rules", "at most 64", slices.Repeat([]prototypes.AgentConfigApprovalRule{valid}, 65)},
		{"too many tool globs", ".tools allows at most 32", []prototypes.AgentConfigApprovalRule{{Tools: slices.Repeat(valid.Tools, 33), Approval: policy}}},
		{"too many source globs", ".sources allows at most 32", []prototypes.AgentConfigApprovalRule{{Sources: slices.Repeat(valid.Tools, 33), Approval: policy}}},
		{"no selector", "must be set", []prototypes.AgentConfigApprovalRule{{Approval: policy}}},
		{"bad tool glob", ".tools[0]", []prototypes.AgentConfigApprovalRule{{Tools: []string{"["}, Approval: policy}}},
		{"bad source glob", ".sources[0]", []prototypes.AgentConfigApprovalRule{{Sources: []string{" "}, Approval: policy}}},
		{"bad annotation", ".annotations[0]", []prototypes.AgentConfigApprovalRule{{Annotations: []string{"unsupported"}, Approval: policy}}},
		{"bad policy", ".approval.policy", []prototypes.AgentConfigApprovalRule{{Tools: valid.Tools, Approval: prototypes.AgentConfigApprovalPolicy{Policy: "unsupported"}}}},
		{"no tags", ".approval.require_tags", []prototypes.AgentConfigApprovalRule{{Tools: valid.Tools, Approval: prototypes.AgentConfigApprovalPolicy{Policy: "tagged"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
				Identity: scope(), AgentID: testAgentID,
				Payload: prototypes.AgentConfigPayload{Approval: &prototypes.AgentConfigApproval{Rules: tc.rules}},
			})
			if !errors.Is(err, agentcfg.ErrInvalidPayload) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error = %v, want invalid payload naming %q", err, tc.field)
			}
		})
	}
	wireID := scope()
	id := identity.Quadruple{Identity: identity.Identity{TenantID: wireID.Tenant, UserID: wireID.User, SessionID: wireID.Session}}
	payload := agentcfg.ConfigPayload{Approval: &agentcfg.ApprovalSection{Rules: []agentcfg.ApprovalRule{
		{Tools: []string{"bad["}, Approval: agentcfg.ApprovalPolicy{Policy: "deny-all"}},
	}}}
	if _, err := reg.SetRevision(ctx, id, testAgentID, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrInvalidPayload) {
		t.Fatalf("direct invalid write error = %v", err)
	}
	payload.Approval.Rules[0].Tools = []string{"crm_*"}
	if _, err := reg.SetRevision(ctx, id, testAgentID, agentcfg.ConfigScopeUser, payload, agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrInvalidPayload) {
		t.Fatalf("direct user-scoped write error = %v", err)
	}
	if got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID}); err != nil || got.Set {
		t.Fatalf("invalid writes published an active revision: %+v, %v", got, err)
	}
	valid.Tools = slices.Repeat(valid.Tools, 32)
	valid.Sources = slices.Repeat([]string{"crm"}, 32)
	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{Approval: &prototypes.AgentConfigApproval{Rules: slices.Repeat([]prototypes.AgentConfigApprovalRule{valid}, 64)}},
	}); err != nil {
		t.Fatalf("approval rule and glob limits must be inclusive: %v", err)
	}
}
