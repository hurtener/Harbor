// Attachment disposition policy (Phase 84b — D-189).
//
// Disposition is a *choice* that belongs to the developer / operator /
// planner, not a fact the runtime hardcodes. Before 84b the per-MIME
// `switch` in `materializeOne` was the sole authority over how an
// uploaded attachment reached the model. This file turns that
// mechanism into declared policy:
//
//   - [AttachmentDisposition] is the enum (`ref` / `inline` /
//     `provider_native` / `tool:<name>`).
//   - [DispositionPolicy] is the per-agent per-MIME map + default.
//   - [ResolveDisposition] is the pure precedence resolver:
//     per-attachment caller hint > per-agent policy map > runtime
//     default. The layers are semantic; the carriers are adapters —
//     the Protocol input-artifact `disposition` field is one carrier
//     of the hint (direct `InputArtifactView.Disposition` construction
//     by a headless library consumer is the other), and `harbor.yaml`
//     is one carrier of the policy map (programmatic
//     [DispositionPolicy] construction is the other).
//   - [EffectiveDisposition] applies runtime capability constraints
//     to a resolved disposition and *returns* any degradation as a
//     typed fact — the resolver and materializer are pure; the CALLER
//     (run loop, devstack, or a consumer's own loop) logs and emits.
//
// The runtime default reproduces the pre-84b dispatch byte-for-byte:
// `image/*` resolves `inline` (the sub-threshold DataURL fast path),
// everything else resolves `ref` (the `ArtifactStub` + `Fetch.Tool`
// hint the planner drives via native tool-calling — 107c / D-167).
package planner

import (
	"errors"
	"fmt"
	"strings"
)

// AttachmentDisposition declares how an uploaded input artifact is
// handed to the model (Phase 84b — D-189). The zero value ("") means
// "no disposition declared" — the materializer falls back to the
// pre-84b per-MIME dispatch, so a headless consumer that never sets
// the field gets today's behaviour unchanged.
type AttachmentDisposition string

const (
	// DispositionRef emits an `ArtifactStub` by reference with the
	// existing `Fetch.Tool` hint — the developer-controllable
	// "process it myself via a tool" path. The runtime default for
	// every non-image MIME.
	DispositionRef AttachmentDisposition = "ref"
	// DispositionInline inlines the artifact bytes as a DataURL part.
	// V1.1 supports inline for `image/*` only (the sub-threshold
	// vision fast path); other MIMEs degrade to ref with a returned
	// [DispositionDegradation]. The runtime default for `image/*`.
	DispositionInline AttachmentDisposition = "inline"
	// DispositionProviderNative hands the artifact to the provider's
	// own understanding: the materializer emits a typed part with the
	// `ProviderNative` flag set, and the LLM driver uploads the
	// content to the provider's file surface inside `Complete`,
	// rewriting the part to an opaque `file_id` reference (RFC §6.5).
	// Opt-in only — never the runtime default. A provider without
	// upload support for the modality degrades loudly to the
	// `ArtifactStub` reference at the driver.
	DispositionProviderNative AttachmentDisposition = "provider_native"
)

// dispositionToolPrefix prefixes the parametrised `tool:<name>`
// disposition value.
const dispositionToolPrefix = "tool:"

// ErrInvalidDisposition is returned by [ParseDisposition] for a value
// outside the disposition grammar (`ref` / `inline` /
// `provider_native` / `tool:<name>`).
var ErrInvalidDisposition = errors.New("planner: invalid attachment disposition")

// DispositionTool builds the parametrised `tool:<name>` disposition
// that forces the named catalog tool via the `ArtifactStub.Fetch.Tool`
// mechanism (e.g. `DispositionTool("pdf.extract")`).
func DispositionTool(name string) AttachmentDisposition {
	return AttachmentDisposition(dispositionToolPrefix + name)
}

// IsZero reports whether no disposition was declared. A zero
// disposition selects the pre-84b per-MIME materializer dispatch.
func (d AttachmentDisposition) IsZero() bool { return d == "" }

// ToolName returns the forced tool name and true when d is a
// `tool:<name>` disposition with a non-empty name.
func (d AttachmentDisposition) ToolName() (string, bool) {
	s := string(d)
	if !strings.HasPrefix(s, dispositionToolPrefix) {
		return "", false
	}
	name := s[len(dispositionToolPrefix):]
	if name == "" {
		return "", false
	}
	return name, true
}

// Valid reports whether d is a canonical disposition value: one of
// the three fixed values or a `tool:<name>` with a non-empty name.
// The zero value is NOT valid — callers that accept "unset" check
// [AttachmentDisposition.IsZero] first.
func (d AttachmentDisposition) Valid() bool {
	switch d {
	case DispositionRef, DispositionInline, DispositionProviderNative:
		return true
	}
	_, ok := d.ToolName()
	return ok
}

