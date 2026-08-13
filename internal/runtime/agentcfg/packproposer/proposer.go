// Package packproposer adapts the configured LLM client to governed agent-pack
// drafting. It only drafts JSON; persistence and server provenance remain in
// the agent-config protocol service.
package packproposer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
)

// Proposer is the production governed pack proposer. The LLM is injected by
// assembly so the protocol layer never chooses a provider or a credential.
type Proposer struct {
	client llm.LLMClient
}

// New constructs a proposer over a configured, safety-wrapped LLM client.
func New(client llm.LLMClient) (*Proposer, error) {
	if client == nil {
		return nil, fmt.Errorf("packproposer: llm client is required")
	}
	return &Proposer{client: client}, nil
}

// Draft requests one structured pack item. The service validates the returned
// item again and stamps all provenance after this method returns.
func (p *Proposer) Draft(ctx context.Context, q identity.Quadruple, agentID, model, intent string) (protocol.AgentPackDraft, error) {
	ctx, err := identity.With(ctx, q.Identity)
	if err != nil {
		return protocol.AgentPackDraft{}, fmt.Errorf("packproposer: identity: %w", err)
	}
	system := "Return exactly one JSON object for an agent skill pack item. Required fields: name, trigger, steps. Never include origin, origin_ref, content_hash, membership, or capabilities. RequiredTools, RequiredNS, and RequiredTags are metadata only and must not grant access."
	user := "Draft a bounded, executable skill pack item from this operator intent:\n" + intent
	resp, err := p.client.Complete(ctx, llm.CompleteRequest{
		Model: model,
		Messages: []llm.ChatMessage{
			{Role: llm.RoleSystem, Content: llm.Content{Text: &system}},
			{Role: llm.RoleUser, Content: llm.Content{Text: &user}},
		},
		ResponseFormat: &llm.ResponseFormat{Kind: llm.FormatJSONObject},
	})
	if err != nil {
		return protocol.AgentPackDraft{}, fmt.Errorf("packproposer: complete: %w", err)
	}
	var item skills.AgentPackItem
	if err := json.Unmarshal([]byte(resp.Content), &item); err != nil {
		return protocol.AgentPackDraft{}, fmt.Errorf("packproposer: decode structured draft: %w", err)
	}
	return protocol.AgentPackDraft{Item: item}, nil
}

var _ protocol.AgentPackProposer = (*Proposer)(nil)
