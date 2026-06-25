---
description: Two identity-scoped, pluggable Harbor subsystems — Memory (three strategies plus opt-in semantic retrieval) and the runtime Skills subsystem (token-savvy, DB-backed, with a Skills.md importer and a persisting generator). Distills RFC §6.6 and §6.7.
---

# Memory and skills

Two of Harbor's Runtime subsystems shape what an agent *knows* at the moment it
reasons: **Memory** decides what prior context survives into the next prompt, and
the **Skills subsystem** decides what reusable know-how the planner can pull in on
demand. They are deliberately paired here because they share the same three
non-negotiable traits — they are **identity-scoped**, **declared-policy**, and
**pluggable across the persistence triad** — and because one of them owns a name
collision worth settling early (see the warning below).

For the authoritative design, defer to the [RFC](/reference/rfc) (§6.6 Memory,
§6.7 Skills). This page distills; it does not redefine.

## Memory: declared policy, identity-scoped

Memory in Harbor is never ambient. It is **declared** — you choose a strategy in
config — and **identity-scoped** to the `(tenant, user, session)` triple that
flows through every Runtime context (see
[Identity and isolation](/concepts/identity-and-isolation)). The session is the
default scope; there is no global memory pool.

The isolation contract here is the same fail-closed rule the rest of the Runtime
honors: a request with a **missing identity component yields an empty result plus
an audit event** — not a best-effort guess, not a silent fall-through to some
other scope. The predecessor's `require_explicit_key=false` knob is removed;
there is no way to weaken this.

::: tip Why declared-policy matters
Memory policy is an explicit, reviewable config decision, not an emergent
side-effect of how much the model happened to remember. That is what makes the
`llm_context` / `tool_context` separation hold: conversation state lives in
`llm_context`, while identifiers stay in `tool_context` (LLM-invisible), so
nothing leaks into what the model sees.
:::

## Three strategies

Memory ships three strategies. All three are backed by the same conformance
suite across all three V1 drivers (in-memory, SQLite, Postgres — see
[Persistence](/concepts/persistence)), so the strategy is independent of where
you store it.

| Strategy | What it does | Reach for it when |
|---|---|---|
| `none` | No memory retained between turns. | The agent is stateless per turn, or you manage context yourself. |
| `truncation` | Keeps a recent window within a token budget; older turns fall off the end. | You want recent-conversation fidelity and predictable token cost. |
| `rolling_summary` | Summarizes older context in the background, carrying a compacted summary forward (with summarization health states: `healthy → retry → degraded → recovering → healthy`). | Long sessions where early context still matters but cannot fit verbatim. |

`truncation` is the simple, cost-predictable default mindset; `rolling_summary`
trades a background summarization step for durable long-horizon recall.

## Opt-in semantic retrieval

On top of whichever strategy you pick, Harbor offers an **opt-in semantic
retrieval mode** (`retrieval: semantic`). It layers embedding-similarity search
over the configured strategy via an **injected `Embedder`** — Harbor does not
ship a built-in embedding model, so retrieval depends on the embedder you wire.
This is composition, never replacement: `GetLLMContext` keeps its
strategy-shaped patch unchanged while a `SearchTurns` surface ranks embedded
turns by cosine similarity.

This is a fail-loud seam, by design:

::: warning Semantic retrieval needs an embedder, and says so
If you turn on `retrieval: semantic` without wiring an `Embedder`, the Runtime
**fails loudly** rather than silently degrading to keyword matching or returning
nothing. Harbor's posture throughout is that a missing capability is an error,
not a quiet downgrade.
:::

To stand up the embedder and run a retrieval round-trip end to end, follow the
[Embed and retrieve recipe](/recipes/embed-and-retrieve).

## The runtime Skills subsystem

Skills are a **Runtime subsystem**: a **token-savvy, DB-backed, identity-scoped**
catalog of reusable know-how the planner can search and inject mid-run. Like
memory, skills sit behind the persistence triad (the `SkillStore` interface) and
honor the identity triple on every read.

Two features define the subsystem:

