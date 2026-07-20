# Harbor configuration reference

This document is the full operator-facing knob reference for
Harbor's `harbor.yaml`. Every leaf field on `internal/config.Config`
has a heading here; `internal/config/doc_drift_test.go` fails the
build when a field lands without documentation.

Conventions used throughout:

- **Default** — the value applied when the key is absent (sourced
  from `internal/config/loader.go::Defaults()`).
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

### server.debug_addr

Optional address for the pprof debug HTTP listener. Empty (the
default) disables it entirely. When set it MUST be a loopback
`host:port` (`127.0.0.0/8` or `::1`) — the validator rejects any
other host so the profiler is never exposed off-box. The listener
runs on its OWN `http.Server` with a private mux serving only the
`net/http/pprof` handlers; it is never mounted on the Protocol mux.
Enabling it prints a stderr banner so the dev-only posture is
visible. It is a diagnostic escape hatch, not a production surface.

Default: `""` (disabled). Set explicitly:

```yaml
server:
  debug_addr: "127.0.0.1:6060"
```

Then `go tool pprof http://127.0.0.1:6060/debug/pprof/heap` (or
`/goroutine`, `/profile`) against the running runtime.

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

### identity.jwks_max_stale

Max-stale ceiling: the longest a cached JWKS key snapshot is honored
without a successful refresh before the validator fails closed (rejects
tokens with a distinct `jwks_stale` reason) rather than serving a
possibly-revoked key during a prolonged IdP outage. Default: `0`, which
applies the safe built-in ceiling (1h). Validation: a negative value is
rejected; a positive value below the 1m floor is rejected (a ceiling
below the minimum refresh window can never be satisfied). There is no
"disable" path — Harbor's posture is fail-closed. This bounds — it does
not make instantaneous — key revocation; pair a tight ceiling with
overlapping IdP signing keys and short token TTLs.

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

### llm.credential_source

Where the PRIMARY provider's API key is sourced: `""` / `"local"`
(default — resolved from `api_key` once at boot) or `"remote"`
(broker-pull from the named `inference_broker` at connect + refresh).
Brokered XOR local — a `remote` source requires a resolvable
`inference_broker` and an empty `api_key` (both set, or neither, is a
boot error). Restart-required; NOT a Protocol surface.

### llm.inference_broker

Names the `inference_brokers[]` entry the primary provider's key is
pulled from when `credential_source` is `"remote"`. Referenced by
non-secret NAME (the pull endpoint / audience / scope ceiling live on
the named broker). Required when `credential_source: remote`, rejected
otherwise. Restart-required.

### llm.inference_brokers

Boot-declared list of NAMED inference-plane credential brokers — the
pinned credential SINK for a runtime's LLM provider key. Each entry
needs `name` / `credential_url` (https or loopback) / `auth_token_env`,
with optional `audience` / `scope_ceiling` / `cache_ttl` / `timeout`.
No URL or secret ever crosses the wire (the credential-plane invariant);
referenced by name from `llm.credential_source: remote` and from the
`agent_config.set_llm_provider` Protocol write. See the
`InferenceBrokerConfig` godoc. Config/file-only, restart-required.

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

## Embeddings

The embedding-client block (Phase 84d — D-191): the model/provider
pair Harbor turns text into vectors with, configured separately from
the chat `llm` block. Fully optional — but REQUIRED the moment an
embedding-consuming mode is enabled (`memory.retrieval: semantic` or
`skills.retrieval: semantic`); the validator names the missing key so
the boot failure is actionable, and a semantic mode never silently
degrades to non-semantic retrieval.

### embeddings.driver

Embeddings driver. Default: empty (resolves to `bifrost`).
Validation: when set, `bifrost` (V1; there is deliberately no mock /
stub embeddings driver).

### embeddings.provider

Embedding provider (e.g. `openai`). Validation: required when the
block is set.

### embeddings.model

Embedding model (e.g. `text-embedding-3-small`). Its own operator
choice — nothing falls back to `llm.model`. Validation: required when
the block is set.

### embeddings.api_key

Literal key or `env.NAME` reference, matching the `llm.api_key`
convention. Validation: required when the block is set. Secret:
redacted.

### embeddings.base_url

Optional endpoint override forwarded to the gateway. Default: empty.

### embeddings.timeout

Optional per-request timeout. Default: `0` (gateway default).
Validation: >= 0.

### embeddings.dimensions

