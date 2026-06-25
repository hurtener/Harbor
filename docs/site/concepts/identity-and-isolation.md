---
description: Harbor's signature mental model — the mandatory (tenant, user, session) identity triple, fail-closed enforcement, elevated scope for cross-cutting reads, and why agent_id is not an isolation principal.
---

# Identity and multi-isolation

Most agent frameworks start single-tenant and bolt on isolation later. Harbor inverts that: the identity triple is load-bearing from the first line of the Runtime, and every subsystem that touches identity-scoped data is built to honor it. This page teaches the mental model. For the design authority behind it, defer to the [RFC](/reference/rfc); for the wire-level view, see the [auth and identity choreography](/protocol/auth-and-identity).

## The triple

Every Runtime context carries a mandatory identity key with three components. They nest, with the session as the innermost scope and the most active concurrency boundary.

| Component | Scope | Lifetime | Role |
| --- | --- | --- | --- |
| `tenant_id` | Outermost | Long-lived | The organization or isolation domain. The hard wall between customers. |
| `user_id` | Middle | Long-lived | A principal within a tenant. One user can hold many sessions at once. |
| `session_id` | Innermost | Per-conversation | A longer-lived, multi-turn conversation that contains many runs. The most active concurrency boundary. |

A run executes *within* `(tenant, user, session)`; adding a run identifier gives the quadruple the Runtime uses for tracing and provenance. The triple is what every storage method filters on.

::: tip Where it comes from
Identity arrives in JWT claims, the Protocol validates it, and it flows through the request `context.Context`. Handlers read it via `identity.MustFrom(ctx)`; storage methods take the triple explicitly and filter with the matching `WHERE` clause. There is no "current identity from a global" and no "fetch all, then filter in Go."
:::

## One user, many concurrent sessions

A single user can be active in multiple sessions at the same time, and those sessions must stay fully isolated — run A's input, state, and cancellation never reach run B. This is not an optional deployment mode; it is the default the Runtime is tested against.

Every persistence-shaped subsystem — `MemoryStore`, `StateStore`, `ArtifactStore`, the task registry, `EventBus.Subscribe`, and the tool-catalog filter — requires the full triple and ships a conformance suite that proves cross-session and cross-tenant no-leak under concurrent stress. Isolation is a property the test suite enforces, not a convention reviewers eyeball.

## Identity is mandatory and fails closed

Harbor has no `require_explicit_key=False`-style escape hatch. A request with a missing identity component does not silently degrade to a default or an empty scope — it fails closed:

- The call is rejected at the seam (storage, subscription, tool dispatch).
- An `identity.required` audit event is emitted so the rejection is observable.
- No partial result leaks past the missing-identity boundary.

This is a direct application of Harbor's fail-loudly principle: capabilities are mandatory, identity is mandatory, and degrading quietly on a missing scope is treated as a bug, not a convenience.

::: warning No identity-downgrading knobs
Flags that allow a missing tenant, user, or session are forbidden by design. If a change can't satisfy the triple without one, the design is wrong — the fix belongs in the [RFC](/reference/rfc), not in a feature flag.
:::

## Cross-cutting reads need an elevated scope claim

Some legitimate reads span the isolation boundary: a fleet view in the Console, an admin audit query, cross-session analytics. These are not done by relaxing identity — they require an **explicit elevated scope claim** carried in the request, and they are **audited unconditionally**.

| Read pattern | What it requires |
| --- | --- |
| Within one session | The plain triple. The default path. |
| Across sessions for one user | An elevated scope claim matching that user. Audited. |
| Across tenants / admin | An elevated scope claim with the matching tenant or admin scope. Audited. |

The shape is the same everywhere: the broader the read, the more explicit the claim, and every cross-boundary read leaves an audit trail. See [governance and security](/concepts/governance-and-security) for how the audit redactor and asymmetric-only JWT validation back this up.

## agent_id is not an isolation principal

This is the disambiguation that trips people up most.

::: warning agent_id is a registration identity, never a WHERE-clause filter
An agent is a runtime *execution entity* with a registration identity (`agent_id`) minted and persisted by the Agent Registry. That identity is **not** an isolation principal. The isolation boundary is and stays `(tenant, user, session)` (plus the run, for the quadruple).

An agent runs *within* a triple — it does not widen, narrow, or replace it. Storage methods, event filters, and memory/state drivers scope by the triple, never by `agent_id`. Adding `agent_id` to a `WHERE` clause as an isolation filter is a bug.
:::

Put plainly: `agent_id` answers "*which* agent is running," and the triple answers "*for whom, in what isolated context*." Don't conflate the two.

## How it appears on the wire

The Protocol never accepts a request without an identity scope. The triple lives in JWT claims, validation is restricted to asymmetric algorithms (the RS and ES families — never `HS*` or `none`), and both the SSE event stream and the REST control surface are server-enforced for identity, so a client can't widen its own scope. Event-bus subscriptions are identity-filtered on the server side; a cross-session or fleet subscription needs the matching elevated claim.

The full request-to-event sequence — claim validation, scope checks, and the audit events that fire — is documented in the [auth and identity choreography](/protocol/auth-and-identity).

## Go deeper

- [Auth and identity choreography](/protocol/auth-and-identity) — the wire-level view: claims, validation, and audited scope checks.
- [Governance and security](/concepts/governance-and-security) — identity-scoped ceilings, the audit redactor, and JWT rules.
- [Glossary](/reference/glossary) — precise definitions of the triple, elevated scope, and the Agent Registry identity.
- [RFC](/reference/rfc) — section 4 is the binding design source for everything on this page.