- **A first-class `Skills.md` importer** — drop in a skill pack and get an
  indexed, searchable Harbor skill. The import path is built in, not a
  bring-your-own script.
- **An in-runtime generator that persists** — an agent can author a skill during
  a run, and that skill is written back to the store so **subsequent runs can
  discover it**. The generator is not a throwaway; persistence is the point.

Injection is governed, not raw: **capability filtering and PII redaction happen
at injection time**, and a **tiered budgeter** keeps injected skill text inside
the token budget so a fat skill never blows out the prompt.

## Planner-facing skill tools

The planner does not reach into the skill store directly. It consults skills
through **ordinary catalog tools** — the same `Tool` abstraction every other
capability uses (see [Tools](/concepts/tools)):

| Tool | Purpose |
|---|---|
| `skill_search` | Find candidate skills relevant to the current step. |
| `skill_get` | Fetch the full body of a specific skill. |
| `skill_list` | Enumerate available skills in scope. |
| `skill_propose` | Author a new skill (the persisting generator's entry point). |

Search runs a deterministic **ranking ladder**: full-text search (FTS5) first,
then a regex pass, then exact match. The FTS5 stage is conditionally available
(it depends on the SQLite build), so the regex/exact fallback is exercised in CI
against `FTS5=off` builds.

```text
skill_search("retry a flaky upload")
        │
        ▼
   FTS5 full-text match   ──▶ ranked candidates
        │ (no/low hits)
        ▼
   regex pattern match    ──▶ ranked candidates
        │ (no hits)
        ▼
   exact match            ──▶ precise lookups
```

An opt-in **semantic-retrieval mode** swaps this ladder's *ranking* for
embedding similarity over the same identity-scoped catalog — through the same
`Embedder` seam memory uses — when an embedder is wired. Everything downstream of
ranking (capability filtering, redaction, the budgeter) stays unchanged, so
`skill_search` remains token-savvy. As with memory, a missing embedder fails
loudly rather than silently falling back to the lexical ladder.

::: warning Two different things are called "skills"
This is the single most common point of confusion, so read it once and move on.

- **The runtime Skills SUBSYSTEM** — *this page*. DB-backed know-how the
  **planner** searches and injects at runtime via `skill_search` /
  `skill_get` / `skill_list` / `skill_propose`. It is part of the Runtime.
- **Operator-skill PLAYBOOKS** — the Claude-Code-style guides this docs site
  ships for **humans operating Harbor** (scaffold an agent, run the dev loop,
  drive the playground). They are documentation, not a runtime feature.

When someone says "skills," figure out which one they mean. The playbooks live in
the [Skills index](/skills/); the [glossary](/reference/glossary) carries the
formal definitions of both senses.
:::

## How they relate to the rest of the Runtime

Both subsystems lean on machinery covered elsewhere:

- **Identity** is the shared isolation key and the fail-closed boundary —
  [Identity and isolation](/concepts/identity-and-isolation).
- **Heavy content** (a large retrieved document, a fat skill body) routes through
  the artifact store by reference rather than into the prompt — see
  [Artifacts and context safety](/concepts/artifacts-and-context-safety).
- **Persistence** is the triad both stores sit behind —
  [Persistence](/concepts/persistence).

## Build it

| Goal | Start here |
|---|---|
| Configure memory strategy + skills in your agent | [Configure memory and skills](/skills/configure-memory-and-skills/SKILL) |
| Read/write memory and skills from Go | [Use memory and skills from Go](/recipes/use-memory-and-skills-from-go) |
| Wire an embedder and run semantic retrieval | [Embed and retrieve](/recipes/embed-and-retrieve) |
| See the config surface | [Config reference](/reference/config) |
| Read the authoritative design | [RFC](/reference/rfc) (§6.6, §6.7) |

::: details Quick mental model
Memory answers *"what from the past does the model see this turn?"* — and you
declare the policy. Skills answer *"what reusable know-how can the planner pull in
on demand?"* — and the planner fetches it through tools. Both are
identity-scoped, both fail loud when a required capability is missing, and both
work identically on in-memory, SQLite, or Postgres.
:::