Optional reduced output dimension for providers that support it.
Default: `0` (the model's native dimension). Validation: >= 0.

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
max-tokens cap). Default: empty (latent — no enforcement, D-044).
**Populated tiers are enforced** (Phase 111a, D-198): the runtime
composes the enforcement subsystem at boot, so the cost ceiling
fails calls with `ErrBudgetExceeded`, the token bucket with
`ErrRateLimited`, and the per-call cap with `ErrMaxTokensExceeded`
(each with a matching `governance.*` event). The same tiers also
feed the read-only `governance.posture` surface. See
`GovernanceTierConfig` godoc for the full surface.

---

## Distributed

### distributed.bus_driver

MessageBus driver. Default: `loopback`. Validation: `loopback` /
`durable`.

- `loopback` (default) — in-process; projects each `BusEnvelope` onto
  the local event bus. No durability, no cross-instance fan-out.
- `durable` — persists every `BusEnvelope` through the `StateStore` and
  projects it onto the local event bus, with a background poller that
  also projects envelopes published by OTHER instances sharing the
  store (or left by a crash) — at-least-once cross-instance fan-out +
  restart-replay. StateStore-backed (Postgres-as-queue on a shared
  Postgres store; single-instance restart-replay on SQLite). Reuses the
  runtime `StateStore`; fails loud at construction when no store is
  wired. Consumers dedupe on `(TaskID, Edge, EventID)`. NATS / Redis
  Streams remain future drivers.

### distributed.bus_poll_interval

How often the `durable` bus driver scans the shared `StateStore` for
envelopes published by other instances (or left by a crash). Optional;
the durable driver applies a built-in default (1s) when unset. Ignored
by `loopback`.

### distributed.remote_driver

RemoteTransport driver. Default: `loopback`. Validation:
`loopback` or `a2a`.

---

## Runtime

Runtime run-lifecycle configuration. Two populated bodies: the
run-completion hook and the `runtime.naming` session auto-naming fleet
default.

### runtime.hooks.run_completion.tool

The catalog tool the run's transcript is dispatched to at the run loop's
terminal boundary (the run-completion hook). Empty (the default) disables
the static hook — the per-agent, versioned agent-config `hooks` section can
still enable it. The tool need not be exposed to the planner: the executor
resolves it against the full catalog. Resolution at run start is
agent-config over this yaml over no hook — a PRESENT agent-config `hooks`
section is authoritative either way, so a section with no/empty
run-completion tool (a bare `hooks: {}`) is an explicit per-agent NO-HOOK
that overrides this yaml fleet hook (section presence is the signal; it is
never dropped as inert). A hook failure never alters the run outcome (it
emits `run.hook_failed` + a Warn log).

### runtime.hooks.run_completion.timeout

Bounds the detached hook dispatch (Go duration, e.g. `10s`). The dispatch
runs under a context detached from the run's cancellation but bounded by
this timeout, so a wedged sink cannot leak the hook goroutine. Non-positive
(the default) falls back to the 10s runtime default. Setting a timeout
without `runtime.hooks.run_completion.tool` is rejected (a timeout for a
hook that will never fire). A negative value is rejected.

### runtime.naming.auto

Enables session auto-naming fleet-wide (the `runtime.naming` block). Opt-in,
default `false` (off): with the block absent or `auto: false` the runtime
writes no naming counters, makes no naming LLM calls, and emits no naming
events. A per-agent, versioned agent-config `naming` section overrides this
default at run start (agent-config over this yaml over off) — a PRESENT
section is authoritative either way, so a bare `{auto: false}` revision is an
explicit per-agent opt-out that wins over a yaml-on fleet default (section
presence is the signal; it is never dropped as inert). When on, the runtime
titles a session itself at each run's terminal boundary via ONE governed
`Complete` call over a bounded transcript digest; a naming failure never
alters the run outcome (it emits `session.naming_failed` + a Warn log).

The naming call is bounded by a FIXED runtime timeout (10s) — unlike the
run-completion hook, whose timeout is per-section configurable, the naming
timeout is not an operator knob. The trigger runs synchronously at the run's
terminal boundary, AFTER the completion hook, so the worst-case post-run
latency is hook timeout + naming timeout, serialized. A naming FAILURE does
not consume the `max_repetitions` cap: as long as a title is due, the runtime
retries on every subsequent completed run until one succeeds — so on a
naming-on fleet whose naming LLM is DOWN, every completed run pays one
failing (≤ 10s) naming attempt and emits one `session.naming_failed` until
the LLM recovers or the policy is switched off.

### runtime.naming.after_turns

The number of completed runs after which the FIRST auto-name fires (fire on
the Nth completed run). Zero (the default) resolves to `1` at run start. A
negative value is rejected.

### runtime.naming.repeat_every

