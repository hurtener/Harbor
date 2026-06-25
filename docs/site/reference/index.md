---
description: The Harbor reference shelf — the RFC, the config schema, the glossary, the decisions log, the master phase plan, the productionization playbook, the changelog, and the generated Protocol reference. What each one answers and when to reach for it.
---

# Reference

This is the precision shelf. The [Concepts](/concepts/) track explains *why* Harbor
is shaped the way it is; the [Guides](/guides/) track shows you *how* to do a
specific thing. Reference is where you go for the exact answer — the binding
design source, every config field, the settled vocabulary, the decisions that
are no longer up for debate, and the catalogued Protocol wire.

Every page here is **canonical and mirrored from the repo**, so it cannot drift
from the code it documents. This overview only points; it never restates.

## What's in Reference

| Document | What it answers | When to reach for it |
|---|---|---|
| [RFC-001](/reference/rfc) | Why Harbor is designed this way — the binding intent. | Any time a concept page or guide cites a decision and you want the authority behind it. |
| [Configuration](/reference/config) | Every `harbor.yaml` field, its type, and its default. | Wiring an agent, a provider, a store, or a governance ceiling. |
| [Glossary](/reference/glossary) | What a Harbor term actually means. | When a word — *run*, *task*, *skill*, *decision* — is ambiguous. |
| [Decisions log](/reference/decisions) | Which architectural calls are settled (`D-NNN`). | Before re-litigating a design choice — grep here first. |
| [Master phase plan](/reference/master-plan) | What has shipped vs. what is pending. | Checking whether a capability is in V1, deferred, or post-V1. |
| [Productionization playbook](/reference/productionization-playbook) | How to take a working agent from `harbor dev` to production. | When the demo works and you need it to run for real. |
| [Changelog](/reference/changelog) | Release history and what each version changed. | Auditing what moved between two versions. |
| [Protocol reference](/protocol/methods) | The wire surface — methods, events, errors, types. | Building or integrating a Protocol client. (Lives in the Protocol track.) |

::: tip Authority order
When two documents disagree, the higher-priority one wins:
**RFC → phase plans → operational rules → research → code comments.** Every
concept page on this site defers to the [RFC](/reference/rfc) for its design
claims, and so should you.
:::

## RFC-001 — the design source of truth

[**RFC-001**](/reference/rfc) is the product intent and the design authority.
It is where the four-layer split (Runtime, Protocol, Console, CLI), the
swappable-planner seam, the mandatory identity triple, the unified pause/resume
primitive, and every subsystem contract are *decided* — not merely described.
Every [Concepts](/concepts/) page is a distillation of an RFC section; when you
want the binding statement rather than the friendly one, the RFC is where it
lives.

Reach for it when a guide says "because Harbor guarantees X" and you need to see
the guarantee stated as a non-negotiable.

## Configuration (`harbor.yaml`)

[**Configuration**](/reference/config) is the complete `harbor.yaml` schema:
every top-level field, every nested option, its type, and its documented
default. This is where you learn which knobs exist — the persistence driver
(in-memory / SQLite / Postgres), the planner (`react`, the V1 reference
driver), the memory strategy (`none` / `truncation` / `rolling_summary`), the
heavy-output threshold, governance ceilings, and the LLM provider wiring.

::: tip Defaults bias toward fail-loud
Harbor does not silently degrade. A missing required dependency — no LLM
provider, no API key — exits non-zero at boot with a message naming the missing
key. The config reference documents which fields are mandatory and which carry a
safe default.
:::

For the *task* of writing the file, the [`define-the-agent-yaml`](/skills/define-the-agent-yaml/SKILL)
skill walks it step by step; the config reference is the field-by-field
authority behind it.

## Glossary

The [**Glossary**](/reference/glossary) is Harbor's controlled vocabulary. The
single most load-bearing entry: the difference between a **runtime skill** and an
**operator skill**.

