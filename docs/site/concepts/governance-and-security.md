---
description: How Harbor governs LLM spend, throughput, and token budgets as identity-scoped middleware between the Runtime and the LLM driver — and the security non-negotiables (asymmetric-only JWT, no logged secrets, audit redaction, passthrough off by default).
---

# Governance and security

Two concerns sit at the edge where a Harbor run reaches the outside world: **governance** — keeping spend, throughput, and token budgets inside the limits an operator declared — and **security** — making sure identity, secrets, and credentials are handled the way a multi-tenant runtime must handle them. Both are designed as enforcement that the LLM substrate underneath cannot see and the planner above cannot bypass.

This page distills the design. For the authoritative treatment, defer to the [RFC](/reference/rfc) (§6.15 governance, §5.5 transport identity, §7 security rules).

## Governance is Runtime-to-LLM middleware

Every call to a model passes through governance on the way out and on the way back. Governance owns identity-scoped policies the LLM driver has no way to know about — what this tenant has already spent today, how fast this user is allowed to call, what the per-call ceiling is for this tier — and enforces them with two hooks:

| Hook | When it runs | What it does |
| --- | --- | --- |
| `PreCall` | Before the request reaches the LLM driver | Checks the cost accumulator against the per-identity ceiling, checks the per-identity rate limit, and clamps or rejects against per-call `MaxTokens`. Fails the call before a token is spent. |
| `PostCall` | After the driver returns | Folds the actual usage back into the cost accumulator so the next `PreCall` sees current truth. |

Because governance sits between the Runtime and the driver — not inside the planner and not inside the provider — the same ceilings apply no matter which planner is reasoning or which of the model providers is answering. The LLM client itself stays deliberately small (one `Complete(ctx, CompleteRequest)` method); governance is the layer that gives that call a budget.

## Fail loud, then emit

Harbor does not silently degrade when a limit is hit. There is no "drop to a cheaper model and hope" fallback at V1. A breached policy returns a typed, distinguishable error and emits a bus event so the breach is observable, not buried:

| Policy | Error on breach | Meaning |
| --- | --- | --- |
| Per-identity cost ceiling | `ErrBudgetExceeded` | This identity has spent its allotment. |
| Per-identity rate limit | `ErrRateLimited` | This identity is calling faster than allowed. |
| Per-call `MaxTokens` | `ErrMaxTokensExceeded` | A single request asked for more than its tier permits. |

::: tip Why typed errors, not a generic 429
Callers — a planner, an embedding surface, the Console — need to tell "you are out of money" apart from "you are going too fast" apart from "this one request is too big." Distinct sentinels let each surface respond correctly (back off, surface a quota wall, or split the request) instead of guessing. This is the [fail-loudly principle](/reference/rfc) applied at the spend boundary.
:::

## Accumulators persist on the triad

Cost accumulators are not in-process counters that reset when the binary restarts. They persist through the **StateStore**, which means they ride the same three-driver [persistence triad](/concepts/persistence) — in-memory for dev, SQLite for the single binary, Postgres for multi-node — that the rest of the Runtime uses, all behind one conformance suite.

Crucially, accumulators are **identity-scoped**, so the same cross-session isolation guarantees that protect memory and state protect spend: one user's runaway run cannot draw down another user's budget, and one tenant's ceiling is invisible to another. The governance suite ships cross-session isolation tests proving exactly this. The mandatory `(tenant, user, session)` triple that makes this possible is described in [Identity and isolation](/concepts/identity-and-isolation).

## Security non-negotiables

Governance answers "how much"; security answers "who, and with what proof." Four rules are binding across the whole Runtime.

### JWT validation is asymmetric-only

The identity triple arrives in JWT claims, and the Protocol rejects any request that carries no identity scope. Token validation accepts **asymmetric algorithms only** — the RS and ES families — and rejects the rest at the parser level:

| Status | Algorithms |
| --- | --- |
| Allowed | `RS256` `RS384` `RS512` `ES256` `ES384` `ES512` |
| Rejected at the parser | every `HS*` (symmetric) algorithm, and `none` |

::: warning Symmetric and `none` are not configurable
There is no flag to re-enable `HS256` or `none`. A symmetric secret shared with every client is a forgery kit; `none` is no verification at all. Both are refused before the claims are read. See [Auth and identity](/protocol/auth-and-identity) for the full handshake.
:::

### Secrets are never hardcoded or logged

No secret appears in source — not in production code, not in tests (tests use documented dummy fixtures). And no secret reaches the logs: Harbor does not log raw tool arguments or tool results, because they routinely carry credentials.

### Every payload passes the audit redactor

The typed event bus is the one canonical projection of runtime state, and **every payload is run through the audit redactor before it is emitted** — the lone carve-out is for types explicitly declared to carry no secret-shaped fields. Logging, telemetry, and the Console all observe the redacted stream; there is no path that quietly leaks an unredacted payload, and a redaction failure emits an `audit.redaction_failed` event rather than shipping the original bytes. (See [Observability](/concepts/observability) for how the same bus drives slog and OpenTelemetry.)

### Credential passthrough is off by default

Forwarding a caller's credentials straight to a downstream tool or service is disabled unless an operator explicitly turns it on. The default posture for tool-side auth is a proper token-exchange flow, and any passthrough that is enabled is audited. The pause/resume primitive — not a bespoke side path — is what carries tool-side OAuth and `AUTH_REQUIRED` interactions; see [Pause, resume, and steering](/concepts/pause-resume-and-steering).

## Post-V1 governance

The V1 surface is intentionally the load-bearing minimum: ceilings, rate limits, `MaxTokens`, and persisted accumulators. The roadmap layers richer operational governance on top of the same middleware seam, each gated behind Console visibility so an operator can see it act:

- **Key rotation** — roll provider keys without a restart.
- **Model swap** — redirect a tier to a different model.
- **Failover chains** — fall through an ordered list of providers.
- **Circuit breakers** — trip a provider out of rotation when it is failing.
- **An LLM cache** — short-circuit repeat calls.

::: details Why these are deferred, not designed-in now
Each of these changes behavior under the operator's feet (a cache returns a stale answer; a failover silently switches providers; a breaker drops a model mid-run). Shipping them before the Console can show *what happened and why* would violate the "no silent degradation" rule. They land when the observability surface can make them legible — not before.
:::

## Configure it

| You want to… | Go to |
| --- | --- |
| Set ceilings, rate limits, `MaxTokens`, and the driver behind the StateStore | [Config reference](/reference/config) |
| Wire JWT issuers, claims, and the identity handshake | [Auth and identity](/protocol/auth-and-identity) |
| Take an agent from `harbor dev` to a governed deployment | [Productionization playbook](/reference/productionization-playbook) |
| Understand the triple that scopes every accumulator | [Identity and isolation](/concepts/identity-and-isolation) |
| Read the authoritative design | [RFC](/reference/rfc) |
