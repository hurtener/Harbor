---
name: configure-memory-and-skills
description: "Wire multi-turn memory + the runtime skill catalog. Use when the agent needs context across turns (chatbots, multi-step research), or when you want token-savvy DB-backed skills (Skills.md importer / in-runtime generator) the planner can search and inject."
license: Apache-2.0
metadata:
  framework: harbor
  surface: memory
  verbs: ""
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

- **Skills.md importer** — you write a `Skills.md` file with one `## skill-name` heading per skill, and `harbor` imports it into the skill catalog.
- **In-runtime generator** — the planner itself can author a new skill at runtime (e.g. "this kind of question seems common — let me save the steps as a skill") and persist it.

Both sources land in the same SQLite-backed catalog.

### Example: a Skills.md file

```markdown
# Skills

## summarise-paper
Compact a 10-page paper to a 3-bullet summary + 1-sentence verdict.

1. Read abstract + conclusion.
2. Note the 3 most-cited prior works.
3. Output: bullet points, then verdict.

## triage-incident
Classify a support ticket into {bug, feature, question} + recommend the next action.

1. Read the user's report.
2. Match against known categories.
3. If "bug", pull the last 5 PRs that touched the area.
```

Import:

```bash
harbor skill import ./Skills.md
```

The planner now sees both skills in its catalog. At reasoning time, it searches the catalog for relevant skill names and injects the matching skill body into the prompt — token-savvy because it doesn't carry every skill every turn, only the ones it actually pulls.

### Yaml config

```yaml
skills:
  driver: localdb
  dsn: /tmp/harbor-validation/my-agent-skills.sqlite    # WAL trap caveat applies

tools:
  built_in:
    - skill_search   # the LLM discovers runtime skills by capability text
    - skill_get      # the LLM pulls the full body of a named skill
```

### LLM-side discovery via meta-tools (Phase 107c)

After 107c the React planner runs on native provider tool-calling. The LLM doesn't ask "what skills do I have?" in prose — it calls the `skill_search` built-in meta-tool when it needs one. Opt those built-ins in (above) and the LLM gets a structured search surface backed by the FTS5 catalog. The meta-tools route through the SAME `SkillStore.Search` + `SkillStore.Get` path your operator-side `harbor skill import` populated, so there's no second-source-of-truth and identity-scoping (`(tenant, user, session)`) carries through.

`skill_search(query, tags?, limit?)` returns ranked candidates (`{name, title, description, score}`); `skill_get(name)` returns the full body. Tag filter is intersection. The LLM typically searches once, picks one or two names, and pulls full bodies in the same or next turn.

If you DON'T opt the meta-tools in, the existing pre-107c flow still works: the planner's per-turn `<skills_context>` section injects relevant skill bodies the planner pre-selected. The meta-tools are the LLM-driven discovery path; the prompt-injection path is the planner-driven one. Most operators want both — let the planner inject the obvious matches AND give the LLM the discovery escape hatch.

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
- **`harbor skill import ./Skills.md` says "duplicate skill name".** The catalog rejects duplicate names. Either rename the skill in the file OR remove the prior entry with `harbor skill rm <name>`.
- **The planner doesn't pick a skill I imported.** Either the skill body doesn't pattern-match the user's input (write more concrete trigger language) or `planner.max_steps` is too low to reach the skill-search turn.
- **Cross-session memory leakage suspected.** It can't happen — the SQL filter is at the driver. If you see it, file a bug with the SQL trace from `telemetry.log_level: debug` — a leak would be a P0 security issue.

## See also

- [`define-the-agent-yaml`](../define-the-agent-yaml/SKILL.md) — the `memory:` and `skills:` blocks in context.
- [`add-an-in-process-tool`](../add-an-in-process-tool/SKILL.md) — when a skill becomes "actually run code".
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) — the Memory tab + the Skills tab show what the planner saw on each turn.
- RFC §6.7 — the runtime skill subsystem design.
- RFC §6.6 — the memory subsystem design.
