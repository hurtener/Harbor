// Package projection holds the run-start agent-config projection shared by
// every run-loop driver (the production dev driver and the harbortest
// devstack twin) and exercised directly by the control-plane integration
// test. Extracting it here keeps the projection logic in ONE place: the
// drivers live in separate binaries (cmd/harbor's package main cannot be
// imported by harbortest/devstack), so an inlined copy in each would drift
// (CLAUDE.md §17.6). The integration test calls the same function, so the
// test exercises the real projection rather than a test-local copy
// (CLAUDE.md §17.4).
package projection

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// ErrSkillBodyMissing is returned by [ActiveSkillViews] when an agent's
// active config pins an ADMIN skill-membership name whose body is absent
// from the store (e.g. the skill was hard-deleted, or a rollback landed on a
// revision referencing a since-deleted skill). An admin-pinned skill whose
// body is absent from the store is a LOUD failure at run-start projection —
// never a silent drop (CLAUDE.md §13).
// Session-personal skill names are exempt (a safe-subset add that may
// legitimately not be in the directory view).
var ErrSkillBodyMissing = errors.New("agentcfg/projection: agent-config pins a skill whose body is absent from the store")

// SourceOwnerResolver reports the physical owner stamped on a runtime-added
// MCP source. The run projection uses it to hide foreign user-owned sources;
// boot and operator-owned sources remain part of the operator ceiling.
type SourceOwnerResolver interface {
	OwnerOfSource(source tools.ToolSourceID) (auth.Owner, bool)
}

type sourceLogicalNameResolver interface {
	LogicalNameOfSource(source tools.ToolSourceID) (string, bool)
}

// physicalizeUserExposure keeps user policy names logical while the live
// catalog uses owner-derived source ids. A user revision may say
// "shared_echo" or "shared"; the process-local catalog may contain
// "shared~u-<digest>_echo" and "shared~u-<digest>" for that same pair.
// Translate the user tier only for the matching owner, preserving the
// original names so the same narrow policy still applies to an operator/boot
// source with that logical name or to a source that is not attached yet.
func physicalizeUserExposure(base tools.PlannerCatalogView, resolver SourceOwnerResolver, owner auth.Owner, paused, disabled []string) ([]string, []string) {
	if base == nil || resolver == nil || owner.User == "" || (len(paused) == 0 && len(disabled) == 0) {
		return paused, disabled
	}
	logicalNames, ok := resolver.(sourceLogicalNameResolver)
	if !ok {
		return paused, disabled
	}

	pausedSet := make(map[string]struct{}, len(paused))
	for _, name := range paused {
		if name != "" {
			pausedSet[name] = struct{}{}
		}
	}
	disabledSet := make(map[string]struct{}, len(disabled))
	for _, name := range disabled {
		if name != "" {
			disabledSet[name] = struct{}{}
		}
	}
	physicalPaused := make(map[string]struct{})
	physicalDisabled := make(map[string]struct{})
	for _, tool := range base.List() {
		if tool.Source == "" {
			continue
		}
		sourceOwner, found := resolver.OwnerOfSource(tool.Source)
		if !found || sourceOwner != owner {
			continue
		}
		logicalSource, found := logicalNames.LogicalNameOfSource(tool.Source)
		if !found || logicalSource == "" {
			continue
		}
		if _, selected := pausedSet[logicalSource]; selected {
			physicalPaused[string(tool.Source)] = struct{}{}
		}
		if !strings.HasPrefix(tool.Name, string(tool.Source)) {
			continue
		}
		suffix := strings.TrimPrefix(tool.Name, string(tool.Source))
		if _, selected := disabledSet[logicalSource+suffix]; selected {
			physicalDisabled[tool.Name] = struct{}{}
		}
	}
	for name := range physicalPaused {
		paused = append(paused, name)
	}
	for name := range physicalDisabled {
		disabled = append(disabled, name)
	}
	return paused, disabled
}

// physicalizeUserLoadingModes adds physical aliases for a user's logical
// loading-mode choices. User revisions intentionally retain logical
// connection/tool names, while a live user-owned MCP source is stored under
// an owner-derived physical source id. Keeping this translation next to the
// disable-set translation prevents a user server-loading choice from silently
// becoming inert merely because the source was namespaced.
func physicalizeUserLoadingModes(cat tools.ToolCatalog, filter tools.CatalogFilter, resolver SourceOwnerResolver, owner auth.Owner, exposure *agentcfg.ToolExposure) *agentcfg.ToolExposure {
	if cat == nil || resolver == nil || owner.User == "" || !hasLoadingOverrides(exposure) {
		return exposure
	}
	logicalNames, ok := resolver.(sourceLogicalNameResolver)
	if !ok {
		return exposure
	}
	out := &agentcfg.ToolExposure{
		PausedServers:      append([]string(nil), exposure.PausedServers...),
		DisabledTools:      append([]string(nil), exposure.DisabledTools...),
		ServerLoadingModes: cloneLoadingModes(exposure.ServerLoadingModes),
		ToolLoadingModes:   cloneLoadingModes(exposure.ToolLoadingModes),
	}
	broad := filter
	broad.LoadingModes = []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred}
	for _, tool := range cat.List(broad) {
		if tool.Source == "" {
			continue
		}
		sourceOwner, found := resolver.OwnerOfSource(tool.Source)
		if !found || sourceOwner != owner {
			continue
		}
		logical, found := logicalNames.LogicalNameOfSource(tool.Source)
		if !found || logical == "" {
			continue
		}
		if mode, found := exposure.ServerLoadingModes[logical]; found {
			if out.ServerLoadingModes == nil {
				out.ServerLoadingModes = make(map[string]string)
			}
			out.ServerLoadingModes[string(tool.Source)] = mode
		}
		if !strings.HasPrefix(tool.Name, string(tool.Source)) {
			continue
		}
		suffix := strings.TrimPrefix(tool.Name, string(tool.Source))
		if mode, found := exposure.ToolLoadingModes[logical+suffix]; found {
			if out.ToolLoadingModes == nil {
				out.ToolLoadingModes = make(map[string]string)
			}
			out.ToolLoadingModes[tool.Name] = mode
		}
	}
	return out
}