When greater than 0, re-names the session every N completed turns after the
first. Zero (the default) names once only. A negative value is rejected.

### runtime.naming.max_repetitions

Caps the TOTAL number of auto-namings (including the first). It is REQUIRED
`>= 1` whenever `runtime.naming.repeat_every > 0` — no unlimited value exists,
so unbounded periodic re-naming is unrepresentable. Ignored when
`repeat_every` is 0 (naming happens once). A negative value is rejected. For
a repeating policy constructed programmatically (an embedder building a
naming spec by hand, bypassing the yaml/wire validators), the policy-level
default of `5` applies when the cap is unset — the no-unlimited invariant
holds on every path. The cap is PER-CYCLE, not per-session-lifetime: a
manual clear (an empty `sessions.set_title`) re-arms auto-naming by zeroing
the naming counters, so each clear opens a fresh arming cycle with its own
cap budget.

### runtime.naming.max_title_len

Bounds the auto-generated title in runes. Zero (the default) resolves to `80`
at run start; a set value must be within `[8, 200]`. The auto title is
deterministically clamped to this bound (unlike the manual `sessions.set_title`
verb, which rejects oversize input — the trusted-internal vs untrusted-boundary
asymmetry is intentional).

### runtime.naming.model

The model the auto-naming `Complete` call requests. Empty (the default) uses
the run's effective model. A set value is validated against
`llm.model_profiles` at boot; point it at a cheap profile to keep naming
inexpensive (the naming call consumes the session identity's governance
budget by design).

---

## Memory

### memory.driver

`MemoryStore` driver. Default: `inmem`. Validation: `inmem` /
`sqlite` / `postgres`.

### memory.dsn

Persistent-driver connection string. Default: empty. Validation:
required when `driver != "inmem"`. Secret: redacted.

Note (D-174): conversation memory durability rides on the configured
**`state.driver`**, not `memory.dsn`. Under the executor-delegation model
all memory drivers persist strategy state through the StateStore, so a SQL
memory driver with a SQL `memory.dsn` but an `inmem` `state.driver` is
NOT durable across a restart. To make memory durable, set a SQL
`state.driver`; `inmem` memory + a SQL StateStore is already durable.

### memory.strategy

Memory shape. Default: `none`. Validation: `none` / `truncation` /
`rolling_summary`. All three strategies run on every memory driver
(`inmem` / `sqlite` / `postgres`) — they delegate to a shared strategy
executor that persists through the configured StateStore, so a SQL
`state.driver` makes `truncation` and `rolling_summary` durable across a
runtime restart (D-174). `rolling_summary` requires an LLM: `harbor dev`
builds the Summarizer from the configured `llm` automatically (no separate
summariser model). Configuring `rolling_summary` with no LLM fails loud at
boot — there is no stub fallback (CLAUDE.md §13).

### memory.budget_tokens

Truncation / rolling-summary budget cap (token estimate). Default:
`0` (unbounded append). Validation: >= 0.

### memory.recovery_backlog_max

Bounded queue size for the `rolling_summary` strategy's recovery
loop (D-035). Default: `16`. Validation: >= 0.

### memory.recent_turns

Number of most-recent conversation turns the `rolling_summary`
strategy keeps verbatim before older turns spill into the rolling
summary (D-242). Default: `0` → strategy default
(`strategy.FullZoneTurns` = 4). Validation: >= 0. Ignored by the
`none` and `truncation` strategies.

### memory.summarizer.model

