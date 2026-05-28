# Harbor configuration reference

This document is the full operator-facing knob reference for
Harbor's `harbor.yaml`. Every leaf field on `internal/config.Config`
has a heading here; `internal/config/doc_drift_test.go` fails the
build when a field lands without documentation.

Conventions used throughout:

- **Default** — the value applied when the key is absent (sourced
  from `internal/config/loader.go::defaults()`).
- **Validation** — what the validator (`internal/config/validate.go`)
  rejects.
- **Restart-required** — restart-required unless explicitly tagged
  `reload:"live"` on the struct field (CLAUDE.md §10).

For the tiered yaml template used by `harbor init`, see
`cmd/harbor/init/templates/default/harbor.yaml.tmpl`.

---

## Server

### server.bind_addr

Listen address (host:port) for Harbor's network surface (Protocol +
health endpoint). Default: `127.0.0.1:8080`. Validation: must parse
via `net.SplitHostPort`.

### server.shutdown_grace_period

Max time `harbor dev` waits for in-flight requests to drain on
SIGTERM before forcing close. Default: `30s`. Validation: > 0.

### server.allowed_origins

CORS allowlist for the D-091 multi-process Console+Runtime posture
(Phase 83v / D-162). Each entry is an exact origin
(`scheme://host[:port]`, no path / query / fragment) the Runtime
accepts cross-origin requests from. Empty list (the default) = no
CORS headers = same-origin only.

On a matching origin the middleware echoes the request's `Origin`
header verbatim into `Access-Control-Allow-Origin` and sets
`Access-Control-Allow-Credentials: true` so the browser sends the
`Authorization` bearer on subsequent requests. The middleware
NEVER emits `Access-Control-Allow-Origin: *` in production — `*` is
incompatible with credentialed requests and the browser refuses
the combination. CLAUDE.md §7: declare exact origins in production.

The validator rejects `*` (and any wildcard shape) unless
`server.cors_dev_allow_any: true` is also set. Validation: each
entry must parse as a URL with `http` or `https` scheme and a
non-empty host; paths / queries / fragments are rejected.

Example:

```yaml
server:
  allowed_origins:
    - https://console.example.com
    - https://console.example.com:8443
    - http://127.0.0.1:18790
```

### server.cors_dev_allow_any

Explicit, dev-only escape hatch that opens the CORS surface to ANY
origin (Phase 83v / D-162). NEVER set in production: a `harbor dev`
boot with this flag set prints a stderr banner so the posture is
visibly dev-only. Provided for first-clone Console iteration
against a `harbor dev` loop where the Console origin (Vite,
`:5173`) varies during development.

Default: `false`. Set explicitly:

```yaml
server:
  cors_dev_allow_any: true
```

The middleware still emits the per-origin `Access-Control-Allow-
Origin` echo (never `*`) so credentialed responses keep working.

---

## Identity

### identity.jwt_algorithms

Asymmetric-only JWT-algorithm allowlist (CLAUDE.md §7 rule 1).
Default: none — operator MUST set at least one. Validation: each
entry must be one of `RS256` / `RS384` / `RS512` / `ES256` / `ES384`
/ `ES512`. HS\* and `none` are forbidden.

### identity.issuer

OIDC issuer URL. Default: none. Validation: non-empty.

### identity.audience

OIDC audience claim. Default: none. Validation: non-empty.

### identity.jwks_url

URL the JWT verifier fetches the JWKS document from. Default:
empty. Validation: one of `jwks_url` or `jwks_file` MUST be set.

### identity.jwks_file

Filesystem path to a static JWKS document (offline / air-gapped
scenarios). Default: empty. Validation: see `jwks_url`.

---

## Telemetry

### telemetry.log_format

Slog handler format. Default: `json`. Validation: `json` or `text`.

### telemetry.log_level

Slog level threshold. Default: `info`. Validation: `debug` / `info`
/ `warn` / `error`.

### telemetry.otel_endpoint