func cloneLoadingModes(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// userLoadingTool projects a physical source back to the logical name used
// by a user's durable revision. The bool is false when the physical source is
// owned by another user, so that user's loading preferences cannot affect a
// foreign attachment. Boot/operator sources and unregistered descriptors
// retain their ordinary names for backward-compatible shared choices.
func userLoadingTool(t tools.Tool, resolver SourceOwnerResolver, owner auth.Owner) (tools.Tool, bool) {
	if resolver == nil || t.Source == "" || owner.User == "" {
		return t, true
	}
	sourceOwner, found := resolver.OwnerOfSource(t.Source)
	if !found || sourceOwner.User == "" {
		return t, true
	}
	if sourceOwner != owner {
		return tools.Tool{}, false
	}
	logicalNames, ok := resolver.(sourceLogicalNameResolver)
	if !ok {
		return t, true
	}
	logical, found := logicalNames.LogicalNameOfSource(t.Source)
	if !found || logical == "" {
		return t, true
	}
	physical := string(t.Source)
	t.Source = tools.ToolSourceID(logical)
	// The physical source is the prefix of every MCP catalog name. Preserve
	// the suffix while replacing only that prefix with the logical source.
	if strings.HasPrefix(t.Name, physical) {
		t.Name = logical + strings.TrimPrefix(t.Name, physical)
	}
	return t, true
}

type userScopedMCPView struct {
	base        tools.PlannerCatalogView
	resolver    SourceOwnerResolver
	owner       auth.Owner
	desiredPair map[string]struct{}
}

func (v userScopedMCPView) allows(tool tools.Tool) bool {
	owner, ok := v.resolver.OwnerOfSource(tool.Source)
	if !ok || owner.User == "" {
		return true
	}
	if owner != v.owner {
		return false
	}
	logical := string(tool.Source)
	if names, ok := v.resolver.(sourceLogicalNameResolver); ok {
		if resolved, found := names.LogicalNameOfSource(tool.Source); found {
			logical = resolved
		}
	}
	_, ok = v.desiredPair[logical]
	return ok
}

func (v userScopedMCPView) Resolve(name string) (tools.Tool, bool) {
	t, ok := v.base.Resolve(name)
	if !ok || !v.allows(t) {
		return tools.Tool{}, false
	}
	return t, true
}

func (v userScopedMCPView) List() []tools.Tool {
	all := v.base.List()
	out := make([]tools.Tool, 0, len(all))
	for _, tool := range all {
		if v.allows(tool) {
			out = append(out, tool)
		}
	}
	return out
}

// loadOverlay reads the session's safe-subset overlay (the lower tier of the
// authorization matrix) for the run's REAL (tenant, user, session) triple. A
// nil store, an empty agentID, or an agent with no overlay returns the zero
// overlay (and a nil error) — the backward-compatible "no session overlay"
// path. An
// overlay read error is returned so the caller fails the run loudly
// (CLAUDE.md §13). The overlay is keyed by the real triple (NOT the synthetic
// agentcfg identity), so it is session-isolated by construction.
func loadOverlay(ctx context.Context, ov sessionoverlay.Store, agentID string, id identity.Quadruple) (sessionoverlay.Overlay, error) {
	if ov == nil || agentID == "" {
		return sessionoverlay.Overlay{}, nil
	}
	o, _, err := ov.Get(ctx, identity.Quadruple{Identity: id.Identity}, agentID)
	return o, err
}

// ConnectionDetacher is the driver-agnostic seam the run-start reconciliation
// uses to detach a no-longer-declared runtime-added MCP server at the
// next-turn projection boundary. The concrete (wired at the cmd/harbor +
// devstack boundary) deregisters the source's tools from the planner catalog
// view + the MCP registry and closes its transport gracefully — the physical
// inverse of the add path's attach. Keeping it an injected interface preserves
// this package's §4.4 boundary: the projection imports no concrete MCP driver
// (auth carries only the plain reconcile-view owner tag, not a driver).
type ConnectionDetacher interface {
	// AttachedSources returns the reconciling owner's RUNTIME-ADDED source ids
	// — the owner-scoped reconcile VIEW the reconcile diffs against the
	// declared set. It deliberately does NOT return boot-declared servers or
	// another owner's runtime-adds: the registry stays process-global and
	// deployment-shared (boot servers resolve + dispatch by bare name across
	// every session), but the reconcile VIEW is owner-scoped so one owner's
	// run-start reconcile can never detach a boot server or another owner's
	// connection. A fresh slice; the caller may retain it.
	AttachedSources(ctx context.Context, owner auth.Owner) []string
	// Detach deregisters the named source's tools from the catalog + MCP
	// registry and closes its transport gracefully. Idempotent — a source
	// already gone is a no-op the concrete swallows.
	//
	// owner is the reconciling owner AttachedSources was called with, carried
	// through so the concrete's registry removal is owner-scoped at the
	// registry's own resolution choke point rather than trusted to this
	// caller's view alone. It mirrors AttachedSources' own owner parameter.
	Detach(ctx context.Context, source string, owner auth.Owner) error
}

// ConnectionReattacher is the driver-agnostic seam the run-start ATTACH pass
// uses to bring a DECLARED-but-ABSENT runtime-added MCP server back under the
// reconciling owner. It is the symmetric twin of [ConnectionDetacher] and pairs
// with it in the same [ReconcileConnections] call; the concrete (wired at the
// cmd/harbor + devstack boundary) drives the SAME attach lifecycle the admin
// add verb drives, so every gate the add door applies is re-applied without
// being re-implemented. Keeping it an injected interface preserves this
// package's §4.4 boundary: the projection imports no concrete MCP driver.
type ConnectionReattacher interface {
	// Reattach attaches desc under owner. It is IDEMPOTENT: a name already
	// registered under owner is a no-op (the concrete re-checks the live
	// registry under its own whole-attach lock, closing the stale-view window
	// between the caller's AttachedSources read and this call). Every gate the
	// admin add door applies is re-applied here against CURRENT boot policy, so
	// a policy that has since tightened refuses the re-attach.
	//
	// id is the reconciling RUN's quadruple. The triple is the authorization
	// scope; the RunID is carried onto the concrete's lifecycle events as the
	// machine-readable discriminator between a reconcile and an admin add
	// (whose RunID is empty). It is NOT an isolation key.
	//
	// The concrete OWNS reporting: a refused or unreachable connection is
	// emitted on its own canonical event with a stable class, bounded by a
	// per-(owner, name) backoff, so the sweep never goes silent and never spams.
	// The returned error is for the caller's loud log + the joined sweep error;
	// it never means "fail the run".
	Reattach(ctx context.Context, owner auth.Owner, id identity.Quadruple, desc agentcfg.MCPConnectionDescriptor) (changed bool, err error)
}

// ErrReconcileRead wraps an active-revision read failure inside
// ReconcileConnections so the run-loop caller can distinguish a fail-loud
// read error (never a silent "detach nothing", CLAUDE.md §13) from a
// best-effort detach error.
var ErrReconcileRead = errors.New("agentcfg/projection: run-start reconcile active-revision read failed")

// ErrReconcileReattach marks every per-connection error the ATTACH pass
// produces, so the run-loop caller can tell an unreachable or refused third
// party (loud, non-fatal, already reported on its own canonical event, and NOT a
// reason to skip the remaining reconcile legs) from a detach failure or a
// fail-loud read error. Without the marker the caller would have to string-match
// the joined error, and one unreachable declared server would silently stop the
// discovery-allowance re-apply for every run.
var ErrReconcileReattach = errors.New("agentcfg/projection: run-start reconcile re-attach failed")

// reattachFailure marks ONE connection's attach-pass error with
// [ErrReconcileReattach] while keeping the underlying error a SINGLE unwrap
// chain.
//
// The single chain is load-bearing, not stylistic. A multi-`%w` fmt.Errorf would
// carry both the marker and the cause, but it also satisfies
// `interface{ Unwrap() []error }` — the very shape a caller uses to walk an
// errors.Join tree. Such a caller would descend INTO the wrap, see the cause on
// its own (stripped of the marker), and misclassify it as a detach failure. This
// type answers Is(ErrReconcileReattach) directly and unwraps to the cause alone,
// so both the marker and every sentinel on the cause stay reachable via
// errors.Is while the value remains a leaf to a join walker.
type reattachFailure struct {
	name string
	err  error
}

func (e *reattachFailure) Error() string {
	return fmt.Sprintf("%s: %q: %v", ErrReconcileReattach.Error(), e.name, e.err)
}

func (e *reattachFailure) Unwrap() error { return e.err }

func (e *reattachFailure) Is(target error) bool { return target == ErrReconcileReattach }

// ReconcileConnections is the run-start connection-reconciliation leg. It is
// BIDIRECTIONAL — the same shape [ReconcileOAuthProviders] already uses — and
// runs its two passes in a fixed order:
//
//  1. DETACH: compare the agent's DECLARED runtime-added connections (the active
//     config revision's connections section) against the reconciling OWNER's
//     ATTACHED set (the owner-scoped reconcile VIEW over the live MCP registry)
//     and detach every server that owner has attached but no longer declares — a
//     connection removed via `agent_config.remove_mcp_connection`, or a server
//     re-declared away by a rollback past an add. Detaching deregisters the
//     server's tools from the catalog + MCP registry and closes its transport, so
//     the NEXT run's projected catalog excludes them, the registry no longer
//     lists it, and the subprocess drains.
//  2. ATTACH: re-read the owner's attached set (FRESH, after the detach pass) and
//     re-establish every connection the active revision DECLARES that the live
//     registry does not carry under that owner. This is what makes a
//     runtime-added connection survive a process restart and what makes a
//     rollback that RE-declares a removed connection bring it back — one
//     mechanism, N triggers, never two code paths.
//
// Detach runs FIRST so a name being replaced within one revision transition is
// torn down before the attach pass considers it.
//
// A nil reattacher yields the detach-only behaviour byte-for-byte — the
// backward-compatible path a driver without an attach concrete gets.
//
// # Owner-scoped reconcile view (the fix for the process-global over-detach)
//
// The attached set the reconcile diffs against is the reconciling OWNER's
// runtime-added entries only — the (tenant, agent) owner stamped at attach.
// Boot-declared servers (untagged) and every OTHER owner's runtime-adds are
// NOT in the view, so a run for owner A can never detach a boot server or a
// tenant-B runtime-added connection, even though the underlying registry +
// catalog stay process-global and deployment-shared (resolution + dispatch
// stay bare-name; boot servers remain visible to every session). This is the
// per-owner reconcile VIEW the design intends — NOT a store re-key, and NOT an
// isolation key (agent_id is not an isolation principal). The owner is derived
// from the run's verified triple (tenant) + agentID; a zero owner yields an
// empty view (nothing to reconcile), so a reconcile without an owner detaches
// nothing rather than falling back to the whole registry. The bootDeclared set
// is retained as belt-and-suspenders (an owner's view already excludes boot
// servers by construction, but the explicit skip documents the invariant).
//
// # Honest in-flight semantics (read this before assuming isolation)
//
// This is a projection-boundary act: it fires at run START, never in the
// middle of the calling run, and EXPOSURE correctness is next-turn — a
// removed server never appears in any catalog view projected after the
// removal revision. Physical TEARDOWN is a separate, process-global effect:
// the catalog and MCP registry are shared across sessions, so a reconcile
// triggered by one session's run-start can deregister a source and close its
// transport while ANOTHER session's run is mid-flight and about to call it.
// That in-flight run's prompt snapshot is unchanged, but its NEXT call to the
// detached server fails LOUDLY — a typed catalog not-found at dispatch, or a
// closed-transport error from the driver — never a hang, a panic, or a
// silent success. This is the same failure class as an operator stopping a
// boot-declared server mid-run, and it is the deliberate trade: a
// refcount/drain protocol was rejected as complexity the removal semantics do
// not need.
//
// # Serialisation and idempotency
//
// Reconcile holds NO cross-run lock of its own: safety comes from the
// registry read being atomic, the detacher being idempotent (an
// already-detached source is a no-op), and each underlying primitive
// (catalog deregister, registry deregister, transport close) being
// internally synchronised. N concurrent reconciles converge to the same
// state; the concurrent tests pin this.
//
// # What the attach pass does NOT do
//
// It never initiates, completes, or re-drives an interactive consent flow, and
// it never mints, holds, refreshes, or exchanges a credential — because the
// attach path it calls has no token step at all: an `oauth_provider` binding is
// a NAME resolved against the boot-declared provider set, and the bearer is
// minted per outbound CALL, one layer later. A connection whose consent is
// genuinely gone therefore still re-attaches, and the shortfall surfaces on the
// first tool call as the shipped typed auth-required error routed onto the
// unified pause/resume primitive. Nothing here duplicates that path.
//
// It also never touches a boot-declared (yaml) server: boot servers carry the
// zero owner, are excluded from the owner view by construction, and are attached
// by the boot loader. The explicit bootDeclared skip documents the invariant on
// both passes.
//
// # Windows
//
// The earlier process-global over-detach — where one owner's reconcile
// enumerated the whole registry and could detach boot servers or another owner's
// runtime-adds — is CLOSED by the owner-scoped reconcile view above:
// AttachedSources returns only the reconciling owner's runtime-added entries.
// The reconcile-racing-a-concurrent-re-add window (a stale declared-set read
// detaching a freshly re-added server) is likewise closed on both sides now: it
// heals at the NEXT RUN START through the attach pass, and the attach pass's own
// same-shaped window is closed inside the reattacher concrete, which re-reads the
// live registry under its own whole-attach lock.
//
// A nil registry, an empty agentID, or a nil detacher returns (0, 0, nil) — the
// backward-compatible "no reconcile" path. A registry read error is returned
// wrapped in ErrReconcileRead (fail loud, never swallowed). Detach AND attach
// errors are joined and returned (the caller logs them loud) but do not abort
// the sweep: one server that refuses to close, or one unreachable third party,
// must not strand the others — and must not fail the run. ctx cancellation
// between attaches ends the pass with the ctx error joined, so an overall sweep
// budget bounds the pass without a per-connection escape.
//
// The identity triple + agentID form the owner-scoped reconcile view; the tenant
// + agent are the owner tag (agent_id is registration metadata here, not an
// isolation key — isolation stays the triple). Returns the number of servers
// detached and the number re-attached.
func ReconcileConnections(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, detacher ConnectionDetacher, reattacher ConnectionReattacher, bootDeclared map[string]struct{}) (detached, attached int, err error) {
	if reg == nil || agentID == "" || detacher == nil {
		return 0, 0, nil
	}
	rev, ok, rerr := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if rerr != nil {
		return 0, 0, fmt.Errorf("%w: agent %q: %w", ErrReconcileRead, agentID, rerr)
	}
	// The generic declared set carries the full DESCRIPTOR, not just the name:
	// the attach pass needs everything the generic attach lifecycle re-validates
	// (transport, url/command, the provider NAME binding, the injection mapping,
	// the annotation set, the discovery allowance). The detach keep-set is wider:
	// it also includes the immutable signed OAuth MCP pair, whose dedicated
	// reconciler owns reattachment and authority validation.
	declared := make(map[string]agentcfg.MCPConnectionDescriptor)
	keep := make(map[string]struct{})
	if ok {
		for _, d := range rev.Payload.ConnectionDescriptors() {
			declared[d.Name] = d
			keep[d.Name] = struct{}{}
		}
		pairs, pairErr := rev.Payload.EffectiveSignedOAuthMCPPairs()
		if pairErr != nil {
			return 0, 0, fmt.Errorf("%w: agent %q signed capability state: %w", ErrReconcileRead, agentID, pairErr)
		}
		for _, pair := range pairs {
			if pair.Connection.Name != "" {
				keep[pair.Connection.Name] = struct{}{}
			}
		}
	}
	// The owner-scoped reconcile view: the (tenant, agent) owner whose
	// runtime-added entries this run reconciles. AttachedSources returns ONLY
	// this owner's runtime-adds — never boot servers, never another owner's.
	owner := auth.Owner{Tenant: id.TenantID, Agent: agentID}
	var errs []error
	for _, src := range detacher.AttachedSources(ctx, owner) {
		if _, boot := bootDeclared[src]; boot {
			continue // defense-in-depth: the owner view already excludes boot servers.
		}
		if _, stillDeclared := keep[src]; stillDeclared {
			continue // still declared — keep it attached.
		}
		if derr := detacher.Detach(ctx, src, owner); derr != nil {
			errs = append(errs, fmt.Errorf("detach %q: %w", src, derr))
			continue
		}
		detached++
	}
	if reattacher == nil {
		// Detach-only: byte-for-byte the behaviour a driver with no attach
		// concrete gets.
		return detached, 0, errors.Join(errs...)
	}
	// The ATTACH pass considers every declared descriptor, including names that
	// are already live. Only the concrete owns the atomic owner+fingerprint read
	// needed to distinguish an exact no-op from a same-name descriptor change;
	// a name-only filter here would make URL/auth/annotation edits inert forever.
	// Deterministic order keeps a partially-cancelled sweep reproducible and the
	// tests can pin which connections were reached.
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// Honour cancellation between connections: the overall sweep budget must
		// be able to end the pass, and each attach is separately bounded inside
		// the concrete.
		if cerr := ctx.Err(); cerr != nil {
			errs = append(errs, &reattachFailure{name: name, err: fmt.Errorf("sweep ended early: %w", cerr)})
			break
		}
		if _, boot := bootDeclared[name]; boot {
			// A revision must never declare a boot-declared name (both write doors
			// refuse it), but the skip is belt-and-suspenders: the leg never spawns
			// or replaces a boot server.
			continue
		}
		changed, aerr := reattacher.Reattach(ctx, owner, id, declared[name])
		if aerr != nil {
			errs = append(errs, &reattachFailure{name: name, err: aerr})
			continue
		}
		if changed {
			attached++
		}
	}
	return detached, attached, errors.Join(errs...)
}