// ParseDisposition validates a wire / config carrier string against
// the disposition grammar and returns the typed value. The carriers
// (Protocol `disposition` hint, `harbor.yaml` map values) call this at
// their edge so the runtime only ever sees canonical values; a
// headless consumer constructing [AttachmentDisposition] directly may
// skip it (garbage degrades loudly at [EffectiveDisposition]).
//
// `internal/config/validate.go::validateMultimodal` mirrors this
// grammar locally (the D-193 import direction forbids config →
// planner); `TestParseDisposition_ConfigGrammarLockstep` pins the two
// in lockstep.
func ParseDisposition(s string) (AttachmentDisposition, error) {
	d := AttachmentDisposition(s)
	if !d.Valid() {
		return "", fmt.Errorf("%w: %q (want ref | inline | provider_native | tool:<name>)", ErrInvalidDisposition, s)
	}
	return d, nil
}

// DispositionLayer names the precedence layer that resolved an
// attachment's disposition — returned by [ResolveDisposition] so the
// caller can record which layer won (the audit criterion: operators
// see the runtime never silently overrode them).
type DispositionLayer string

const (
	// DispositionLayerCallerHint — the per-attachment caller hint won
	// (the Protocol `disposition` field, or a directly-set
	// `InputArtifactView.Disposition` upstream of the resolver).
	DispositionLayerCallerHint DispositionLayer = "caller_hint"
	// DispositionLayerAgentPolicy — the per-agent policy map won
	// (`harbor.yaml` `multimodal.disposition`, or a programmatic
	// [DispositionPolicy]).
	DispositionLayerAgentPolicy DispositionLayer = "agent_policy"
	// DispositionLayerRuntimeDefault — the built-in runtime default
	// won: `image/*` → inline, everything else → ref (byte-for-byte
	// the pre-84b dispatch).
	DispositionLayerRuntimeDefault DispositionLayer = "runtime_default"
)

// DispositionPolicy is the per-agent disposition policy: a per-MIME
// map plus an agent-wide default. The type is the programmatic seam —
// `internal/config` merely decodes the `harbor.yaml`
// `multimodal.disposition` block into it (see
// [DispositionPolicyFromConfig]); a headless library consumer
// constructs the value directly. Both construction paths produce the
// same semantics.
//
// Lookup order within the policy layer: exact MIME match
// (`application/pdf`) > family wildcard (`application/*`) > Default.
// A zero policy (nil map, zero Default) contributes nothing and
// resolution falls through to the runtime default.
type DispositionPolicy struct {
	// ByMIME maps a MIME key to its disposition. Keys are an exact
	// IANA type (`application/pdf`) or a family wildcard
	// (`image/*`).
	ByMIME map[string]AttachmentDisposition
	// Default is the agent-wide disposition applied when no ByMIME
	// key matches. Zero means "no agent default" — resolution falls
	// through to the runtime default layer.
	Default AttachmentDisposition
}

// IsZero reports whether the policy contributes nothing (no map
// entries and no default).
func (p DispositionPolicy) IsZero() bool {
	return len(p.ByMIME) == 0 && p.Default.IsZero()
}

// lookup resolves mime against the policy: exact key, then the
// `family/*` wildcard, then Default. ok is false when the policy
// contributes nothing for this MIME.
func (p DispositionPolicy) lookup(mime string) (AttachmentDisposition, bool) {
	if d, found := p.ByMIME[mime]; found && !d.IsZero() {
		return d, true
	}
	if family, _, found := strings.Cut(mime, "/"); found && family != "" {
		if d, foundFam := p.ByMIME[family+"/*"]; foundFam && !d.IsZero() {
			return d, true
		}
	}
	if !p.Default.IsZero() {
		return p.Default, true
	}
	return "", false
}

// DefaultDisposition is the runtime-default layer: the built-in map
// that reproduces the pre-84b materializer dispatch. `image/*` →
// [DispositionInline] (the sub-threshold DataURL fast path);
// everything else → [DispositionRef] (the `ArtifactStub` +
// `Fetch.Tool` path). Exported so a headless consumer can read the
// default the runtime would apply.
func DefaultDisposition(mime string) AttachmentDisposition {
	if strings.HasPrefix(mime, "image/") {
		return DispositionInline
	}
	return DispositionRef
}