Pins the model the `rolling_summary` compaction summariser requests,
independent of the planner's model — set it to a cheaper/faster model
to keep compaction cheap (D-243). Default: empty → the main LLM's
default model (today's behavior). A model with no matching
`model_profiles` entry fails at runtime like any unsupported model; it
is not rejected at load time. Ignored by the `none` and `truncation`
strategies.

### memory.summarizer.prompt

Operator guidance APPENDED to the baseline `rolling_summary`
summariser system prompt behind an explicit "extend, do not override"
separator (D-243) — it never replaces the baseline role framing or
conciseness/preserve-goals guarantees. Default: empty → baseline
prompt only (no behavior change). Ignored by the `none` and
`truncation` strategies.

### memory.retrieval

Opt-in retrieval mode layered ON TOP of the strategy (Phase 84d —
D-191). Empty (the default) keeps strategy-shaped retrieval;
`semantic` additionally embeds turns at `AddTurn` and serves
similarity search via `MemoryStore.SearchTurns`, composing with —
never replacing — `rolling_summary`. Default: empty. Validation:
empty or `semantic`; `semantic` requires the `embeddings` block.

### memory.retrieval_top_k

Result cap for a semantic `SearchTurns` when the caller passes no
limit. Default: `0` (resolves to the subsystem default, 5).
Validation: >= 0. Ignored unless `memory.retrieval = "semantic"`.

### memory.retrieval_min_score

Cosine-similarity floor for semantic recall: a scored turn must meet
or exceed this value to be injected into the prompt's External memory
tier. Turns that fall below the floor are silently skipped. Default:
`0.0`. Validation: must be in the range `[-1, 1]`. Ignored unless
`memory.retrieval = "semantic"`.

---

## Skills

### skills.driver

`SkillStore` driver. Default: empty (block fully optional; an empty
block disables the subsystem). Validation: when set, `localdb`
(V1).

### skills.dsn

Driver connection string. Default: empty. Validation: required when
`driver = "localdb"`. Secret: redacted.

### skills.directory.pinned

Skill names anchored at the top of every `<skills_context>` view, in
declaration order (Phase 111d — D-201). Pinning is an ordering
preference only — pinned skills are never exempt from the capability
filter. Default: empty. Validation: entries non-empty and unique;
requires `skills.driver` to be set.

### skills.directory.max_entries

Cap on the injected directory view's length. Default: `0` — falls
back to `planner.skills_context_max`'s resolved value (default 5) so
the pre-111d injection-budget knob keeps its meaning. Validation: `0`
or in `[1, 200]`.

### skills.directory.selection

Ordering of the unpinned remainder. Default: `pinned_then_recent`
(UpdatedAt DESC). Validation: when set, must be `pinned_then_recent`.
`pinned_then_top` (UseCount DESC) is recognised but **rejected as not
yet wired** — no production path increments skill usage counters, so
the ordering would silently degrade to alphabetical; the validator
fails loud instead (the `tools.http_manifests` precedent). It becomes
accepted when a usage-tracking path lands.

### skills.retrieval

Opt-in `Search` / `skill_search` ranking mode (Phase 84d — D-191).
Empty (the default) keeps the token-savvy FTS5 → regex → exact
ladder; `semantic` ranks by embedding similarity over the
identity-scoped catalog (result path `semantic`). Capability
filtering, redaction, and the budgeter apply unchanged on top.
Default: empty. Validation: empty or `semantic`; `semantic` requires
the `embeddings` block and `skills.driver` to be set.

---

## Tasks

### tasks.driver

`TaskRegistry` driver. Default: `inprocess`. Validation: `inprocess` /
`durable`.

- `inprocess` (default) — keeps task/group/patch state in memory. Live
  state is observable, but a runtime restart starts with an empty
  registry.
- `durable` — persists task/group/patch records through the configured
  `StateStore`, so they survive a runtime restart. On open it replays
  every record and runs a recovery sweep: a task left `running` by a
  crash is transitioned to `failed` with the reserved error code
  `runtime_restarted` (the record is recovered; execution is not
  re-driven). It reuses the runtime's `StateStore` — no extra config
  beyond `tasks.driver: durable`. For survival across a real **process**
  restart, pair it with a durable `state.driver` (`sqlite` or
  `postgres`); with `state.driver: inmem` records survive only an
  in-process driver reopen. Selecting `durable` with no `StateStore`
  wired fails loudly at boot.

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

## Pause/Resume

### pauseresume.max_park_duration

Ceiling on how long a pause may stay parked before the pause sweeper
resumes it with the typed `timeout` Decision (`pause.resumed`, D-096)
and the waiting run terminates as a constraints-conflict (Phase 111c /
D-200). Default: `0` — pauses never expire and the sweeper is not
started. Validation: >= 0.

### pauseresume.sweep_interval

Pause-sweeper scan period (consumed only when `max_park_duration` >
0). Default: `1m` (0 = the default applies). Validation: >= 0 AND <=
`max_park_duration` when both are set.

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

Paths to UTCP-style YAML manifests for the Phase 27 HTTP driver.
Default: empty list. Loaded at boot by the HTTP-manifest boot loader
(D-279): assembly calls `http.LoadManifest` on each entry and
`http.RegisterManifest` to register its tools on the runtime catalog
by name — AFTER built-in tools and BEFORE `tools.entries[]` applies
its middleware, so an `entries[]` block naming a manifest tool
(approval / OAuth / loading-mode) resolves cleanly. Once registered, a
manifest tool is indistinguishable from one registered inline via
`RegisterHTTPTool` — same descriptor, same `ToolPolicy` shell, same
catalog wiring — so `tools.entries[].oauth` (see below) binds an
OAuth provider to it with zero new machinery.

**Path resolution.** A relative entry resolves against the loaded
config file's directory (`filepath.Clean(filepath.Join(configDir,
entry))`); one that escapes the directory is rejected at `Load` time
with a `fieldError` naming `tools.http_manifests[i]` (§7 rule 5) —
lexically (a `../` that walks outside it) always, and via a
symlink-containment re-check when the joined path exists on disk (a
symlink inside the directory pointing outside it is rejected; a
not-yet-existing manifest gets the lexical check only, since boot is
the existence-enforcement home). An absolute entry is
`filepath.Clean`ed and accepted as-is — the same trust posture as
`artifacts.fs_root`. A hand-built `*Config` constructed without
`config.Load` (a headless Go embedder) skips this resolution step, as
do entries injected post-Load via `config.WithOverrides` (no config
directory is retained on `*Config`); pass absolute paths in both
cases.

**Validation vs. boot.** `Validate` checks the list structurally only
— each entry non-empty after trim, unique after `filepath.Clean` — so
`harbor validate` accepts a config whose manifest file does not exist
yet (existence/parsing is boot's job, matching the `tools.mcp_servers`
precedent of not probing URLs at validate time). A listed manifest
that is missing, unreadable, unparseable, or fails the driver's own
validation (`http.ErrManifestInvalid`: literal secrets, `.Auth`
template leaks, unknown fields, missing env refs) fails `Assemble`
loudly, naming both the file and `tools.http_manifests[i]`. A manifest
tool whose name collides with an already-registered catalog tool
fails the boot the same way (`tools.ErrToolDuplicateName`) — never a
silent skip.

Restart-required: manifests are boot-only and do not hot-reload. See
`examples/tools/http-weather.yaml` for a worked manifest and
`examples/harbor.yaml` / `examples/dev.yaml` for the paired
`tools.entries[].oauth` binding that exercises catalog OAuth wrapping
end to end.

### tools.mcp_servers

MCP southbound attachments (Phase 28). Each entry needs `name`,
`transport_mode`, and either `url` (HTTP transports) or `command`
(stdio). See `MCPServerConfig` godoc for the full surface.

#### tools.mcp_servers[].oauth_provider (and .meta_annotations)

Southbound per-identity OAuth binding (D-278). `oauth_provider` names a
declared `tools.oauth_providers[]` entry (a NON-SECRET provider name); a
bound http(s) connection resolves a fresh bearer for the calling identity
on every identity-stamped per-call RPC and injects `Authorization: Bearer
<tok>` on that request only (the connect-time `headers` stay for the
initialize/discovery handshake). A bound provider whose token fetch fails
aborts the call loud — never an unauthenticated fallback; a
`consent_required` refusal parks the run on the unified pause/resume
primitive. Validation rejects an unknown provider name (the error lists the
declared names), a binding on any connection without an http(s) `url` —
explicit `stdio` AND an omitted/`auto` transport with only a `command`,
which would auto-select stdio at connect and silently never inject — and a
static `Authorization` header alongside the binding (one auth mode per
connection).

`meta_annotations` is a static, NON-SECRET `map[string]string` merged
verbatim into the MCP `_meta` on every identity-stamped call — the
deployment's own attribution vocabulary, passed to the server alongside the
`(tenant, user, session)` triple and the provenance `agent_id`. Reserved
keys (`tenant` / `user` / `session` / `agent_id` / `traceparent` /
`tracestate` and any `io.modelcontextprotocol/`-prefixed key) and empty keys
are rejected at validation. See `examples/dev.yaml` for a worked stanza.

#### tools.mcp_servers[].policy (and .tool_policies)

Optional retry/timeout policy for the tools a server registers (Phase
26b / D-175). Without it, every tool uses the runtime default (30 s
per-attempt deadline, 4 total attempts). `policy:` sets the per-server
default; `tool_policies: { <tool-name>: { … } }` overrides individual
tools (keyed by the server-side MCP tool name). Fields, all optional:

- `max_attempts` — TOTAL attempts including the first (e.g. `1` = one
  attempt, no retry; `4` = the default). Projected to `MaxRetries =
  max_attempts - 1`.
- `timeout_ms` — per-attempt deadline in milliseconds.
- `retry_on` — error-class allowlist: `transient` / `timeout` / `5xx` /
  `permanent`.
- `backoff_base_ms` / `backoff_mult` / `backoff_max_ms` — exponential
  backoff between retries.

Per-field fall-through: a field you omit inherits the runtime default —
setting only `timeout_ms` keeps the default 4 attempts. Example: a slow,
reliably-throttled tool can be set to `max_attempts: 1, timeout_ms:
90000` so it makes one long attempt instead of four 30 s ones. Validation:
`max_attempts >= 1`, `timeout_ms >= 0`, `retry_on` in the allowlist,
override keys non-empty + unique. See `examples/harbor.yaml`.

### tools.a2a_peers

A2A southbound peers (Phase 29). Each entry needs `url`,
`trust_tier` in `[1, 5]`, `latency_tier_ms` >= 0,
`agent_card_ttl` >= 0. HTTPS-only unless `allow_insecure_loopback`
is set on a loopback host.

### tools.entries

Per-tool catalog wiring (Phase 64a / D-090). Attaches approval gates,
OAuth bindings, and / or a loading mode to a tool name without writing
Go wiring code. See `ToolEntryConfig` godoc. Each entry carries:

- `approval` — an approval-gate binding (optional).
- `oauth` — an OAuth binding (optional).
- `loading_mode` — when the tool appears in the planner's prompt-time
  catalog. `""` (default) or `always` means every turn; `deferred`
  hides the tool by default and lets the LLM discover it via
  meta-tools. This boot value is the base the runtime loading-mode
  overrides layer on top of (D-281): the effective mode resolves
  per-tool `agent_config.set_tool_exposure` override > per-server
  override > this boot `loading_mode` > driver default, applied
  next-turn.

Validation: an entry must set at least one of `approval`, `oauth`, or
`loading_mode` (an entry with no fields is a configuration typo).

### tools.oauth_providers

Operator-configured OAuth providers (D-095). Each entry needs `name`,
`driver`, `client_id_env`, `client_secret_env`. See
`ToolOAuthProviderConfig` godoc. Two drivers ship:

- `oauth2` — the generic OAuth2/PKCE Authorization Code flow. Needs
  `auth_url`, `token_url`, and `redirect_url`.
- `tokenexchange` (D-271) — pull-based external-credential provisioning.
  Instead of the interactive authorization-code flow, at token-miss time
  the runtime obtains a downstream tool credential from an external
  credential broker (a fleet orchestrator, an enterprise token vault, an
  STS) via an RFC-8693 token exchange keyed on the **verified** ctx
  identity triple, so one central grant serves N runtimes without N
  consents. Fields:
  - `token_url` — the broker's token-exchange endpoint. **Mandatory.**
  - `auth_url` / `redirect_url` — interactive-flow fields; **not used**
    by this driver (waived, not required).
  - `client_id_env` / `client_secret_env` — name the env vars holding the
    runtime's OWN credential for authenticating to the broker.
  - `extra.audience` — the RFC-8693 audience. Optional; defaults to the
    provider name. Superseded by `audience` (the ceiling) when that is set.
  - `extra.cache_ttl_cap` — max in-memory cache lifetime (a Go duration,
    default `5m`; values below `30s` are rejected at boot).
  - `audience` — the boot-declared token-audience **ceiling** (D-300).
    When set it is the authority for the exchanged token's audience —
    decoupled from the caller-chosen provider name. Optional; empty
    preserves the legacy audience-from-name behaviour.
  - `scope_ceiling` — the boot-declared **scope ceiling** (D-300). When
    set, the requested `scopes` are INTERSECTED against it — a requested
    scope outside the ceiling is dropped. Optional; empty preserves the
    legacy pass-through.

**Credential-sink allow-list — `allowed_downstream_hosts` (D-300).** Every
provider a `tools.mcp_servers[]` connection binds MUST declare
`allowed_downstream_hosts` — the boot-declared set of downstream connection
hosts (`host[:port]`) the provider's credential may be injected into. The
credential-plane invariant is that **no admin-writable field determines
where a credential is sent**, so the sink set is boot-declared,
config/file-only. A binding whose MCP connection host is not listed is
refused at boot (and at runtime-add) fail-closed — never a silent
unauthenticated dial. Host comparison folds the well-known default port
(`https://host` ≡ `https://host:443`) and is case-insensitive. This is
mandatory for a bindable provider (an empty allow-list on a bound provider
is a boot error); it applies to BOTH the `oauth2` and `tokenexchange`
drivers.

  Brokered tokens are TTL-cached **in memory only and never persisted**
  (`TokenStore.Put` is never called — the broker stays the single source
  of truth); a cached token is served until the EARLIER of the
  broker-advertised expiry and the cap. Broker failure fails the run
  loudly (`ErrExchangeFailed`, no silent interactive fallback); a
  `consent_required` refusal parks on the unified pause primitive. See
  `examples/dev.yaml` for a fully-commented block.

