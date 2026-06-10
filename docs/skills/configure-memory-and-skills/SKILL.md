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
  budget_tokens: 8000        # max tokens the planner replays per turn
  summary_keep_recent_turns: 6   # the N most-recent turns kept verbatim
```

`budget_tokens` is the hard cap; `summary_keep_recent_turns` is the floor — older turns are summarised together into one assistant-role message. The planner sees: `[summary of turns 1-12] [turn 13] [turn 14] ... [turn 18]`.

### Identity scoping

Every memory write/read is keyed by `(tenant_id, user_id, session_id)`. The planner cannot read user A's memory from user B's session — the SQL `WHERE` clause filters before the rows reach the planner. This is enforced at the driver level, not at the planner; even a buggy planner cannot leak cross-session.

## 2. Runtime skills — DB-backed playbooks the planner searches

Runtime skills are typed, token-savvy reusable patterns the planner can ask for by name mid-reasoning. They originate from two sources:

- **Skills.md importer** — you write a Skills.md file (one skill per file: YAML frontmatter + a `## Steps` body) and ingest it with `harbor skill import <path>`.
- **In-runtime generator** — the planner itself can author a new skill at runtime (e.g. "this kind of question seems common — let me save the steps as a skill") and persist it via the `skill_propose` built-in (opt-in; see below).

Both sources land in the same SQLite-backed catalog.

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

### Yaml config

```yaml
skills:
  driver: localdb
  dsn: /tmp/harbor-validation/my-agent-skills.sqlite    # WAL trap caveat applies
  directory:                       # optional — shapes the per-turn <skills_context> block
    pinned: [triage-incident]      # always listed first, in this order
    max_entries: 10                # 0/unset → planner.skills_context_max (default 5)
    selection: pinned_then_recent  # or pinned_then_top (most-used first)

tools:
  built_in:
    - skill_search    # the LLM discovers runtime skills by capability text
    - skill_get       # the LLM pulls the full bodies of named skills
    - skill_list      # the LLM enumerates the catalog (paged, summary-only)
    # - skill_propose # OPT-IN: lets the LLM author + persist new skills
```

### LLM-side discovery via meta-tools

The React planner runs on native provider tool-calling: the LLM doesn't ask "what skills do I have?" in prose — it calls the `skill_search` built-in when it needs one. The meta-tools are the rich skills handlers (capability filter + redaction + token budgeter) over the SAME store your `harbor skill import` populated — one source of truth, identity-scoping carries through:

- `skill_search(query, limit?)` — ranked candidates, capability-filtered to the tools this run can actually see (a skill requiring a tool the run isn't granted never surfaces).
- `skill_get(names[], max_tokens?)` — full bodies, budget-fit through a tiered ladder (full → drop optional sections → cap steps at 3); an impossibly small budget errs loudly rather than silently truncating.
- `skill_list(scope?, task_type?, tags?, limit?, offset?)` — paged enumeration, summary-only.
- `skill_propose({skill, persist})` — the in-runtime generator. **Deliberately opt-in** (list it in `tools.built_in` only when you want the LLM persisting skills): persisted skills are stamped `Origin=generated`, can never overwrite an imported pack skill, and every persist emits a mandatory redacted `skill.proposed` audit event.

### The per-turn skills directory (`<skills_context>`)

Independent of the meta-tools, every planner turn carries a compact `<skills_context>` block produced by the **skills directory**: a bounded, stable, pinned-then-recent browse window over the catalog (name / title / trigger / task type / pinned flag — never full bodies). The block tells the LLM *what exists*; pulling content is `skill_get`'s job. Tune it with the `skills.directory` yaml block above — `pinned` guarantees your flagship skills are always visible, `max_entries` caps the budget, and the stable ordering keeps the prompt prefix KV-cache-friendly. Capability filtering applies to the directory too: pinning never bypasses it.

### Skill vs tool — when to pick which

- **Tool** — there's code to run, an API to call, a typed input/output. Build a [tool](../add-an-in-process-tool/SKILL.md).
- **Skill** — there's a *reasoning pattern* the planner should follow (a recipe, a checklist, a domain heuristic). Build a skill.
- **Both** — many real agents do both. A `triage-incident` skill whose step 4 says "call the `ticket.find_related_prs` tool" reaches into both subsystems.

## 3. Operator-skill vs runtime-skill — the naming clarification

`docs/skills/` (what you're reading right now) holds **operator playbooks** — markdown docs for humans building agents. They are NOT loaded into the planner at runtime; they're adoption material.

`internal/skills/` (RFC §6.7) holds the **runtime skill subsystem** — the SQLite catalog, the Skills.md importer, the in-runtime generator, the planner's mid-reasoning skill lookup path.

The two are unrelated. The glossary entry pins this distinction (`docs/glossary.md` → "skill (operator)" vs "skill (runtime)"). Don't conflate them.

## Common failure modes

- **Memory blows the token budget mid-conversation.** Lower `budget_tokens` OR switch strategy from `truncation` to `rolling_summary`. The summariser uses ~1500 tokens of LLM per turn but saves ~5000 tokens of payload.
- **`harbor dev` reboots in a loop after enabling memory.** Your `memory.dsn` is inside the project directory and the SQLite WAL trap fires. Move the DSN to `/tmp/harbor-validation/<project>-memory.sqlite` or `~/.harbor/<project>-memory.sqlite`.
- **`harbor skill import` fails with "skill name already exists".** The catalog rejects duplicate names by default. Re-import with `--overwrite`, remove the old entry first (`harbor skill rm <name>`), or rename the skill in the file.
- **The planner doesn't pick a skill I imported.** Either the skill's `trigger:` doesn't pattern-match the user's input (write more concrete trigger language), the run can't see a tool the skill requires (`required_tools` is capability-filtered — default-deny), or `planner.max_steps` is too low to reach the skill-search turn. Pin it (`skills.directory.pinned`) to guarantee it's at least visible in every `<skills_context>` block.
- **Cross-session memory leakage suspected.** It can't happen — the SQL filter is at the driver. If you see it, file a bug with the SQL trace from `telemetry.log_level: debug` — a leak would be a P0 security issue.

## See also

- [`define-the-agent-yaml`](../define-the-agent-yaml/SKILL.md) — the `memory:` and `skills:` blocks in context.
- [`add-an-in-process-tool`](../add-an-in-process-tool/SKILL.md) — when a skill becomes "actually run code".
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) — the Memory tab + the Skills tab show what the planner saw on each turn.
- RFC §6.7 — the runtime skill subsystem design.
- RFC §6.6 — the memory subsystem design.
