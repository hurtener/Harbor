# Research briefs — subsystem index

Reverse map from "I'm authoring a phase about X" → "these are the briefs to read first."

The briefs themselves live alongside this file (`docs/research/01..08.md`). Each brief is a phase-planning research artifact distilled from the predecessor's source code; collectively they encode the hard-won lessons Harbor inherits. **A phase plan that doesn't list at least one informing brief is a drift signal** — the drift-audit script enforces this.

## Briefs at a glance

| Brief | Title | Word count | Date |
|------:|-------|-----------:|------|
| 01 | Core runtime + streaming | 2798 | 2026-05-08 |
| 02 | Planner + steering + HITL | 3464 | 2026-05-08 |
| 03 | Tools + integrations + LLM client | 2677 | 2026-05-08 |
| 04 | Memory + skills | 2989 | 2026-05-08 |
| 05 | State, tasks, artifacts, sessions, distributed | 2872 | 2026-05-08 |
| 06 | Events, observability, devx | 2620 | 2026-05-08 |
| 07 | Code-level tool calling (the elegance principle) | 3247 | 2026-05-08 |
| 08 | LLM client validation (bifrost) | 1319 | 2026-05-08 |

## Subsystem → briefs reverse index

When authoring a phase plan whose subsystem matches the left column, **read at least the briefs in the right column** before drafting. The "RFC §" column tells you which RFC section anchors the phase.

| Subsystem | RFC § | Briefs |
|-----------|-------|--------|
| Core runtime — engine, messages, streaming, routers, concurrency, playbooks | §6.1 | **01** |
| Planner interface + Trajectory + RunContext | §6.2 | **02**, 07 |
| Steering and unified pause/resume | §3.3, §6.3 | **02** |
| Tool catalog + transports (in-proc / HTTP / MCP / A2A) | §6.4 | **03**, **07** |
| LLM client + provider correction | §6.5 | **03**, **07**, **08** |
| Memory subsystem (strategies, scoping, drivers) | §6.6 | **04** |
| Skills subsystem (catalog, virtual directory, importer, generator) | §6.7 | **04** |
| Tasks (unified foreground/background) | §6.8 | **05** |
| Sessions and SessionManager | §6.9 | **05** |
| Artifacts (mandatory routing) | §6.10 | **05** |
| StateStore (in-mem / SQLite / Postgres) | §6.11 | **05** |
| Distributed contracts (V1: contracts only) | §6.12 | **05** |
| Typed event bus | §6.13 | **06** |
| Telemetry (slog + OTel) | §6.14 | **06** |
| Harbor Protocol (Console-decoupling, transport, versioning) | §5 | 06, **07** |
| Console — observability + control plane UI | §7 | **06** (Playground anti-pattern) |
| CLI — `harbor dev`, scaffolding, deployment | §8 | **06** |
| Persistence triad — interfaces + conformance | §9 | **05** |
| Identity & isolation contract | §4 | (cross-cutting; every brief touches identity) |

Bold = primary brief for that subsystem. Non-bold = relevant context.

## How briefs interact with the RFC and phase plans

- **Briefs** are *authoritative for context*, not for design (AGENTS.md §2). They encode what the predecessor did, what worked, what broke, and what Harbor must inherit cleanly vs. fix.
- **The RFC** distills brief findings into Harbor's settled design.
- **A phase plan** translates an RFC section into a shippable unit of work.

If a phase plan's design conflicts with a brief finding, the phase plan must explicitly justify the departure (template's "Findings I'm departing from" section). Silent departure is forbidden.

## Cross-fork synthesis (load-bearing across multiple briefs)

These findings emerged across multiple briefs and are recorded in memory under `harbor_research_briefs.md`:

1. **Unified pause/resume primitive** (HITL + tool OAuth + A2A AUTH_REQUIRED + steering PAUSE) — runtime level, see §3.3 and §6.3.
2. **Fail-loudly is a runtime principle** — explicit errors over silent degradation, see RFC §5 prose.
3. **No optional-capability ceremony when all V1 drivers will implement everything** — see §4.4 Extensibility seams in AGENTS.md.
4. **Console-as-protocol-client is forced by source evidence** — Playground anti-pattern, see RFC §5 + §7.
5. **OTel from t=0** — bake in, do not retrofit, see RFC §6.14.
6. **Planner concerns leak into runtime in the source** — Harbor extracts to keep `Planner.Next` truly swappable, see RFC §6.1 + §6.2.
7. **Code-level tool calling** — runtime owns dispatch; LLM is decision-maker not runner, see RFC §6.4.

## Adding a new brief

A new brief lands when:
- A new subsystem is being scoped that's not covered above.
- An empirical validation produces phase-shaping findings (brief 08 is an example).
- A surface in the predecessor needs deeper investigation than the existing briefs covered.

When you add a brief, update this index in the same PR.