Each provider's OWN client credential resolves through the
`credential_source` seam (D-285):

- `env` (the **default**; omit the field) — `client_id` / `client_secret`
  are read from `client_id_env` / `client_secret_env` **once at boot**,
  fail-loud when either env var is unset. Existing configs are
  byte-compatible.
- `remote` — the runtime **pulls** `client_id` / `client_secret` from a
  coordinator-served endpoint at **first need** (not at boot), so a
  credential a coordinator mints AFTER the runtime booted reaches it with
  zero touch. The pulled credential is held **in memory only**, TTL-capped,
  single-flighted, and refetched on expiry — nothing is persisted. With
  `remote` the broker secret **never enters the runtime's environment**
  (defense-in-depth). Valid **only for the `tokenexchange` driver**;
  declaring `remote` alongside `client_id_env` / `client_secret_env` is a
  validation error (one source per entry — §13). Requires a `remote:`
  block:
  - `url` — the coordinator credential endpoint. **Required**, validated
    well-formed at boot. **Must be `https`** — the fetch sends the
    runtime's service bearer token, so TLS is mandatory; the one
    carve-out is a loopback host (`127.0.0.1` / `::1` / `localhost`, the
    local fixture / dev case), where plaintext `http` is accepted.
    Redirects from the endpoint are refused (never followed) — a
    credential endpoint that redirects is treated as a fault.
  - `auth_token_env` — names the env var holding the runtime's own service
    token, sent as `Authorization: Bearer`. **Required**; read lazily at
    fetch time so a rotated token is picked up without restart.
  - `cache_ttl` — optional; caps the in-memory serve horizon (a Go
    duration; default 5m). The effective horizon is `min(response
    expires_in, cache_ttl)`.
  - `timeout` — optional; bounds a single fetch (default 30s).

  **The fetch contract** (Harbor-defined, versioned) — a single
  authenticated GET:

  ```http
  GET <url>
  Authorization: Bearer <token from auth_token_env>
  Accept: application/json
  ```

  The coordinator responds with a strict JSON object:

  ```json
  {
    "format_version": 1,
    "client_id":      "...",
    "client_secret":  "...",
    "expires_in":     3600
  }
  ```

  `format_version` (required) is the contract version — the runtime
  accepts version `1`; unknown top-level fields are rejected (strict
  parse). `expires_in` (optional, seconds) drives the cache TTL. A fetch
  that is unreachable, non-200, or malformed fails the tool call loud with
  a typed sentinel and emits a `tool.provider_credential_fetch_failed`
  audit event — **never** a fallback to env, to an unauthenticated call,
  or to the interactive flow. A successful fetch emits
  `tool.provider_credential_fetched` (both events carry zero secret bytes).
  A push of the credential over the Protocol stays rejected as credential
  passthrough (D-271). See `examples/dev.yaml` for a fully-commented
  `remote` block.

