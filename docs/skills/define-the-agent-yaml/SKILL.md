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
  jwks_max_stale: 1h                           # OPTIONAL — max age a cached JWKS snapshot is honored without a refresh (0/omit = 1h default)
```

For local dev, the scaffold drops placeholders — these pass `harbor validate` but reject any real token. `harbor dev` mints its own ephemeral signing key and bypasses the issuer/jwks_url path entirely (see [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md)). For production, point `issuer` + `jwks_url` at your real IdP. HS256 / `none` are forbidden — the loader rejects them at boot. `jwks_max_stale` bounds how long the verifier keeps trusting a cached signing key when your IdP is unreachable: past this age it fails closed (rejects tokens) rather than serving a key your IdP may have revoked. Omit it (or set `0`) for the safe 1h default; a negative or below-1m value is rejected at boot. There is no way to disable the ceiling — it tunes, it does not remove. Pair a tight ceiling with overlapping IdP signing keys and short token TTLs.

### `llm`

Pick exactly one provider block from the scaffolded examples. Bifrost (Harbor's LLM driver) speaks many providers under one wire surface; you swap providers by swapping the block, not by changing code.

```yaml
llm:
  driver: bifrost                              # only driver shipped in V1.1
  provider: openrouter
  model: anthropic/claude-haiku-4.5
  api_key: env.OPENROUTER_API_KEY              # `env.NAME` resolves via os.Getenv
  timeout: 60s
  model_profiles:                              # effectively REQUIRED — one entry per model you use
    anthropic/claude-haiku-4.5:
      context_window_tokens: 200000            # the runtime uses this for context-window budgeting
```

`model_profiles.<llm.model>.context_window_tokens` is what the runtime consults for context-window budgeting. There is no silent fallback: a model with no matching `model_profiles` entry hard-fails the FIRST LLM call with `ErrUnsupportedModel` (fail-loudly — the error names the missing `model_profiles[<model>]` key). Set a profile for every model you reference. See [`wire-the-llm-provider`](../wire-the-llm-provider/SKILL.md).

## COMMON — planner, memory, state, tools, skills, governance

### `planner`

V1.1 ships one planner: `react`. The block tunes its budget and gives the planner extra domain guidance.

```yaml
planner:
  max_steps: 12                                # planner step tranche: steps per cycle before the run is
                                               # parked for continuation — a typed constraints_conflict pause
                                               # (cause max_steps_exceeded), never forced finalisation;
                                               # 0 (default) = driver default (12)
  extra_guidance: |
    Voice/tone rules. Hard negatives. Safety notes.
    Operator-supplied; injected into the planner's system prompt.
  reasoning_replay: never                      # or `text` to round-trip the trace into the next turn
  token_budget: 0                              # 0 (default) = trajectory compression OFF; > 0 = once the
                                               # trajectory's token estimate exceeds it, the runtime
                                               # compacts step history into a summary (one compression
                                               # per run; needs the llm block)
```

`max_steps` is a **continuable tranche**, not a termination knob. When a tranche of planner steps is consumed without a terminal Finish, the run is **parked** through the unified pause primitive — a typed `constraints_conflict` pause carrying `{cause: max_steps_exceeded, max_steps, steps_observed}` — instead of being forced to finalise. An authorised RESUME continues the SAME run with a fresh tranche (the tranche counter resets; the cumulative trajectory is untouched), so long-running work spans repeated cycles as ONE run (D-418); a fresh process cannot resume a parked run and answers the typed `ErrRestartUnavailable` (D-417). Zero (the default) resolves to the driver default (12) and never means unbounded; the planner-side per-tranche breaker ends the cycle with the typed `NoPath` Finish (`max_steps_exceeded`), and the runtime's outer `ErrMaxStepsExceeded` guard (default 64) remains the runaway backstop when tranche pausing is unavailable. See `docs/CONFIG.md` › `planner.max_steps`.

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

Three sources: `built_in` (tools shipped in the harbor binary; opt-in by name), `mcp_servers` (MCP southbound subprocesses Harbor spawns at boot), and `http_manifests` (UTCP-style YAML manifests describing HTTP endpoints as tools, loaded at boot — see `docs/CONFIG.md` › `tools.http_manifests`; `tools.entries[]` OAuth/approval/loading bindings apply to manifest tools by name like any other).

```yaml
tools:
  built_in:
    - clock.now
    - text.echo
  mcp_servers:
    - name: weather
      transport_mode: stdio                           # auto / sse / streamable_http / stdio
      command: [uvx, mcp-weather]                      # argv form; required for stdio
      headers: { Authorization: "Bearer ${env.WEATHER_TOKEN}" }   # HTTP transports; redacted as secrets
      keep_alive: 30s                                  # session-ping interval; 0 disables
      policy:                                          # optional per-server tool reliability defaults
        timeout_ms: 60000                              # per-attempt deadline (default 30000)
        max_attempts: 4                                # total attempts incl. the first
