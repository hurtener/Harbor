---
name: configure-memory-and-skills
description: "Wire multi-turn memory + the runtime skill catalog. Use when the agent needs context across turns (chatbots, multi-step research), or when you want token-savvy DB-backed skills (Skills.md importer / in-runtime generator) the planner can search and inject."
license: Apache-2.0
metadata:
  framework: harbor
  surface: memory
  verbs: "skill import, skill rm"
---

# Configure memory + runtime skills

Two subsystems that look similar but solve different problems:

- **Memory** — multi-turn context within a session. Lets the agent remember what it said three turns ago without re-reading the whole event log every step.
- **Runtime skills** — token-savvy, DB-backed playbooks the planner can search by name and inject into a prompt mid-reasoning. Distinct from "operator skills" (the docs/skills/ directory you're reading) — runtime skills are mechanism inside the planner, not docs for humans.

Both subsystems share a key contract: **identity-scoped by (tenant, user, session)** — the same multi-isolation triple that gates everything else in Harbor. No cross-session leakage. Ever.

## 1. Memory — strategies + drivers

Memory has two axes you tune independently:

- **Strategy** (`memory.strategy`) — how the planner uses memory each turn.
- **Driver** (`memory.driver`) — where memory is stored.

### Strategies

| Strategy           | When to use                                                                 |
|--------------------|------------------------------------------------------------------------------|
| `none` (default)   | Single-turn agents. No memory; each run starts cold.                         |
| `truncation`       | Chat agents with short windows. Keep last N messages; drop older verbatim.    |
| `rolling_summary`  | Long-running chat agents. Summarise older turns; keep recent N verbatim.     |

`rolling_summary` is the sweet spot for chatbots — it preserves the conversation arc without blowing the context window. The summariser is the same LLM as the planner (Bifrost reuses the configured provider).

### Drivers

| Driver     | When to use                                                                |
|------------|----------------------------------------------------------------------------|
| `inmem`    | Dev. Memory dies on `harbor dev` restart.                                  |
| `sqlite`   | Single-node production. Survives restarts. Default for self-hosted agents. |
| `postgres` | Multi-replica production. Use behind a load balancer.                      |

### Example: chat agent with rolling summary on SQLite

```yaml
memory:
  driver: sqlite
  dsn: /tmp/harbor-validation/my-agent-memory.sqlite   # outside the project dir (WAL trap)
  strategy: rolling_summary
  budget_tokens: 8000          # max tokens the planner replays per turn (0 = unbounded)
  recovery_backlog_max: 16     # bounded queue for the summariser's recovery loop (default 16)
```

`budget_tokens` is the hard cap — once a conversation exceeds it, older turns are summarised together into one assistant-role message while recent turns stay verbatim. The planner sees: `[summary of turns 1-12] [turn 13] [turn 14] ... [turn 18]`. `recovery_backlog_max` bounds the `rolling_summary` recovery loop's queue; on overflow it drops the oldest and emits `memory.recovery_dropped`. Both knobs are ignored by the `none` and `truncation` strategies.

### Opt-in semantic retrieval

`memory.retrieval: semantic` layers embedding-similarity search ON TOP of the strategy you picked above — it composes with `rolling_summary`, never replaces it. Turns are embedded as they land (`AddTurn`) and a `SearchTurns` surface ranks them by cosine; `GetLLMContext` keeps its normal summary + recent-turn patch. Vectors persist identity-scoped through the same state store, on all three drivers.

```yaml
memory:
  driver: sqlite
  dsn: /tmp/harbor-validation/my-agent-memory.sqlite
  strategy: rolling_summary
  retrieval: semantic        # opt-in; composes with the strategy
  retrieval_top_k: 5         # optional result cap (default 5)
  retrieval_min_score: 0.0   # cosine similarity floor [-1, 1]; 0.0 is the default

embeddings:                  # REQUIRED when any retrieval is semantic
  provider: openai
  model: text-embedding-3-small
  api_key: env.OPENAI_API_KEY
```

The `embeddings:` block is the embedding model/provider pair — configured **separately from the chat `llm` block** (they routinely come from different providers). Enabling a semantic mode without it fails validation loudly, naming the missing keys; there is no silent fallback to non-semantic retrieval and no mock embeddings driver.

When `memory.retrieval: semantic` is set the run loop calls `SearchTurns` on every task, applies the `retrieval_min_score` floor, deduplicates against the recent-turn window, caps each recalled turn at 2 KiB per side, and injects the result into the prompt's `<read_only_external_memory>` tier under the map key `recalled_turns`. A `SearchTurns` error fails the run loudly (`runtime_fetch_error`) — there is no silent fall-back to summary-only.

**Sizing `retrieval_top_k` is on you, and there is no safety net beneath it.** The 2 KiB cap bounds each TURN, not the aggregate: the injected block is at most `top_k × 2 × 2 KiB`. Nothing downstream re-checks the total. The LLM-edge context-leak guard byte-exempts everything that is not tool-role text, and memory tiers render under the system role — so the only backstop is the token-budget check, which fires after the whole prompt is assembled and **fails the run** rather than trimming it. A large `top_k` plus a long trajectory is how a run dies late.

### Caller-supplied memory — the second producer of that tier (D-364)

The `<read_only_external_memory>` tier has **two** producers, and they compose at map-key granularity rather than competing for the slot. Semantic recall writes `recalled_turns`; a Protocol client writes the fixed `caller_supplied` key by sending `caller_memory` on a `start` request (see the [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md) skill). Neither can displace the other, and a caller names no key at all — it supplies only a value — so no deny-list is needed and no future runtime producer can collide with one.

What this means for you as the operator:

- **You do not enable it.** There is no config key. It is on the Protocol surface for every caller with a valid identity, and it is bounded at 32 KiB per request at the edge, refused before any task is created. Your runtime advertises `caller_memory` in `runtime.info.capabilities`, so a client can tell whether it is supported before relying on it.
- **That 32 KiB is a resource bound, not a security boundary — do not budget your threat model against it.** It exists because nothing downstream re-checks these bytes (see the `retrieval_top_k` note above): without it an oversized document reaches the token-budget guard and fails the whole run late instead of costing one cheap refusal. It tells you nothing about how much content a caller can put in front of the model — the same caller can send more through the uncapped `query`, which lands in the *unframed* conversation position, and through `agent_config.session.set_user_prompt`, which needs no admin scope, takes a 1 MiB body, and lands *inside* the system prompt. What contains a caller's payload is the tier it lands in, never its size.
- **A caller can only reach that ONE prompt position.** It never touches the trusted base prompt (that is `system_prompt_override`, a different and strictly more powerful knob) and it never writes the conversation-memory tier, which is a claim about the session's stored turns only the runtime makes.
- **You can see it happening.** Every admitting run emits `memory.caller_block_admitted` with `bytes` / `tier` / `key` — a size, never content — so an audit trail shows caller-asserted memory entering a run without becoming a copy of it. In a trace, the tier reads `{"recalled_turns":[…],"caller_supplied":{…}}`: you can always tell which half came from where.
- **It is stored redacted, like its siblings.** The payload is persisted on the task record (and so to disk), and it goes through the audit redactor on the way in — the same one the run's `query` and description take. The redactor walks the decoded JSON, so structure survives and only secret-shaped keys (`api_key` / `password` / `secret` / `token` / `cookie` / `authorization`) and inline `Bearer …` / `Basic …` values become `***`. The redacted form is what the prompt sees too, which is already true of `query`.
- **The framing is the mitigation, and neither it nor the redactor is a sanitiser.** The tier's five-line anti-injection preamble tells the MODEL not to obey the content; it does nothing about what the bytes contain. The audit redactor is a PATTERN redactor: it does not detect PII, does not detect a credential that reads as ordinary prose, and cannot make hostile text safe. A caller that pipes unredacted third-party content through `caller_memory` still has a data-leakage path no prompt wrapper and no pattern redactor closes.
- **Nothing meters admission volume.** Token spend is metered at the LLM edge by the governance layer, so the cost is governed — but a caller may send 32 KiB on every `start` and no per-tenant accounting of admission itself exists yet.

### Identity scoping

Every memory write/read is keyed by `(tenant_id, user_id, session_id)`. The planner cannot read user A's memory from user B's session — the SQL `WHERE` clause filters before the rows reach the planner. This is enforced at the driver level, not at the planner; even a buggy planner cannot leak cross-session.

## 2. Runtime skills — DB-backed playbooks the planner searches

Runtime skills are typed, token-savvy reusable patterns the planner can ask for by name mid-reasoning. What a run sees is an **effective composition**, not one flat table: the identity-scoped **base/user/session** rungs plus **one operator tier** applied LAST, so operator-authored content deterministically wins a name collision against caller content.

The operator tier is itself a strict merge of TWO operator-managed sources:

- **Active revision packs** — the selected agent's durable `agent_packs` section of its active agent-config revision (protocol-managed and versioned; the `agent_config.agent_packs.*` verbs author it).
- **HA-66 boot packs** — the node-local `skills.boot_agent_packs` config-declared baseline, loaded eagerly and immutably before readiness (see below).

The merge is strict, never last-write-wins: the same canonical name with the same canonical semantic content hash dedupes to ONE item marked `source=both` (migration-safe — moving an unchanged body between the durable revision and the boot config must not split the composed view); the same name with a differing hash **fails loud**; and the unique combined tier holds at most **256** items. Run snapshots and the composition preview report per-item `boot|revision|both` provenance plus the deterministic `boot_pack_set_hash` (and combined/revision set hashes), so you can verify exactly what the resolved agent composes.

Skills enter the catalog through these producers:

- **Skills.md importer** — you write a Skills.md file (one skill per file: YAML frontmatter + a `## Steps` body) and ingest it with `harbor skill import <path>`.
- **In-runtime generator** — the planner itself can author a new skill at runtime (e.g. "this kind of question seems common — let me save the steps as a skill") and persist it via the `skill_propose` built-in (opt-in; see below).
- **Caller-authorized import (HA-61)** — the two-phase `import_validate` / `import_commit` Protocol flow installs a reviewed personal-skill package (see below).
- **Draft-only authoring (HA-62)** — `skill_create_draft` writes a reviewable draft artifact only, never an installed skill (see below).

### Example: a Skills.md file

One skill per file. The frontmatter `trigger:` is the planner-visible match cue (mandatory), `## Steps` needs at least one item; `## Preconditions` and `## Failure modes` are optional sections.

```markdown
---
name: triage-incident
title: Triage an incident
trigger: when a support ticket needs classification
tags: [support, triage]
---
Classify a support ticket into {bug, feature, question} and recommend the next action.

## Steps

- Read the user's report.
- Match against known categories.
- If "bug", pull the last 5 PRs that touched the area.
```

### Import / remove with the CLI

```console
$ harbor skill import ./skills/triage-incident.skill.md
imported "triage-incident" (scope=project, steps=3, attachments=0)
store: driver=localdb dsn=/tmp/harbor-validation/my-agent-skills.sqlite

$ harbor skill rm triage-incident
removed "triage-incident"
store: driver=localdb dsn=/tmp/harbor-validation/my-agent-skills.sqlite
```

Behaviour you can rely on:

- The verbs resolve the store from `harbor.yaml`'s `skills:` block — the **same store `harbor dev` serves** — and print the resolved `driver=… dsn=…` so you can see exactly where the skill landed. Pass `--config <path>` for a non-default config.
- Skills are identity-scoped (`(tenant, user, session)`). The verbs default to the `harbor dev` triple (`dev`/`dev`/`dev`); use `--tenant` / `--user` / `--session` for anything else.
- A duplicate name is **rejected with exit 1** ("pass --overwrite to replace, or `harbor skill rm <name>` first"); `--overwrite` replaces it. An invalid file (missing frontmatter, empty `trigger:`, no steps) exits 1 with the validator's reason. `rm` of a missing name exits 1.
- The global `--json` flag switches both verbs to a machine-readable result (`{"result":"imported","driver":…,"dsn":…,"report":{…}}`).
- Inline attachments (`![alt](relative/path)`) resolve relative to the file's own directory and upload to the configured artifact store; paths escaping that directory are rejected.

Go-level: the verbs are thin callers over `importer.ImportAndStore` — a headless embedder calls the same function (see `docs/recipes/use-memory-and-skills-from-go.md`).

Once the skills are in the catalog, the planner sees them at reasoning time two ways: a bounded per-turn `<skills_context>` browse window (the skills directory — see below) and on-demand retrieval via the `skill_search` / `skill_get` meta-tools — token-savvy because full skill bodies only enter the prompt when the LLM actually pulls them.

### Caller-authorized reviewed import (HA-61) — `import_validate` / `import_commit`

For a caller (a Protocol client, not an operator with a shell), installing a personal skill is a two-phase, reviewed proposal flow — never an immediate upsert:

- **`agent_config.user.skills.import_validate`** — the caller uploads a bounded complete skill package (zip carrying exactly one root-level `SKILL.md`, or a single Markdown document) as a caller-owned artifact via `artifacts.put`, then calls validate with that artifact ref plus the effective agent. The runtime runs the ONE production importer/validator and returns the closed normalized review, hashes, and warnings together with a **stateless opaque proposal token**. Validate performs **ZERO writes** of any kind: no SkillStore body/package write, no agent-config membership write, no proposal-ledger write — the review rides entirely inside the sealed token (24h window, refreshable by re-validating).
- **`agent_config.user.skills.import_commit`** — the caller echoes the token plus the reviewed `PackageHash`, expected config hash, canonical name, and explicit replace consent. The runtime authenticates and strictly decodes the sealed claims, re-derives identity and signed effective-agent reach, rechecks lifecycle / policy / ceilings / expected config hash, forces `ScopeUser` + caller ownership server-side, and atomically materializes the approved package plus membership in ONE conditional write. Response-loss retry is idempotent; competing commits have one winner.

Authority model to rely on: identity comes from the VERIFIED context (no selectable tenant/user/session/scope — the request carries no authority fields), signed session-reach and agent-reach gates run before any artifact lookup or persistence, and the package frontmatter is CLOSED (authority-bearing fields are rejected by the importer). `required_tools` / `required_namespaces` / `required_tags` are **applicability metadata, never grants**: a requirement outside the run-visible capability snapshot is a WARNING (the injection-time filter scrubs it), and the policy snapshot is sealed into the token so a policy change between validate and commit is a typed revocation refusal. A boot-owned canonical name is read-only to this flow too, even at equal hash (see HA-66 below).

### Draft-only authoring (HA-62) — `skill_create_draft`

`skill_create_draft` is an ordinary runtime tool that turns a bounded authoring intent (plus optional revision feedback) into ONE validated, caller-scoped, resource-free `SKILL.md` **draft artifact** — `installed: false`, `state: draft`. It is deliberately non-authoritative: it writes exactly one immutable artifact under the invocation's verified run identity and has **zero mutation authority** — no skill-store upsert, no membership/revision write, no operator-pack proposal/publication, no capability registration, no tool exposure, no approval/OAuth path. Identity comes exclusively from the run context; the closed argument shape (intent + optional feedback) rejects any owner/scope/identity/persistence/publication/grant field.

It is **disabled by default**: the tool is a known built-in in the registry's `KnownNames()` and an operator enables it by listing it in `tools.built_in`, exactly like every other built-in — the recommended/default configs simply do not list it. The assembly supplies the composed LLM client; explicitly listing the tool on a runtime without a usable LLM fails the boot loud. Once enabled it is opt-in per agent through the ordinary tool policy, with the same policy / approval / governance / rate-and-cost / deadline / cancellation / redaction / audit wrappers as every other ordinary tool. Installing the draft is a SEPARATE explicit caller action through the HA-61 validate/commit flow — creating the draft never installs or enables a skill. The result may warn that declared `required_tools` are metadata only and never grant capability.

### Yaml config

```yaml
skills:
  driver: localdb                  # localdb (default, SQLite) | postgres (durable/shared)
  dsn: /tmp/harbor-validation/my-agent-skills.sqlite    # WAL trap caveat applies
  # Postgres variant — durable/shared storage for multi-instance
  # deployments (every replica sees the same skills catalog):
  #   driver: postgres
  #   dsn: postgres://harbor:${HARBOR_SKILLS_PG_PASSWORD}@db:5432/harbor?sslmode=require
  #   migration_mode: apply # direct endpoint; switch to verify with pooled dsn after apply
  # retrieval: semantic            # optional — skill_search ranks by embedding
  #                                # similarity instead of the FTS5/regex/exact
  #                                # ladder (requires the embeddings: block; the
  #                                # capability filter, redaction, and budgeter
  #                                # apply unchanged on top)
  directory:                       # optional — shapes the per-turn <skills_context> block
    pinned: [triage-incident]      # always listed first, in this order
    max_entries: 10                # 0/unset → planner.skills_context_max (default 5)
    selection: pinned_then_recent  # the one wired value (pinned_then_top is rejected: not yet wired)
  # boot_agent_packs:              # optional (HA-66) — node-local operator baseline
  #   - tenant_id: acme            # exact, case-sensitive tenant key
  #     agent_id: harbor-dev-agent # must equal the resolved boot/default agent
  #     directory: /etc/harbor/skills   # config-file-relative, never CWD
  #     include: [workbench-foundation] # package directories, one SKILL.md each

tools:
  built_in:
    - skill_search    # the LLM discovers runtime skills by capability text
    - skill_get       # the LLM pulls the full bodies of named skills
    - skill_list      # the LLM enumerates the catalog (paged, summary-only)
    # - skill_propose      # OPT-IN: lets the LLM author + persist new skills
    # - skill_create_draft # OPT-IN (HA-62): drafts ONE immutable caller-scoped
    #                      # SKILL.md artifact; needs the assembly's composed
    #                      # LLM client (boot fails loud without one)
```

**Store drivers.** Skills ship at two-driver parity (the memory triple has no skills equivalent):

| Driver     | Use when                                                                 |
| ---------- | ------------------------------------------------------------------------ |
| `localdb`  | Default. CGo-free SQLite, a per-instance file. Single-node / embedded.    |
| `postgres` | Durable, shared Postgres for multi-instance deployments — every replica sees the same skills catalog. Same conflict policy, identity scoping, and FTS → regex → exact ranking ladder as `localdb` (the full-text tier rides a `tsvector` + GIN index). |

Switching backends is a `driver:` + `dsn:` change — the CLI verbs, meta-tools, directory, and semantic mode all behave identically (proven by the shared conformance suite). Identity scoping (`(tenant, user, session)` in the SQL `WHERE`) is enforced at the driver on both.

For Postgres, empty/`apply` runs forward migrations under a session advisory lock and therefore needs a direct/session-capable endpoint. A transaction-pooled steady-state deployment first completes that apply separately, then switches BOTH the DSN to the pooler and `migration_mode` to `verify`. Verify mode reads the existing `schema_migrations` ledger only and refuses startup if any embedded version is missing; it never creates or repairs schema through the pool.

### Node-local boot agent packs (HA-66) — `skills.boot_agent_packs`

An operator may declare a **node-local, resource-free operator skill baseline** for the resolved boot/default agent in boot config (the `boot_agent_packs:` block above). Behaviour you can rely on:

- **Loaded eagerly, immutably, before readiness.** The strict loader reads every declared package directory through the ONE production importer/validator at boot — before the runtime is ready or any listener binds — and fails the boot LOUD on any malformed, unresolvable, or un-importable entry. The frozen set is fixed for the process lifetime (restart-only; never hot-reloaded).
- **Zero persistence, zero admin verbs.** The loader performs **no SkillStore writes, no ArtifactStore writes, no AgentConfig revisions, no lifecycle materialization, and no admin pack verbs** — it is a pure eager filesystem read + parse + validate. It never creates agents or widens reach. `EnsureBootAgentLifecycle` is a separate mechanism and MAY write a revision.
- **Node-local, never converged.** Each node reloads its own files at boot. The durable Postgres `${SKILLS_DSN}` store persists agent revisions and personal state but NEVER the boot packs — a file change on one node never converges the others, and boot packs are never shared through the store.
- **One combined operator tier.** Boot entries merge with the agent's active durable revision-pack items into ONE strict operator tier (see the §2 intro): same canonical name + same semantic hash dedupes as `source=both`; differing hash fails loud; at most 256 unique combined items; the tier applies LAST over base/user/session skills. Every declared tenant-agent active revision is pre-read before readiness, so a conflict surfaces at boot, not at first run.
- **Boot/revision provenance + `boot_pack_set_hash`.** The run snapshot and the read-only composition preview (`agent_config.composition.preview`) report per-item `boot|revision|both` provenance alongside the deterministic `boot_pack_set_hash` (a node-local digest over the normalized boot entries, distinct from per-package hashes, stable across restarts for an unchanged config) — so you can verify exactly what the resolved agent composes.
- **Boot-owned names are read-only.** `upsert`, every proposal-commit path, and rollback/activation refuse a boot-declared canonical name with a typed error, even at equal hash — the NAME is boot-owned, not the bytes. Removal is the narrow exception: it may delete a real legacy active-revision shadow at that name, while a boot-only removal is a typed read-only refusal, never false success.
- **Config removal affects new runtime/new runs only.** Removing a `boot_agent_packs` declaration removes the boot contribution on the NEXT deployment/run; already-captured in-flight snapshots retain their bytes and hash, and an independently persisted durable revision remains revision-only (no tombstone, no erasure). A name with both contributions absent stays non-oracularly unavailable.
- **Headless `RunOnce` against a boot-pack agent is unsupported and fails loud** — it never silently runs without the packs. Production `harbor serve` and the devstack resolve the SAME loader path.

### LLM-side discovery via meta-tools

The React planner runs on native provider tool-calling: the LLM doesn't ask "what skills do I have?" in prose — it calls the `skill_search` built-in when it needs one. The meta-tools are the rich skills handlers (capability filter + redaction + token budgeter) over the SAME store your `harbor skill import` populated — one source of truth, identity-scoping carries through:

- `skill_search(query, limit?)` — ranked candidates, capability-filtered to the tools this run can actually see (a skill requiring a tool the run isn't granted never surfaces).
- `skill_get(names[], max_tokens?)` — full bodies, budget-fit through a tiered ladder (full → drop optional sections → cap steps at 3); an impossibly small budget errs loudly rather than silently truncating.
- `skill_list(scope?, task_type?, tags?, limit?, offset?)` — paged enumeration, summary-only.
- `skill_propose({skill, persist})` — the in-runtime generator. **Deliberately opt-in** (list it in `tools.built_in` only when you want the LLM persisting skills): persisted skills are stamped `Origin=generated`, can never overwrite an imported pack skill, and every persist emits a mandatory redacted `skill.proposed` audit event. This is the STRUCTURED generator; the natural-language draft path is the separate HA-62 `skill_create_draft` tool above.

### The per-turn skills directory (`<skills_context>`)

Independent of the meta-tools, every planner turn carries a compact `<skills_context>` block produced by the **skills directory**: a bounded, stable, pinned-then-recent browse window over the catalog (name / title / trigger / task type / pinned flag — never full bodies). The block tells the LLM *what exists*; pulling content is `skill_get`'s job. Tune it with the `skills.directory` yaml block above — `pinned` guarantees your flagship skills are always visible, `max_entries` caps the budget, and the stable ordering keeps the prompt prefix KV-cache-friendly. Capability filtering applies to the directory too: pinning never bypasses it.

### Session-personal skill cutover — operator-only, deny writes until verified

Phase 233a moves agent-owned session-personal skill bodies to durable records.
The rollout is intentionally **not** automatic: the default is read-only
`dual_read`. Existing eligible legacy session skills remain readable, but every
session-personal mutation is refused with `session_skill_cutover_pending`
(HTTP 409) until the tenant completes its declared migration. Do not retry that
error by writing a legacy shared-skill row.

Only declare a tenant after all older writers for that tenant have drained.
The declaration is static and takes effect on restart; it neither discovers
tenants/writers nor makes writes safe merely because its flag is set. It needs
the normal configured SkillStore and a durable StateStore in production:

```yaml
state:
  driver: sqlite
  dsn: /var/lib/harbor/state.sqlite

skills:
  driver: localdb
  dsn: /var/lib/harbor/skills.sqlite
  session_personal_cutover:
    tenants:
      - tenant_id: tenant-acme
        epoch: 2026-08-cutover-01
        roster_digest: sha256-... # an operator-attested label, never a secret
        legacy_writers_drained: false
```

`tenant_id` is case-sensitive. The list is capped at 256 declarations, and its
identifiers/epoch/digest are bounded printable-ASCII tokens; bad or duplicate
entries fail boot. Start with `legacy_writers_drained: false`. After draining
all older writers and attesting the roster, change it to `true` and restart.
Harbor resumes its bounded checkpointed migration and performs a fresh
verification pass. It alone writes durable `state_only`; until then, mutation
refusal is expected and protects the authoritative legacy view.

### Skill vs tool — when to pick which

- **Tool** — there's code to run, an API to call, a typed input/output. Build a [tool](../add-an-in-process-tool/SKILL.md).
- **Skill** — there's a *reasoning pattern* the planner should follow (a recipe, a checklist, a domain heuristic). Build a skill.
- **Both** — many real agents do both. A `triage-incident` skill whose step 4 says "call the `ticket.find_related_prs` tool" reaches into both subsystems.

## 3. Operator-skill vs runtime-skill — the naming clarification

`docs/skills/` (what you're reading right now) holds **operator playbooks** — markdown docs for humans building agents. They are NOT loaded into the planner at runtime; they're adoption material.

`internal/skills/` (RFC §6.7) holds the **runtime skill subsystem** — the identity-scoped catalog (localdb/postgres), the Skills.md importer, the in-runtime generator, the HA-61 reviewed import flow, the HA-62 draft tool, the HA-66 boot baseline loader, and the planner's mid-reasoning skill lookup path.

The two are unrelated. The glossary entry pins this distinction (`docs/glossary.md` → "skill (operator)" vs "skill (runtime)"). Don't conflate them.

## Common failure modes

- **Memory blows the token budget mid-conversation.** Lower `budget_tokens` OR switch strategy from `truncation` to `rolling_summary`. The summariser uses ~1500 tokens of LLM per turn but saves ~5000 tokens of payload.
- **`harbor dev` reboots in a loop after enabling memory.** Your `memory.dsn` is inside the project directory and the SQLite WAL trap fires. Move the DSN to `/tmp/harbor-validation/<project>-memory.sqlite` or `~/.harbor/<project>-memory.sqlite`.
- **`harbor skill import` fails with "skill name already exists".** The catalog rejects duplicate names by default. Re-import with `--overwrite`, remove the old entry first (`harbor skill rm <name>`), or rename the skill in the file.
- **The planner doesn't pick a skill I imported.** Either the skill's `trigger:` doesn't pattern-match the user's input (write more concrete trigger language), the run can't see a tool the skill requires (`required_tools` is capability-filtered — default-deny), or `planner.max_steps` is too low to reach the skill-search turn. Pin it (`skills.directory.pinned`) to guarantee it's at least visible in every `<skills_context>` block.
- **A session-personal skill update returns `session_skill_cutover_pending`.** This is the production-safe default while the tenant is unlisted, undrained, or still being freshly verified. Do not write through to a legacy shared body. Drain old writers, attest the exact roster, set the declaration true, restart, and wait for Harbor to reach `state_only`.
- **A boot pack and a revision pack collide at the same name with different content.** That is the strict operator-tier conflict: same canonical name, differing semantic hash — the boot fails loud (pre-read at readiness) and every later run refuses too. It is never a silent overwrite. Align the bodies (or the names), then restart; the boot baseline and the durable revision must agree on the canonical content hash to compose as `source=both`.
- **A mutation on a boot-owned name returns a typed read-only refusal.** Boot-declared names are read-only to `upsert`, every proposal-commit path, and rollback/activation — even at equal hash (the NAME is boot-owned, not the bytes). Removal is the one narrow exception: it may delete an ACTUAL legacy durable revision shadow at that name, while a boot-only name (owned by the baseline, absent from the durable revision) still returns the typed read-only refusal and is never removed. Change the boot config (or the revision's name), then restart; do not try to write through.
- **Cross-session memory leakage suspected.** It can't happen — the SQL filter is at the driver. If you see it, file a bug with the SQL trace from `telemetry.log_level: debug` — a leak would be a P0 security issue.

## See also

- [`define-the-agent-yaml`](../define-the-agent-yaml/SKILL.md) — the `memory:` and `skills:` blocks in context.
- [`add-an-in-process-tool`](../add-an-in-process-tool/SKILL.md) — when a skill becomes "actually run code".
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) — the Memory tab + the Skills tab show what the planner saw on each turn.
- [`docs/recipes/embed-and-retrieve.md`](../../recipes/embed-and-retrieve.md) — the embedding client à la carte + both semantic-retrieval opt-ins from Go.
- RFC §6.7 — the runtime skill subsystem design.
- RFC §6.6 — the memory subsystem design.