### tools.oauth_credential_brokers

Boot-declared list of NAMED credential **sinks** (D-300) — the config home
for the pinned token endpoints + allowed downstream hosts a
Protocol-installed, zero-URL provider descriptor references **by name**. The
credential-plane invariant keeps every sink-determining value boot-declared,
config/file-only; this list is where those pinned sinks live. It is
**additive** — the inline `oauth_providers[].remote` block stays valid — and
restart-required (NOT a Protocol surface). Each entry:

- `name` — operator-facing broker identifier. **Required**, unique within
  the list; referenced by name from a provider descriptor.
- `token_url` — the broker's RFC-8693 token-exchange endpoint (the pinned
  credential sink the org's `client_id` / `client_secret` are POSTed to).
  **Required**; must be `https` (or a loopback host for the dev/fixture
  case). Boot-pinned — never wire-writable.
- `allowed_downstream_hosts` — the sink allow-list for the exchanged
  bearer. **Required, non-empty** (a bearer-minting broker must declare
  where its tokens may be injected — fail-closed).
- `auth_token_env` — names the env var holding the runtime's own broker
  credential (§7 rule 2 — never hardcoded). **Required, non-empty.**
- `cache_ttl` — optional; caps the in-memory serve horizon (Go duration).
- `timeout` — optional; bounds a single broker exchange (Go duration).

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

