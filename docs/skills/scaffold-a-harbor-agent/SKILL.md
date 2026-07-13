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
harbor init --target <dir>         # drop the tiered yaml + companion docs
$EDITOR <dir>/harbor.yaml          # pick a provider, set api_key
harbor validate <dir>/harbor.yaml  # fail-loud config check
harbor scaffold --name <name>      # materialise the Go project
```

The flow is deliberately bounded — `harbor init` does not assume what LLM you have keys for or what tools you need. It drops a *tiered* `harbor.yaml` (REQUIRED → COMMON → ADVANCED sections) plus three companion files (AGENTS.md / CLAUDE.md / README.md) that document the project for human contributors and AI coding agents alike. You edit the yaml, validate it, then scaffold a Go project around it.

## 1. Drop the tiered yaml

```bash
harbor init --target ~/my-first-agent
```

You get:

```text
~/my-first-agent/
├── harbor.yaml      # the tiered config (REQUIRED / COMMON / ADVANCED)
├── AGENTS.md        # contributor + AI-agent rules (verbatim mirror of CLAUDE.md)
├── CLAUDE.md        # same
└── README.md        # quickstart pointer for human contributors
```

The yaml has a `REQUIRED` block at the top with four commented LLM-provider examples (OpenRouter / Anthropic direct / OpenAI direct / NVIDIA NIM). **Uncomment exactly one block and set `api_key: env.YOUR_KEY_NAME`.** Bifrost (Harbor's LLM driver) speaks many providers under one wire surface; the four examples cover the common cases. The full provider list is in `docs/CONFIG.md`.

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
├── README.md
├── agent.go                    # your agent code (worked EchoAgent + RegisterTools)
├── agent_test.go               # harbortest-driven smoke test
├── tools/                      # one typed stub per tools.custom[] yaml entry
│   └── <name>.go / <name>_test.go
└── harbor.yaml                 # the same yaml init dropped (copied verbatim)
```

When the yaml declares tools (`tools.built_in` / `tools.custom`), `agent.go` gains a generated `RegisterTools` function and each **custom** tool gets a typed stub (input struct, output struct, handler) under `tools/`. The generated imports are the public `sdk/` facade paths, so the project builds as a standalone external module. When the yaml declares custom tools, the generated `agent_test.go` also gains a register-and-dispatch test that calls `RegisterTools` and invokes a declared tool **through the tool catalog** — so `go test ./...` proves your tools are actually wired, not just that the project compiles. Keep that test green as you replace the stubs with real tools. Use the stubs as the template for your real tools (see [`add-an-in-process-tool`](../add-an-in-process-tool/SKILL.md)).

**`RegisterTools` carries your COMPILED tools only — never built-ins.** A tool you opt into under `tools.built_in` (`clock.now`, `artifact_fetch`, the `skill_*` set, …) is registered **by the runtime** at boot, straight from config, with its backing stores (skill store, artifact store, event bus, redactor) already threaded in. Registering one in `RegisterTools` as well hands the catalog the same name twice and the boot dies with `duplicate tool name`. The registrar seam exists for the tools *your module compiles*; the yaml is the whole opt-in for everything Harbor ships.

`go.mod` requires the Harbor release that scaffolded the project, so `go mod tidy && go build ./...` resolves straight from the module proxy — no edit needed. (Building against a local Harbor checkout? Uncomment the `replace` at the bottom of `go.mod`.)

## 5. Boot the runtime

```bash
go build -o bin/harbor ./cmd/harbor   # only the first time; harbor dev re-builds incrementally
harbor dev
```

`harbor dev` boots the Runtime on `127.0.0.1:18080` and mints an ephemeral `HARBOR_DEV_TOKEN` (printed on stderr). The Console is a separate process — see [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) for the attach flow.

## 6. Serve the Protocol from your own binary (`--with-server`)

`harbor dev` is the development loop; when you want your agent — with its compiled in-process Go tools — to serve the Harbor Protocol from its **own** binary at parity with the stock `harbor serve`, add `--with-server`:

