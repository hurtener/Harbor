// Package protocol implements the eight `agents.*` Protocol methods the
// Console Agents page (Phase 73e / D-124) consumes:
//
//   - agents.list        — paginated, faceted agent catalog projection.
//   - agents.get         — one agent's full registration-identity
//     projection (identity / hosting / status / health / AgentConfig).
//   - agents.tools       — the agent's tool bindings + per-binding OAuth.
//   - agents.memory      — the agent's memory strategy + TTL + scope.
//   - agents.governance  — per-identity-tier ceilings + spend + limits.
//   - agents.skills      — the agent's attached skills.
//   - agents.permissions — the agent's permission model (V1: implicit).
//   - agents.metrics     — the registry-wide rollup the page hero shows.
//
// # The seam (CLAUDE.md §4.4)
//
// The Service depends on the `Projector` interface, not on a concrete
// Agent Registry. The V1 production implementation is
// `RegistryProjector` (registry_projector.go) — a thin read-only
// projection over a `registry.AgentRegistry`. A future projector that
// joins richer operational metadata (live task counts, real governance
// accumulators) slots in behind the same interface without reshaping
// the Service.
//
// # Identity is mandatory (CLAUDE.md §6 rule 9)
//
// Every method takes the wire request's `IdentityScope`. An incomplete
// triple fails closed with `ErrIdentityRequired` — there is no
// identity-downgrading knob. The Service NEVER reads identity from a
// package-level global; the triple flows in via the request. The
// registry filters by the (tenant, user, session) tuple, NEVER by
// `agent_id` — `agent_id` is a registration identity, not a WHERE-clause
// isolation key (D-059, CLAUDE.md §6 clarifying note).
//
// # No `agents.*` control method
//
// The five agent-control verbs the Agents page exposes (Pause / Drain /
// Restart / Force-Stop / Deregister) are NOT `agents.*` methods — they
// are the EXISTING shipped `registry.*` control verbs (D-066), gated on
// the elevated control-scope claim. Phase 73e mints NO control method;
// the page invokes the shipped registry control surface. All eight
// `agents.*` methods here are READ-ONLY projections (CLAUDE.md §13).
//
// # Concurrent reuse (D-025)
//
// A constructed *Service is immutable after NewService and safe to
// share across N concurrent goroutines: it holds only the Projector
// reference + a logger; every method's per-call state lives in the
// call's arguments and locals, never on the Service.
package protocol
