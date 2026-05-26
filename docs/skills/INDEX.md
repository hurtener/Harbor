# Harbor operator skills

Skills (in the [Anthropic `claude-code` sense](https://docs.claude.com/en/docs/claude-code/skills)) for building agents with Harbor. Each one is a focused playbook: read it, follow the numbered steps, get a working surface.

These are **operator skills** — playbooks for humans (and AI coding agents) building Harbor agents. They are NOT the runtime [skill subsystem](https://github.com/hurtener/Harbor/blob/main/RFC-001-Harbor.md#67-skills) the planner consults during reasoning (those live in `internal/skills/`). The glossary entry in `docs/glossary.md` pins the distinction.

## The first-five-minutes adoption chain

A fresh contributor follows this exact chain to get from `git clone` to "I'm chatting with a working agent" in under five minutes:

1. [scaffold-a-harbor-agent](scaffold-a-harbor-agent/SKILL.md) — `harbor init` + `harbor scaffold` + the tiered yaml.
2. [run-the-dev-loop](run-the-dev-loop/SKILL.md) — `harbor dev` + `harbor console` + the token handshake.
3. [drive-the-playground](drive-the-playground/SKILL.md) — chat against the agent, upload files, steer mid-run.

That's the load-bearing path. Everything else is built around it.

## Skills by phase

### Start a project

- [scaffold-a-harbor-agent](scaffold-a-harbor-agent/SKILL.md) — start a fresh agent project from zero.
- [define-the-agent-yaml](define-the-agent-yaml/SKILL.md) — every field in `harbor.yaml` explained.

### Build the agent

- [add-an-in-process-tool](add-an-in-process-tool/SKILL.md) — typed Go tools the planner can call in-process.
- [wire-the-llm-provider](wire-the-llm-provider/SKILL.md) — OpenRouter / Anthropic / OpenAI / Azure / NIM via Bifrost.
- [configure-memory-and-skills](configure-memory-and-skills/SKILL.md) — multi-turn memory + the runtime skill catalog.

### Drive it interactively

- [run-the-dev-loop](run-the-dev-loop/SKILL.md) — `harbor dev` + Console attach, single- or multi-process.
- [drive-the-playground](drive-the-playground/SKILL.md) — chat, file uploads, multimodal dispatch, steering.

### Observe + debug

- [observe-with-the-console](observe-with-the-console/SKILL.md) — the 14-page Console tour.

### Ship

- [validate-and-package](validate-and-package/SKILL.md) — `harbor validate` + `make preflight` + production checklist.

### Build a custom frontend

- [use-the-harbor-protocol](use-the-harbor-protocol/SKILL.md) — talk to the Runtime directly; ship a chatbot in a day.

## Drift discipline

Per CLAUDE.md §18, any PR that changes a documented surface (a CLI verb, a Protocol method, a Console route, a `harbor.yaml` field) MUST update the matching skill in the **same PR**. Use this grep to find which skill is affected by a surface change:

```bash
grep -lE "surface: <changed-surface>" docs/skills/*/SKILL.md
```

The frontmatter audit (`scripts/skills/check-frontmatter.sh`, run as part of `make drift-audit`) catches shape drift. Content drift is human-reviewed; reviewers look for it.

## Convention

Each skill is `docs/skills/<slug>/SKILL.md` with Dockyard-style frontmatter:

```yaml
---
name: <slug-kebab-case>
description: "Use when <framing>"
license: Apache-2.0
metadata:
  framework: harbor
  surface: cli | agent-yaml | tools | mcp | llm | memory | playground | console | tasks | protocol
  verbs: "<harbor cli verbs>"
---
```

Sibling project [Dockyard](https://github.com/hurtener/dockyard) ships the same convention for MCP-server projects.