// DiscoveryOriginReconciler is the driver-agnostic seam the run-start
// allowance-reconcile leg uses to re-apply each of the reconciling owner's
// runtime-added connections' OAuth-discovery allow-list to the live MCP
// registry. Like [ConnectionDetacher] it is owner-scoped: AttachedSources
// returns ONLY the reconciling owner's runtime-adds (never boot servers, never
// another owner's). The concrete (wired at the cmd/harbor + devstack boundary)
// delegates to the process-global bare-name registry.
type DiscoveryOriginReconciler interface {
	// AttachedSources returns the reconciling owner's runtime-added source ids —
	// the owner-scoped reconcile VIEW (identical to ConnectionDetacher's).
	AttachedSources(ctx context.Context, owner auth.Owner) []string
	// SetOAuthDiscoveryOrigins FULL-REPLACES the named connection's allow-list on
	// the live registry (and prunes a now-unallowed recorded requirement),
	// returning the prior set. Identity-mandatory for authorization, and
	// OWNER-SCOPED for the write: owner is the reconciling (tenant, agent), and
	// the replacement lands only on a registration carrying that same owner tag,
	// so the re-apply stays inside the owner's own runtime-added set.
	SetOAuthDiscoveryOrigins(ctx context.Context, owner auth.Owner, name string, origins []string) (prev []string, err error)
}

// ReconcileDiscoveryOrigins is the ALLOWANCE-RECONCILE leg of run-start
// reconciliation — the rollback / set_revision path for the
// OAuth-discovery cross-origin allow-list. For each of the reconciling OWNER's
// runtime-added connections that the active revision still declares, it
// re-derives the connection's declared allow-list from the CURRENT revision and
// re-applies it to the live registry (a FULL IDEMPOTENT re-prune, not a
// rollback-delta). So a rollback past a grant REVOKES the origin live (the
// current revision no longer declares it), and any stale requirement record
// (e.g. from a revoke that landed mid-Discover) is corrected at the next run
// start — a bounded self-heal.
//
// It is owner-scoped exactly like [ReconcileConnections]: AttachedSources
// returns only the reconciling owner's runtime-adds, so a run for owner A never
// touches a boot server's or tenant-B's allow-list. A detach handled by
// ReconcileConnections removes the source from the owner view, so a
// no-longer-declared connection is not re-applied here (it is torn down there).
//
// A nil registry, an empty agentID, or a nil reconciler returns (0, nil) — the
// backward-compatible "no reconcile" path. A registry read error is returned
// wrapped in ErrReconcileRead (fail loud). Apply errors are joined and returned
// (logged loud by the caller) but do not abort the sweep. Returns the number of
// connections whose allow-list was re-applied.
func ReconcileDiscoveryOrigins(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, reconciler DiscoveryOriginReconciler) (int, error) {
	if reg == nil || agentID == "" || reconciler == nil {
		return 0, nil
	}
	rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return 0, fmt.Errorf("%w: agent %q: %w", ErrReconcileRead, agentID, err)
	}
	declared := make(map[string][]string)
	if ok {
		for _, d := range rev.Payload.ConnectionDescriptors() {
			declared[d.Name] = append([]string(nil), d.OAuthDiscoveryAllowedOrigins...)
		}
	}
	owner := auth.Owner{Tenant: id.TenantID, Agent: agentID}
	var applied int
	var errs []error
	for _, src := range reconciler.AttachedSources(ctx, owner) {
		origins, isDeclared := declared[src]
		if !isDeclared {
			continue // detach territory (ReconcileConnections) — not re-applied here.
		}
		if _, aerr := reconciler.SetOAuthDiscoveryOrigins(ctx, owner, src, origins); aerr != nil {
			errs = append(errs, fmt.Errorf("reapply allowance %q: %w", src, aerr))
			continue
		}
		applied++
	}
	return applied, errors.Join(errs...)
}

