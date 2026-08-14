package drafter

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/artifacts"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// handler.go — the ordinary tool handler for `skill_create_draft`.
//
// The handler is the ONLY place this lane persists anything, and the
// only mutation it can perform is ONE immutable caller-scoped,
// resource-free SKILL.md artifact written through the injected narrow
// ArtifactWriter. There is no path here to a skill-store upsert,
// user-skill membership/revision, operator-pack proposal/publication,
// capability registration, tool exposure, approval, OAuth, or admin
// credential — those seams are never imported, and the handler's
// arguments and model output can neither name them nor reach them.

// StateDraft is the draft state stamped on every successful result.
const StateDraft = "draft"

// ProvenanceGeneratedDraft is the fixed server-stamped provenance
// marker of the draft-only lane. It is NEVER model-supplied: the
// decoder rejects any provenance member in the model output, so this
// value is the only provenance this lane can produce.
const ProvenanceGeneratedDraft = "generated-draft:skill_create_draft:v1"

// Args is the CLOSED input shape of the tool. Only a bounded intent
// and optional non-authorizing revision feedback are accepted; any
// identity / scope / persistence / publication / capability /
// approval field is rejected by the derived schema before the handler
// runs (unknown fields fail dispatch validation), so a caller can
// never select an owner, a scope, or a persistence behavior.
type Args struct {
	// Intent is the bounded authoring intent. Required.
	Intent string `json:"intent"`
	// Feedback is optional non-authorizing revision feedback.
	Feedback string `json:"feedback,omitempty"`
}

// Result is the bounded non-secret review metadata returned by the
// tool. It never carries raw model output or the full draft body —
// the bytes live in the content-addressed artifact and are reachable
// only through authorized artifact reads. The explicit `installed:
// false` state means this lane never installs; a later validate/commit
// consumer decides installation.
type Result struct {
	// ArtifactRef is the caller-scoped content-addressed ref of the
	// single immutable SKILL.md draft artifact.
	ArtifactRef string `json:"artifact_ref"`
	// PackageHash is the versioned hash of the complete resource-free
	// package (v1:<64-hex>), identical to what the canonical
	// validate/commit ingest computes from the artifact bytes.
	PackageHash string `json:"package_hash"`
	// Name is the normalized (lowercase, trimmed) skill name.
	Name string `json:"name"`
	// Title is the human-readable title, when present.
	Title string `json:"title,omitempty"`
	// Summary is the bounded normalized description excerpt.
	Summary string `json:"summary"`
	// Warnings are bounded non-secret review notes (e.g. declared
	// required tools are metadata only, never grants).
	Warnings []string `json:"warnings,omitempty"`
	// RequiredTools are the capability-annotation tool names the draft
	// declares. Metadata only — NEVER a grant; the run tool set does
	// not widen.
	RequiredTools []string `json:"required_tools,omitempty"`
	// Provenance is the fixed server-stamped marker above.
	Provenance string `json:"provenance"`
	// State is always StateDraft on success.
	State string `json:"state"`
	// Installed is ALWAYS false: this lane never installs, publishes,
	// or activates anything.
	Installed bool `json:"installed"`
}

// CreateDraft is the ordinary tool handler body. It runs the adapter,
// renders the canonical resource-free SKILL.md document, computes the
// versioned package hash, writes the ONE immutable artifact through
// the injected narrow writer, and returns the bounded review metadata
// with an explicit `installed: false` state.
//
// Identity is read exclusively from ctx and is mandatory. Refusal,
// malformed output, cancellation, timeout, and write failure all fail
// loud; no success, failure, retry, cancellation, replay, or
// response-loss path can reach a skill-store / membership / pack /
// capability mutation.
func CreateDraft(ctx context.Context, a *Adapter, w ArtifactWriter, args Args) (Result, error) {
	if a == nil {
		return Result{}, fmt.Errorf("drafter: adapter is required")
	}
	if w == nil {
		return Result{}, ErrWriterRequired
	}
	q, ok := identityQuad(ctx)
	if !ok || q.TenantID == "" || q.UserID == "" || q.SessionID == "" {
		return Result{}, ErrMissingIdentity
	}

	skill, err := a.Draft(ctx, args.Intent, args.Feedback)
	if err != nil {
		return Result{}, err
	}
	doc, err := RenderSkillMD(skill)
	if err != nil {
		return Result{}, err
	}
	pkg := skillpkg.Package{Name: skillpkg.CanonicalName(skill.Name), Skill: skill}
	hash, err := skillpkg.PackageHash(pkg)
	if err != nil {
		return Result{}, fmt.Errorf("drafter: package hash: %w", err)
	}

	ref, err := w.Write(ctx, doc, artifacts.PutOpts{
		MimeType: "text/markdown",
		Filename: skillpkg.RootSkillFileName,
	})
	if err != nil {
		return Result{}, fmt.Errorf("drafter: persist draft artifact: %w", err)
	}

	return Result{
		ArtifactRef:   ref.ID,
		PackageHash:   hash,
		Name:          skill.Name,
		Title:         skill.Title,
		Summary:       boundedSummary(skill.Description),
		Warnings:      buildWarnings(skill),
		RequiredTools: skill.RequiredTools,
		Provenance:    ProvenanceGeneratedDraft,
		State:         StateDraft,
		Installed:     false,
	}, nil
}

// boundedSummary returns a bounded excerpt of the normalized
// description for the tool result. The full body lives in the
// artifact; the result carries at most MaxSummaryRunes.
func boundedSummary(desc string) string {
	if desc == "" {
		return ""
	}
	r := []rune(desc)
	if len(r) <= MaxSummaryRunes {
		return string(r)
	}
	return string(r[:MaxSummaryRunes-1]) + "…"
}

// buildWarnings assembles the bounded non-secret review warnings for a
// validated draft: capability-annotation fields are metadata and never
// become grants.
func buildWarnings(s skillpkg.PackageSkill) []string {
	var warnings []string
	if len(s.RequiredTools) > 0 || len(s.RequiredNS) > 0 || len(s.RequiredTags) > 0 {
		warnings = append(warnings,
			"declared required_tools/required_namespaces/required_tags are metadata only and are never treated as capability grants in this run")
	}
	return warnings
}