OTLP/gRPC endpoint URL for span + metric export. Default: empty
(noop exporter — spans / metrics collected but not shipped).

### telemetry.service_name

Service identifier on emitted spans + metrics. Default: `harbor`.
Validation: non-empty.

---

## State

### state.driver

`StateStore` driver. Default: `inmem`. Validation: `inmem` / `sqlite`
/ `postgres`.

### state.dsn

Driver connection string. Default: empty. Validation: required when
`driver != "inmem"`. SQLite: file path. Postgres: libpq URL.
Secret: redacted in audit logs.

---

## LLM

### llm.driver

LLM driver. Default: `bifrost` (Phase 64 / D-089). Validation:
`bifrost` (production) or `mock` (tests only). Empty resolves to
`bifrost`.

### llm.provider

Provider name routed through bifrost. Default: empty. Validation:
required when `driver != "mock"`. May reference a `custom_providers`
entry (NIM / vLLM / ollama / any OpenAI-compatible endpoint) instead
of a native bifrost provider.

### llm.model

Canonical model identifier. Default: empty. Validation: required
when `driver != "mock"`. Must have a matching `model_profiles[name]`
entry for the safety-net token-budget guard.

### llm.api_key

Provider API key, typically given as `env.NAME` so the driver reads
`os.Getenv("NAME")` at boot. Default: empty. Validation: required
when `driver != "mock"` AND the provider is not a custom-provider
name. Secret: redacted in audit logs.

### llm.base_url