// OAuthProviderReconciler is the driver-agnostic seam the run-start
// provider-reconcile leg uses to make the reconciling owner's installed OAuth
// providers match its current active revision on the live owner-tagged provider
// set. Like [ConnectionDetacher] it is owner-scoped: InstalledFor returns ONLY
// the reconciling owner's installed providers (never boot-seeded, never another
// owner's). The concrete (wired at the cmd/harbor + devstack boundary) delegates
// to the process-global bare-name provider set.
type OAuthProviderReconciler interface {
	// InstalledFor returns the reconciling owner's installed provider names —
	// the owner-scoped reconcile VIEW.
	InstalledFor(ctx context.Context, owner auth.Owner) []string
	// InstallProvider installs (upserts) the descriptor owner-tagged (used when
	// a rollback FORWARD re-declares a provider that is not currently installed).
	// The (tenant, agentID) pair is the owner tuple (shared shape with the
	// agent-config service's ProviderInstaller so one concrete satisfies both).
	InstallProvider(ctx context.Context, tenant, agentID string, desc agentcfg.OAuthProviderDescriptor) error
	// UninstallProvider removes the named provider from the owner-tagged set and
	// CLOSES it (used when a rollback past an install no longer declares it). The
	// (tenant, agentID) pair is the reconciling owner; the set refuses a
	// cross-owner drop at its own boundary (defense in depth). The shared shape
	// with the agent-config service's ProviderInstaller lets one concrete satisfy
	// both.
	UninstallProvider(ctx context.Context, tenant, agentID, name string) error
}

// ReconcileOAuthProviders is the PROVIDER-RECONCILE leg of run-start
// reconciliation — the rollback / set_revision path for the Protocol-installed
// OAuth providers. For the reconciling OWNER it makes the live owner-tagged
// provider set match the CURRENT active revision's installed-provider section: a
// declared provider not currently installed is installed; an installed provider
// the current revision no longer declares is UNINSTALLED (and CLOSED). So a
// rollback past an install REVOKES the provider live, and a rollback of a
// removal re-installs it — one mechanism, N triggers (a full idempotent
// reconcile, not a rollback-delta).
//
// It is owner-scoped exactly like [ReconcileConnections]: InstalledFor returns
// only the reconciling owner's installed providers, so a run for owner A never
// touches a boot-seeded provider's or tenant-B's install — the cross-tenant
// uninstall/outage is closed by owner-scoping.
//
// A nil registry, an empty agentID, or a nil reconciler returns (0, nil). A
// registry read error is returned wrapped in ErrReconcileRead (fail loud).
// Apply errors are joined and returned (logged loud by the caller) but do not
// abort the sweep. Returns the number of providers whose install state changed.
func ReconcileOAuthProviders(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, reconciler OAuthProviderReconciler) (int, error) {
	if reg == nil || agentID == "" || reconciler == nil {
		return 0, nil
	}
	rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return 0, fmt.Errorf("%w: agent %q: %w", ErrReconcileRead, agentID, err)
	}
	declared := make(map[string]agentcfg.OAuthProviderDescriptor)
	if ok {
		for _, d := range rev.Payload.OAuthProviderDescriptors() {
			declared[d.Name] = d
		}
	}
	owner := auth.Owner{Tenant: id.TenantID, Agent: agentID}
	installed := make(map[string]struct{})
	var changed int
	var errs []error
	// Uninstall any installed-for-owner provider the current revision no longer
	// declares (rollback past an install).
	for _, name := range reconciler.InstalledFor(ctx, owner) {
		installed[name] = struct{}{}
		if _, stillDeclared := declared[name]; stillDeclared {
			continue
		}
		if uerr := reconciler.UninstallProvider(ctx, owner.Tenant, owner.Agent, name); uerr != nil {
			errs = append(errs, fmt.Errorf("uninstall provider %q: %w", name, uerr))
			continue
		}
		changed++
	}
	// Install any declared provider not currently installed (rollback of a
	// removal / a set_revision that re-declares it).
	for name, desc := range declared {
		if _, isInstalled := installed[name]; isInstalled {
			continue
		}
		if ierr := reconciler.InstallProvider(ctx, owner.Tenant, owner.Agent, desc); ierr != nil {
			errs = append(errs, fmt.Errorf("install provider %q: %w", name, ierr))
			continue
		}
		changed++
	}
	return changed, errors.Join(errs...)
}

// ActiveSkillViews applies an agent's active-config skills membership to the
// run's skill-directory views at run start. It resolves the agent's active
// revision once and, when the revision pins a skills section, keeps only the
// views whose name is in the membership set. A nil registry, an empty
// agentID, or an agent with no active revision / no skills section returns
// the views unchanged — the backward-compatible "ungated" path. A registry
// read error is returned so the caller fails the run loudly (CLAUDE.md §13):
// no silent fall-through to the unfiltered view on a read failure.
//
// The active revision is read ONCE per run; the returned slice is fresh, so
// concurrent / in-flight runs keep their own snapshot (the concurrent-reuse
// contract). `id` carries the run's identity; only the triple is used (the
// registry is identity-scoped, never keyed by run).
func ActiveSkillViews(ctx context.Context, reg agentcfg.Registry, ov sessionoverlay.Store, agentID string, id identity.Quadruple, views []skills.SkillView) ([]skills.SkillView, error) {
	// Resolve the session's ephemeral personal-skill names FIRST: they are a
	// safe-subset ADD (the session's own session-scoped skills, never a
	// capability the admin restricts) that survive the admin membership
	// filter below. They never promote past the session (the overlay + the
	// SkillStore are both session-keyed).
	overlay, oerr := loadOverlay(ctx, ov, agentID, id)
	if oerr != nil {
		return nil, oerr
	}
	personal := overlay.PersonalSkills

	if reg == nil || agentID == "" {
		return views, nil
	}
	rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return nil, err
	}
	if !ok || rev.Payload.Skills == nil {
		// The admin pins no skills membership: every directory-visible skill
		// (which already includes the session's personal skills, stored under
		// the session triple) is in scope.
		return views, nil
	}
	// The admin pinned a membership. An ADMIN-pinned name whose body is ABSENT
	// from the directory view is a LOUD failure (the plan's "skill not found
	// in store", §13) — a rollback onto a since-hard-deleted skill must fail
	// the run, not quietly run without a skill the admin expects. (Session
	// PERSONAL names stay silent below — a safe-subset add that may
	// legitimately not be in the view.)
	present := make(map[string]struct{}, len(views))
	for _, v := range views {
		present[v.Name] = struct{}{}
	}
	for _, name := range rev.Payload.Skills.Names {
		if _, ok := present[name]; !ok {
			return nil, fmt.Errorf("%w: agent %q pins skill %q", ErrSkillBodyMissing, agentID, name)
		}
	}
	// The durable user-scope personal-skill membership: a safe-subset ADD that
	// survives the admin membership filter (the durable analogue of the
	// session's ephemeral personal skills). Read from the caller's active
	// USER-scope config revision — the same ConfigScopeUser arm the
	// tool-exposure and user-prompt projections read. Their BODIES already
	// resolved into `views` because the SkillStore returns ScopeUser rows for
	// every session of the (tenant, user); this keeps them visible when the
	// admin pins a membership.
	durableUser, uerr := activeDurableUserSkillNames(ctx, reg, agentID, id)
	if uerr != nil {
		return nil, uerr
	}
	// Keep the admin members AND add back BOTH the session's ephemeral personal
	// skills and the durable user-scope personal skills (the user + session
	// safe-subset adds ON TOP of the admin baseline). A name absent from views
	// is harmless — FilterSkillViewsByMembership keeps only names present in
	// views.
	allowed := append(append(append([]string(nil), rev.Payload.Skills.Names...), personal...), durableUser...)
	return FilterSkillViewsByMembership(views, allowed), nil
}

// ActiveSessionSkillMembership captures the selected agent's complete
// run-start skill-membership authority. The AGENT scope is read exactly once;
// a present Skills section (including an explicitly empty Names slice) sets
// AdminMembershipSet, while an absent revision/section leaves the base view
// ungated. A present AgentPacks section contributes the fully-converted
// operator pack skills (HA-55) — the pack rides the same immutable revision
// as the membership, so body + membership can never dangle apart. The exact
// USER scope is then read exactly once and contributes only
// its durable personal-skill names. Returned slices are fresh copies suitable
// for binding into an immutable per-run resolver.
func ActiveSessionSkillMembership(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple) (sessionoverlay.SessionSkillMembership, error) {
	var membership sessionoverlay.SessionSkillMembership
	if reg == nil || agentID == "" {
		return membership, nil
	}

	agentRevision, found, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return sessionoverlay.SessionSkillMembership{}, err
	}
	if found && agentRevision.Payload.Skills != nil {
		membership.AdminMembershipSet = true
		membership.AdminNames = append([]string(nil), agentRevision.Payload.Skills.Names...)
	}
	if found && agentRevision.Payload.AgentPacks != nil {
		packs, perr := skills.PackItemsToSkills(agentRevision.Payload.AgentPacks)
		if perr != nil {
			// A malformed pack body in an active revision is a loud
			// run-start failure (CLAUDE.md §13): the pack is operator
			// authority and must never be silently dropped or partially
			// composed.
			return sessionoverlay.SessionSkillMembership{}, fmt.Errorf("agent %q: %w", agentID, perr)
		}
		membership.Packs = packs
	}

	userRevision, found, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeUser)
	if err != nil {
		return sessionoverlay.SessionSkillMembership{}, err
	}
	if found && userRevision.Payload.Skills != nil {
		membership.UserPersonalNames = append([]string(nil), userRevision.Payload.Skills.Names...)
	}
	return membership, nil
}