### tools.mcp_app_host

Deployment-wide MCP App (`io.modelcontextprotocol/ui`) host capability the
runtime advertises to every MCP server during the initialize handshake. The
host's rendering ability does not vary per server, so this is a single
deployment-level block, not a per-server field.

`display_modes` lists the MCP App display modes this host can render, from the
closed set `inline` / `fullscreen` / `pip` (the ext-apps `McpUiDisplayMode`
values). A spec-conformant server reads these to tailor the app references it
returns. Each entry must be in the closed set and unique; a typo fails at
config load.

Omitting the block (or leaving `display_modes` empty) resolves to the
inline-only **baseline** (`[inline]`) — the mode the Console renders out of
the box. Set the full set once the deployment's Console serves the
fullscreen-tab and side-by-side (pip) layouts.

Default: nil (resolves to `[inline]`). Restart-required.

Example:

```yaml
tools:
  mcp_app_host:
    display_modes: [inline, fullscreen, pip]
```

### tools.mcp_add_connection

Gates the admin-driven runtime add of a NEW MCP server connection over the
Protocol (`agent_config.add_mcp_connection`). Adding a stdio MCP server spawns
an operator-supplied command — an RCE surface — so the add path is
**fail-closed**: a stdio add is permitted ONLY when its command `argv[0]` is
listed exactly in `stdio_allowlist`. An empty / omitted block rejects **every**
stdio add (the secure default). `http` adds are admin-scoped but are not gated
by this allowlist.