Override base URL for the provider's API. Default: empty
(provider's hardcoded default).

### llm.timeout

Per-request timeout. Default: `60s`. Validation: > 0 (unless
provider is a custom-provider name).

### llm.context_window_reserve

Safety margin (fraction) the token-budget guard reserves above the
model's hard cap. Default: `0.05` (5%). Validation: `[0.0, 1.0)`.

### llm.model_profiles

Per-model knobs (context-window cap, JSON-schema mode, default-max-
tokens, reasoning-effort, cost overrides, max-retries, correction-
layer overrides). Each entry's `context_window_tokens` MUST be > 0.
See the `LLMModelProfileConfig` godoc for the full surface.

### llm.corrections.enabled

Top-level toggle for the Phase 34 per-provider correction layer.
Default: `true`. Set `false` only for safety-pass isolation tests.

### llm.custom_providers

Operator-declared OpenAI-compatible providers (Phase 33a — NIM /
vLLM / ollama / lm-studio / in-house gateways). Each entry needs
`name` / `base_url` / `api_key_env_var` / `models`. See the
`LLMCustomProviderConfig` godoc for the full surface.

### llm.network_defaults.timeout

Default per-provider timeout. Default: `0` → bifrost's package
default. Restart-required.

### llm.network_defaults.max_retries

Default per-provider retry count. Default: `0` → bifrost default.

### llm.network_defaults.retry_backoff_initial

Default initial backoff before retry. Default: `0`.

### llm.network_defaults.retry_backoff_max

Default cap on backoff growth. Default: `0`.

### llm.network_defaults.concurrency

Default in-flight request limit per provider. Default: `0`.

### llm.network_defaults.buffer_size

Default request-queue buffer per provider. Default: `0`.

---

## Governance

### governance.repair_attempts

Per-LLM-call schema-repair budget. Default: `3`. Validation: >= 0.

### governance.default_tier

Tier applied to an identity not matched by a custom resolver.
Default: empty (no default → no enforcement for unmatched
identities). Validation: when set, MUST reference a key in
`identity_tiers`.

### governance.identity_tiers

Per-tier policy bundle (cost ceiling, rate-limit token bucket,
max-tokens cap). Default: empty (latent — no enforcement). See
`GovernanceTierConfig` godoc for the full surface.

---

## Distributed

### distributed.bus_driver

MessageBus driver. Default: `loopback`. Validation: `loopback`
(V1).

### distributed.remote_driver

RemoteTransport driver. Default: `loopback`. Validation:
`loopback` or `a2a`.

---

## Runtime (reserved)

Reserved block — populated as runtime/* phases land. No leaf
fields today.

---

## Memory

### memory.driver

`MemoryStore` driver. Default: `inmem`. Validation: `inmem` /
`sqlite` / `postgres`.

### memory.dsn

Persistent-driver connection string. Default: empty. Validation:
required when `driver != "inmem"`. Secret: redacted.

### memory.strategy

Memory shape. Default: `none`. Validation: `none` / `truncation` /
`rolling_summary`.

### memory.budget_tokens

Truncation / rolling-summary budget cap (token estimate). Default:
`0` (unbounded append). Validation: >= 0.

### memory.recovery_backlog_max

Bounded queue size for the `rolling_summary` strategy's recovery
loop (D-035). Default: `16`. Validation: >= 0.

---

## Skills

### skills.driver

`SkillStore` driver. Default: empty (block fully optional; an empty
block disables the subsystem). Validation: when set, `localdb`
(V1).

### skills.dsn

Driver connection string. Default: empty. Validation: required when
`driver = "localdb"`. Secret: redacted.

---

## Tasks

### tasks.driver

`TaskRegistry` driver. Default: `inprocess`. Validation: `inprocess`.

### tasks.retain_turn_timeout

Max time the engine blocks a foreground turn waiting for retain-
turn groups to resolve. Default: `5m`. Validation: > 0.

### tasks.continuation_hop_limit

Max background-continuation hops a planner runtime may take before
requiring user confirmation. Default: `8`. Validation: > 0.

---

## Sessions

### sessions.idle_ttl

Time before an idle session is swept. Default: `24h`. Validation:
> 0 AND <= `hard_cap`.

### sessions.hard_cap

Absolute max session lifetime. Default: `720h` (30 days).
Validation: > 0.

### sessions.sweep_interval

Background sweeper period. Default: `15m`. Validation: > 0 AND <=
`idle_ttl`.

---

## Artifacts

### artifacts.driver

`ArtifactStore` driver. Default: `inmem`. Validation: `inmem` / `fs`
/ `sqlite` / `postgres` / `s3`.

### artifacts.fs_root

Root directory for the `fs` driver. Default: empty. Validation:
required when `driver = "fs"`. Auto-created at driver `New`.

### artifacts.dsn

SQL-driver connection string. Default: empty. Validation: required
when `driver` is `sqlite` or `postgres`. Secret: redacted.

### artifacts.heavy_output_threshold_bytes

Byte size at which the runtime mandatorily routes a payload through
the ArtifactStore (D-022 / D-026). Default: `32768` (32 KiB).
Validation: >= 0.

### artifacts.s3_bucket

S3 bucket name (Phase 19). Default: empty. Validation: required
when `driver = "s3"`.

### artifacts.s3_endpoint

Base URL for non-AWS S3-compatible backends (MinIO / R2). Default:
empty (AWS default endpoint resolution).

### artifacts.s3_region

AWS region. Default: `us-east-1`.

### artifacts.s3_prefix

Path prefix inside the bucket. Lets multiple Harbor deployments
share one bucket. Default: empty.

### artifacts.s3_access_key_id

S3 access key. Default: empty (SDK default credential chain).
Secret: redacted.

### artifacts.s3_secret_access_key

S3 secret key. Default: empty. Secret: redacted.

### artifacts.s3_use_path_style

Use path-style addressing instead of virtual-host (MinIO / older R2
endpoints). Default: `false`.

---

## Events

### events.driver

EventBus driver. Default: `inmem`. Validation: `inmem` or `durable`
(Phase 57 — StateStore-backed).

### events.max_subscribers_per_session

Cap on concurrent subscribers per session. Default: `16`.
Validation: > 0.

### events.subscriber_buffer_size

Per-subscriber channel buffer. Default: `256`. Validation: > 0.

### events.idle_timeout

Max idle time before a subscriber is reaped. Default: `60s`.
Validation: > 0.

### events.drop_window

Backpressure drop-policy window. Default: `1s`. Validation: > 0.

### events.replay_buffer_size

In-memory ring-buffer depth for replay. Default: `10000`.
Validation: >= 0 (zero disables replay on the inmem driver).

### events.state_driver

StateStore driver the `durable` event driver persists through.
Default: empty (degrades to best-effort in-memory ring with a loud
warning per D-074).

### events.state_dsn

DSN for the durable event log's StateStore. Default: empty.
Validation: required when `state_driver` is non-empty AND
non-inmem. Secret: redacted.

---

## Audit (reserved)

Reserved block — populated as audit phases land. No leaf fields
today.

---

## Protocol

### protocol.max_request_bytes

Upper bound on `artifacts.put` upload body size (Phase 73l / D-120).
Default: `4 MiB` (`DefaultMaxRequestBytes`). Bodies above this fail
with HTTP 413.

---

## CLI

### cli.dev_hot_reload.enabled

`harbor dev` hot-reload watcher toggle (Phase 65 / D-099). Default:
`true`. The `--no-hot-reload` flag is the operator-facing escape
hatch.

### cli.dev_hot_reload.policy

Retain-in-flight policy on a triggered restart. Default: `drain`.
Validation: `drain` / `cancel` / `disabled`.

### cli.dev_hot_reload.drain_timeout

Cap on the `drain` policy's wait for in-flight RunLoops. Default:
`5s`. Validation: > 0.

### cli.dev_hot_reload.watch_roots

Paths the fsnotify watcher monitors. Default: `[".harbor/agents"]`
(the Phase 66 drafts directory). The dev cmd unions this with the
loaded config file's directory.

---

## Tools

### tools.http_manifests

Paths to UTCP-style YAML manifests loaded by the Phase 27 HTTP
driver. Default: empty list. Validation: each path non-empty.

### tools.mcp_servers

MCP southbound attachments (Phase 28). Each entry needs `name`,
`transport_mode`, and either `url` (HTTP transports) or `command`
(stdio). See `MCPServerConfig` godoc for the full surface.

### tools.a2a_peers

A2A southbound peers (Phase 29). Each entry needs `url`,
`trust_tier` in `[1, 5]`, `latency_tier_ms` >= 0,
`agent_card_ttl` >= 0. HTTPS-only unless `allow_insecure_loopback`
is set on a loopback host.

### tools.entries

Per-tool catalog wiring (Phase 64a / D-090). Attaches approval gates
and / or OAuth bindings to a tool name without writing Go wiring
code. See `ToolEntryConfig` godoc.

### tools.oauth_providers

Operator-configured OAuth providers (D-095). V1 ships the `oauth2`
driver (generic OAuth2/PKCE Authorization Code flow). Each entry
needs `name`, `driver`, `client_id_env`, `client_secret_env`. See
`ToolOAuthProviderConfig` godoc.

### tools.oauth_token_kek_env

Env-var name holding the 32-byte hex-encoded key-encryption key
(KEK) used for AES-256-GCM token encryption at rest. Default: empty.
Validation: required when `oauth_providers` is non-empty.

### tools.built_in

Opt-in list of Harbor-shipped built-in tools to register against the
catalog at boot (Phase 83n / D-153). Default: empty (no built-ins
registered). Validation: each name MUST be in
`internal/tools/builtin.KnownNames()`. V1.1 ships:

- `clock.now` — return current UTC time as RFC 3339 + epoch
  milliseconds.
- `text.echo` — echo input text verbatim.

### tools.custom

Operator-declared custom tools whose Go shell is generated by
`harbor scaffold` (Phase 83o / D-154). Each entry takes:

- `name` — catalog tool name. Required. Unique within `custom`; no
  collision with `built_in`.
- `description` — one-line summary. Required.
- `input` — flat map of `field: type`. Type allowlist (V1.1):
  `string` / `integer` / `number` / `boolean` / `[]string`.
- `output` — same shape as `input`.

The scaffolder materialises one `tools/<name>.go` stub + matching
`tools/<name>_test.go` per entry. The runtime does NOT auto-discover
these tools — the operator imports the generated `tools/` package and
calls `RegisterTools` from the agent's bootstrap path.

Example:

```yaml
tools:
  custom:
    - name: get_weather
      description: Look up current weather by city.
      input:
        city: string
        units: string
      output:
        temp_c: number
        summary: string
```

### tools.granted_scopes

Operator-declared list of authorization scopes the dev runtime's
planner-facing catalog view treats as granted (Phase 83m / D-156).
The runtime catalog projects only tools whose `AuthScopes` are
entirely contained in this set; tools that require a missing scope
are invisible to the planner. Tools with no `AuthScopes` are always
visible.

Default: empty (no scopes granted — tools that declare `AuthScopes`
are invisible). Validation: each entry MUST be a non-empty string;
scope names are operator-defined per their tool sources (no
allowlist). Restart-required.

Example:

```yaml
tools:
  granted_scopes:
    - read:repo
    - write:issues
    - admin
```

---

### tools.search_cache_dsn

SQLite DSN backing the Phase 107c / D-167 tool **SearchCache** (FTS5 +
regex fallback). The discovery meta-tools (`tool_search`, `tool_get`)
delegate to this index; an empty value selects the in-memory default,
which is suitable for development (discovery state lives for the
process lifetime).

Operators that want the cache to persist across reboots set a `file:`
URI; the driver layers `journal_mode(WAL)` + `busy_timeout(5000)`
automatically when the URI does not already declare them.

Default: empty (in-memory cache). Restart-required.

Example:

```yaml
tools:
  search_cache_dsn: file:./harbor-tools.db
```

---

## Planner

### planner.driver

Planner driver. Default: `react` (V1 reference). Validation: `react`
(V1.1).

### planner.max_steps

Step circuit-breaker cap. Default: `0` → driver default
(`react.DefaultMaxSteps` = 12). Validation: >= 0.

### planner.extra_guidance

Operator-supplied domain-specific guidance injected into the
planner's `<additional_guidance>` system-prompt section (Phase 83a
/ RFC §6.2). Default: empty.

### planner.reasoning_replay

Whether the ReAct planner re-injects a prior step's captured
provider reasoning trace (Phase 83e / D-148). Default: `never`.
Validation: `never` / `text` (no `provider_native` in V1.1).

### planner.max_tool_examples_per_tool

Cap on curated examples rendered per tool in the planner's
`<available_tools>` section (Phase 83b / D-144). Default: `0` →
driver default of 3. Validation: >= 0.

### planner.parallel_tool_calls

Toggles native parallel tool-call emission (Phase 107d / D-169).
When the LLM returns N>1 tool-calls in one response, `true` makes the
React planner emit a native `CallParallel` and the dev `ToolExecutor`
dispatch the branches concurrently; `false` selects the Phase 107c
serialization fallback (one `CallTool` per step via
`RunContext.PendingToolCalls`). Pointer-bool: an omitted key resolves
to `true` (the native-parallel default). Validation: none (both states
are correct).

### planner.skills_context_max

Cap on skill bodies the dev run loop fetches from
`SkillStore.Search` and hands the planner via
`RunContext.SkillsContext` (Phase 83f / D-149). Default: `0` →
dev-runtime default of 5. Validation: >= 0.

### planner.planning_hints.constraints

Free-form text rendered verbatim into the planner's
`<planning_constraints>` system-prompt section (Phase 83c).
Default: empty.

### planner.planning_hints.preferred_tools

Tool names the planner should prefer when multiple satisfy the
same goal. Default: empty.

### planner.extra

Per-driver opaque extras map. Reserved for future drivers'
per-flow knobs. Default: empty. V1.1 `react` driver ignores it.