// activeDurableUserSkillNames resolves the caller's active USER-scope durable
// config revision and returns its skills-membership names — the durable
// per-user personal-skill set. It keys by the run's identity triple with
// agent_id as the per-agent key (the USER config scope), so the real
// (tenant, user) is the isolation principal and the tuple is never widened.
// nil registry / empty agentID / no active user revision / a revision with no
// skills section yields nil (the backward-compatible "no durable user skills"
// path). A registry read error is returned so the caller fails the run loudly
// — never a silent drop (CLAUDE.md §13). Mirrors activeDurableUserPrompt.
func activeDurableUserSkillNames(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple) ([]string, error) {
	if reg == nil || agentID == "" {
		return nil, nil
	}
	rev, found, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeUser)
	if err != nil {
		return nil, err
	}
	if !found || rev.Payload.Skills == nil {
		return nil, nil
	}
	return rev.Payload.Skills.Names, nil
}

// ActiveLLMOverrides resolves the PER-AGENT LLM-parameter override layer from
// the agent's active config revision at run start. It returns the per-agent
// sampling defaults (model / temperature / max-tokens / reasoning-effort) the
// agent has pinned, as a [planner.LLMOverrides] carrying ONLY those four
// dimensions — the layer the run loop folds BETWEEN the session override and
// the tenant-wide baseline (precedence session › per-agent › tenant-wide
// baseline › config default).
//
// A nil registry, an empty agentID, an agent with no active revision, or an
// active revision with no LLM-params section returns (nil, nil) — the
// backward-compatible "no per-agent override" path. A registry read error is
// returned so the caller fails the run loudly (CLAUDE.md §13): no silent
// fall-through to the tenant baseline on a read failure.
//
// The active revision is read ONCE per run; the returned bundle is fresh
// (its pointers are copies), so concurrent / in-flight runs keep their own
// snapshot (the concurrent-reuse contract). `id` carries the run's identity;
// only the triple is used (the registry is identity-scoped, never keyed by
// run). This is sampling parameters only — ExtraInstructions / prompt layers
// are resolved elsewhere (the agent-config prompt-layer projection), so the
// per-agent LLM layer never carries prompt text.
func ActiveLLMOverrides(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple) (*planner.LLMOverrides, error) {
	if reg == nil || agentID == "" {
		return nil, nil
	}
	rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	lp, set := rev.Payload.LLMParamsView()
	if !set {
		return nil, nil
	}
	// Copy each pointer so the returned bundle never shares backing storage
	// with the (immutable) stored revision.
	out := &planner.LLMOverrides{}
	any := false
	if lp.Model != nil && *lp.Model != "" {
		v := *lp.Model
		out.Model = &v
		any = true
	}
	if lp.Temperature != nil {
		v := *lp.Temperature
		out.Temperature = &v
		any = true
	}
	if lp.MaxTokens != nil {
		v := *lp.MaxTokens
		out.MaxTokens = &v
		any = true
	}
	if lp.ReasoningEffort != nil && *lp.ReasoningEffort != "" {
		v := *lp.ReasoningEffort
		out.ReasoningEffort = &v
		any = true
	}
	if !any {
		return nil, nil
	}
	return out, nil
}

// RunCompletionHookFromConfig projects the static
// `runtime.hooks.run_completion` yaml block onto a
// steering.CompletionHookSpec, or nil when no static hook is configured (an
// empty / whitespace-only tool). It is the ONE yaml half of the hook
// resolution, shared by every run-loop driver (the production dev binary,
// the devstack twin, and the embed RunOnce path) so the yaml projection
// cannot drift between binaries. AgentID is left empty — the run-start
// resolution stamps the acting agent id; timeout defaulting happens at fire
// time.
func RunCompletionHookFromConfig(rc config.RunCompletionHookConfig) *steering.CompletionHookSpec {
	if strings.TrimSpace(rc.Tool) == "" {
		return nil
	}
	return &steering.CompletionHookSpec{Tool: rc.Tool, Timeout: rc.Timeout}
}

// ActiveRunCompletionHook resolves the run-completion hook for a run at run
// start with next-run projection semantics. Resolution precedence is
// pinned here (and by a table test — CLAUDE.md §17.6): the agent-config
// `hooks` section (when PRESENT) over the static yaml default over no hook.
// The two run-loop drivers (the production dev driver and the harbortest
// devstack twin) call this ONE helper, so the precedence cannot drift
// between binaries.
//
// A PRESENT hooks section is authoritative (mirrors the naming
// section): a run-completion hook with a non-empty tool pins it, while a
// present section with no/empty run-completion tool is an explicit per-agent
// NO-HOOK that WINS over yamlDefault (returns (nil, false)). Only an ABSENT
// (nil) hooks section falls through to yamlDefault — otherwise a per-agent
// opt-out of transcript egress would be silently discarded.
//
// yamlDefault is the operator's static `runtime.hooks.run_completion`
// projection (nil when unset). The returned spec's AgentID is stamped from
// agentID (registration metadata, never an isolation key — §6). A nil
// registry, an empty agentID, or an agent with no active revision falls
// through to yamlDefault. A registry read error is returned so the caller
// fails the run loudly (CLAUDE.md §13): no silent fall-through to yaml on a
// read failure.
//
// The active revision is read ONCE per run; the returned spec is fresh, so
// concurrent / in-flight runs keep their own snapshot (the concurrent-reuse
// contract). Only the identity triple is used (the registry is
// identity-scoped, never keyed by run).
func ActiveRunCompletionHook(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, yamlDefault *steering.CompletionHookSpec) (*steering.CompletionHookSpec, bool, error) {
	if reg != nil && agentID != "" {
		rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
		if err != nil {
			return nil, false, err
		}
		if ok && rev.Payload.Hooks != nil {
			// A PRESENT hooks section is authoritative (mirrors the naming
			// section): a run-completion hook with a non-empty tool pins
			// it, and a present section with no/empty run-completion tool is the
			// explicit per-agent NO-HOOK that WINS over the yaml default (returns
			// (nil, false) rather than falling through — otherwise a per-agent
			// opt-out of transcript egress would be silently discarded and the
			// yaml hook would keep dispatching).
			if rc, set := rev.Payload.RunCompletionHookView(); set && strings.TrimSpace(rc.Tool) != "" {
				spec := &steering.CompletionHookSpec{
					Tool:    rc.Tool,
					Timeout: time.Duration(rc.TimeoutMS) * time.Millisecond,
					AgentID: agentID,
				}
				return spec, true, nil
			}
			return nil, false, nil
		}
	}
	// Fall through to the static yaml default (when set).
	if yamlDefault != nil && yamlDefault.Tool != "" {
		// Copy so the caller cannot mutate the shared boot-time default, and
		// stamp the acting agent id (the boot default knows no agent).
		spec := &steering.CompletionHookSpec{
			Tool:    yamlDefault.Tool,
			Timeout: yamlDefault.Timeout,
			AgentID: agentID,
		}
		return spec, true, nil
	}
	return nil, false, nil
}

// NamingResolution is the resolved session auto-naming policy for a run: the
// defaulted [steering.NamingPolicy] plus the model the naming call should
// request (empty = the run's effective model / the client default). Returned by
// [ActiveNamingPolicy] when a policy is active for the run.
type NamingResolution struct {
	Policy steering.NamingPolicy
	Model  string
}