The command is always launched argv-form (never via a shell). Secret auth
material (bearer headers, OAuth tokens) supplied with an add flows to the live
transport only and is NEVER persisted in the recorded config revision, the
diff, or any emitted event.

Default: nil (every stdio add rejected). Restart-required.

Example:

```yaml
tools:
  mcp_add_connection:
    stdio_allowlist:
      - /usr/local/bin/harbor-mcptest-stdio
      - /opt/mcp/servers/my-trusted-server
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

### planner.absolute_max_spawn_depth

Caps the `ParentTaskID`-chain depth of planner-spawned background
tasks (Phase 107e / D-170). When a planner emits `_spawn_task`, the
dev `ToolExecutor` reads the parent chain depth and rejects loudly —
an error observation the planner re-plans against, never a silent
drop — any spawn whose child would exceed this depth, so a background
sub-agent that itself emits `_spawn_task` cannot recurse without
bound. The cap bounds depth, not breadth. Default: `0` → dev-runtime
default of 4. Validation: >= 0.

### planner.max_batch_spawns

Caps how many task spawns a single `Batch` decision may carry — the
BREADTH ceiling (Phase 186 / D-323). A `Batch` is the shape a projector
builds when a model batches `_spawn_task` calls alongside other calls in
one response; the dev `ToolExecutor` rejects the WHOLE batch (no tool
branch dispatches, no spawn registers) — never a silent truncation to
the first N — when its spawn count exceeds this cap. Distinct from
`absolute_max_spawn_depth`, which bounds spawn-chain DEPTH, not the
breadth of one response's spawns. Default: `0` → dev-runtime default of
5 (conservative, operator-revisable). Validation: >= 0.

### planner.token_budget

Trajectory-compression threshold in estimated tokens (Phase 111e /
D-202). When > 0, the runtime builds the LLM-backed trajectory
summariser and the steering run loop invokes it at each step boundary:
a trajectory whose token estimate exceeds the budget is compacted into
the five-field `Trajectory.Summary`, which replaces the raw per-step
history in subsequent prompt builds (the prompt shrinks). One
compression per run at V1.1.x — no auto-cascade. Emits
`trajectory.compressed` / `trajectory.compression_failed` on the
canonical event stream. Requires a configured `llm` block when
non-zero (fail-loud at boot otherwise). Default: `0` → compression
disabled. Validation: >= 0.

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

## Multimodal

### multimodal.disposition

Per-agent attachment disposition policy (Phase 84b / D-189): a map
from a MIME key — an exact media type (`application/pdf`), a family
wildcard (`image/*`), or the literal `*` (the agent-wide default) —
to a disposition value: `ref` (emit an `ArtifactStub` + `Fetch.Tool`
hint; the developer processes the bytes via a tool), `inline`
(DataURL inline; `image/*` only at V1.1), `provider_native` (opt-in
provider-side understanding — the LLM driver uploads the attachment
to the provider's file surface and the model sees it via an opaque
`file_id` (Phase 84c / D-190); a provider without support for the
modality degrades to `ref` with a logged notice), or `tool:<name>`
(force the named catalog tool via `Fetch.Tool`). Precedence:
per-attachment Protocol
`disposition` hint > this map > the runtime default (`image/*` →
`inline`, everything else → `ref` — byte-for-byte the pre-84b
behaviour). Default: empty (runtime default applies). Validation:
keys/values must satisfy the grammar above. Restart-required.