```

The planner discovers every MCP server's tools at boot — there's no per-server enable flag; listing the server registers its tools. Built-in tools live in the harbor binary — list `clock.now` to enable, omit to disable. MCP servers are external processes; see [`configure-memory-and-skills`](../configure-memory-and-skills/SKILL.md) for the skill-vs-tool axis.

> **Southbound OAuth binding — the credential-sink allow-list is mandatory (D-300).** When an `mcp_servers[]` connection sets `oauth_provider: <name>` (a per-identity bearer injected per call), the named `tools.oauth_providers[]` entry MUST declare a non-empty `allowed_downstream_hosts` that lists the connection's host — the credential-plane invariant is that no admin-writable field determines where a credential is sent, so the sink set is boot-pinned. A bound provider with no allow-list, or a connection host that isn't listed, fails at **boot** (not at first call). See `docs/CONFIG.md` › `tools.oauth_providers` and `tools.oauth_credential_brokers`.
>
> **Per-user credential injection for receiver-style servers (D-341).** A server that expects its credential handed to it DIRECTLY on each request — an arbitrary header, an `Authorization: Basic` value, or a `_meta` key — instead of pulling it via RFC 8693 uses an `mcp_servers[]` `injection:` mapping instead of `oauth_provider`. The runtime SOURCES the acting user's credential from the named broker per call (the SAME per-user broker-pull as `oauth_provider` — never held, never client-pushed) and INJECTS it in the declared, **non-secret** form: `provider` (a declared `tools.oauth_providers[]` broker) + `form: header|basic|meta` + the target key (`header:` / `basic_username:` / `meta_key:`). Only the pulled value is secret. It is **mutually exclusive** with `oauth_provider` / `tool_oauth_providers` / a static `Authorization` header (one auth mode per connection), the broker's `allowed_downstream_hosts` must list the connection host (fail-closed), and the target header / `_meta` leaf key MUST be a redaction-covered credential key (a `-api-key` / `-token` / `-secret`-shaped segment) so the audit redactor always holds the injected value to `***`. A broker error fails the call loud (never an unauthenticated send). See `examples/harbor.yaml` › `tools.mcp_servers[].injection`.
>
> **`_meta` keys are PATHS, and two rules now fail at BOOT (D-362).** A dotted key nests: `injection.meta_key: vendor.api_key` writes `_meta.vendor.api_key`, and — as of v1.24 — a dotted `meta_annotations` key does the same instead of landing as one literal flat key. One `_meta` namespace, one meaning, whichever mechanism wrote into it. **A `harbor.yaml` that validated before can now fail to start**, because two constraints that previously existed only on the `agent_config.add_mcp_connection` wire path now apply at boot as well:
>
> - **A 16-segment depth cap on any declared `_meta` path**, `injection.meta_key` included. A boot-declared path deeper than the cap used to be accepted where the identical wire-declared one was refused; both now consult one constant. No shipped config nests past two segments.
> - **No two declared `_meta` paths on one connection may collide** — be equal, or be a prefix of one another — across `meta_annotations` keys AND `injection.meta_key`. So `{vendor: x, vendor.id: y}` is refused, and so is a flat `vendor` annotation alongside `injection.meta_key: vendor.api_key`. That second shape previously discarded the operator's annotation silently at merge time and made the merged `_meta` depend on map iteration order.
>
> Also newly refused: a key whose whole value **or any dot-segment** is reserved (`tenant.foo` now fails; the whole-key check is retained, because it is the arm that sees the spec-reserved `io.modelcontextprotocol/` namespace), and a path with an empty segment (`a..b`). Run `harbor validate` against your config before upgrading. See `docs/CONFIG.md` › `tools.mcp_servers[].meta_annotations`.
>
> **Handing an MCP tool a large document by reference (D-359).** A remote tool that must READ something large — a stored CSV, an uploaded PDF, a prior tool's materialised result — should not be given it through the model's context. Declare the connection byte-eligible and map the parameters that carry artifact bytes:
>
> ```yaml
> tools:
>   mcp_servers:
>     - name: docstore
>       transport_mode: streamable_http
>       url: https://docs.internal.example.com/mcp
>       artifact_byte_eligible: true
>       artifact_params:
>         ingest_document: [content]     # server-side tool name -> parameter names
> ```
>
> The model writes an artifact **id**; the runtime resolves it under the run's own `(tenant, user, session)` and writes the resolved bytes into the outbound call as RFC 4648 §4 standard base64. `artifact_params` REQUIRES `artifact_byte_eligible` (a mapping without it is refused, never silently ignored — the flag IS the containment boundary), both are **http-only** (refused on `stdio`, explicit or auto-selected from a command-only config), and each mapped parameter must be DECLARED and declared **string-typed** in the server's own discovered `inputSchema` — an absent or non-string parameter fails the attach loudly rather than the first call silently. One substituted value is bounded by `tools.mcp_artifact_egress_max_bytes` (default 8 MiB); an oversize artifact **fails loud rather than truncating**, because a half-delivered document is a corruption, not a bounded read. Every substitution emits `mcp.artifact_egressed` — ids, sizes and a `sha256:` digest, never the bytes — **fail-closed before the wire request**, so a substitution that could not be recorded does not happen. Two things to weigh before enabling it: artifact bytes are stored as authored (unredacted), so a byte-eligible connection can move whatever an artifact contains; and a byte-mapped parameter is **not** reachable from an MCP App's tool callback (no run, so no seated resolver — the call fails loud). It widens which SERVERS may receive a user's content, never which artifacts a run can reach. See `docs/CONFIG.md` › `tools.mcp_servers[].artifact_byte_eligible` and `tools.mcp_artifact_egress_max_bytes`.

`runtime.hooks.run_completion.{tool, timeout}` names a catalog tool that receives every run's transcript at completion — see `docs/CONFIG.md` › Runtime. `runtime.naming.{auto, after_turns, repeat_every, max_repetitions, max_title_len, model}` is the opt-in, default-off **session auto-naming** fleet default: with it enabled, the runtime titles a session itself at each run's terminal boundary via one governed LLM call over a bounded transcript digest (`max_repetitions` is required `>= 1` whenever `repeat_every > 0` — no unlimited value). A per-agent agent-config `naming` section overrides this default; see `docs/CONFIG.md` › Runtime.

### `skills`

Skills are token-savvy DB-backed playbooks the planner searches by name. Distinct from "operator skills" (the docs/skills/ directory you're reading right now) — these are *runtime* skills the planner consults during a reasoning turn.

```yaml
skills:
  driver: localdb
  dsn: ./my-agent-skills.sqlite                # WAL trap caveat applies
  directory:                                   # optional — the per-turn <skills_context> browse window
    pinned: [triage-incident]                  # anchored first, declaration order
    max_entries: 10                            # 0/unset → planner.skills_context_max (default 5)
    selection: pinned_then_recent              # the one wired value (pinned_then_top is rejected: not yet wired)
