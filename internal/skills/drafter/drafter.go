// Package drafter owns the draft-only personal-skill authoring lane:
// a separate safety-wrapped LLM adapter (its own intent prompt and its
// own closed model-output decoder) plus the ordinary tool handler for
// `skill_create_draft`.
//
// The lane is deliberately narrow. It turns a bounded authoring intent
// plus optional non-authorizing revision feedback into ONE validated,
// resource-free `SKILL.md` DRAFT stored as a single immutable
// caller-scoped artifact. It has ZERO mutation authority beyond that
// single artifact write:
//
//   - no skill-store upsert, user-skill membership/revision write,
//     operator-pack proposal/publication, or capability registration;
//   - no scope / owner / tenant / user / session / agent / audience
//     selection from tool arguments or model output — identity comes
//     exclusively from the invocation's run context;
//   - no `persist`, `save`, `publish`, `replace`, or grant semantics,
//     even when a model emits them in free text or structured output;
//   - installing a draft is an explicit later consumer action through
//     the canonical complete-skill-package validate/commit workflow —
//     the artifact is stored with an explicit `installed: false`
//     state and nothing in this lane ever installs it.
//
// Reuse boundary. The adapter shares the canonical semantic core of
// the complete-skill-package lane — the DTO, validator, deterministic
// serializer, and versioned package hash — but it is NOT the
// structured `skill_propose` surface (which receives a caller-supplied
// draft and invokes no LLM) and NOT the operator-pack proposer. The
// intent prompt, the model-output decoder, and the closed output shape
// are this package's own, so structured-argument fields can never be
// minted by model text.
//
// Concurrency. The `Adapter` is a compiled artifact: it holds only the
// injected `llm.LLMClient` and immutable bounds, spawns no goroutines,
// and carries no per-invocation state. A single shared instance is
// safe for N concurrent invocations under `-race`; every per-run value
// (identity, intent, feedback) lives in the call's `ctx` and
// arguments.
package drafter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// ToolName is the ordinary tool name this lane registers under. The
// tool is DISABLED BY DEFAULT: nothing registers it until the
// composition owner explicitly wires the registration carrier into the
// built-in registry, so it is absent from the model-visible catalog
// unless an operator enables it per-agent through the ordinary tool
// policy surface.
const ToolName = "skill_create_draft"

// Bounds. Every bound is a named limit enforced before any artifact
// persistence, and every limit error is typed and carries no raw model
// output or secret-bearing prompt text.
const (
	// MaxIntentRunes bounds the caller-supplied authoring intent.
	MaxIntentRunes = 4000
	// MaxFeedbackRunes bounds the optional revision feedback.
	MaxFeedbackRunes = 2000
	// MaxModelOutputBytes bounds the raw model response before it is
	// decoded, so an oversized or pathological response cannot drive
	// unbounded allocation.
	MaxModelOutputBytes = 64 << 10 // 64 KiB
	// MaxSummaryRunes bounds the normalized description summary the
	// tool result carries back (the full body lives in the artifact,
	// never in the result).
	MaxSummaryRunes = 500
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrIntentRequired — the caller supplied an empty intent.
	ErrIntentRequired = errors.New("drafter: intent is required")
	// ErrIntentTooLarge — intent exceeds the rune bound.
	ErrIntentTooLarge = errors.New("drafter: intent exceeds the rune bound")
	// ErrFeedbackTooLarge — feedback exceeds the rune bound.
	ErrFeedbackTooLarge = errors.New("drafter: feedback exceeds the rune bound")
	// ErrModelOutputTooLarge — the model response exceeds the byte
	// bound.
	ErrModelOutputTooLarge = errors.New("drafter: model output exceeds the byte bound")
	// ErrMalformedModelOutput — the model response is not a valid
	// draft: not JSON, unknown fields, trailing content, or a
	// canonical-validation failure. The wrapped detail names fields,
	// never the raw model text.
	ErrMalformedModelOutput = errors.New("drafter: malformed model output")
	// ErrForbiddenAuthorityField — the model response (or caller
	// argument) carried a field this lane never accepts: identity,
	// scope, persistence, replacement, publication, capability, grant,
	// or provenance authority. The field is rejected, never
	// interpreted.
	ErrForbiddenAuthorityField = errors.New("drafter: forbidden authority field")
	// ErrModelRefused — the model explicitly refused to draft (a
	// refusal/error member in its structured output). Fails loud; no
	// artifact is created.
	ErrModelRefused = errors.New("drafter: model refused to draft")
	// ErrMissingIdentity — the invocation context carries no complete
	// (tenant, user, session) identity. Identity is mandatory.
	ErrMissingIdentity = errors.New("drafter: identity (tenant/user/session) is mandatory in the invocation context")
	// ErrWriterRequired — the handler was invoked without an injected
	// artifact writer.
	ErrWriterRequired = errors.New("drafter: artifact writer is required")
	// ErrUnrenderableSkill — the validated draft cannot be rendered as
	// the canonical resource-free SKILL.md document (embedded support
	// references, `## `-heading prose, or multi-line list items).
	ErrUnrenderableSkill = errors.New("drafter: draft cannot be rendered as a resource-free SKILL.md")
	// ErrDraftDocumentTooLarge — the rendered SKILL.md document exceeds
	// the canonical document bound, so persisting it would produce an
	// artifact the validate/commit workflow rejects.
	ErrDraftDocumentTooLarge = errors.New("drafter: rendered SKILL.md exceeds the document bound")
)