// ResolveDisposition is the pure precedence resolver (Phase 84b —
// D-189): per-attachment caller hint > per-agent policy map > runtime
// default. It returns the resolved disposition AND the layer that
// won, so the caller can log/emit which disposition fired and why.
//
// Pure function — no I/O, no logging, no state. The run loop
// (`cmd/harbor` + the devstack mirror) is a thin caller; a headless
// library consumer calls the same function, or skips it entirely by
// setting `InputArtifactView.Disposition` directly (the consumer IS
// the top-precedence layer).
//
// The hint is trusted verbatim (carriers validate via
// [ParseDisposition] at their edge); capability constraints (unknown
// tool, non-image inline) are applied by [EffectiveDisposition],
// which returns typed degradation facts.
func ResolveDisposition(hint AttachmentDisposition, policy DispositionPolicy, mime string) (AttachmentDisposition, DispositionLayer) {
	if !hint.IsZero() {
		return hint, DispositionLayerCallerHint
	}
	if d, ok := policy.lookup(mime); ok {
		return d, DispositionLayerAgentPolicy
	}
	return DefaultDisposition(mime), DispositionLayerRuntimeDefault
}

// DispositionDegradationReason is the typed vocabulary of why a
// resolved disposition could not be honoured and degraded to
// [DispositionRef].
type DispositionDegradationReason string

const (
	// DegradationUnknownTool — a `tool:<name>` disposition named a
	// tool absent from the run's catalog view. Degrades to ref so the
	// run proceeds; the caller logs the warning (revisit as a hard
	// error if operators prefer — plan §risks).
	DegradationUnknownTool DispositionDegradationReason = "unknown_tool"
	// DegradationInlineUnsupportedMIME — `inline` resolved for a
	// non-`image/*` MIME; V1.1 inlines images only.
	DegradationInlineUnsupportedMIME DispositionDegradationReason = "inline_unsupported_mime"
	// DegradationInvalidDisposition — a directly-constructed
	// disposition value outside the grammar reached resolution.
	DegradationInvalidDisposition DispositionDegradationReason = "invalid_disposition"
)

// DispositionDegradation is the typed degradation fact
// [EffectiveDisposition] returns when a resolved disposition cannot
// be honoured. The resolver never logs — the caller (run loop,
// devstack, or a consumer's own loop) logs and emits the fact
// (fail-loud, never a silent drop — CLAUDE.md §13).
type DispositionDegradation struct {
	// From is the disposition that was requested / resolved.
	From AttachmentDisposition
	// To is the disposition actually applied (always ref at V1.1).
	To AttachmentDisposition
	// Reason is the typed degradation cause.
	Reason DispositionDegradationReason
	// Tool carries the unknown tool name when Reason is
	// [DegradationUnknownTool].
	Tool string
}

// EffectiveDisposition applies runtime capability constraints to a
// resolved disposition, returning the disposition the materializer
// will honour plus a typed degradation fact when they differ (nil
// when honoured as-is). Pure function — the caller logs/emits the
// fact.
//
// Constraints at V1.1:
//
//   - `provider_native` is honoured as-is — the materializer emits a
//     `ProviderNative`-flagged typed part and the LLM driver performs
//     the provider upload inside `Complete`. A provider without
//     upload support for the modality degrades at the DRIVER (the
//     `ArtifactStub` universal degradation, with a logged notice) —
//     not here.
//   - `tool:<name>` degrades to ref when the catalog view does not
//     resolve the name ([DegradationUnknownTool]). A nil catalog
//     skips the check — the forced tool is kept verbatim (the hint
//     on the emitted stub is advisory to the LLM; a headless caller
//     without a catalog view still gets the forced routing).
//   - `inline` degrades to ref for non-`image/*` MIMEs
//     ([DegradationInlineUnsupportedMIME]).
//   - a non-grammar value degrades to ref
//     ([DegradationInvalidDisposition]).
//
// A zero disposition passes through unchanged — it selects the
// pre-84b materializer dispatch and there is nothing to degrade.
func EffectiveDisposition(resolved AttachmentDisposition, mime string, catalog ToolCatalogView) (AttachmentDisposition, *DispositionDegradation) {
	switch {
	case resolved.IsZero(), resolved == DispositionRef:
		return resolved, nil
	case resolved == DispositionInline:
		if strings.HasPrefix(mime, "image/") {
			return DispositionInline, nil
		}
		return DispositionRef, &DispositionDegradation{
			From: resolved, To: DispositionRef, Reason: DegradationInlineUnsupportedMIME,
		}
	case resolved == DispositionProviderNative:
		return DispositionProviderNative, nil
	}
	if name, ok := resolved.ToolName(); ok {
		if catalog != nil {
			if _, found := catalog.Resolve(name); !found {
				return DispositionRef, &DispositionDegradation{
					From: resolved, To: DispositionRef, Reason: DegradationUnknownTool, Tool: name,
				}
			}
		}
		return resolved, nil
	}
	return DispositionRef, &DispositionDegradation{
		From: resolved, To: DispositionRef, Reason: DegradationInvalidDisposition,
	}
}