- A **runtime skill** is a Runtime subsystem entity — token-savvy, DB-backed,
  identity-scoped, discoverable by the planner through catalog tools
  (`skill_search`, `skill_get`, `skill_list`, `skill_propose`). See
  [Memory and skills](/concepts/memory-and-skills).
- An **operator skill** is a playbook *you* follow — the `/skills/...` pages on
  this very site, like [`scaffold-a-harbor-agent`](/skills/scaffold-a-harbor-agent/SKILL).

Same word, two worlds. The glossary keeps them straight, along with *run*,
*session*, *task*, *decision*, and the identity-triple terms.

## Decisions log

The [**Decisions log**](/reference/decisions) is the append-only record of
settled architectural calls, each numbered `D-NNN`. When the RFC or a concept
page cites a decision — `D-026` (the context-window safety net), `D-091` (the
decoupled Console), `D-196` (the production driver aggregator) — this is where
the call and its rationale live.

::: warning Don't re-litigate silently
A settled decision changes only through a new RFC entry plus a new decisions
record — never through a quiet code change. If you think one needs to move, the
decisions log is where you confirm what's settled before proposing the change.
:::

## Master phase plan

The [**Master phase plan**](/reference/master-plan) is the execution index: the
canonical list of phases, each marked *Shipped* or *Pending*. Harbor is built in
waves of coherent subsystem slices, and this is the ledger of what has actually
landed.

Reach for it to answer "is this in V1?" with certainty rather than inference —
the [Changelog](/reference/changelog) tells you *when* something shipped; the
master plan tells you *what* the full surface is and where each piece stands.

## Productionization playbook

The [**Productionization playbook**](/reference/productionization-playbook) is
the bridge from a working `harbor dev` agent to a deployed one. It covers the
choices that only matter once real traffic arrives: picking SQLite vs. Postgres
from the [persistence triad](/concepts/persistence), setting
[governance ceilings and rate limits](/concepts/governance-and-security),
asymmetric-only JWT auth, audit redaction, and the observability wiring
(OTLP plus the built-in Prometheus `/metrics` endpoint).

Read it after the agent behaves correctly and before it faces users.

## Changelog

The [**Changelog**](/reference/changelog) is the release history. Two version
lines matter and they move independently:

- **Harbor** is at **v1.6** (v1.6.0, 2026-06-25) — Apache-2.0, built CGo-free as
  a single static binary on Go 1.26+.
- **The Harbor Protocol** is pinned at **0.1.0**, versioned separately from the
  product. Bumping it is an RFC change with a deprecation window, so a
  third-party client knows exactly what it's building against.

::: tip Two clocks on purpose
The product can ship features every wave while the wire contract stays stable.
That separation is what lets a remote Console — or your own client — keep working
across Harbor releases. See [versioning and compatibility](/protocol/versioning-and-compatibility).
:::

## The generated Protocol reference

The catalogued wire surface lives in the [**Protocol track**](/protocol/),
because it is generated from the same canonical sources the Runtime compiles
from and is gated in CI against drift. Four pages, each marked *do not edit*:

| Page | What it catalogues |
|---|---|
| [Methods](/protocol/methods) | Every canonical method: route, classification, request/response types, auth posture. |
| [Events](/protocol/events) | Every canonical event type with its field-level payload shape. |
| [Errors](/protocol/errors) | Every error code: HTTP binding, when it fires, retry guidance. |
| [Types](/protocol/types) | Every wire struct, field-level, with its snake_case JSON keys. |

If you are *building* against the Protocol rather than looking up a single field,
start at the [Protocol overview](/protocol/) and its
[15-minute quickstart](/protocol/quickstart).

## Cross-links

- For the *why* behind any of these documents, read [Concepts](/concepts/) —
  start with [the architecture](/concepts/architecture).
- For the *how* of a concrete task, read the [Guides](/guides/) or pick an
  operator [skill](/skills/).
- New to Harbor entirely? Begin at [Get started](/get-started).
