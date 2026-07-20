# Phase 196 — inference-plane broker-pull credential source + `agent_config.set_llm_provider` install/rotate write (HA-30)

## Summary

Brings the tool-plane's Plane-B custody model to the one credential plane
that lacks it — the LLM provider key. Two pieces shipped together (a
primitive and its consumer, per CLAUDE.md §13): (D-333) an inference-plane
broker-pull credential source on the LLM `Account` that, at connect +
refresh (NEVER a per-hot-path-call KEK decrypt), pulls the provider key
from the coordinator's broker instead of local config — fail-loud on
broker-unreachable / refresh-fail (`ErrProviderKeyUnavailable`), no silent
local fallback, no stale-key-past-refresh, brokered XOR local, riding the
existing atomic key-swap (D-019); and (D-334) a SEPARATE
`agent_config.set_llm_provider` Protocol write that binds a runtime to a
NAMED, boot-declared inference-broker in the D-303 zero-URL / zero-secret
shape, gated on `auth.ScopeAdmin` only. The runtime-scoped
`llm.provider_credential_fetched` audit event is keyed to the per-runtime
service principal (D-271's runtime service token), resolving the emission-ctx
gap D-333 flags without widening the isolation tuple (§4).

## RFC anchor

- RFC §6.5
- RFC §6.15
- RFC §7
- RFC §9

## Briefs informing this phase

- brief 09
- brief 03
- brief 08

## Brief findings incorporated

- brief 09 §"What Harbor must add… fail closed on missing components":
  a broker-pull credential path MUST fail closed, never silently degrade to
  a weaker source. Carried verbatim into the inference plane: a broker
  unreachable at connect OR a failed refresh raises the typed sentinel
  `ErrProviderKeyUnavailable`; the runtime NEVER falls back to a local/boot
  key and NEVER serves a stale cached key past its refresh contract (D-271
  item 2, in full — its fail-loud half, not just its custody half).
- brief 09 §"Custody stays coordinator-side; nothing is persisted
  per-runtime": the runtime never persists the pulled key, the coordinator
  remains sole custodian, the pull is per-runtime-authenticated. The
  inference-plane source holds the key in memory only (TTL-capped,
  single-flighted, refetched on expiry) — the exact posture the
  `tokenexchange` `credential_source: remote` driver established
  (`internal/tools/auth/credsource/drivers/remote`), mirrored onto the LLM
  `Account`.
- brief 09 §"Discovery is report-only; no admin-writable field determines a
  credential sink": the D-303 zero-URL invariant. `set_llm_provider`'s
  writable descriptor carries NO URL and NO env-var name — the pull
  endpoint / audience / scope ceiling live on the boot-declared
  inference-broker config, referenced by non-secret NAME. A reflective
  no-sink-field decode test pins this structurally (the model is
  `internal/protocol/types/agentconfig_oauth_test.go::
  TestSetOAuthProviderWire_HasNoSinkField`, ported for the LLM descriptor).
- brief 03 §"The LLM client is one method; the runtime owns everything
  above it": the broker-pull source sits BELOW `LLMClient.Complete` (it
  feeds bifrost's `Account.GetKeysForProvider`), so the one-method interface
  is untouched — provider-key custody is an `Account` concern, not a new
  `CompleteRequest` field or a second client method.
- brief 08 §"bifrost invokes `Account.GetKeysForProvider` per request; keys
  can change without `ReloadConfig`": the pulled key rides the existing
  `LiveKey` atomic-pointer swap (`internal/llm/livekey.go`), so a broker-side
  rotation lands on the next call with no `ReloadConfig` race (D-019). The
  pull happens at connect + refresh, the swap is atomic, the hot path reads
  the live pointer — no per-call decrypt.

## Findings I'm departing from (if any)

D-285 note (2) restricted the `remote` credential source to per-identity,
LAZY pulls precisely because a connect-time pull "has no run identity to
attribute" its mandatory fetch event to. This phase defines a
runtime-scoped variant WITH its own attribution (D-333 explicitly supersedes
that note for this one defined, audited case): the connect/refresh pull
emits a runtime-scoped SafePayload `llm.provider_credential_fetched` audit
event keyed by the per-runtime service principal. This is a documented
extension of D-285, not a silent contradiction — filed as D-333 in the
decisions log. No other brief finding is departed from.

## Goals

- Add an inference-plane broker-pull credential source following the §4.4
  seam PATTERN (interface + drivers + factory), consumed by the LLM `Account`
  impl. It is a DISTINCT interface in `internal/llm`, NOT the OAuth-plane
  `credsource.Source` (`internal/tools/auth/credsource`): that seam resolves
  an OAuth provider's `client_id`/`client_secret`, whereas the inference
  source resolves an LLM PROVIDER API KEY for the bifrost `Account` — a
  different resolution target, a different consumer, and a different custody
  model (runtime-scoped connect+refresh vs D-285's per-identity lazy pull,
  the split D-285/D-333 deliberately keep). This is therefore NOT a §13
  "two parallel implementations of the same feature" — they are different
  credential features that share the §4.4 seam pattern; the sibling is
  justified, not incidental.
- Pull the provider key at **connect + refresh only** — cached, refreshed,
  NEVER a per-hot-path-call KEK decrypt on the inference critical path.
- **Fail loud** (D-271 item 2, verbatim): broker-unreachable-at-connect OR
  refresh-fail raises `ErrProviderKeyUnavailable`; no silent local fallback;
  no serving a stale cached key past its refresh contract (a broker-side
  revocation that fails to propagate surfaces, not masked by the cache).
- Enforce brokered XOR local, config-declared — one source per provider, no
  dual path (CLAUDE.md §13).
- Ride the atomic key-swap (D-019 / `LiveKey`), so a broker rotation lands
  on the next call with no `ReloadConfig` race.
- Name the boot-declared, config/file-only **inference-broker config** in
  `internal/config` — the D-300 analogue of `ToolOAuthCredentialBrokerConfig`
  — pinning the pull endpoint / audience / scope ceiling; referenced by
  non-secret name, never on the wire.
- Ship `agent_config.set_llm_provider` — a SEPARATE inference-plane method
  (NOT a relaxation of `set_oauth_provider`'s `tokenexchange`-only
  allowlist), with its OWN reflective zero-URL / zero-secret decode test,
  the D-303 provider-SET model (bare-name resolution, owner-tagged reconcile,
  uninstall closes the binding and fails bound calls loud), gated on
  `auth.ScopeAdmin` ONLY.
- Resolve D-333's flagged open item: specify the emission ctx for the
  runtime-scoped `llm.provider_credential_fetched` audit event outside a
  `(tenant, user, session)` request — keyed to the per-runtime service
  principal (D-271's runtime service token) that authenticates the pull,
  NOT a synthesized session, and NOT widening the isolation tuple (§4).

## Non-goals

- No coordinator-side inference and no provider-key mirror beyond the single
  central custody the broker already holds (the runtime does all inference;
  the coordinator never calls an LLM) — D-334 "Explicitly not part of this".
- No change to model selection — `runs.set_overrides` stays the model-*name*
  selector against the runtime's own key; `set_llm_provider` is a
  provider-*key-source* binding.
- No relaxation of `agent_config.set_oauth_provider`'s hard
  `tokenexchange`-only allowlist and no touch to its reflective no-URL decode
  test — widening it would reopen exactly the surface D-303 hardened.
- No per-call KEK decrypt on the inference hot path (the whole point of the
  connect+refresh cache).
- No new `LLMClient` method and no new `CompleteRequest` field — the
  one-method interface is untouched (§6.5).
- No failover / ordered-chain walking — that is D-335 / Phase 197 (which
  depends on THIS phase's pull source).
- No auto-apply of discovered / confirmed values — operator-gated, never
  auto-applied config (D-334).

## Acceptance criteria

- [ ] A new inference-broker credential source (a distinct `internal/llm`
      interface following the §4.4 seam pattern, NOT the OAuth `credsource`
      interface — see Goals) pulls the provider key from the coordinator
      broker at connect + refresh; the LLM `Account`
      (`internal/llm/drivers/bifrost/account.go`) sources its key from it
      when the provider is config-declared brokered, and from local config
      (env / file key) when declared local — brokered XOR local, validated
      loud at boot (both set, or neither, is a config error).
- [ ] The pulled key is cached in memory only (TTL-capped, single-flighted,
      refetched on expiry), NEVER persisted, and rides the `LiveKey` atomic
      swap (D-019) — `GetKeysForProvider` reads the live pointer on the hot
      path with no per-call broker round-trip and no KEK decrypt.
- [ ] Fail-loud, no dual path: broker-unreachable at connect raises
      `ErrProviderKeyUnavailable` (boot fails loud, naming the broker + the
      missing config, per CLAUDE.md §13 "fail loudly at boot"); a failed
      refresh raises `ErrProviderKeyUnavailable` and the runtime does NOT
      serve the stale cached key past its refresh contract and does NOT fall
      back to a local key.
- [ ] Every connect/refresh pull emits a runtime-scoped SafePayload
      `llm.provider_credential_fetched` audit event through the wired
      Redactor + Bus, keyed by the per-runtime service principal (D-271's
      runtime service token) — NOT a synthesized `(tenant, user, session)`,
      NOT widening the isolation tuple (§4). The event carries a
      non-reversible key fingerprint (never the key value), the broker name,
      and the outcome; a fetch that cannot be audited fails the pull loud.
- [ ] `agent_config.set_llm_provider` exists as a SEPARATE canonical method
      (`internal/protocol/methods/methods.go`), distinct from
      `set_oauth_provider`, gated on `auth.ScopeAdmin` ONLY (D-066 / D-079 —
      NOT the `admin` OR `console:fleet` two-scope set), authority derived
      server-side from the verified session (D-219), route
      `POST /v1/agent_config/set_llm_provider`.
- [ ] The writable descriptor is the D-303 shape EXACTLY — zero-URL,
      zero-secret: `{name, provider, credential_source:"remote",
      inference_broker, model_allow?}`, NO `token_url` / `credential_url` /
      `*_env` / secret field. A NEW reflective decode test
      (`TestSetLLMProviderWire_HasNoSinkField`, its OWN test — not a shared
      one with the OAuth descriptor) asserts the struct carries no URL and no
      env-var name; the wire handler's `DisallowUnknownFields` rejects any
      sink field BY NAME (400) before the method runs.
- [ ] The descriptor references the boot-declared inference-broker config by
      non-secret NAME; that config — never the wire descriptor — pins the
      pull endpoint / audience / scope ceiling, so no admin-writable field
      determines where the credential is sourced (D-300 credential-plane
      invariant preserved). An unknown broker name is rejected loud (400).
- [ ] Installed providers follow the D-303 provider-SET model: bare-name
      resolution, owner-tagged reconcile at run start, and uninstall closes
      the binding and fails subsequently-bound calls loud (not a silent
      no-op serving the old key).
- [ ] The boot-declared inference-broker config
      (`config.InferenceBrokerConfig`, the D-300 analogue) lands in
      `internal/config`: config/file-only, restart-required, NOT a Protocol
      surface; boot validation enforces its REQUIRED fields (name,
      credential_url, auth_token_env) and the OPTIONAL audience / scope-ceiling
      pins (`omitempty` — an LLM provider key is not always audience/scope-
      shaped; when present they are enforced as the D-300 sink pin, when
      absent the broker's own binding governs), and the
      brokered-XOR-local rule.
- [ ] `LLMClient` stays one method; no `CompleteRequest` field added; the
      broker-pull is entirely an `Account`-layer concern below the client.
- [ ] Full D-223 / D-209 lockstep for the new method + wire descriptor
      types; `ProtocolVersion` stays `0.1.0`.
- [ ] `scripts/smoke/phase-196.sh` exercises the security invariants (a
      URL/secret-carrying `set_llm_provider` is rejected 400; an unknown
      broker is rejected 400; a `console:fleet`-only token is rejected 403)
      against the live server, skipping gracefully on a pre-196 build.
- [ ] §18: `use-the-harbor-protocol` and the LLM/console-surfaced skill(s)
      (grepped per §18 — `surface: protocol` / `surface: llm` /
      `surface: console`) updated in the same PR; docs-site nav + generated
      protocol reference regenerated in the same PR; new config fields
      documented in `examples/`.

## Files added or changed

```text
internal/config/config.go                              # InferenceBrokerConfig (D-300 analogue); LLMProviderConfig.credential_source brokered/local
internal/config/validate.go                            # boot validation: required broker fields, brokered-XOR-local, unknown-broker resolution
internal/llm/credsource/inference_source.go            # inference-plane broker-pull source feeding the Account (new SIBLING interface, §4.4 pattern — NOT the OAuth credsource.Source; connect+refresh, cache, fail-loud). May extract the HTTP-pull/single-flight mechanics into a shared internal helper if it stays credential-agnostic, but the interface is distinct.
internal/llm/credsource/inference_source_test.go       # unit: fail-loud, no-stale-cache, single-flight, atomic-swap
internal/llm/drivers/bifrost/account.go                # GetKeysForProvider reads LiveKey seeded/refreshed by the broker-pull source (brokered XOR local)
internal/llm/livekey.go                                 # (reuse) atomic key-swap the pulled key rides; refresh path documented
internal/llm/events.go                                  # LLMProviderCredentialFetchedPayload (SafePayload, runtime-principal-keyed)
internal/runtime/agentcfg/protocol/setllmprovider.go   # agent_config.set_llm_provider handler (zero-URL, admin-only, provider-SET model) (new)
internal/runtime/agentcfg/protocol/setllmprovider_test.go  # zero-URL reflective, admin gate, boot-collision, unknown-broker
internal/runtime/agentcfg/protocol/service.go          # LLMProviderInstaller seam (mirrors ProviderInstaller)
internal/protocol/methods/methods.go                   # MethodAgentConfigSetLLMProvider + admin-method membership
internal/protocol/types/agentconfig_llm.go             # AgentConfigLLMProviderDescriptor (zero-URL) + set/response types (new)
internal/protocol/types/agentconfig_llm_test.go        # TestSetLLMProviderWire_HasNoSinkField (its OWN reflective test)
internal/protocol/agentconfig.go (or wire handler)     # DisallowUnknownFields decode + admin gate + dispatch
web/console/src/lib/protocol/agentconfig.ts            # typed set_llm_provider client + wire types
web/console/src/lib/protocol/wire-manifest.gen.json    # regenerated (make protocol-ts-gen)
docs/site/protocol/methods.md, types.md                # regenerated (make protocol-docs-gen)
docs/skills/use-the-harbor-protocol/SKILL.md            # set_llm_provider + broker-pull note
docs/skills/<llm/console skill>/SKILL.md                 # broker-pull provider install/rotate (grep-identified)
docs/glossary.md                                         # 3 new terms
examples/*.yaml                                          # inference_brokers block + brokered provider example
scripts/smoke/phase-196.sh
```

## Public API surface

```go
// internal/config/config.go — the D-300 analogue, boot-declared, config-only.

// InferenceBrokerConfig declares one NAMED, boot-declared inference-plane
// credential broker — the pinned credential SINK for a runtime's LLM
// provider key. The credential-plane invariant (no admin-writable field
// determines a credential sink) keeps every sink-determining value HERE,
// never on a wire-writable descriptor. Config/file-only, restart-required.
type InferenceBrokerConfig struct {
    Name          string        `yaml:"name"`            // referenced by non-secret name from set_llm_provider
    CredentialURL string        `yaml:"credential_url"`  // boot-pinned pull endpoint (https / loopback for fixtures)
    AuthTokenEnv  string        `yaml:"auth_token_env"`  // env holding the runtime's OWN broker credential (§7 rule 2)
    Audience      string        `yaml:"audience,omitempty"`      // boot-pinned audience ceiling
    ScopeCeiling  []string      `yaml:"scope_ceiling,omitempty"` // boot-pinned scope ceiling
    CacheTTL      time.Duration `yaml:"cache_ttl,omitempty"`
    Timeout       time.Duration `yaml:"timeout,omitempty"`
}
```

```go
// internal/protocol/types/agentconfig_llm.go — D-303 shape EXACTLY: zero-URL, zero-secret.

// AgentConfigLLMProviderDescriptor is the writable inference-provider
// binding. It carries NO URL and NO env-var name — every sink-determining
// value lives on the boot-declared inference_broker it references by name.
// The allowed field set is exactly
// {name, provider, credential_source, inference_broker, model_allow}.
type AgentConfigLLMProviderDescriptor struct {
    Name             string   `json:"name"`
    Provider         string   `json:"provider"`          // e.g. "openai", "anthropic"
    CredentialSource string   `json:"credential_source"` // "remote" only (broker-pull); "" (env) is rejected
    InferenceBroker  string   `json:"inference_broker"`  // non-secret broker NAME
    ModelAllow       []string `json:"model_allow,omitempty"`
}

type AgentConfigSetLLMProviderRequest struct {
    AgentID  string                           `json:"agent_id"`
    Provider AgentConfigLLMProviderDescriptor `json:"provider"`
    // No identity/scope field — authority is server-side (D-219).
}
```

```go
// internal/llm/credsource/inference_source.go

// ErrProviderKeyUnavailable — the broker was unreachable at connect OR a
// refresh failed. The runtime NEVER falls back to a local key and NEVER
// serves a stale cached key past its refresh contract. Callers compare via
// errors.Is.
var ErrProviderKeyUnavailable = errors.New("llm/credsource: provider key unavailable from broker (fail-loud; no local fallback, no stale cache)")

// InferenceKeySource pulls a provider key from a boot-declared broker at
// connect + refresh, caches it in memory (TTL-capped, single-flighted), and
// seeds/refreshes the LiveKey the bifrost Account reads. Immutable after
// construction; safe to share across N goroutines. It NEVER decrypts on the
// per-call hot path — the hot path reads the live atomic pointer.
type InferenceKeySource struct { /* broker cfg, http client, LiveKey, clock, runtime principal */ }

// Connect performs the first authenticated pull; fails loud on
// broker-unreachable. Refresh re-pulls on the TTL boundary; a failed
// refresh raises ErrProviderKeyUnavailable rather than serving stale.
func (s *InferenceKeySource) Connect(ctx context.Context) error
func (s *InferenceKeySource) Refresh(ctx context.Context) error
```

```go
// internal/runtime/agentcfg/protocol/setllmprovider.go

// LLMProviderInstaller is the driver-agnostic seam (mirrors ProviderInstaller)
// the set_llm_provider verb uses to install/uninstall a zero-URL, broker-pull
// inference provider on the LIVE owner-tagged set. Concrete wired at the
// cmd/harbor + devstack boundary (resolves inference_broker → builds the
// InferenceKeySource → installs owner-tagged).
type LLMProviderInstaller interface {
    InstallLLMProvider(ctx context.Context, tenant, agentID string, desc agentcfg.LLMProviderDescriptor) error
    UninstallLLMProvider(ctx context.Context, name string) error // closes the binding; bound calls then fail loud
}
```

## Test plan

- **Unit:**
  - `inference_source_test.go`: an httptest broker fixture serves a key at
    connect → `GetKeysForProvider` returns it; broker returns 5xx/unreachable
    at connect → `ErrProviderKeyUnavailable` (no local fallback); a refresh
    that fails after a good connect → `ErrProviderKeyUnavailable`, and the
    source does NOT serve the prior cached key past its refresh contract
    (no-stale-cache assertion); single-flight — N concurrent first-need
    callers cause exactly ONE broker pull; a broker rotation lands on the
    LiveKey atomic swap and the next `GetKeysForProvider` returns the new key
    (D-019, no ReloadConfig).
  - `setllmprovider_test.go`: `TestSetLLMProviderWire_HasNoSinkField` (its
    OWN reflective test — allowed set {name, provider, credential_source,
    inference_broker, model_allow}, forbidden substrings
    `url` / `_env` / `secret` / `token_` / `auth_` / remote-as-url); a
    descriptor carrying a URL/secret field is
    rejected by `DisallowUnknownFields`; `credential_source:""` (env) is
    rejected loud; a `console:fleet`-only session is rejected (admin-only);
    an unknown `inference_broker` is rejected 400; a boot-name collision is
    a distinct loud error; uninstall closes the binding and a subsequent
    bound call fails loud.
  - `config/validate_test.go`: `InferenceBrokerConfig` required-field
    validation; brokered-XOR-local rejection; unknown-broker-name
    resolution at install.
- **Integration:**
  - `internal/llm/credsource` (or `test/integration/inference_broker_test.go`):
    an httptest broker fixture (§17.8 — a captured/spec-shaped broker
    transcript, NOT a hand fixture encoding the implementer's guess) drives
    the FULL seam — `InferenceKeySource` → `LiveKey` → bifrost `Account` →
    a completion — under real drivers; asserts the runtime-scoped
    `llm.provider_credential_fetched` event fires on the bus keyed by the
    runtime principal (identity propagation for the runtime service token,
    NOT a session triple); ≥1 failure mode — a broker that goes unreachable
    on refresh surfaces `ErrProviderKeyUnavailable` and no stale key is
    served. Runs under `-race`.
- **Conformance:** N/A for the credential source itself (it is not a
  persistence-shaped subsystem — nothing is persisted, per D-333). The
  §9 conformance surface this phase touches is only the audit-event emit
  path, covered by the integration test across the real Bus/Redactor.
- **Concurrency / leak:**
  - `TestConcurrentReuse_InferenceKeySource_NoCrossRuntimeBleed`: N≥128
    concurrent `GetKeysForProvider` reads (+ interleaved refreshes) against a
    single shared source under `-race` — no data race on the LiveKey, no
    torn read across a refresh swap, no goroutine leak after teardown, and
    (with two sources bound to two brokers in the same test) no
    cross-runtime key bleed (source A's key never surfaces on source B's
    read). This is the D-025 reusable-artifact gate for the source.

## Smoke script additions

- `scripts/smoke/phase-196.sh` (`PREFLIGHT_REQUIRES: live-server`):
  - Static trip-wire: `agent_config.set_llm_provider` present in
    `wire-manifest.gen.json` + `docs/site/protocol/methods.md` (SKIP pre-196).
  - Probe `POST /v1/agent_config/set_llm_provider` — 404/405/501/000 → SKIP.
  - Security invariant (modeled on phase-169): a `set_llm_provider` whose
    descriptor carries a `credential_url` / `token_url` / `*_env` field is
    REJECTED 400 (DisallowUnknownFields — the D-300 exfil guard); a
    descriptor with `credential_source:""` is rejected 400; an unknown
    `inference_broker` name is rejected 400.
  - Scope gate: a `console:fleet`-only token (no `admin`) is rejected 403 —
    a leaked read-only fleet token cannot rebind a runtime's provider.
  - Where a fixture broker is wired into the preflight config: a valid
    zero-URL `set_llm_provider` returns 200 and a subsequent bound completion
    path is reachable (best-effort; SKIP when the fixture broker is absent).

## Coverage target

- `internal/llm/credsource` (new): 85%
- `internal/llm` (touched — account/livekey/events): 85%
- `internal/runtime/agentcfg/protocol` (touched): 85%
- `internal/protocol/types` (touched): 85%
- `internal/config` (touched): 85%

## Dependencies

- Gate-0 (the v1.17 RFC amendment: RFC §6.5 + D-333/D-334 — shipped on the
  branch base). Composes with the shipped tool-plane broker-pull spine
  (D-271 `tokenexchange` remote source, D-300 broker config invariant, D-303
  zero-URL install shape, D-019 atomic key-swap) — all already landed. No
  sibling-phase dependency; 196 parallelizes with 195 in Stage 2.

## Risks / open questions

- **Design choice the decision left for the phase to confirm — reuse the
  `credsource/drivers/remote` mechanics vs a sibling inference source.**
  D-333 says the source lives "behind the §4.4 credential-source seam" (the
  seam the tool-plane `remote` driver already occupies). Two realizations:
  (a) reuse the existing `internal/tools/auth/credsource/drivers/remote`
  pull mechanics directly, parameterized runtime-scoped; or (b) a sibling
  `internal/llm/credsource` source that shares the seam's contract but keeps
  the LLM-plane pull isolated from the tool-plane's per-identity lazy pull
  (which D-285 deliberately scoped per-identity). **This plan takes (b)** —
  a sibling under `internal/llm/credsource` — because the two pulls differ
  in granularity (runtime-scoped vs per-identity), attribution (runtime
  principal vs run identity), and lifecycle (connect+refresh cache vs lazy),
  and folding both into one driver risks the per-identity assumptions
  D-285's driver bakes in leaking onto the runtime-scoped path. **Flag for
  the coordinator to confirm** — (a) is less code but couples two custody
  models the decisions deliberately separated.
- **The emission ctx for the runtime-scoped audit event (D-333's flagged
  open item — resolved here, confirm the resolution).** The
  `llm.provider_credential_fetched` event fires outside any `(tenant, user,
  session)` request. This plan keys it to the per-runtime SERVICE PRINCIPAL
  — the runtime service token (D-271) that already authenticates the pull —
  carried on a runtime-scoped `context.Context` built at boot / refresh, NOT
  a synthesized session and NOT widening the isolation tuple (§4). The
  event's SafePayload carries a runtime-principal id + a non-reversible key
  fingerprint. **Confirm** the runtime service principal is the correct
  attribution key (the alternative — a boot-time process id — is weaker for
  correlating a fleet of runtimes to a coordinator; the service token is the
  right join key).
- **No-stale-cache vs refresh jitter.** "Never serve a stale key past its
  refresh contract" must not mean "fail every call during a transient broker
  blip." The refresh contract is: the cached key is valid until its TTL; a
  refresh attempt that fails BEFORE the TTL expires may retry within a
  bounded window, but once the TTL is crossed with no successful refresh the
  source fails loud rather than serving the expired key. The fixture test
  pins the boundary (a refresh failure AT/AFTER TTL → `ErrProviderKeyUnavailable`;
  a transient failure well before TTL → retried, key still served). This is
  the honest reading of D-333's "past its refresh contract".
- **§17.8 fixture provenance.** The broker fixture MUST be a captured /
  spec-shaped transcript of a real broker exchange, not a hand-authored
  self-consistent fixture (the D-216 rubber-stamp failure mode). Committed
  under `testdata/` with a PROVENANCE.md entry.

## Glossary additions

- **inference-plane broker-pull**
- **`agent_config.set_llm_provider`**
- **inference-broker config (boot-declared credential sink)**

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: N/A — the provider key is a
      runtime-level credential, NOT identity-scoped data; it does not widen
      the isolation tuple (§4). The audit event is runtime-principal-keyed,
      asserted by the integration test.
- [ ] This phase builds a reusable artifact (`InferenceKeySource`):
      concurrent-reuse test passes — N≥128 concurrent invocations against a
      single shared instance under `-race`, no cross-runtime key bleed.
- [ ] This phase consumes shipped subsystems (LLM Account, Bus, Redactor,
      agentcfg revision spine, config): integration test exists, wires real
      drivers + a spec-shaped broker fixture end-to-end, asserts
      runtime-principal attribution, covers ≥1 failure mode
      (broker-unreachable / stale-refresh), runs under `-race`.
- [ ] Glossary updated (3 terms above)
- [ ] Brief finding departed from (D-285 note 2 → runtime-scoped attribution)
      justified above + filed as D-333 in decisions.md (Gate-0 authored it)
