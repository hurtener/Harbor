package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tools"
)

type agentPackTestProposer struct {
	item skills.AgentPackItem
}

func (p agentPackTestProposer) Draft(context.Context, identity.Quadruple, string, string, string, agentcfgprotocol.AgentPackAuthoringPolicy) (agentcfgprotocol.AgentPackDraft, error) {
	return agentcfgprotocol.AgentPackDraft{Item: p.item}, nil
}

// preparedButUnpublishedRegistry models the crash window after the target
// revision is durable and before its active pointer publication. The embedded
// driver first lands the candidate; restoring the prior pointer leaves the
// exact target record available for the retry to resume.
type preparedButUnpublishedRegistry struct {
	agentcfg.Registry
	once bool
}

func (r *preparedButUnpublishedRegistry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	if !r.once {
		r.once = true
		previous, hadPrevious, err := r.Active(ctx, id, agentID, scope)
		if err != nil || !hadPrevious {
			return agentcfg.Revision{}, err
		}
		rev, err := r.Registry.SetRevision(ctx, id, agentID, scope, payload, opts)
		if err != nil {
			return rev, err
		}
		if _, err := r.Rollback(ctx, id, agentID, previous.RevisionID, scope, agentcfg.SetOptions{}); err != nil {
			return agentcfg.Revision{}, err
		}
		return agentcfg.Revision{}, errors.New("injected crash before active publication")
	}
	return r.Registry.SetRevision(ctx, id, agentID, scope, payload, opts)
}

func TestAgentPacksCommit_ResumesPreparedTargetWithoutSecondRevision(t *testing.T) {
	ctx := context.Background()
	reg, proposals := newRegistryWithState(t)
	base, err := reg.SetRevision(ctx, identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed base revision: %v", err)
	}
	item := skills.AgentPackItem{Name: "playbook", Trigger: "trigger", Steps: []string{"step"}}
	proposer := agentPackTestProposer{item: item}
	catalog := tools.NewCatalog()
	if err := catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "playbook_tool"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }}); err != nil {
		t.Fatalf("register catalog tool: %v", err)
	}
	first, err := agentcfgprotocol.NewService(&preparedButUnpublishedRegistry{Registry: reg}, agentcfgprotocol.WithAgentPackProposer(proposer), agentcfgprotocol.WithAgentPackProposalState(proposals), agentcfgprotocol.WithAgentPackCatalog(catalog))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	proposal, err := first.AgentPacksPropose(ctx, prototypes.AgentConfigAgentPacksProposeRequest{
		Identity: scope(), AgentID: testAgentID, Intent: "make a playbook", ExpectedContentHash: base.ContentHash,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	commit := prototypes.AgentConfigAgentPacksCommitRequest{
		Identity: scope(), AgentID: testAgentID, Skill: proposal.Skill, ReviewedHash: proposal.Hash,
		Provenance: proposal.Provenance, ProposalID: proposal.ProposalID, ExpectedContentHash: proposal.ExpectedContentHash,
	}
	if _, err := first.AgentPacksCommit(ctx, commit); err == nil {
		t.Fatal("first commit unexpectedly succeeded across injected crash window")
	}
	receipt, err := proposals.Load(ctx, identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, "agentcfg.agent_pack.proposal."+proposal.ProposalID)
	if err != nil {
		t.Fatalf("load committing receipt: %v", err)
	}
	var receiptFields map[string]string
	if err := json.Unmarshal(receipt.Bytes, &receiptFields); err != nil {
		t.Fatalf("decode committing receipt: %v", err)
	}
	if err := catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "newly-visible-tool"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }}); err != nil {
		t.Fatalf("register changed capability tool: %v", err)
	}
	if receipt.ID == "" || receiptFields["phase"] != "committing" || receiptFields["target_revision_id"] == "" || receiptFields["target_content_hash"] == "" {
		t.Fatalf("receipt did not durably capture exact target: %v", receiptFields)
	}

	second, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithAgentPackProposalState(proposals), agentcfgprotocol.WithAgentPackCatalog(catalog))
	if err != nil {
		t.Fatalf("NewService retry: %v", err)
	}
	resumed, err := second.AgentPacksCommit(ctx, commit)
	if err != nil {
		t.Fatalf("retry commit: %v", err)
	}
	if resumed.Revision.RevisionID != receiptFields["target_revision_id"] {
		t.Fatalf("retry revision = %q, want prepared target %q", resumed.Revision.RevisionID, receiptFields["target_revision_id"])
	}
	active, activeSet, err := reg.Active(ctx, identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("load resumed active revision: %v", err)
	}
	if !activeSet || active.RevisionID != receiptFields["target_revision_id"] || active.ContentHash != receiptFields["target_content_hash"] {
		t.Fatalf("active revision = %+v (set=%t), want exact committing target", active, activeSet)
	}
	revisions, err := reg.ListRevisions(ctx, identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, testAgentID, agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revisions) != 2 {
		t.Fatalf("revision count = %d, want base plus one prepared target", len(revisions))
	}
}