// ActiveNamingPolicy resolves the session auto-naming policy for a run at run
// start with next-run projection semantics. Resolution precedence is
// pinned here (and by a table test — CLAUDE.md §17.6): the agent-config
// `naming` section (when present) over the static yaml `runtime.naming` fleet
// default over off. The two run-loop drivers (production dev + devstack twin)
// call this ONE helper, so the precedence cannot drift between binaries.
//
// A section is "active" only when its Auto is true — an agent-config section
// present with Auto false is an explicit per-agent off that WINS over the yaml
// default (returns active=false), and a config-free runtime resolves to off, so
// the opt-in invariant holds (no counters, no LLM calls, no events). The
// returned policy is defaulted (WithDefaults) so the trigger consumes concrete
// values.
//
// A nil registry, an empty agentID, or an agent with no active revision / no
// naming section falls through to the yaml default. A registry read error is
// returned so the caller fails the run loudly (CLAUDE.md §13): no silent
// fall-through on a read failure. The active revision is read ONCE per run; the
// returned value is fresh (concurrent-reuse clean). Only the identity triple is
// used (the registry is identity-scoped, never keyed by run).
func ActiveNamingPolicy(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, yamlDefault config.RuntimeNamingConfig) (NamingResolution, bool, error) {
	if reg != nil && agentID != "" {
		rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
		if err != nil {
			return NamingResolution{}, false, err
		}
		if ok {
			if n, set := rev.Payload.NamingView(); set {
				// The agent-config section WINS (present overrides yaml). Auto
				// false is an explicit per-agent off.
				if !n.Auto {
					return NamingResolution{}, false, nil
				}
				return NamingResolution{
					Policy: steering.NamingPolicy{
						AfterTurns:     n.AfterTurns,
						RepeatEvery:    n.RepeatEvery,
						MaxRepetitions: n.MaxRepetitions,
						MaxTitleLen:    n.MaxTitleLen,
						ReasoningMode:  steering.NamingReasoningMode(n.ReasoningMode),
					}.WithDefaults(),
					Model: n.Model,
				}, true, nil
			}
		}
	}
	// Fall through to the static yaml default (when it enables naming).
	if yamlDefault.Auto {
		return NamingResolution{
			Policy: steering.NamingPolicy{
				AfterTurns:     yamlDefault.AfterTurns,
				RepeatEvery:    yamlDefault.RepeatEvery,
				MaxRepetitions: yamlDefault.MaxRepetitions,
				MaxTitleLen:    yamlDefault.MaxTitleLen,
				ReasoningMode:  steering.NamingReasoningMode(yamlDefault.ReasoningMode),
			}.WithDefaults(),
			Model: yamlDefault.Model,
		}, true, nil
	}
	return NamingResolution{}, false, nil
}

// ActivePlannerCatalogView builds the run's planner-facing catalog view at
// run start, applying the agent's active-config tool exposure: a paused MCP
// server's tools and any individually-disabled tool are excluded from the
// view (next-turn projection — the live transport stays WARM). Loading-mode
// overrides compose in authority order: operator baseline, then the acting
// user's durable choices; the session overlay remains narrow-only. The
// exclusion set is the order-independent union of THREE narrow-only disable
// tiers — the admin baseline, the durable per-user disable set (which spans
// that user's sessions for the agent), and the ephemeral session overlay — so
// it can only ever grow: no tier can re-expose a tool a higher tier disabled.
// It always
// returns a usable view: a nil registry, an empty agentID, an agent with no
// active revision, or an active revision with no tool-exposure section (or an
// empty one) returns the plain [tools.NewPlannerView] over cat+filter — the
// backward-compatible "ungated" path. A registry read error is returned so
// the caller fails the run loudly (CLAUDE.md §13): no silent fall-through to
// the unfiltered view on a read failure.
//
// The active revision is read ONCE per run; the returned view is fresh, so
// concurrent / in-flight runs keep their own snapshot (the concurrent-reuse
// contract). Only the identity triple is used (the registry is
// identity-scoped, never keyed by run).
func ActivePlannerCatalogView(ctx context.Context, reg agentcfg.Registry, ov sessionoverlay.Store, agentID string, id identity.Quadruple, cat tools.ToolCatalog, filter tools.CatalogFilter, ownerResolvers ...SourceOwnerResolver) (tools.PlannerCatalogView, error) {
	// Admin exposure (the baseline). Loading-mode overrides are composed below
	// with the durable user tier; the ephemeral session tier remains
	// narrow-only and contributes disable sets only.
	var adminPaused, adminDisabled []string
	var adminToolExposure *agentcfg.ToolExposure
	if reg != nil && agentID != "" {
		rev, ok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
		if err != nil {
			return nil, err
		}
		if ok && rev.Payload.ToolExposure != nil {
			adminPaused = rev.Payload.PausedServers()
			adminDisabled = rev.Payload.DisabledTools()
			adminToolExposure = rev.Payload.ToolExposure
		}
	}

	// Durable user-scope exposure: the per-user narrow-only disable set that
	// spans the user's sessions for this agent. The run's full triple is
	// passed; the user-scope storage key (session + run zeroed, agent_id in the
	// session slot) is derived INSIDE the registry — agent_id is a per-agent
	// key here, NEVER an isolation filter, so isolation stays the run's
	// (tenant, user). A read error fails the run loudly (an incomplete triple
	// surfaces identity_required); a missing active user revision is the
	// ungated path. The set is narrow-only — there is no user enable field.
	var userPaused, userDisabled []string
	var userToolExposure *agentcfg.ToolExposure
	userPairNames := make(map[string]struct{})
	if reg != nil && agentID != "" {
		urev, uok, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeUser)
		if err != nil {
			return nil, err
		}
		if uok && urev.Payload.ToolExposure != nil {
			userPaused = urev.Payload.PausedServers()
			userDisabled = urev.Payload.DisabledTools()
			userToolExposure = urev.Payload.ToolExposure
		}
		if uok {
			pairs, pairErr := urev.Payload.EffectiveSignedOAuthMCPPairs()
			if pairErr != nil {
				return nil, pairErr
			}
			for _, pair := range pairs {
				if pair.Connection.Name != "" {
					userPairNames[pair.Connection.Name] = struct{}{}
				}
			}
		}
	}
	var ownerResolver SourceOwnerResolver
	if len(ownerResolvers) > 0 {
		ownerResolver = ownerResolvers[0]
	}
	actingOwner := auth.Owner{Tenant: id.TenantID, Agent: agentID, User: id.UserID}
	userLoadingExposure := physicalizeUserLoadingModes(cat, filter, ownerResolver, actingOwner, userToolExposure)

	// Loading-mode override projection composes the operator baseline first,
	// then the acting user's durable choices. The inner view is rebuilt over a
	// BROADENED filter (both loading modes) whenever either tier carries an
	// override, so each layer can see the complete catalog before the final
	// prompt-time filter is applied. The effective map is resolved once per run
	// from an immutable view snapshot; the session tier has no loading-mode
	// write path and can only narrow through the disable union below.
	base := composeLoadingView(cat, filter, adminToolExposure, userLoadingExposure)

	// Session overlay (narrow-only): the session's disable set is UNIONED into
	// the exclusion set — it can only ADD to the disabled set, never remove an
	// admin or user exclusion. The exclusion set can only ever GROW, so a
	// session edit can only narrow the allowed exposure, never widen it. A
	// session "disable" of a source not in the catalog view is inert, and there
	// is no session "enable" path at all.
	overlay, oerr := loadOverlay(ctx, ov, agentID, id)
	if oerr != nil {
		return nil, oerr
	}

	// The three disable sets — admin, user, session — are UNIONED into one
	// grow-only exclusion set. unionSorted is commutative and idempotent, so
	// admin ∪ user ∪ session is order-independent: there is no precedence for
	// tool exposure, only narrowing. Neither the user nor the session tier can
	// re-widen past the admin-provisioned palette.
	paused := unionSorted(unionSorted(adminPaused, userPaused), overlay.DisabledServers)
	disabled := unionSorted(unionSorted(adminDisabled, userDisabled), overlay.DisabledTools)
	if ownerResolver != nil && id.UserID != "" {
		userPaused, userDisabled = physicalizeUserExposure(base, ownerResolver, actingOwner, userPaused, userDisabled)
		paused = unionSorted(unionSorted(adminPaused, userPaused), overlay.DisabledServers)
		disabled = unionSorted(unionSorted(adminDisabled, userDisabled), overlay.DisabledTools)
		base = userScopedMCPView{base: base, resolver: ownerResolver, owner: actingOwner, desiredPair: userPairNames}
	}
	if len(paused) == 0 && len(disabled) == 0 {
		return base, nil
	}
	return tools.NewExclusionView(base, paused, disabled), nil
}

// ToolExposedAtCurrentRevision reports whether the named catalog tool is in
// the same effective exposure view a newly-started run would receive for the
// acting identity. It deliberately delegates to [ActivePlannerCatalogView]
// instead of re-reading the admin, user, and session tiers here: the durable
// ConfigScopeUser revision, logical-to-physical source translation, and
// narrow-only disable union therefore have one implementation for planner
// discovery and late app callbacks alike.
//
// The caller supplies a catalog containing the current dispatch descriptor
// and should pass the same source-owner resolver used by runtime MCP
// registrations. Both loading modes are included in the intermediate view so
// loading preferences do not turn a callback authorization check into a
// prompt-presence check. A nil registry or empty agent id preserves the
// existing inert-gate behavior and reports exposed.
func ToolExposedAtCurrentRevision(ctx context.Context, reg agentcfg.Registry, ov sessionoverlay.Store, agentID string, id identity.Quadruple, cat tools.ToolCatalog, tool tools.Tool, ownerResolvers ...SourceOwnerResolver) (bool, error) {
	if reg == nil || agentID == "" {
		return true, nil
	}
	// App-only callbacks are intentionally absent from the ordinary planner
	// catalog. Include the already-resolved callback as a read-only target in
	// the temporary projection so user logical-to-physical translation and the
	// exclusion view still apply to that callback. It never escapes this
	// predicate into planner discovery or catalog registration.
	cat = exposureTargetCatalog{ToolCatalog: cat, target: tool}
	filter := tools.CatalogFilter{
		TenantID:     id.TenantID,
		UserID:       id.UserID,
		SessionID:    id.SessionID,
		LoadingModes: []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred},
	}
	view, err := ActivePlannerCatalogView(ctx, reg, ov, agentID, id, cat, filter, ownerResolvers...)
	if err != nil {
		return false, err
	}
	resolved, ok := view.Resolve(tool.Name)
	if !ok {
		return false, nil
	}
	// The descriptor is resolved before this check by the Apps accessor. A
	// source mismatch means the catalog changed between those operations, so
	// do not let a stale descriptor cross the exposure boundary.
	return resolved.Source == tool.Source, nil
}

