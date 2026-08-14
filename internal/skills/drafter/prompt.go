package drafter

import "strings"

// prompt.go — the draft-only lane's OWN intent prompt. It is
// deliberately separate from the structured `skill_propose` surface
// (which receives a caller-supplied draft and invokes no LLM) and from
// the operator-pack proposer's prompt: model text can never mint the
// structured lane's fields, because this prompt never names them as
// acceptable output.

// systemPrompt instructs the model to emit exactly the closed JSON
// object shape the decoder accepts, and to never emit authority,
// persistence, or publication fields. The shape mirrors the canonical
// semantic skill fields only.
func systemPrompt() string {
	return "You are Harbor's skill-draft authoring assistant. " +
		"Emit a SINGLE JSON object and nothing else: no prose around it, no markdown fences. " +
		"The object describes a bounded, resource-free skill draft. " +
		"Use EXACTLY these keys (all optional except name, trigger, steps): " +
		"name (lowercase, trimmed, short), title, description, trigger (a planner match cue), " +
		"task_type, tags, steps (a non-empty list of concrete single-line actions), " +
		"preconditions, failure_modes, required_tools, required_ns, required_tags. " +
		"Keep every field short and the whole draft small. " +
		"NEVER emit any other key — in particular never emit: origin, origin_ref, content_hash, " +
		"scope, tenant, tenant_id, user, user_id, session, session_id, agent, agent_id, model, " +
		"owner, audience, membership, capabilities, policy, policy_hash, permissions, provenance, " +
		"persist, replace, publish, publication, grant, grants, tool_visibility, oauth, approval, " +
		"authority, refusal, error. " +
		"Never reference support files, images, or local paths in the description or steps: " +
		"the draft is a single self-contained document."
}

// userPrompt assembles the bounded user-side instruction from the
// intent and the optional non-authorizing revision feedback.
func userPrompt(intent, feedback string) string {
	var b strings.Builder
	b.WriteString("Author intent: ")
	b.WriteString(intent)
	if strings.TrimSpace(feedback) != "" {
		b.WriteString("\n\nRevision feedback: ")
		b.WriteString(feedback)
	}
	return b.String()
}
