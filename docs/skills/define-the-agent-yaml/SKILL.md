---
name: define-the-agent-yaml
description: "Walk every field in `harbor.yaml` — REQUIRED (identity + llm), COMMON (planner / memory / state / tools / skills / governance), ADVANCED (server / telemetry / artifacts / events / sessions / tasks / distributed). Use when editing the agent config beyond the scaffolded defaults."
license: Apache-2.0
metadata:
  framework: harbor
  surface: agent-yaml
  verbs: "validate"
---

# Define the agent yaml

`harbor.yaml` is the single declarative file Harbor's runtime reads at boot. It's tiered by importance — REQUIRED at the top (the binary won't boot without it), then COMMON (the knobs you'll edit most), then ADVANCED (every other lever). Every absent key gets a documented default; the only fields you MUST set are `identity` + `llm`.

Pair this skill with `harbor validate ./harbor.yaml` — the validator is the loudest, most file:line-precise feedback you'll get on a yaml mistake. Run it after every edit.

## REQUIRED — identity + llm

### `identity`

The identity block configures JWT verification — the Runtime's authentication boundary. Every Protocol call carries a JWT; identity decides what algorithm to trust and where to fetch the public key.

```yaml
identity:
  jwt_algorithms: [RS256]                      # allowlist: RS256/RS384/RS512/ES256/ES384/ES512
  issuer: https://issuer.example.com           # exact match against the JWT `iss` claim
  audience: my-agent                           # exact match against the JWT `aud` claim
  jwks_url: https://issuer.example.com/.well-known/jwks.json
```

For local dev, the scaffold drops placeholders — these pass `harbor validate` but reject any real token. `harbor dev` mints its own ephemeral signing key and bypasses the issuer/jwks_url path entirely (see [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md)). For production, point `issuer` + `jwks_url` at your real IdP. HS256 / `none` are forbidden — the loader rejects them at boot.

### `llm`