// exposureTargetCatalog is the narrow adapter used by
// ToolExposedAtCurrentRevision for app-only callbacks, which do not belong to
// the planner catalog. It delegates every catalog operation except the two
// planner-view reads, and adds only the target descriptor to those reads when
// the ordinary catalog does not already contain that name. The target is
// never registered or made globally discoverable.
type exposureTargetCatalog struct {
	tools.ToolCatalog
	target tools.Tool
}

func (c exposureTargetCatalog) Resolve(name string) (tools.ToolDescriptor, bool) {
	if desc, ok := c.ToolCatalog.Resolve(name); ok {
		return desc, true
	}
	if name == c.target.Name {
		return tools.ToolDescriptor{Tool: c.target}, true
	}
	return tools.ToolDescriptor{}, false
}

func (c exposureTargetCatalog) List(filter tools.CatalogFilter) []tools.Tool {
	out := c.ToolCatalog.List(filter)
	for _, existing := range out {
		if existing.Name == c.target.Name {
			return out
		}
	}
	// ActivePlannerCatalogView always broadens loading-mode filtering before
	// it uses List for the exposure translation. App callbacks have already
	// passed source/identity admission; retaining the target here is therefore
	// safe and lets the canonical projection apply all user/admin/session
	// exclusion semantics to it.
	out = append(out, c.target)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// resolveEffectiveLoading applies the top two layers of the loading-mode
// precedence order (tool_loading_modes[name] > server_loading_modes
// [source], the latter restricted to TOOL-form descriptors) to one catalog
// tool. Returns (mode, true) when an override applies; (_, false) when
// neither map names this tool — the caller keeps the boot-effective mode
// (the bottom two precedence layers, already baked into t.Loading at boot).
// This is the ONE canonical implementation shared by the run-start
// projection and the tools.describe effective-mode read surface, so the
// two seams cannot drift (CLAUDE.md §17.6).
func resolveEffectiveLoading(te *agentcfg.ToolExposure, t tools.Tool) (tools.LoadingMode, bool) {
	if te == nil {
		return "", false
	}
	if mode, ok := te.ToolLoadingModes[t.Name]; ok {
		return tools.LoadingMode(mode), true
	}
	if t.Form == tools.ToolFormTool && t.Source != "" {
		if mode, ok := te.ServerLoadingModes[string(t.Source)]; ok {
			return tools.LoadingMode(mode), true
		}
	}
	return "", false
}

// buildEffectiveLoading resolves the loading-mode override for every tool in
// list, returning a map keyed by catalog Name containing ONLY the entries an
// override actually changes (a tool with no applicable override is omitted
// — [tools.LoadingOverrideView] falls back to the tool's own boot-effective
// Loading for an absent key). A nil ToolExposure section returns nil.
func buildEffectiveLoading(list []tools.Tool, te *agentcfg.ToolExposure) map[string]tools.LoadingMode {
	if te == nil {
		return nil
	}
	out := make(map[string]tools.LoadingMode, len(list))
	for _, t := range list {
		if mode, ok := resolveEffectiveLoading(te, t); ok {
			out[t.Name] = mode
		}
	}
	return out
}

func hasLoadingOverrides(te *agentcfg.ToolExposure) bool {
	return te != nil && (len(te.ServerLoadingModes) > 0 || len(te.ToolLoadingModes) > 0)
}

// composeLoadingView applies the loading-mode layers in authority order:
// boot-effective catalog, operator/agent revision, then acting-user
// revision. User modes are not an enable set — they operate on the same
// operator-ceiling catalog and only change prompt-time presence. The helper
// deliberately keeps intermediate layers broad so a lower layer can promote
// a descriptor the preceding layer demoted; only the final view applies the
// caller's requested visible modes.
func composeLoadingView(cat tools.ToolCatalog, filter tools.CatalogFilter, admin, user *agentcfg.ToolExposure) tools.PlannerCatalogView {
	adminOverrides := hasLoadingOverrides(admin)
	userOverrides := hasLoadingOverrides(user)
	if !adminOverrides && !userOverrides {
		return tools.NewPlannerView(cat, filter)
	}

	broadFilter := filter
	broadFilter.LoadingModes = []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred}
	var view tools.PlannerCatalogView = tools.NewPlannerView(cat, broadFilter)
	if adminOverrides {
		visible := []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred}
		if !userOverrides {
			visible = filter.LoadingModes
		}
		view = tools.NewLoadingOverrideView(view, buildEffectiveLoading(view.List(), admin), visible)
	}
	if userOverrides {
		view = tools.NewLoadingOverrideView(view, buildEffectiveLoading(view.List(), user), filter.LoadingModes)
	}
	return view
}

// EffectiveLoadingMode resolves tool t's projected EFFECTIVE LoadingMode
// under agentID's operator then acting-user active config revisions — the
// SAME precedence [ActivePlannerCatalogView] applies at run start, exposed
// for the `tools.describe` read surface's optional `agent_id` path. boot is
// the boot-effective mode (already reflecting the driver default + boot
// config); a nil registry, an empty agentID, or no relevant revisions returns
// boot unchanged — the backward-compatible path byte-identical to
// `tools.describe` behaviour before this projection existed. A registry read
// error is returned so the caller fails the request loudly (CLAUDE.md §13).
func EffectiveLoadingMode(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple, t tools.Tool, boot tools.LoadingMode, ownerResolvers ...SourceOwnerResolver) (tools.LoadingMode, error) {
	if reg == nil || agentID == "" {
		return boot, nil
	}
	q := identity.Quadruple{Identity: id.Identity}
	admin, adminOK, err := reg.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return "", err
	}
	user, userOK, err := reg.Active(ctx, q, agentID, agentcfg.ConfigScopeUser)
	if err != nil {
		return "", err
	}
	mode := boot
	if adminOK {
		if resolved, ok := resolveEffectiveLoading(admin.Payload.ToolExposure, t); ok {
			mode = resolved
		}
	}
	if userOK {
		var ownerResolver SourceOwnerResolver
		if len(ownerResolvers) > 0 {
			ownerResolver = ownerResolvers[0]
		}
		userTool, allowed := userLoadingTool(t, ownerResolver, auth.Owner{Tenant: id.TenantID, Agent: agentID, User: id.UserID})
		if allowed {
			if resolved, ok := resolveEffectiveLoading(user.Payload.ToolExposure, userTool); ok {
				mode = resolved
			}
		}
	}
	return mode, nil
}

// CatalogViewResolver adapts the canonical run-start projection to the
// Tools Protocol's per-request catalog-view seam. It is immutable after
// construction and safe for concurrent reuse. A blank agentID intentionally
// builds an agent-less view: operator/boot tools remain compatible, while a
// configured SourceOwnerResolver makes private user sources fail closed until
// a request names the effective agent that owns the revision.
type CatalogViewResolver struct {
	Registry       agentcfg.Registry
	SessionOverlay sessionoverlay.Store
	Catalog        tools.ToolCatalog
	OwnerResolver  SourceOwnerResolver
}

// CatalogView implements the structurally identical
// internal/tools/protocol.CatalogViewResolver seam without importing the
// Tools Protocol package. Every request is projected through the same
// ActivePlannerCatalogView used at run start; this adapter is the assembly
// boundary that keeps the runtime agent-config registry out of the Protocol
// package's dependency graph.
func (a CatalogViewResolver) CatalogView(ctx context.Context, id identity.Identity, agentID string) (tools.PlannerCatalogView, error) {
	if a.Catalog == nil {
		return nil, errors.New("agentcfg/projection: catalog view requires a non-nil tool catalog")
	}
	filter := tools.CatalogFilter{
		TenantID:     id.TenantID,
		UserID:       id.UserID,
		SessionID:    id.SessionID,
		LoadingModes: []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred},
	}
	if a.OwnerResolver == nil {
		return ActivePlannerCatalogView(ctx, a.Registry, a.SessionOverlay, agentID,
			identity.Quadruple{Identity: id}, a.Catalog, filter)
	}
	return ActivePlannerCatalogView(ctx, a.Registry, a.SessionOverlay, agentID,
		identity.Quadruple{Identity: id}, a.Catalog, filter, a.OwnerResolver)
}

// LoadingResolverAdapter adapts a Registry into the `internal/tools/protocol`
// package's narrow LoadingResolver seam (the `tools.describe` optional
// `agent_id` path) via [EffectiveLoadingMode], so the SAME projection
// precedence backs both the run-start prompt-time projection and the read
// surface — one shared helper, no binary-local reimplementation (CLAUDE.md
// §17.6). Satisfies `tools/protocol.LoadingResolver` structurally; this
// package does not import `tools/protocol` to avoid a needless dependency
// (Go interfaces are satisfied structurally).
type LoadingResolverAdapter struct {
	Registry      agentcfg.Registry
	OwnerResolver SourceOwnerResolver
}