```

Ingest skills with `harbor skill import <path>` and remove them with `harbor skill rm <name>` — both operate on this block's store.

> **Boot-declared agent skill packs (HA-66, v1.28).** `skills.boot_agent_packs`
> loads node-local operator skills for the resolved boot/default agent at
> boot. It REQUIRES a configured `skills.driver` + `skills.dsn` — a
> declaration on an unconfigured skills block fails boot loud. Each entry
> binds EXACTLY ONE `(tenant_id, agent_id)`: an exact tenant key and the
> runtime-resolved boot agent (no wildcards, no tenant-wide application;
> honored only when the effective agent resolves to the declared
> `agent_id`). `directory` is the pack root, resolved **relative to the
> config file's directory — never the process CWD**; each `include` entry is
> one package directory containing exactly one case-sensitive top-level
> regular UTF-8 `SKILL.md`, and the v1.28 contract is resource-free (no
> auxiliary resource payloads ride alongside the skill body). Loading is
> strict, eager, and immutable **before readiness**: symlinks / hardlinks /
> special files / traversal / duplicates are rejected, the loaded set is
> fixed for the process lifetime, and file changes take effect only on a
> restart / new runtime — never hot-reloaded. Boot packs stay node-local:
> each node reloads them from its own files at boot, so a change on one node
> never converges the others even over a shared Postgres skill store.
> Boot-pack items compose with the agent's durable revision into ONE
> operator tier (same canonical name + same content hash dedupes; differing
> hashes fail loud; combined count capped at 256). Headless `RunOnce`
> against a boot-pack agent is unsupported and fails loud. Full contract:
> `docs/CONFIG.md` › `skills.boot_agent_packs`.

### `governance`

Per-identity cost ceilings + rate limits + max-token caps, keyed by tier.

> **Declared tiers are enforced.** A populated `identity_tiers` block
> composes the enforcement subsystem at boot: the budget ceiling fails
> over-budget calls with `ErrBudgetExceeded`, the token bucket with
> `ErrRateLimited`, and the per-call cap with `ErrMaxTokensExceeded` — each
> emitting a matching `governance.*` event you can watch on the events
> stream. The same block drives the read-only `governance.posture` Protocol
> surface.

```yaml
governance:
  default_tier: free
  identity_tiers:
    free:
      budget_ceiling_usd: 5.00                 # enforced cap per (tenant, user, session)
      max_tokens: 4096                         # per-call MaxTokens cap
      rate_limit:                              # token bucket per (identity, model)
        capacity: 100000
        refill_tokens: 50000
        refill_interval: 1h