Pick exactly one provider block from the scaffolded examples. Bifrost (Harbor's LLM driver) speaks many providers under one wire surface; you swap providers by swapping the block, not by changing code.

```yaml
llm:
  driver: bifrost                              # only driver shipped in V1.1
  provider: openrouter
  model: anthropic/claude-haiku-4.5
  api_key: env.OPENROUTER_API_KEY              # `env.NAME` resolves via os.Getenv
  timeout: 60s
  model_profiles:                              # optional but recommended
    anthropic/claude-haiku-4.5:
      context_window_tokens: 200000            # planner uses this for budgeting
```

`model_profiles` is what the planner consults for context-window budgeting — set it for every model you use; otherwise the planner falls back to a conservative default (32k tokens) that under-uses big models. See [`wire-the-llm-provider`](../wire-the-llm-provider/SKILL.md).

## COMMON — planner, memory, state, tools, skills, governance

### `planner`

V1.1 ships one planner: `react`. The block tunes its budget and gives the planner extra domain guidance.

```yaml
planner:
  max_steps: 12                                # how many reasoning turns before forced finalisation
  extra_guidance: |
    Voice/tone rules. Hard negatives. Safety notes.
    Operator-supplied; injected into the planner's system prompt.
  reasoning_replay: never                      # or `text` to round-trip the trace into the next turn
```

### `memory`

Multi-turn context. Default strategy is `none` (no memory across runs in a session); flip to `rolling_summary` for chatbot agents that need it.

```yaml
memory:
  driver: sqlite                               # or `inmem` (dev default) / `postgres`
  dsn: ./my-agent-memory.sqlite                # MOVE outside the project dir to avoid the WAL trap
  strategy: rolling_summary                    # or `truncation` / `none`
  budget_tokens: 8000                          # max tokens replayed per turn
```

The WAL trap: `dsn: ./...` inside the project directory triggers `harbor dev`'s fsnotify watcher and reboots the runtime in a loop. Default-drop the DSN at `/tmp/harbor-validation/my-agent-memory.sqlite` or `~/.harbor/my-agent-memory.sqlite`. See [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) §3.

### `state`

Durable run/task/session state. The default `inmem` driver is process-local — runs disappear on restart. Flip to SQLite for single-node persistence, Postgres for multi-replica.

```yaml
state:
  driver: sqlite
  dsn: /tmp/harbor-validation/my-agent-state.sqlite   # WAL trap caveat applies
```

### `tools`

Two sources: `built_in` (tools shipped in the harbor binary; opt-in by name) and `mcp_servers` (MCP southbound subprocesses Harbor spawns at boot).

```yaml
tools:
  built_in:
    - clock.now
    - text.echo
  mcp_servers:
    - name: weather
      transport_mode: stdio
      command: [uvx, mcp-weather]
      env: { WEATHER_API_KEY: "${env.WEATHER_API_KEY}" }
      timeout: 60s
      auto_register_with_planner: true                # default true; the planner discovers tools at boot
```

Built-in tools live in the harbor binary — list `clock.now` to enable, omit to disable. MCP servers are external processes; see [`configure-memory-and-skills`](../configure-memory-and-skills/SKILL.md) for the skill-vs-tool axis.

### `skills`

Skills are token-savvy DB-backed playbooks the planner searches by name. Distinct from "operator skills" (the docs/skills/ directory you're reading right now) — these are *runtime* skills the planner consults during a reasoning turn.

```yaml
skills:
  driver: localdb
  dsn: ./my-agent-skills.sqlite                # WAL trap caveat applies
```

### `governance`

Per-identity cost ceilings + rate limits + max-token caps, keyed by tier.

```yaml
governance:
  default_tier: free
  identity_tiers:
    free:
      budget_ceiling_usd: 5.00                 # cap per (tenant, user) per billing window
      max_tokens: 4096                         # planner enforced; tool calls counted
      rate_limit:
        requests_per_minute: 30
```

Empty `identity_tiers: {}` = no enforcement.

## ADVANCED — every other lever

The scaffold drops a commented summary of advanced defaults. The full reference is `docs/CONFIG.md`. The blocks you most often touch:

- **`server`**: `bind_addr` (default `127.0.0.1:8080` for `harbor serve`; `harbor dev` always binds `:18080`), `allowed_origins` (CORS allowlist for multi-process Console), `shutdown_grace_period` (drain timeout for hot reload).
- **`telemetry`**: `log_format` (`json` / `text`), `log_level` (`debug` / `info` / `warn` / `error`), `service_name` (OTel resource).
- **`artifacts`**: `driver` (`inmem` / `fs` / `sqlite` / `postgres`), `heavy_output_threshold_bytes` (the LLM-edge context-leak guard, default 32768 — see RFC §6.5).
- **`events`**: `driver` (`inmem` / `sqlite` / `postgres`); events power the Console's live streaming.
- **`sessions`**: `idle_ttl` (default 24h), `hard_cap` (default 720h / 30d), `sweep_interval`.
- **`tasks`**: `driver` (`inprocess` only in V1.1).
- **`distributed`**: `bus_driver` + `remote_driver` (V1.1 ships `loopback` only; durable bus + A2A wire are post-V1).

## Validation — the loud loop

```bash
harbor validate ./harbor.yaml
```

Failure modes the validator catches:

- **Required field missing** — `llm.driver`, `llm.provider`, `llm.model`, `identity.issuer`, etc.
- **Type mismatches** — `memory.budget_tokens: "8000"` (string instead of int).
- **Enum violations** — `memory.strategy: "summary"` (not one of `none` / `truncation` / `rolling_summary`).
- **Bound violations** — `governance.identity_tiers.free.budget_ceiling_usd: -1` (negative).
- **Cross-field constraints** — `memory.driver: sqlite` without `memory.dsn`.

Every error carries the `file:line` of the offending key. Fix one, re-run, repeat until clean.

## Common failure modes

- **`harbor validate` says `unknown field "X"`.** Either a typo (check indentation — YAML is whitespace-sensitive) or the field belongs in a different block. Check `docs/CONFIG.md` for the canonical block.
- **`harbor dev` boots but every Protocol call returns 401.** Your `identity` block points at a real IdP but the JWKS isn't reachable. For local dev, use the dev-token flow (see [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md)) — the issuer/jwks_url path is for production.
- **`harbor dev` reboots in an infinite loop.** SQLite WAL trap — `dsn:` inside the project directory. Move it outside.
- **A model swap silently degrades.** You forgot to set `model_profiles.<model>.context_window_tokens` for the new model — the planner falls back to a 32k default. Add the profile.

## See also

- [`scaffold-a-harbor-agent`](../scaffold-a-harbor-agent/SKILL.md) — drops the tiered yaml in the first place.
- [`wire-the-llm-provider`](../wire-the-llm-provider/SKILL.md) — the full provider matrix + the mock vs real posture.
- [`configure-memory-and-skills`](../configure-memory-and-skills/SKILL.md) — the memory strategies + runtime skill catalog in depth.
- [`validate-and-package`](../validate-and-package/SKILL.md) — preflight before shipping.
- The full per-key reference: `docs/CONFIG.md`.
