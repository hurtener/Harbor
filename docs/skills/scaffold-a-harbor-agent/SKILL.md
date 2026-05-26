---
name: scaffold-a-harbor-agent
description: "Scaffold a new Harbor agent project with `harbor init` + `harbor scaffold`. Use when starting a fresh agent from zero — drops a tiered, commented `harbor.yaml`, the companion AGENTS.md / CLAUDE.md / README.md, and materialises the Go project so `harbor dev` boots a working runtime in under five minutes."
license: Apache-2.0
metadata:
  framework: harbor
  surface: cli
  verbs: "init scaffold validate"
---

# Scaffold a new Harbor agent

Harbor ships one static binary — `harbor` — and a four-step flow that lands a working agent in under five minutes:

```bash
harbor init <dir>                  # drop the tiered yaml + companion docs
$EDITOR <dir>/harbor.yaml          # pick a provider, set api_key
harbor validate <dir>/harbor.yaml  # fail-loud config check
harbor scaffold --name <name>      # materialise the Go project
```

The flow is deliberately bounded — `harbor init` does not assume what LLM you have keys for or what tools you need. It drops a *tiered* `harbor.yaml` (REQUIRED → COMMON → ADVANCED sections) plus three companion files (AGENTS.md / CLAUDE.md / README.md) that document the project for human contributors and AI coding agents alike. You edit the yaml, validate it, then scaffold a Go project around it.

## 1. Drop the tiered yaml

```bash
harbor init ~/my-first-agent
```

You get:

```text
~/my-first-agent/
├── harbor.yaml      # the tiered config (REQUIRED / COMMON / ADVANCED)
├── AGENTS.md        # contributor + AI-agent rules (verbatim mirror of CLAUDE.md)
├── CLAUDE.md        # same
└── README.md        # quickstart pointer for human contributors
```

The yaml has a `REQUIRED` block at the top with four commented LLM-provider examples (OpenRouter / Anthropic direct / OpenAI direct / Azure OpenAI). **Uncomment exactly one block and set `api_key: env.YOUR_KEY_NAME`.** Bifrost (Harbor's LLM driver) speaks many providers under one wire surface; the four examples cover the common cases. The full provider list is in `docs/CONFIG.md`.

`AGENTS.md` and `CLAUDE.md` are verbatim mirrors — Claude Code picks up `CLAUDE.md` automatically; other agents pick up `AGENTS.md`. Edit one, then `cp AGENTS.md CLAUDE.md` (or vice versa). If they drift, your project's drift-audit catches it.

## 2. Pick a provider, set an API key

```bash
$EDITOR ~/my-first-agent/harbor.yaml
```

Uncomment one provider block:

```yaml
llm:
  driver: bifrost
  provider: openrouter
  model: anthropic/claude-haiku-4.5
  api_key: env.OPENROUTER_API_KEY
  timeout: 60s
```

Then export the key:

```bash
export OPENROUTER_API_KEY=sk-or-...   # or set via .env
```

Harbor's LLM driver reads the env var at boot. A missing key fails LOUDLY at startup with `ErrMissingAPIKey` — there is no silent fallback to a stub (CLAUDE.md §13: test stubs are never production defaults). If you want a dev-only mock for first-clone convenience, set `HARBOR_DEV_ALLOW_MOCK=1` and see [`wire-the-llm-provider`](../wire-the-llm-provider/SKILL.md).

## 3. Validate before scaffolding

```bash
harbor validate ~/my-first-agent/harbor.yaml
```

`harbor validate` runs Harbor's config loader against your yaml with **file:line precision** error messages — a missing `llm.driver`, a malformed `tools.mcp_servers[0].command`, or a `memory.budget_tokens` set to a negative number all surface here. Run this every time you edit the yaml; the failure modes are usually obvious from the message.

If `validate` is clean, you're cleared to scaffold the Go project.

## 4. Materialise the Go project

```bash
cd ~/my-first-agent
harbor scaffold --name my-first-agent
```

`harbor scaffold` reads the yaml and drops a Go project around it:

```text
my-first-agent/
├── go.mod
├── go.sum
├── main.go                     # entry point — wires the agent
├── tools/                      # in-process tool implementations
│   └── hello/hello.go          # one example tool (typed args, typed result)
├── harbor.yaml                 # the same yaml init dropped
├── AGENTS.md / CLAUDE.md / README.md
└── .gitignore
```

The example `hello` tool exercises the typed-tool contract — input struct, output struct, handler function. Use it as the template for your real tools (see [`add-an-in-process-tool`](../add-an-in-process-tool/SKILL.md)).

## 5. Boot the runtime

```bash
go build -o bin/harbor ./cmd/harbor   # only the first time; harbor dev re-builds incrementally
harbor dev
```

`harbor dev` boots the Runtime on `127.0.0.1:18080` and mints an ephemeral `HARBOR_DEV_TOKEN` (printed on stderr). The Console is a separate process — see [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) for the attach flow.

## Common failure modes

- **`harbor init` overwrote my edits.** It won't — `harbor init <dir>` refuses to write into a non-empty directory unless `--force` is passed. The default is fail-safe.
- **`harbor validate` says `unknown field "tools"`.** You're either on an old `harbor` binary or your yaml uses a renamed key. Run `harbor version` and check the upgrade notes; field renames are flagged in the CHANGELOG.
- **`harbor dev` exits with `ErrMissingAPIKey` immediately.** Your provider block declares `api_key: env.OPENROUTER_API_KEY` (or similar) but the env var isn't set in the shell that ran `harbor dev`. Check with `echo $OPENROUTER_API_KEY` before re-running. Source your `.env` if you keep keys there.
- **`harbor scaffold` says "name must match the project directory".** The `--name` flag is the Go module name, not a free string — keep it in `kebab-case` and match the directory name to keep imports clean.

## See also

- [`define-the-agent-yaml`](../define-the-agent-yaml/SKILL.md) — every field in `harbor.yaml` explained.
- [`wire-the-llm-provider`](../wire-the-llm-provider/SKILL.md) — provider selection, models, the mock-vs-real posture.
- [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) — `harbor dev` + Console attach.
- [`drive-the-playground`](../drive-the-playground/SKILL.md) — chat against your agent once it's running.
- Sibling project: Dockyard's [`scaffold-a-server`](https://github.com/hurtener/dockyard) — the same flow for MCP-server projects (different product, identical convention).