// EffectiveLoading implements the `tools/protocol.LoadingResolver` seam.
func (a LoadingResolverAdapter) EffectiveLoading(ctx context.Context, id identity.Identity, agentID string, t tools.Tool, boot tools.LoadingMode) (tools.LoadingMode, error) {
	return EffectiveLoadingMode(ctx, a.Registry, agentID, identity.Quadruple{Identity: id}, t, boot, a.OwnerResolver)
}

// ActivePromptLayers resolves the agent's active-config layered system
// prompt at run start. It returns the base + user layer text and whether the
// active revision carries a prompt-layer section (ok). A nil registry, an
// empty agentID, an agent with no active revision, or an active revision with
// no prompt-layer section returns ("", "", false, nil) — the
// backward-compatible "no durable prompt layers" path (the run keeps its
// configured base). A registry read error is returned so the caller fails the
// run loudly (CLAUDE.md §13): no silent fall-through on a read failure.
//
// An unset layer within a present section returns the empty string for that
// layer (the caller treats empty as "inherit the configured default" for the
// base, and "no user layer" for the user layer).
//
// The active revision is read ONCE per run; the returned values are plain
// strings, so concurrent / in-flight runs keep their own snapshot (the
// concurrent-reuse contract). Only the identity triple is used (the registry
// is identity-scoped, never keyed by run).
func ActivePromptLayers(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple) (base string, user string, ok bool, err error) {
	if reg == nil || agentID == "" {
		return "", "", false, nil
	}
	rev, found, rerr := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if rerr != nil {
		return "", "", false, rerr
	}
	if !found || rev.Payload.PromptLayers == nil {
		return "", "", false, nil
	}
	base, _ = rev.Payload.BasePrompt()
	user, _ = rev.Payload.UserPrompt()
	return base, user, true, nil
}

// ActiveExtraSystemBlocks resolves the agent's active-config ORDERED
// additive prompt blocks at run start. It returns the blocks in their
// declared order and whether the active revision carries a blocks section
// (ok). A nil registry, an empty agentID, an agent with no active revision,
// or an active revision with no blocks section returns (nil, false, nil) —
// the backward-compatible "no durable blocks" path, which contributes
// nothing to the prompt. A registry read error is returned so the caller
// fails the run loudly (CLAUDE.md §13): no silent fall-through.
//
// The returned slice is a fresh copy, so the run's snapshot cannot be
// mutated through the registry's retained payload and vice versa. The
// active revision is read ONCE per run; concurrent / in-flight runs keep
// their own snapshot (the concurrent-reuse contract). Only the identity
// triple is used (the registry is identity-scoped, never keyed by run) —
// agent_id is a KEY, never an isolation filter (CLAUDE.md §6).
func ActiveExtraSystemBlocks(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple) ([]agentcfg.NamedBlock, bool, error) {
	if reg == nil || agentID == "" {
		return nil, false, nil
	}
	rev, found, rerr := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if rerr != nil {
		return nil, false, rerr
	}
	if !found || rev.Payload.ExtraSystemBlocks == nil {
		return nil, false, nil
	}
	blocks := rev.Payload.ExtraSystemBlockList()
	if len(blocks) == 0 {
		return nil, false, nil
	}
	// Positional copy — the declared order IS the render order.
	out := make([]agentcfg.NamedBlock, len(blocks))
	copy(out, blocks)
	return out, true, nil
}

// ApplyPromptLayers overlays the agent's active-config durable prompt layers
// onto the run's resolved per-run override bundle at run start. It is the
// ONE shared seam both run-loop drivers (the production dev driver and the
// harbortest devstack twin) call after resolving the LLM-parameter overrides,
// so the two binaries cannot drift (CLAUDE.md §17.6).
//
// A non-empty base layer is set as ov.BasePromptLayer (overriding the run's
// configured base at the prompt builder); a non-empty user layer is set as
// ov.UserPromptLayer (composed below the base in the lower-trust position).
// An empty layer is treated as unset (the base inherits the configured
// default; no user layer is added) — so a run with no durable prompt layers
// is unchanged. When ov is nil but a layer is present, a fresh bundle is
// allocated. A registry read error is returned so the caller fails the run
// loudly. The returned bundle is fresh-per-run (no shared mutable state).
func ApplyPromptLayers(ctx context.Context, reg agentcfg.Registry, overlayStore sessionoverlay.Store, agentID string, id identity.Quadruple, ov *planner.LLMOverrides) (*planner.LLMOverrides, error) {
	base, adminUser, _, err := ActivePromptLayers(ctx, reg, agentID, id)
	if err != nil {
		return nil, err
	}

	// Session user layer (the safe subset): the session writes ONLY the user
	// layer — the overlay shape carries no base field, so a session caller
	// physically cannot edit the operator base. The session layer composes
	// ABOVE the admin base in the lower-trust `<user_instructions>` position
	// (escaped by the prompt builder), appended below any admin user layer.
	overlay, oerr := loadOverlay(ctx, overlayStore, agentID, id)
	if oerr != nil {
		return nil, oerr
	}

	// Durable USER-scope layer (the per-user standing instruction the durable
	// user-scope config tier persists): read back the active user-scope
	// revision's user_prompt and
	// compose it BETWEEN the admin user layer and the ephemeral session
	// overlay — admin Base > admin User > USER-durable > session User. A read
	// error fails the run loudly (no silent drop of the durable layer).
	durableUser, derr := activeDurableUserPrompt(ctx, reg, agentID, id)
	if derr != nil {
		return nil, derr
	}
	user := composeUserLayer(adminUser, durableUser, overlay.UserPrompt)

	// The durable ORDERED additive prompt blocks. They ride the same
	// run-start seam as the prompt layers so both binaries reach them
	// through ONE function and cannot drift, but they are a DIFFERENT
	// position: they compose into the operator-trusted additive guidance
	// and are NOT suppressed by a session SystemPromptOverride, which
	// replaces only the base+user spine.
	blocks, _, berr := ActiveExtraSystemBlocks(ctx, reg, agentID, id)
	if berr != nil {
		return nil, berr
	}

	// The admin base is ALWAYS the spine — it is never sourced from the
	// session overlay (base-unwritable-by-session is structural).
	if base == "" && user == "" && len(blocks) == 0 {
		return ov, nil
	}
	if ov == nil {
		ov = &planner.LLMOverrides{}
	}
	if base != "" {
		b := base
		ov.BasePromptLayer = &b
	}
	if user != "" {
		u := user
		ov.UserPromptLayer = &u
	}
	if len(blocks) > 0 {
		// Positional projection onto the planner shape — no sort, no map:
		// the declared order is the render order.
		pb := make([]planner.NamedBlock, 0, len(blocks))
		for _, b := range blocks {
			pb = append(pb, planner.NamedBlock{Name: b.Name, Body: b.Body})
		}
		ov.ExtraSystemBlocks = pb
	}
	return ov, nil
}

// composeUserLayer joins the three caller-authored user layers — the admin
// user layer, the durable user-scope layer, and the ephemeral session user
// layer, IN THAT ORDER — into the single lower-trust `<user_instructions>`
// block. The order is the security boundary (admin Base, the always-present
// spine, sits above all three): a later, lower-trust layer can EXTEND the
// operator's standing instruction, never precede or weaken it. Any segment
// may be empty (whitespace-only segments are dropped); an all-empty input
// yields "".
func composeUserLayer(adminUser, durableUser, sessionUser string) string {
	segs := make([]string, 0, 3)
	for _, s := range []string{adminUser, durableUser, sessionUser} {
		if t := strings.TrimSpace(s); t != "" {
			segs = append(segs, t)
		}
	}
	return strings.Join(segs, "\n\n")
}

// activeDurableUserPrompt resolves the caller's active USER-scope durable
// config revision and returns its user_prompt — the durable user-scope prompt
// layer. It keys by the run's identity triple with agent_id as the per-agent
// key (the USER config scope), so the real (tenant, user) is the isolation
// principal and the tuple is never widened. nil registry / empty agentID / no
// active user revision / a revision with no user prompt yields "" (the
// backward-compatible "no durable user layer" path). A registry read error is
// returned so the caller fails the run loudly — never a silent drop.
func activeDurableUserPrompt(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple) (string, error) {
	if reg == nil || agentID == "" {
		return "", nil
	}
	rev, found, err := reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeUser)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	user, _ := rev.Payload.UserPrompt()
	return user, nil
}

// unionSorted returns the sorted, de-duplicated union of two string sets. The
// union is the structural enforcement of NARROW-ONLY in the tool-exposure
// projection: a session disable set can only ADD to the admin exclusion set,
// never remove a member — so the resulting exclusion can only grow and the
// session can only narrow the admin-allowed exposure.
func unionSorted(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		set[s] = struct{}{}
	}
	for _, s := range b {
		set[s] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// FilterSkillViewsByMembership keeps only the views whose Name is in the
// membership set. An empty membership keeps nothing (the rollback-to-empty
// case — an explicit empty skills section disables every skill for the next
// run). The returned slice is always freshly allocated.
func FilterSkillViewsByMembership(views []skills.SkillView, names []string) []skills.SkillView {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	out := make([]skills.SkillView, 0, len(views))
	for _, v := range views {
		if _, ok := set[v.Name]; ok {
			out = append(out, v)
		}
	}
	return out
}