```

Empty `identity_tiers: {}` = fully latent (the default).

## ADVANCED — every other lever

The scaffold drops a commented summary of advanced defaults. The full reference is `docs/CONFIG.md`. The blocks you most often touch:

- **`server`**: `bind_addr` (default `127.0.0.1:8080` for `harbor serve`; `harbor dev` always binds `:18080`), `allowed_origins` (CORS allowlist for multi-process Console), `shutdown_grace_period` (drain timeout for hot reload).
- **`telemetry`**: `log_format` (`json` / `text`), `log_level` (`debug` / `info` / `warn` / `error`), `service_name` (OTel resource).
- **`observability`**: `rollups.{driver,dsn}` (HA-65) wires the durable observability rollup projection — empty block = unwired; `driver` `inmem` / `sqlite` / `postgres`, `dsn` required for `sqlite` / `postgres`; restart-required.
- **`artifacts`**: `driver` (`inmem` / `fs` / `sqlite` / `postgres`), `heavy_output_threshold_bytes` (the LLM-edge context-leak guard, default 131072 / 128 KiB — see RFC §6.5). It bounds what may enter a model's context window only; Console-facing Protocol replies (`pause.list`, `memory.get`/`list`, the flow catalog, the `mcp.apps.*` reads) select inline-versus-reference at a pinned 32 KiB bound that does not track this key (D-358).
- **`events`**: `driver` (`inmem` / `durable`); events power the Console's live streaming. Durable persistence is NOT selected on `driver` — set `driver: durable` and then pick the backing store with `state_driver` (`sqlite` / `postgres`) + `state_dsn`. With `driver: durable` and an empty `state_driver` the bus loudly degrades to best-effort in-memory (not durable across restart).
- **`sessions`**: `idle_ttl` (default 24h), `hard_cap` (default 720h / 30d), `sweep_interval`; `turns.{driver,dsn,retention}` (HA-64) wires the durable conversation-turn projection behind `sessions.turns.list` / `.get` — empty block = unwired (the routes answer 501); `driver` `inmem` / `sqlite` / `postgres`, `dsn` required for `sqlite` / `postgres`, `retention` 0 → the projection's documented default; restart-required.
- **Postgres migration mode**: every Postgres-backed store block (`state`, `memory`, `artifacts`, `skills`, `sessions.turns`, `observability.rollups`) accepts `migration_mode: apply | verify`. Empty defaults to `apply`, which takes a session advisory lock and therefore uses a direct/session-capable endpoint. After migrations have been applied separately, set the pooled DSN and `migration_mode: verify`; startup then reads `schema_migrations` only, performs no DDL/transaction/advisory lock, and fails loud when the ledger is missing or stale. Never point `apply` at a transaction pool.
- **`pauseresume`**: `max_park_duration` (ceiling on how long a pause — HITL approval, tool OAuth — may stay parked before the runtime resumes it with the typed `timeout` decision and the run ends as a constraints-conflict; default `0` = never expire), `sweep_interval` (sweeper cadence, default 1m).
- **`tasks`**: `driver` (`inprocess` or `durable`). `inprocess` (default) keeps task/group/patch state in memory — a restart starts empty. `durable` persists those records through the `StateStore` so they survive a restart; on open it replays them and recovers any task left `running` by a crash to `failed` (code `runtime_restarted`). It reuses the runtime `StateStore`, so pair it with a durable `state.driver` (`sqlite` / `postgres`) for cross-process survival; selecting `durable` with no store wired fails loudly at boot.
- **`distributed`**: `bus_driver` (`loopback` or `durable`) + `remote_driver` (`loopback` only in V1.1; A2A wire is post-V1). `loopback` is in-process; `durable` persists every `BusEnvelope` through the `StateStore` and projects it onto the local event bus, with a poller for cross-instance fan-out + restart-replay (StateStore-backed — Postgres-as-queue on a shared Postgres store; tune with `bus_poll_interval`). NATS / Redis Streams remain future drivers.

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
- **A model swap fails the first call with `ErrUnsupportedModel`.** You forgot to add a `model_profiles.<model>.context_window_tokens` entry for the new model. There's no silent fallback — add the profile and the call succeeds.

## See also

- [`scaffold-a-harbor-agent`](../scaffold-a-harbor-agent/SKILL.md) — drops the tiered yaml in the first place.
- [`wire-the-llm-provider`](../wire-the-llm-provider/SKILL.md) — the full provider matrix + the mock vs real posture.
- [`configure-memory-and-skills`](../configure-memory-and-skills/SKILL.md) — the memory strategies + runtime skill catalog in depth.
- [`validate-and-package`](../validate-and-package/SKILL.md) — preflight before shipping.
- The full per-key reference: `docs/CONFIG.md`.
