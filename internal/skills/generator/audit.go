package generator

import (
	"context"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// auditExcerptCap bounds the caller-controlled string excerpts on the
// `SkillProposedPayload`. Caps the audit-bus byte footprint and makes
// the redactor's reflective walk's pass cheap (each excerpt is at most
// this long after truncation).
const auditExcerptCap = 256

// redactExcerpt runs `s` through the audit redactor and returns a
// truncated version capped at `auditExcerptCap`. The redactor walks
// caller bytes for secret-shaped substrings; the truncation bounds
// the post-redaction payload size irrespective of input length.
//
// Returns a wrapped error when the redactor errors; the caller MUST
// treat this as "do not emit" — fail-loudly per audit's contract.
func redactExcerpt(ctx context.Context, r audit.Redactor, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	// The redactor's reflective walk operates over maps / structs; a
	// bare string passes through unchanged unless wrapped in a
	// map[string]any. Mirror the pattern Phase 20's tasks driver
	// uses for caller-controlled strings.
	out, err := r.Redact(ctx, map[string]any{"v": s})
	if err != nil {
		return "", fmt.Errorf("skills/generator: redact excerpt: %w", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		return "", fmt.Errorf("skills/generator: redactor returned %T, want map[string]any", out)
	}
	v, ok := m["v"].(string)
	if !ok {
		// A buggy redactor might replace the string with a marker
		// of a different type. Coerce defensively rather than emit
		// caller bytes.
		v = fmt.Sprintf("%v", m["v"])
	}
	if len(v) > auditExcerptCap {
		v = v[:auditExcerptCap] + "…"
	}
	return v, nil
}

// emitProposed builds a `SkillProposedPayload` from `skill` +
// `result` + `reason`, runs the caller-controlled excerpts through
// the redactor, and publishes the event on `bus`. Returns a wrapped
// error on redactor failure OR publish failure — fail-loudly per
// CLAUDE.md §13. The caller is responsible for rolling back any DB
// write performed BEFORE this emit call.
func emitProposed(ctx context.Context, deps Deps, q identity.Quadruple, draft SkillDraft, s skills.Skill, result ProposeResult, reason string, promotion bool) error {
	if deps.Redactor == nil {
		return fmt.Errorf("skills/generator: emitProposed called without Redactor")
	}
	if deps.Bus == nil {
		return fmt.Errorf("skills/generator: emitProposed called without Bus")
	}

	title, err := redactExcerpt(ctx, deps.Redactor, draft.Title)
	if err != nil {
		return err
	}
	trigger, err := redactExcerpt(ctx, deps.Redactor, draft.Trigger)
	if err != nil {
		return err
	}

	payload := SkillProposedPayload{
		Name:                   s.Name,
		Origin:                 s.Origin,
		Scope:                  s.Scope,
		OriginRef:              s.OriginRef,
		ContentHash:            s.ContentHash,
		Result:                 string(result),
		Reason:                 reason,
		RedactedTitleExcerpt:   title,
		RedactedTriggerExcerpt: trigger,
		Promotion:              promotion,
	}

	ev := events.Event{
		Type:       skills.EventTypeSkillProposed,
		Identity:   q,
		OccurredAt: time.Now(),
		Payload:    payload,
	}
	if err := deps.Bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("skills/generator: emit %s: %w", skills.EventTypeSkillProposed, err)
	}
	return nil
}
