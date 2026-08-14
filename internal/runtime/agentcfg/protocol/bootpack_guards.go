// bootpack_guards.go — the boot-owned mutation guards for the operator pack
// verbs.
//
// A boot-declared operator skill baseline (internal/skills/bootpacks) is
// READ-ONLY from the control plane: the `agent_config.agent_packs.*` verbs
// govern the agent's DURABLE agent_packs revision, and a canonical name the
// boot baseline owns for the exact (tenant, agent) pair must never be
// reintroduced into that durable revision by any write path. The guards here
// are the single choke point the write verbs consume BEFORE any mutation:
//
//   - upsert refuses a boot-owned name even when the submitted body hashes
//     identically to the boot entry — an equal hash proves nothing, boot
//     wins (the baseline is edited in the boot config and applied on the
//     next deployment, never through the control plane);
//   - every commit path (initial, response-loss replay, prepared/committing
//     resume, and the publication/activation write itself) re-checks fresh
//     ownership, so no proposal path can smuggle a boot-owned name into the
//     durable revision;
//   - remove may delete an ACTUAL legacy durable revision shadow (the
//     pre-baseline durable copy of a now-boot-owned name) while leaving
//     boot, but a boot-only name is a typed read-only refusal — never a
//     false success;
//   - the pure [GuardBootOwnedRevision] helper is what the generic rollback
//     door invokes before repointing the active pointer at any revision that
//     contains a boot-owned name.
//
// # The injected reader (the seam)
//
// The guards consume a NARROW read-only reader, [BootOwnership], keyed by
// exact (tenant, agent, canonical name). The eager immutable bootpacks.Index
// satisfies it directly (internal/skills/bootpacks — `OwnsName`). A nil
// reader means no boot baseline is bound on this runtime: every guard is
// inert and the verbs keep their exact pre-baseline behavior.
//
// The reader is injected PER REQUEST through the context seam
// ([WithBootOwnership] / bootOwnershipFromContext), so the guard code holds
// no Service state, no package-level mutable state, and is safe for N
// concurrent requests under -race. The integration owner wires the concrete
// reader at the handler boundary — or, once the Service gains its
// boot-ownership field + option, re-points the SINGLE read inside
// bootOwnershipFromContext at that field (every verb goes through it).
package protocol

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/skills"
)

// ErrBootPackOwned refuses a control-plane mutation whose target canonical
// pack name is boot-declared for the exact (tenant, agent) pair. The boot
// baseline is read-only from the pack verbs: the operator edits the boot
// config and restarts. The refusal is typed (errors.Is) and fires BEFORE
// any revision, proposal, or store write — no partial effect, no false
// success.
var ErrBootPackOwned = errors.New("agentcfg/protocol: pack name is boot-declared and read-only to the control plane")

// BootOwnership is the narrow, injected, read-only authority over which
// canonical pack names the boot baseline owns for an exact (tenant, agent)
// pair. The eager immutable bootpacks.Index satisfies it directly
// (internal/skills/bootpacks — `OwnsName`). A nil reader means no baseline
// is bound on this runtime and every guard is inert.
type BootOwnership interface {
	// OwnsName reports whether name is a boot-declared canonical pack name
	// for the exact (tenantID, agentID) pair. Implementations canonicalize
	// the name (lowercase, trimmed) before the lookup, so callers may pass a
	// raw or already-canonical name. Implementations MUST be safe for
	// concurrent use.
	OwnsName(tenantID, agentID, name string) bool
}

// bootOwnershipContextKey is the private context key carrying the injected
// reader for one request. An unexported struct type keeps the key unique to
// this package (no cross-package collision).
type bootOwnershipContextKey struct{}

// WithBootOwnership returns a context carrying the injected boot-ownership
// reader, so the mutation guards can consume it without a Service field. A
// nil reader (or nil ctx) is a no-op: the guards stay inert.
func WithBootOwnership(ctx context.Context, owner BootOwnership) context.Context {
	if ctx == nil || owner == nil {
		return ctx
	}
	return context.WithValue(ctx, bootOwnershipContextKey{}, owner)
}

// bootOwnershipFromContext resolves the injected reader for a request. It
// returns nil when no reader is bound — every guard is then inert and the
// verbs keep their exact pre-baseline behavior. This is the SINGLE seam the
// integration owner re-points at a Service field once the field + option
// land; every verb reads the reader through this one function.
func bootOwnershipFromContext(ctx context.Context) BootOwnership {
	owner, _ := ctx.Value(bootOwnershipContextKey{}).(BootOwnership)
	return owner
}

// guardBootOwnedName refuses a control-plane write whose target pack name is
// boot-declared for the exact (tenant, agent) pair. The name is
// canonicalized (lowercase, trimmed) before the ownership lookup, mirroring
// the pack and boot-index identity. A nil owner or a non-owned name is a
// no-op (nil).
func guardBootOwnedName(owner BootOwnership, tenantID, agentID, name string) error {
	if owner == nil {
		return nil
	}
	canonical := skills.CanonicalPackName(name)
	if canonical == "" {
		return nil
	}
	if owner.OwnsName(tenantID, agentID, canonical) {
		return fmt.Errorf("%w: %q (boot-declared for tenant=%q agent=%q; edit the boot config and restart)",
			ErrBootPackOwned, canonical, tenantID, agentID)
	}
	return nil
}

// GuardBootOwnedRevision is the pure target-revision guard the generic
// rollback door invokes before repointing the active pointer at any revision
// whose agent_packs section contains a boot-owned canonical name. It is
// pure: no Service receiver, no context, no I/O — the caller supplies the
// reader and the target items. It returns the typed ErrBootPackOwned naming
// the first owned canonical name, or nil when the owner is nil, the items
// are empty, or no item is boot-owned.
func GuardBootOwnedRevision(owner BootOwnership, tenantID, agentID string, items []skills.AgentPackItem) error {
	if owner == nil || len(items) == 0 {
		return nil
	}
	for _, item := range items {
		if err := guardBootOwnedName(owner, tenantID, agentID, item.Name); err != nil {
			return err
		}
	}
	return nil
}
