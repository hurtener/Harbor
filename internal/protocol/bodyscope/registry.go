package bodyscope

import (
	"sort"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// Surface is the registry key for a Protocol surface that decodes an
// identity scope from a request body. One surface owns one body-identity
// posture; every method the surface serves reconciles under it.
type Surface string

// The closed set of body-identity surfaces. A Protocol request type that
// carries an identity scope belongs to exactly one of them — the join is
// requestSurfaces below, and a lockstep test fails when a canonical
// request type has no row.
//
// Two keys are deliberately not the surface's method prefix.
// SurfacePause and SurfaceStateHistory would otherwise read `pause` and
// `state.history`, which are canonical Protocol method names — and a
// method name is single-sourced in internal/protocol/methods, with a
// build-gating scan that rejects the literal anywhere else. The keys
// carry an underscore so the registry does not become a second place a
// method string is written. Operator-facing refusals use the Wire field,
// not the key.
const (
	SurfaceAgentConfig     Surface = "agent_config"
	SurfaceAgents          Surface = "agents"
	SurfaceApps            Surface = "mcp.apps"
	SurfaceArtifacts       Surface = "artifacts"
	SurfaceArtifactsPut    Surface = "artifacts.upload"
	SurfaceArtifactsDelete Surface = "artifacts.removal"
	// SurfaceArtifactsRef governs BOTH artifact content reads — the
	// driver-independent byte read and the presigned-URL resolver. One
	// posture, two methods: a surface key is a posture key, not a method
	// name, and a per-method key here could not even be spelled (a
	// Protocol method string outside internal/protocol/methods is a
	// single-source violation the build scan rejects).
	SurfaceArtifactsRef Surface = "artifacts.ref"
	SurfaceAuth         Surface = "auth"
	SurfaceControlTask  Surface = "task"
	SurfaceEvents       Surface = "events"
	SurfaceFlows        Surface = "flows"
	SurfaceGovernance   Surface = "governance"
	SurfaceMCP          Surface = "mcp.servers"
	SurfaceMemory       Surface = "memory"
	SurfacePause        Surface = "pause_page"
	SurfacePosture      Surface = "runtime"
	SurfaceRuns         Surface = "runs"
	SurfaceSessions     Surface = "sessions"
	SurfaceStateHistory Surface = "state_history"
	SurfaceTasks        Surface = "tasks"
	SurfaceTools        Surface = "tools"
	SurfaceTopology     Surface = "topology"
)

// policies is the closed declaration table: one row per Surface, each
// naming its per-component rule and the reason it holds that posture.
//
// The table is the whole point of this package. A surface's posture used
// to live in a code comment beside a hand-written helper, which meant
// the next handler author copied the comment and re-derived the check —
// and a comment that says "the surface enforces the admin gate" is not
// enforcement. Here the posture is a value the gate reads, the audit
// requirement follows from the value, and the lockstep test in
// registry_lockstep_test.go fails when a surface goes missing.
//
// Read the rows as: what may a caller name in the body that is not its
// own verified identity?
var policies = map[Surface]Policy{
	SurfaceAgentConfig: {
		Surface: SurfaceAgentConfig, Wire: "agent_config",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "Agent configuration is written under the caller's own triple; the durable per-user and per-session variants are selected by the verb family, never by renaming the caller.",
	},
	SurfaceAgents: {
		Surface: SurfaceAgents, Wire: "agents",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "The registry's fleet-wide enumeration is a scope-derived widening on the read path; the body triple stays the caller's own.",
	},
	SurfaceApps: {
		Surface: SurfaceApps, Wire: "mcp.apps",
		Tenant: Pinned, User: Pinned, Session: Pinned,
		Reason: "The MCP Apps surface has no admin-elevation path — its verb gate is identity-scoped with no claim that widens it — so a body triple that disagrees with the verified one is unconditionally invalid.",
	},
	SurfaceArtifacts: {
		Surface: SurfaceArtifacts, Wire: "artifacts",
		Tenant: AdminScoped, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "A fleet operator enumerates another tenant's artifacts under either admin-tier claim; an empty user or session arrives here as the list filter's wildcard, so the gate leaves it empty rather than backfilling it. What an empty component MEANS is the listing's own call, and it is not the same on both axes — the surface folds an elided user to the caller's own unless an admin-tier claim widens it, and keeps an elided session a wildcard within that user. This gate cannot express that split (an empty component short-circuits before any rule runs), which is why the listing's identity bound lives at the surface and this row stops at 'left empty'.",
	},
	SurfaceArtifactsDelete: {
		Surface: SurfaceArtifactsDelete, Wire: "artifacts.removal",
		Tenant: AdminScoped, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Grants: []auth.Scope{auth.ScopeAdmin},
		Reason: "Destroying another tenant's artifact is a write, so it takes the administrative claim alone — a read-only fleet token enumerates but does not destroy. Naming that here rather than only at the surface keeps the transport from recording a crossing the surface will refuse.",
	},
	SurfaceArtifactsPut: {
		Surface: SurfaceArtifactsPut, Wire: "artifacts.upload",
		Tenant: AdminScoped, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Grants: []auth.Scope{auth.ScopeAdmin},
		Reason: "Seeding another tenant's store is a write, so it takes the administrative claim alone — a read-only fleet token enumerates but does not deposit. Naming that here rather than only at the surface keeps the transport from recording a crossing the surface will refuse.",
	},
	SurfaceArtifactsRef: {
		Surface: SurfaceArtifactsRef, Wire: "artifacts.ref",
		Tenant: Pinned, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		PinnedDeniedCode: protoerrors.CodeScopeMismatch,
		Reason:           "Both artifact CONTENT reads land here — the driver-independent byte read and the presigned reference — because they hand over the same thing over different transports and so hold one posture, not two. Content is materially broader than the metadata a listing returns, so no claim widens either. The scope also requires the full triple, so the only foreign-tenant shape that reaches them already carries the caller's own user and session — not a fleet-observation shape, and nothing to elevate.",
	},
	SurfaceAuth: {
		Surface: SurfaceAuth, Wire: "auth",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "Token rotation acts on the caller's own credential; there is no cross-identity rotation.",
	},
	SurfaceControlTask: {
		Surface: SurfaceControlTask, Wire: "task",
		Tenant: Pinned, User: Pinned, Session: Pinned,
		Reason: "Starting and steering a run happens under the caller's own triple. Acting as another identity is the impersonation shape, which carries its own triplet, its own admin claim and its own audit anchor — it never travels as a plain body triple.",
	},
	SurfaceEvents: {
		Surface: SurfaceEvents, Wire: "events",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "Cross-tenant event fan-in is requested by the subscription's own admin filter and derived from the verified scope set, never from the body triple.",
	},
	SurfaceFlows: {
		Surface: SurfaceFlows, Wire: "flows",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "A cross-tenant flow read is expressed by the request's own filter and gated on the verified claim by the service; the body triple stays the caller's own.",
	},
	SurfaceGovernance: {
		Surface: SurfaceGovernance, Wire: "governance",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "Tenant overrides are addressed by an explicit target field the service gates, not by renaming the caller in the body.",
	},
	SurfaceMCP: {
		Surface: SurfaceMCP, Wire: "mcp.servers",
		Tenant: Pinned, User: Pinned, Session: Pinned,
		Reason: "The MCP connection catalog is process-global and its verb gate mints no claim that widens the tenant, so a body triple that disagrees with the verified one is unconditionally invalid.",
	},
	SurfaceMemory: {
		Surface: SurfaceMemory, Wire: "memory",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "A cross-tenant memory read is expressed by the list filter's tenant set and gated on the verified claim; the body triple stays the caller's own.",
	},
	SurfacePause: {
		Surface: SurfacePause, Wire: "pause listing",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "A cross-tenant pause listing is expressed by the list filter's tenant set and gated on the verified claim; the body triple stays the caller's own.",
	},
	SurfacePosture: {
		Surface: SurfacePosture, Wire: "runtime",
		Tenant: AdminScoped, User: Pinned, Session: Pinned,
		Reason: "A fleet operator reads another tenant's governance and provider posture under the admin claim. The user and session stay the caller's own: posture is not an impersonation surface.",
	},
	SurfaceRuns: {
		Surface: SurfaceRuns, Wire: "runs",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "Run overrides target a run inside the caller's own session; a run outside it is refused by the service on scope, not admitted by the body triple.",
	},
	SurfaceSessions: {
		Surface: SurfaceSessions, Wire: "sessions",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "A cross-tenant session listing is expressed by the list filter's tenant set and gated on the verified claim; the body triple stays the caller's own.",
	},
	SurfaceStateHistory: {
		Surface: SurfaceStateHistory, Wire: "state.history read",
		Tenant: AdminScoped, User: AdminScoped, Session: AdminScoped,
		Grants:          []auth.Scope{auth.ScopeAdmin},
		ScopeDeniedCode: protoerrors.CodeNotFound,
		Reason:          "A fleet operator reads another identity's whole state timeline under the admin claim, so all three components travel together — the target session is the point of the read. The deny path returns the not-found code rather than a scope refusal so the absence of the claim and the absence of the session are indistinguishable to the caller.",
	},
	SurfaceTasks: {
		Surface: SurfaceTasks, Wire: "tasks",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "A fleet task listing is a scope-derived widening on the read path; the body triple stays the caller's own.",
	},
	SurfaceTools: {
		Surface: SurfaceTools, Wire: "tools",
		Tenant: PinnedOrEmpty, User: PinnedOrEmpty, Session: PinnedOrEmpty,
		Reason: "The tool catalog's admin verbs are gated per method on the verified claim and act on a tool id, not on a renamed caller.",
	},
	SurfaceTopology: {
		Surface: SurfaceTopology, Wire: "topology",
		Tenant: AdminScoped, User: Pinned, Session: Pinned,
		Grants: []auth.Scope{auth.ScopeAdmin},
		Reason: "A fleet operator reads another tenant's runtime graph under the admin claim. The user and session stay the caller's own: a topology snapshot is not an impersonation surface.",
	},
}

// PolicyFor returns the registered Policy for surface and a presence
// bool. Absence is a construction bug — the registry is closed — so
// Reconcile turns it into a loud CodeRuntimeError rather than a default.
func PolicyFor(surface Surface) (Policy, bool) {
	p, ok := policies[surface]
	return p, ok
}

// RegisteredSurfaces returns every registered Surface, sorted. Used by
// the lockstep test to pin the closed set and by operator tooling that
// renders the posture table.
func RegisteredSurfaces() []Surface {
	out := make([]Surface, 0, len(policies))
	for s := range policies {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RegisteredPolicies returns a copy of the closed policy table.
func RegisteredPolicies() map[Surface]Policy {
	out := make(map[Surface]Policy, len(policies))
	for s, p := range policies {
		out[s] = p
	}
	return out
}
