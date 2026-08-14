package drafter

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// decoder.go — the draft-only lane's CLOSED model-output decoder.
//
// The decoder accepts exactly the canonical semantic skill field set.
// Everything else is rejected by one of two gates:
//
//  1. Unknown fields fail the strict JSON decode (DisallowUnknownFields)
//     — the response cannot smuggle an arbitrary key past the wire.
//  2. Authority / persistence / publication / capability fields are
//     DECLARED on the closed shape so their presence gets a typed
//     rejection naming the field, and the field is never interpreted.
//
// A response carrying a refusal/error member is a refusal
// (ErrModelRefused). Malformed output, trailing content, or a
// canonical-validation failure is ErrMalformedModelOutput. Neither
// path creates an artifact.

// modelDraft is the closed structured-output shape the model may emit.
// Only the canonical semantic skill fields are accepted; every other
// member is rejected (unknown keys via strict decode, authority keys
// via the explicit declarations below).
type modelDraft struct {
	Name          string   `json:"name"`
	Title         string   `json:"title,omitempty"`
	Description   string   `json:"description,omitempty"`
	Trigger       string   `json:"trigger"`
	TaskType      string   `json:"task_type,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Steps         []string `json:"steps"`
	Preconditions []string `json:"preconditions,omitempty"`
	FailureModes  []string `json:"failure_modes,omitempty"`
	RequiredTools []string `json:"required_tools,omitempty"`
	RequiredNS    []string `json:"required_ns,omitempty"`
	RequiredTags  []string `json:"required_tags,omitempty"`

	// Refusal/error members: an explicit model-side refusal.
	Refusal string `json:"refusal,omitempty"`
	Error   string `json:"error,omitempty"`

	// Authority / persistence / publication / capability members.
	// Declared so their presence is rejected with a typed error naming
	// the field — never silently accepted, never interpreted. A
	// `null` value still lands in the RawMessage (non-nil), so the
	// rejection fires for both `"field": null` and `"field": {...}`.
	Origin         json.RawMessage `json:"origin,omitempty"`
	OriginRef      json.RawMessage `json:"origin_ref,omitempty"`
	ContentHash    json.RawMessage `json:"content_hash,omitempty"`
	Scope          json.RawMessage `json:"scope,omitempty"`
	Tenant         json.RawMessage `json:"tenant,omitempty"`
	TenantID       json.RawMessage `json:"tenant_id,omitempty"`
	User           json.RawMessage `json:"user,omitempty"`
	UserID         json.RawMessage `json:"user_id,omitempty"`
	Session        json.RawMessage `json:"session,omitempty"`
	SessionID      json.RawMessage `json:"session_id,omitempty"`
	Agent          json.RawMessage `json:"agent,omitempty"`
	AgentID        json.RawMessage `json:"agent_id,omitempty"`
	Model          json.RawMessage `json:"model,omitempty"`
	Owner          json.RawMessage `json:"owner,omitempty"`
	Audience       json.RawMessage `json:"audience,omitempty"`
	Membership     json.RawMessage `json:"membership,omitempty"`
	Capabilities   json.RawMessage `json:"capabilities,omitempty"`
	Policy         json.RawMessage `json:"policy,omitempty"`
	PolicyHash     json.RawMessage `json:"policy_hash,omitempty"`
	Permissions    json.RawMessage `json:"permissions,omitempty"`
	Provenance     json.RawMessage `json:"provenance,omitempty"`
	Persist        json.RawMessage `json:"persist,omitempty"`
	Replace        json.RawMessage `json:"replace,omitempty"`
	Publish        json.RawMessage `json:"publish,omitempty"`
	Publication    json.RawMessage `json:"publication,omitempty"`
	Grant          json.RawMessage `json:"grant,omitempty"`
	Grants         json.RawMessage `json:"grants,omitempty"`
	ToolVisibility json.RawMessage `json:"tool_visibility,omitempty"`
	OAuth          json.RawMessage `json:"oauth,omitempty"`
	Approval       json.RawMessage `json:"approval,omitempty"`
	Authority      json.RawMessage `json:"authority,omitempty"`
}

// firstAuthorityField returns the name of the first declared
// authority/persistence/publication member the model emitted, or ""
// when none is present.
func (d *modelDraft) firstAuthorityField() string {
	for _, f := range []struct {
		name string
		v    json.RawMessage
	}{
		{"origin", d.Origin},
		{"origin_ref", d.OriginRef},
		{"content_hash", d.ContentHash},
		{"scope", d.Scope},
		{"tenant", d.Tenant},
		{"tenant_id", d.TenantID},
		{"user", d.User},
		{"user_id", d.UserID},
		{"session", d.Session},
		{"session_id", d.SessionID},
		{"agent", d.Agent},
		{"agent_id", d.AgentID},
		{"model", d.Model},
		{"owner", d.Owner},
		{"audience", d.Audience},
		{"membership", d.Membership},
		{"capabilities", d.Capabilities},
		{"policy", d.Policy},
		{"policy_hash", d.PolicyHash},
		{"permissions", d.Permissions},
		{"provenance", d.Provenance},
		{"persist", d.Persist},
		{"replace", d.Replace},
		{"publish", d.Publish},
		{"publication", d.Publication},
		{"grant", d.Grant},
		{"grants", d.Grants},
		{"tool_visibility", d.ToolVisibility},
		{"oauth", d.OAuth},
		{"approval", d.Approval},
		{"authority", d.Authority},
	} {
		if f.v != nil {
			return f.name
		}
	}
	return ""
}

// toPackageSkill projects the closed shape onto the canonical semantic
// skill DTO. The decoder is the ONLY producer of PackageSkill values
// in this lane.
func (d *modelDraft) toPackageSkill() skillpkg.PackageSkill {
	return skillpkg.PackageSkill{
		Name:          d.Name,
		Title:         d.Title,
		Description:   d.Description,
		Trigger:       d.Trigger,
		TaskType:      d.TaskType,
		Tags:          d.Tags,
		Steps:         d.Steps,
		Preconditions: d.Preconditions,
		FailureModes:  d.FailureModes,
		RequiredTools: d.RequiredTools,
		RequiredNS:    d.RequiredNS,
		RequiredTags:  d.RequiredTags,
	}
}

// decodeDraftModelOutput decodes the model's response into a
// validated canonical PackageSkill. The response is bounded by the
// caller (the adapter checks MaxModelOutputBytes before calling).
//
// Rejections carry field names, never raw model text:
//   - a refusal/error member → ErrModelRefused;
//   - an authority member → ErrForbiddenAuthorityField (naming it);
//   - unknown fields, non-JSON, trailing content → ErrMalformedModelOutput;
//   - a canonical-validation failure → ErrMalformedModelOutput wrapping
//     the validator detail (which names fields and bounds, not values).
func decodeDraftModelOutput(content string) (skillpkg.PackageSkill, error) {
	if strings.TrimSpace(content) == "" {
		return skillpkg.PackageSkill{}, fmt.Errorf("%w: empty model response", ErrMalformedModelOutput)
	}
	var d modelDraft
	dec := json.NewDecoder(strings.NewReader(content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return skillpkg.PackageSkill{}, fmt.Errorf("%w: decode structured draft: %w", ErrMalformedModelOutput, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return skillpkg.PackageSkill{}, fmt.Errorf("%w: trailing JSON after the draft", ErrMalformedModelOutput)
		}
		return skillpkg.PackageSkill{}, fmt.Errorf("%w: trailing content: %w", ErrMalformedModelOutput, err)
	}
	if d.Refusal != "" || d.Error != "" {
		return skillpkg.PackageSkill{}, ErrModelRefused
	}
	if field := d.firstAuthorityField(); field != "" {
		return skillpkg.PackageSkill{}, fmt.Errorf("%w: %q", ErrForbiddenAuthorityField, field)
	}
	skill := d.toPackageSkill()
	// Bounded repair: the canonical envelope's name identity IS the
	// canonical (lowercase, trimmed) form — the package hash and the
	// validate/commit ingest both treat it that way — so the decoded
	// name is canonicalized here and the renderer emits it verbatim,
	// making the artifact round-trip byte-exact.
	skill.Name = skillpkg.CanonicalName(skill.Name)
	if err := skill.Validate(); err != nil {
		return skillpkg.PackageSkill{}, fmt.Errorf("%w: %w", ErrMalformedModelOutput, err)
	}
	return skill, nil
}