```bash
harbor scaffold --name my-first-agent --with-server
```

This is purely additive — the default (flagless) scaffold stays headless. `--with-server` also emits a serving entry point:

```text
my-first-agent/
├── cmd/my-first-agent/
│   └── main.go        # loads harbor.yaml, blank-imports sdk/drivers/prod,
│                      # passes agent.RegisterTools to sdk/server.Open, serves
└── ... (the usual skeleton)
```

`main.go` mirrors `harbor serve`'s `--config` / `--port` / `--bind` flags and reaches the Protocol through the public `sdk/server` facade, so the module builds and serves as a standalone external binary. It is **production-only by construction**: `server.Open` always builds the JWKS verifier from `identity.*` and fails loud when the JWKS source is missing — there is no dev signer and no mock. Your `RegisterTools` runs at the pre-policy catalog seam, so every tool keeps its declared `tools.entries` approval / OAuth / policy wrapping (see [`add-an-in-process-tool`](../add-an-in-process-tool/SKILL.md)).

Because the server verifies every request against your identity provider's JWK Set, the **local-development loop** is the three-command `harbor token` flow — the same loop a self-hosted `harbor serve` operator uses:

```bash
# 1. Generate a signing keypair + public JWK Set.
harbor token keygen --out ./keys

# 2. In harbor.yaml, point identity.jwks_file at the emitted set
#    (and remove identity.jwks_url — set exactly one source):
#      identity:
#        jwks_file: ./keys/jwks.json

# 3. Build + run the server.
go build ./cmd/my-first-agent && ./my-first-agent --config ./harbor.yaml

# 4. Mint a short-lived JWT whose iss/aud match harbor.yaml, then call the
#    Protocol with it (Authorization: Bearer <token>):
harbor token mint --key ./keys/private.pem \
  --tenant acme --user alice --session s1 \
  --issuer <identity.issuer> --audience <identity.audience>
```

See [`configure-production-identity`](../configure-production-identity/SKILL.md) for the JWKS posture and [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md) for driving the wire surface.

## Common failure modes

- **`harbor init` overwrote my edits.** It won't — `harbor init --target <dir>` refuses to overwrite any of its four target files: if `harbor.yaml`, `AGENTS.md`, `CLAUDE.md`, or `README.md` already exists it errors out (per-file, fail-loud). Delete the conflicting file or pick a fresh `--target` directory and re-run.
- **`harbor validate` says `unknown field "tools"`.** You're either on an old `harbor` binary or your yaml uses a renamed key. Run `harbor version` and check the upgrade notes; field renames are flagged in the CHANGELOG.
- **`harbor dev` exits with `ErrMissingAPIKey` immediately.** Your provider block declares `api_key: env.OPENROUTER_API_KEY` (or similar) but the env var isn't set in the shell that ran `harbor dev`. Check with `echo $OPENROUTER_API_KEY` before re-running. Source your `.env` if you keep keys there.
- **`harbor scaffold` says `name must match ^[a-z0-9][a-z0-9_-]{0,63}$`.** The `--name` flag is the Go module name, not a free string — it must start with a lowercase letter or digit and contain only lowercase letters, digits, hyphens, or underscores (up to 64 chars). It does *not* have to match the directory name; `kebab-case` keeps imports clean.

## See also

- [`define-the-agent-yaml`](../define-the-agent-yaml/SKILL.md) — every field in `harbor.yaml` explained.
- [`wire-the-llm-provider`](../wire-the-llm-provider/SKILL.md) — provider selection, models, the mock-vs-real posture.
- [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) — `harbor dev` + Console attach.
- [`drive-the-playground`](../drive-the-playground/SKILL.md) — chat against your agent once it's running.
- Sibling project: Dockyard's [`scaffold-a-server`](https://github.com/hurtener/dockyard) — the same flow for MCP-server projects (different product, identical convention).
