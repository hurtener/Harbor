# Phase 92k — auth.Provider runtime config registration seam

## Summary

The OAuth `auth.Provider`'s config set is immutable after construction
(`NewProvider(configs, deps)`; `Provider.configs` is read-only) — so a
runtime-added MCP server has no way to register an `OAuthConfig` and the
agent-bound flow cannot be reached for a server added over the Protocol. This
phase adds a runtime config-registration seam — `RegisterConfig(cfg OAuthConfig)
error` (validate + upsert by `Source`) and `UnregisterConfig(source)`
(idempotent) — by moving the `configs` map behind a documented
internally-synchronised mutex while preserving the D-025 concurrent-reuse
contract. The boot provider construction migrates from the static `NewProvider`
config list to construct-empty + `RegisterConfig`-per-config, so the seam ships
with a real production caller, not just a test.

## RFC anchor

- RFC §6.4 — Tool catalog and transports (the tool-side OAuth substrate the MCP
  southbound driver consults; the runtime config registry widens it for
  runtime-added sources).
- RFC §3.3 — the unified pause/resume primitive (the parking substrate a
  registered config's `InitiateFlow` drives; this phase does not change it, only
  makes the config it needs registrable at runtime).

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- **brief 09 (MCP OAuth lessons from bifrost):** an agent-bound OAuth attachment
  has a real lifecycle (config → flow → token → refresh); the config a
  runtime-added server needs must be registrable after boot without tearing down
  the shared provider. This phase makes the `OAuthConfig` registry mutable at
  runtime while keeping the in-flight flow's captured config immutable for that
  flow (re-registration applies to the next flow only) — the dynamic-registration
  footgun of mutating a config mid-flow is closed by definition.
- **brief 14 (MCP client/host compliance, spec 2025-11-25):** a runtime-added
  server's auth attachment is supplied (92m) or discovered (92p) after the
  provider exists; the provider must accept a late-arriving config. The seam is
  the precondition for the transport-level token injection (92l) that the
  compliance matrix requires.

## Findings I'm departing from (if any)

None.

## Goals

- Add `RegisterConfig(cfg OAuthConfig) error` to `auth.Provider`: validate via
  the existing `OAuthConfig.Validate`, then upsert by `Source` (a second
  `RegisterConfig` for the same `Source` replaces the prior config).
- Add `UnregisterConfig(source tools.ToolSourceID)`: idempotent removal (no error
  when the source was never registered).
- Move the `configs` map behind a documented internally-synchronised mutex,
  guarded exactly like the in-flight `flows` map / the coordinator's `pauses`
  map, so the Provider stays a compiled artifact (D-025) while the config
  registry is shared mutable state.
- Define the in-flight-flow interaction: a flow already parked keeps the config
  it captured at `InitiateFlow`/`buildAuthRequired` time; a re-registration of
  that `Source` applies only to the next `Token`/`InitiateFlow`.
- **Consumer in-phase (§13):** migrate the production boot provider construction
  from the `NewProvider(configs, deps)` static list to constructing empty +
  `RegisterConfig`-ing each boot config, so a real production caller exercises the
  seam on day one.

## Non-goals

- The MCP transport token injection + typed `ErrAuthRequired` (92l).
- `add_mcp_connection`'s OAuth block + `InitiateFlow` parking (92m).
- Spec-faithful 401 → RFC 9728 → AS discovery that synthesises a config to
  register (92p).
- Any change to the pause/resume coordinator, the TokenStore, or the encryption
  envelope.

## Acceptance criteria

- [ ] `RegisterConfig` validates via `OAuthConfig.Validate` (a malformed config
  is rejected loud with the wrapped sentinel, never silently dropped) and upserts
  by `Source` (re-registering a `Source` replaces its config).
- [ ] `UnregisterConfig` is idempotent: removing an absent source is a no-op
  (no error), removing a present source drops it from the registry.
- [ ] An in-flight flow is unaffected by a re-registration of its `Source`: it
  completes against the config captured at flow start; the new config governs the
  next flow only. Asserted by a test that re-registers mid-flow then
  `CompleteFlow`s.
- [ ] Concurrent-reuse test: N≥100 concurrent `RegisterConfig`/`Token` (and
  `UnregisterConfig`) against ONE shared `*Provider` under `-race` — no data
  races, no context bleed, no cross-cancellation, baseline goroutines restored.
- [ ] The boot path is migrated to construct-empty + `RegisterConfig`-per-config
  and the existing tool-side OAuth integration test stays green (the migration is
  behaviour-preserving for the boot-config set).
- [ ] No secret logged: `RegisterConfig` never logs `ClientSecret` or any config
  field that could carry credential material; failures name the `Source` only.

## Files added or changed

- `internal/tools/auth/provider.go` — add `RegisterConfig` / `UnregisterConfig`;
  move `configs` behind a documented internally-synchronised mutex (reuse
  `flowsMu` or add a dedicated `cfgMu`); update the read sites (`Token`,
  `InitiateFlow`, `CompleteFlow`, `Revoke`, `ConfigFor`, `DenyFlow`) to take the
  guard; relax `NewProvider` to accept an empty config list (validation of any
  supplied seed configs is retained).