// Options tunes one adapter. The zero value is production-usable: the
// configured LLM client supplies the model default, and every bound
// takes its package constant.
type Options struct {
	// Model is the model name passed to the LLM client. Empty uses
	// the configured client default (the ordinary planner behaviour).
	Model string
	// MaxIntentRunes overrides MaxIntentRunes. Non-positive uses the
	// package constant.
	MaxIntentRunes int
	// MaxFeedbackRunes overrides MaxFeedbackRunes.
	MaxFeedbackRunes int
	// MaxModelOutputBytes overrides MaxModelOutputBytes.
	MaxModelOutputBytes int
}

// normalize fills zero-valued option fields with the package
// constants. Returns a copy; the receiver is untouched.
func (o Options) normalize() Options {
	if o.MaxIntentRunes <= 0 {
		o.MaxIntentRunes = MaxIntentRunes
	}
	if o.MaxFeedbackRunes <= 0 {
		o.MaxFeedbackRunes = MaxFeedbackRunes
	}
	if o.MaxModelOutputBytes <= 0 {
		o.MaxModelOutputBytes = MaxModelOutputBytes
	}
	return o
}

// Adapter is the safety-wrapped LLM authoring adapter: it owns the
// intent prompt and the closed model-output decoder for the
// draft-only lane and produces a validated canonical
// `skillpkg.PackageSkill`. The LLM client is injected by assembly —
// this package never chooses a provider or a credential.
//
// The wrapped client is the one the runtime assembles (safety net,
// corrections, retry, governance already composed), so this adapter
// rides the same guarantees as every other governed LLM call.
type Adapter struct {
	client llm.LLMClient
	opts   Options
}

// New constructs an Adapter over the injected client. A nil client is
// a wiring bug and fails loud.
func New(client llm.LLMClient, opts Options) (*Adapter, error) {
	if client == nil {
		return nil, fmt.Errorf("drafter: llm client is required")
	}
	return &Adapter{client: client, opts: opts.normalize()}, nil
}

// Draft turns a bounded authoring intent plus optional revision
// feedback into a validated canonical `skillpkg.PackageSkill`.
//
// Refusal, malformed output, timeout, and cancellation all fail loud
// and create NO artifact: nothing is written inside this method, and
// the returned DTO is only ever persisted by the handler under the
// caller's verified scope. Identity is read from ctx (mandatory); the
// adapter never accepts an identity from arguments.
func (a *Adapter) Draft(ctx context.Context, intent, feedback string) (skillpkg.PackageSkill, error) {
	if !llm.HasIdentity(ctx) {
		return skillpkg.PackageSkill{}, ErrMissingIdentity
	}
	intent = strings.TrimSpace(intent)
	if intent == "" {
		return skillpkg.PackageSkill{}, ErrIntentRequired
	}
	if rl := len([]rune(intent)); rl > a.opts.MaxIntentRunes {
		return skillpkg.PackageSkill{}, fmt.Errorf("%w: intent is %d runes (limit %d)", ErrIntentTooLarge, rl, a.opts.MaxIntentRunes)
	}
	feedback = strings.TrimSpace(feedback)
	if rl := len([]rune(feedback)); rl > a.opts.MaxFeedbackRunes {
		return skillpkg.PackageSkill{}, fmt.Errorf("%w: feedback is %d runes (limit %d)", ErrFeedbackTooLarge, rl, a.opts.MaxFeedbackRunes)
	}

	system := systemPrompt()
	user := userPrompt(intent, feedback)
	resp, err := a.client.Complete(ctx, llm.CompleteRequest{
		Model: a.opts.Model,
		Messages: []llm.ChatMessage{
			{Role: llm.RoleSystem, Content: llm.Content{Text: &system}},
			{Role: llm.RoleUser, Content: llm.Content{Text: &user}},
		},
		ResponseFormat: &llm.ResponseFormat{Kind: llm.FormatJSONObject},
	})
	if err != nil {
		// The wrapped client already honours ctx cancellation and
		// timeouts; surface the cause, never the request text.
		return skillpkg.PackageSkill{}, fmt.Errorf("drafter: complete: %w", err)
	}
	if len(resp.Content) > a.opts.MaxModelOutputBytes {
		return skillpkg.PackageSkill{}, fmt.Errorf("%w: model response is %d bytes (limit %d)", ErrModelOutputTooLarge, len(resp.Content), a.opts.MaxModelOutputBytes)
	}
	return decodeDraftModelOutput(resp.Content)
}

// Client exposes the injected client for tests and for composition
// owners that need to build sibling adapters over the same client.
func (a *Adapter) Client() llm.LLMClient { return a.client }

// identityQuad reads the caller's quadruple (or identity) from ctx for
// handlers that need the artifact scope.
func identityQuad(ctx context.Context) (identity.Quadruple, bool) {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		return q, true
	}
	id, ok := identity.From(ctx)
	if !ok {
		return identity.Quadruple{}, false
	}
	return identity.Quadruple{Identity: id}, true
}
