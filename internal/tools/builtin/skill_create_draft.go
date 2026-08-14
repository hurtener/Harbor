package builtin

// skill_create_draft.go — the draft-only personal-skill tool carrier.
//
// `skill_create_draft` is an ordinary runtime tool that turns a bounded
// authoring intent plus optional revision feedback into ONE validated,
// caller-scoped, resource-free SKILL.md DRAFT artifact. The
// implementation lives in internal/skills/drafter; this file is the
// thin registration carrier that adapts the drafter lane to the
// ordinary tool catalog and policy shell.
//
// DISABLED BY DEFAULT. The carrier IS wired into the builtin registry
// map, so `skill_create_draft` is present in `KnownNames()` and an
// operator enables it by listing it in `tools.built_in`, exactly like
// every other built-in. It stays off by default only because the
// recommended/default configs do not list it. Registration pulls the
// assembly's COMPOSED LLM client from the registry context; listing the
// tool on a runtime without a usable LLM fails the boot loud rather
// than silently skipping. Once enabled it is opt-in per agent through
// the ordinary tool policy, with policy, approval, governance,
// rate/cost limits, deadline, cancellation, redaction, and audit
// wrappers identical to every other ordinary tool.
//
// Zero mutation authority. The registered body writes exactly ONE
// immutable artifact through the drafter's narrow write-only seam
// under the invocation's verified run identity. There is no
// skill-store upsert, membership/revision, operator-pack
// proposal/publication, capability registration, tool exposure, or
// approval/OAuth path. Identity comes exclusively from the run
// context; the tool's closed argument shape (intent + optional
// feedback) rejects any owner/scope/identity/persistence/publication/
// grant field at dispatch validation.

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/skills/drafter"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// SkillCreateDraftArgs is the model-facing input shape. Alias of the
// drafter lane's closed Args (bounded intent + optional feedback).
type SkillCreateDraftArgs = drafter.Args

// SkillCreateDraftResult is the model-facing output shape. Alias of
// the drafter lane's Result: artifact ref, versioned package hash,
// normalized name/title/summary, warnings, provenance, and explicit
// `installed: false` / `state: draft`.
type SkillCreateDraftResult = drafter.Result

// RegisterSkillCreateDraft installs the `skill_create_draft` ordinary
// tool onto the catalog. The builtin registry's `skill_create_draft`
// entry invokes it when the operator lists the tool in
// `tools.built_in` — the ordinary opt-in every built-in shares — so
// the tool stays disabled by default because the recommended/default
// configs do not list it.
//
// The client is the assembly's composed LLM client (safety net,
// corrections, retry, governance already wrapped); the registry
// supplies it from the context, and this package never chooses a
// provider or a credential. The catalog and store come from the
// registry context. A nil client or catalog fails the registration
// loudly (wiring-shaped); a nil ArtifactStore fails at invoke time
// with an operator-readable message (store-shaped), mirror of the
// other store-backed built-ins.
func RegisterSkillCreateDraft(rc RegistryContext, client llm.LLMClient) error {
	if rc.Catalog == nil {
		return fmt.Errorf("skill_create_draft: RegistryContext.Catalog is required")
	}
	if client == nil {
		return fmt.Errorf("skill_create_draft: llm client is required (assembly must thread the composed LLM client)")
	}
	adapter, err := drafter.New(client, drafter.Options{})
	if err != nil {
		return fmt.Errorf("skill_create_draft: %w", err)
	}
	return inproc.RegisterFunc[SkillCreateDraftArgs, SkillCreateDraftResult](
		rc.Catalog, drafter.ToolName,
		func(ctx context.Context, args SkillCreateDraftArgs) (SkillCreateDraftResult, error) {
			if rc.ArtifactStore == nil {
				return SkillCreateDraftResult{}, fmt.Errorf("skill_create_draft: backing ArtifactStore is nil (operator misconfiguration: builtin.RegistryContext.ArtifactStore was not threaded)")
			}
			q, err := requireIdentity(ctx)
			if err != nil {
				return SkillCreateDraftResult{}, err
			}
			writer, err := drafter.NewScopedWriter(rc.ArtifactStore, artifacts.ArtifactScope{
				TenantID:  q.TenantID,
				UserID:    q.UserID,
				SessionID: q.SessionID,
				TaskID:    q.RunID,
			})
			if err != nil {
				return SkillCreateDraftResult{}, err
			}
			result, err := drafter.CreateDraft(ctx, adapter, writer, args)
			if err != nil {
				return SkillCreateDraftResult{}, err
			}
			appendUnavailableToolWarnings(&result, rc, q)
			return result, nil
		},
		tools.WithDescription("Draft a personal skill from a bounded authoring intent. Emit `intent` describing the skill you want (and optionally `feedback` to revise an earlier draft). The tool returns a caller-scoped immutable `SKILL.md` draft artifact reference plus its versioned package hash and a bounded review summary. The draft is NOT installed: it is a reviewable artifact you install later through the explicit validate/commit workflow. The draft may warn that declared required tools are metadata only and never grant capability."),
		tools.WithSideEffect(tools.SideEffectWrite),
		tools.WithLoading(tools.LoadingAlways),
		tools.WithTags("builtin", "meta", "skills", "generate"),
		tools.WithSource(tools.ToolSourceID("skills/drafter")),
	)
}

// appendUnavailableToolWarnings annotates the result with one bounded
// warning per declared required tool that is absent from the run's
// reachable tool set. The draft may warn, but the run tool set never
// widens: the warning is review metadata, not a grant.
func appendUnavailableToolWarnings(result *SkillCreateDraftResult, rc RegistryContext, q identity.Quadruple) {
	visible := make(map[string]struct{})
	for _, name := range tools.VisibleNames(rc.Catalog, tools.CatalogFilter{
		TenantID:      q.TenantID,
		UserID:        q.UserID,
		SessionID:     q.SessionID,
		GrantedScopes: rc.GrantedScopes,
	}) {
		visible[name] = struct{}{}
	}
	for _, tool := range result.RequiredTools {
		if _, ok := visible[tool]; !ok {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("required tool %q is not in the current run's tool set; the draft records it as metadata only and never grants it", tool))
		}
	}
}