- `internal/tools/auth/provider_runtimeconfig_test.go` — unit + the
  in-flight-re-registration test.
- `internal/tools/auth/concurrent_test.go` — extend the existing D-025 harness to
  interleave `RegisterConfig`/`UnregisterConfig` with `Token`.
- `cmd/harbor/cmd_dev.go` (+ `harbortest/devstack/devstack.go` if it constructs
  the provider — D-094 twin) — migrate boot provider construction to
  construct-empty + `RegisterConfig`-per-config.
- `scripts/smoke/phase-92k.sh` — `unit-tests` classification (the surface is a Go
  API exercised by `go test`, not an HTTP endpoint).

## Public API surface

```go
// On *auth.Provider (implements auth.OAuthProvider, plus the runtime seam):

// RegisterConfig validates cfg (OAuthConfig.Validate) and upserts it into the
// provider's config registry keyed by cfg.Source. A second call for the same
// Source replaces the prior config; an in-flight flow keeps its captured config.
func (p *Provider) RegisterConfig(cfg OAuthConfig) error

// UnregisterConfig removes the config for source. Idempotent: no error when the
// source was never registered.
func (p *Provider) UnregisterConfig(source tools.ToolSourceID)
```

## Test plan

- **Unit:** `RegisterConfig` validates + upserts by `Source` (replace on
  re-register); `UnregisterConfig` is idempotent (absent → no-op, present →
  removed); a malformed config is rejected with the wrapped `Validate` sentinel;
  `Token`/`InitiateFlow` against an unregistered source still returns the
  no-config error; no secret appears in any returned error string.
- **Integration:** in-flight re-registration round-trip — `InitiateFlow` a
  `Source`, `RegisterConfig` a new config for the same `Source`, then
  `CompleteFlow` the original flow and assert it used the captured config; the
  next `InitiateFlow` uses the new config. Real `TokenStore` (in-mem driver),
  real `events.EventBus` (in-mem driver), real `audit.Redactor`, real
  `pauseresume.Coordinator`, httptest authorization server — identity propagates
  through the triple; ≥1 failure mode (a config that fails `Validate`); `-race`.
- **Conformance:** N/A — no new driver interface; the seam is a method on the
  existing concrete `*Provider`.
- **Concurrency / leak:** N≥100 concurrent `RegisterConfig`/`UnregisterConfig`/
  `Token` against one shared `*Provider` under `-race`; assert no data races, no
  cross-cancellation, baseline `runtime.NumGoroutine` restored after teardown.

## Smoke script additions

- `scripts/smoke/phase-92k.sh` is classified `# PREFLIGHT_REQUIRES: unit-tests`
  (the seam is a Go API, not a network surface). When the phase ships it runs
  `go test ./internal/tools/auth/...` (the unit + concurrent-reuse suites);
  parked until then it is a single `skip` line so preflight stays green.

## Coverage target

- `internal/tools/auth`: 85%

## Dependencies

- 30 (tool-side OAuth — the `auth.Provider`, `OAuthConfig`, `TokenStore`,
  `InitiateFlow`/`CompleteFlow` this seam extends).
- 50 (the unified pause/resume coordinator the provider parks flows on).

## Risks / open questions

- **Mutating a security primitive.** Adding a runtime config registry widens the
  provider's mutable surface. Mitigated by: a single documented
  internally-synchronised mutex guarding the `configs` map (the D-025 pattern —
  the registry is shared mutable state, the Provider stays a compiled artifact);
  the concurrent-reuse test; and the wave-end adversarial review. An in-flight
  flow's captured config is immutable for that flow — re-registration applies to
  the next flow only, so a mid-flow config swap cannot corrupt an outstanding
  exchange.
- **Read-site coverage.** Every existing `p.configs[...]` read must move behind
  the guard in the same change; a missed read site is a latent race the
  concurrent-reuse test is designed to flush out.
- **Boot migration parity.** The construct-empty + `RegisterConfig` boot path
  must be behaviour-identical to the old static-list path for the boot-config set;
  the existing tool-side OAuth integration test is the regression guard, and the
  devstack twin (D-094) migrates in lockstep so the two boot paths cannot drift.

## Glossary additions

- **runtime OAuth config registration** — registering / unregistering an
  `auth.OAuthConfig` on a live `auth.Provider` after construction
  (`RegisterConfig` / `UnregisterConfig`), guarded by an internally-synchronised
  mutex. The precondition for authenticating a runtime-added MCP connection;
  distinct from the boot-time config set supplied at construction.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **This phase mutates a reusable artifact (`auth.Provider`): concurrent-reuse
  test passes — N≥100 concurrent `RegisterConfig`/`Token` against one shared
  `*Provider` under `-race`, asserting no data races, no context bleed, no
  cancellation cross-talk, no goroutine leaks.**
- [ ] **This phase consumes shipped subsystems (tool-side OAuth, pause/resume):
  the in-flight re-registration integration test wires real drivers end-to-end,
  asserts identity propagation, covers ≥1 failure mode, runs under `-race`.**
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry
  filed
- [ ] D-241 (reserved) logged in `docs/decisions.md` on ship (§17.7 step 3)
