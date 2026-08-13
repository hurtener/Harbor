// Package packproposer adapts the configured LLM client to governed agent-pack
// drafting. It only drafts JSON; persistence and server provenance remain in
// the agent-config protocol service.
package packproposer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

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
func (p *Proposer) Draft(ctx context.Context, q identity.Quadruple, agentID, model, intent string, policy protocol.AgentPackAuthoringPolicy) (protocol.AgentPackDraft, error) {
	if policy.ProposerSchema != protocol.AgentPackAuthoringProposerSchema() {
		return protocol.AgentPackDraft{}, fmt.Errorf("packproposer: unsupported proposer schema")
	}
	ctx, err := identity.With(ctx, q.Identity)
	if err != nil {
		return protocol.AgentPackDraft{}, fmt.Errorf("packproposer: identity: %w", err)
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return protocol.AgentPackDraft{}, fmt.Errorf("packproposer: encode policy: %w", err)
	}
	system := protocol.AgentPackAuthoringSystemMessage(policyJSON)
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
	var item proposerItem
	decoder := json.NewDecoder(strings.NewReader(resp.Content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&item); err != nil {
		return protocol.AgentPackDraft{}, fmt.Errorf("packproposer: decode structured draft: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return protocol.AgentPackDraft{}, fmt.Errorf("packproposer: decode structured draft: trailing JSON")
		}
		return protocol.AgentPackDraft{}, fmt.Errorf("packproposer: decode structured draft: trailing JSON: %w", err)
	}
	if item.hasAuthorityFields() {
		return protocol.AgentPackDraft{}, fmt.Errorf("packproposer: decode structured draft: server-owned authority field")
	}
	return protocol.AgentPackDraft{Item: item.toAgentPackItem()}, nil
}

// proposerItem is deliberately closed. Authority fields are listed so their
// rejection remains explicit if this output is embedded in a broader shape.
type proposerItem struct {
	Name          string            `json:"name"`
	Title         string            `json:"title,omitempty"`
	Description   string            `json:"description,omitempty"`
	Trigger       string            `json:"trigger"`
	TaskType      string            `json:"task_type,omitempty"`
	Tags          []string          `json:"tags,omitempty"`
	Steps         []string          `json:"steps"`
	Preconditions []string          `json:"preconditions,omitempty"`
	FailureModes  []string          `json:"failure_modes,omitempty"`
	RequiredTools []string          `json:"required_tools,omitempty"`
	RequiredNS    []string          `json:"required_ns,omitempty"`
	RequiredTags  []string          `json:"required_tags,omitempty"`
	Extra         map[string]string `json:"extra,omitempty"`
	Origin        json.RawMessage   `json:"origin"`
	OriginRef     json.RawMessage   `json:"origin_ref"`
	ContentHash   json.RawMessage   `json:"content_hash"`
	Membership    json.RawMessage   `json:"membership"`
	Capabilities  json.RawMessage   `json:"capabilities"`
	Policy        json.RawMessage   `json:"policy"`
	PolicyHash    json.RawMessage   `json:"policy_hash"`
	Provenance    json.RawMessage   `json:"provenance"`
	Permissions   json.RawMessage   `json:"permissions"`
}

func (p proposerItem) hasAuthorityFields() bool {
	return p.Origin != nil || p.OriginRef != nil || p.ContentHash != nil || p.Membership != nil || p.Capabilities != nil || p.Policy != nil || p.PolicyHash != nil || p.Provenance != nil || p.Permissions != nil
}

func (p proposerItem) toAgentPackItem() skills.AgentPackItem {
	return skills.AgentPackItem{Name: p.Name, Title: p.Title, Description: p.Description, Trigger: p.Trigger, TaskType: p.TaskType, Tags: p.Tags, Steps: p.Steps, Preconditions: p.Preconditions, FailureModes: p.FailureModes, RequiredTools: p.RequiredTools, RequiredNS: p.RequiredNS, RequiredTags: p.RequiredTags, Extra: p.Extra}
}

var _ protocol.AgentPackProposer = (*Proposer)(nil)
