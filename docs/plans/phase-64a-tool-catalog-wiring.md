# Phase 64a — Tool catalog OAuth + approval wiring

## Summary

Phase 64a is the **catalog-side wiring** that consumes operator config
and auto-wraps registered tool descriptors with the matching approval
gate and / or OAuth-aware invocation wrapper. Operators get HITL
approval AND tool-side OAuth out of the box by declaring per-tool
middleware in `tools.entries[]` — no Go wiring code. Sibling to
Phase 64 (`harbor dev` v1, D-089); permitted by the Phase 64 pre-plan
note's "may split into sibling phase" clause and required by
CLAUDE.md §13's primitive-with-consumer rule in the same wave.
Settles **D-090**.

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 03
- brief 02
- brief 09

## Brief findings incorporated

- **brief 03 §"Pause-state taxonomy and the single primitive"** —
  every consumer of pause/resume routes through ONE Coordinator. The
  catalog wiring layers approval + OAuth as two consumers of the same
  primitive; the wrapping logic emits via the gate (for approval) and
  the provider (for OAuth), both converging on the Phase 50
  Coordinator. No second pause path.
- **brief 02 §"Pause-reason taxonomy"** — `ReasonApprovalRequired` is
  the canonical reason for HITL gates; `ReasonExternalEvent` is the
  canonical reason for OAuth flows. Phase 64a inherits the two reasons
  by routing through the gate (Phase 31) and the OAuth provider
  (Phase 30) verbatim — no new pause-reason enum values.
- **brief 03 §"Tool catalog seam"** — the unified Tool struct is the
  same regardless of transport; middleware wraps the descriptor's
  Invoke closure without leaking transport-specific knobs. Phase 64a's
  `WrapWithApproval` / `WrapWithOAuth` follow this shape; the
  wrappers know nothing about HTTP / MCP / A2A.
- **brief 09 §"BindingScope is declared, not inferred"** — the
  catalog entry's OAuth declaration MUST name the binding scope
  explicitly. Phase 64a's `ToolOAuthConfig.BindingScope` is a required
  field; an empty value fails validation.

## Findings I'm departing from (if any)

None.

## Goals

- A tool registration can declare an `approval` policy AND / OR an
  `oauth` binding via operator config; the catalog auto-wraps it.
- Unknown policy / unknown provider / unknown tool name fails
  `harbor validate` and / or `harbor dev` boot loud, with a wrapped
  error naming the offending value (§13 fail-loud).
- Wrapper composition order is pinned (D-090): approval outermost,
  OAuth innermost. The composition is documented in package doc + a
  unit test asserts it.
- `harbor validate` and `harbor dev` both pick up the new
  `tools.entries` block via `internal/config.Validate`.
- The wiring is concurrent-safe (D-025 obligation): a single Builder
  applies once at boot; the produced wrapped descriptors are safe
  for N concurrent goroutines.

## Non-goals

- A wire-side bridge that routes Protocol `approve` / `reject` calls
  through the catalog wiring's gate's `pending` map (this remains the
  Wave 11 wave-end E2E in Stage 4). The gate's `ResolveApproval`
  in-process helper is the surface this PR exercises end-to-end.
- Operator config for OAuth `provider` definitions (the actual
  `tools.oauth_providers[]` block). Phase 64a adds the binding side;
  provider construction is a later phase. For now the dev cmd builds
  an empty provider map; an entry that declares `oauth` without a
  provider in `Deps.OAuthProviders` fails closed.
- Hot reload of `tools.entries` (Phase 65).
- A `Deregister` method on `ToolCatalog`. Phase 64a uses
  `CatalogReplacer.Replace` for atomic per-tool swap at boot; runtime
  reconfiguration is post-V1.

## Acceptance criteria

- [x] `internal/tools/catalog.Builder` accepts `[]config.ToolEntryConfig`
  plus a `Deps` struct (Catalog + Coordinator + Bus + Redactor +
  OAuthProviders) and applies wrappers per entry.
- [x] `WrapWithApproval(d, gate, opts)` wraps a `ToolDescriptor` so
  every Invoke routes through `gate.RunGuarded`.
- [x] `WrapWithOAuth(d, prov, opts)` wraps a `ToolDescriptor` so every
  Invoke pre-checks token availability via `prov.Token`.
- [x] When BOTH approval AND OAuth are declared on the same tool,
  approval is the outermost wrapper (D-090).
- [x] `internal/config.Validate` rejects an unknown approval policy /
  unknown OAuth binding scope / empty entry name / duplicate entry
  name / empty middleware block / tagged policy with no require_tags
  — all with a wrapped error naming the offending field path.
- [x] `harbor validate` surfaces every new validation error verbatim
  (Phase 68 inherits the validator without changes).
- [x] The catalog builder fails closed at boot when an entry names
  an unknown tool / unknown policy / unknown OAuth provider — wrapped
  errors via `ErrToolNotRegistered` / `ErrUnknownApprovalPolicy` /
  `ErrUnknownOAuthProvider`.
- [x] Identity is mandatory in every wrapped Invoke; a missing triple
  fails closed.
- [x] The wrapped descriptors are safe for N≥128 concurrent goroutines
  under `-race` (D-025).
- [x] `cmd/harbor/cmd_dev.go::bootDevStack` constructs the catalog
  and applies `cfg.Tools.Entries` against it before serving traffic.
- [x] Integration test `test/integration/phase64a_catalog_wiring_test.go`
  exercises the full APPROVE / REJECT round-trip, the OAuth wrapper
  happy + ErrAuthRequired paths, the composition order pin, identity
  propagation, concurrency stress (N=16), and the goroutine-leak
  on initiate-then-cancel.

## Files added or changed

- `internal/tools/catalog/catalog.go` — new package; the `Builder` +
  `WrapWithApproval` + `WrapWithOAuth` surfaces.
- `internal/tools/catalog/catalog_test.go` — unit tests + policy /
  binding-scope allowlist mirror tests.
- `internal/tools/catalog/concurrent_test.go` — D-025 N=128
  concurrent-reuse test under `-race`.
- `internal/tools/tools.go` — extended with the `CatalogReplacer`
  optional interface.
- `internal/tools/catalog.go` — implements `Replace` on the
  in-memory catalog.
- `internal/config/config.go` — extended with `ToolsConfig.Entries`,
  `ToolEntryConfig`, `ToolApprovalConfig`, `ToolOAuthConfig`.
- `internal/config/validate.go` — extended with the
  `tools.entries[]` validation + the policy / binding-scope
  allowlists.
- `internal/config/validate_test.go` — new tests for the new
  validation rules.
- `cmd/harbor/cmd_dev.go` — boot-stack wiring: constructs
  `tools.NewCatalog()` + `pauseresume.New()` and applies the catalog
  builder against `cfg.Tools.Entries`. Exposes the catalog +
  Coordinator on `devStack` for future phases.
- `scripts/smoke/phase-64a.sh` — package-test + integration-test +
  `harbor validate` accept-and-reject smoke.
- `test/integration/phase64a_catalog_wiring_test.go` — cross-subsystem
  integration test (§17.3 mandatory: real drivers everywhere).
- `docs/plans/README.md` — Phase 64a row appended; pre-plan note
  constraint #7 marked closed.
- `docs/plans/phase-64a-tool-catalog-wiring.md` — this file.
- `docs/decisions.md` — D-090 entry appended.
- `README.md` — Status row Phase 64a → Shipped.

## Public API surface

- `tools.CatalogReplacer` — optional `Replace(wrapped)` interface on
  catalog implementations supporting atomic per-tool swap.
- `catalog.Builder`, `catalog.Deps`, `catalog.New(entries, deps)`.
- `catalog.WrapWithApproval(d, gate, ApprovalWrapperOptions)`.
- `catalog.WrapWithOAuth(d, prov, OAuthWrapperOptions)`.
- Sentinels: `catalog.ErrCatalogRequired`, `ErrCoordinatorRequired`,
  `ErrBusRequired`, `ErrRedactorRequired`, `ErrToolNotRegistered`,
  `ErrUnknownApprovalPolicy`, `ErrUnknownOAuthProvider`,
  `ErrInvalidBindingScope`, `ErrAlreadyApplied`.
- `config.ToolEntryConfig`, `config.ToolApprovalConfig`,
  `config.ToolOAuthConfig`, `config.ToolsConfig.Entries`.

## Test plan

- **Unit:**
  - `TestApply_NilCatalog_FailsLoud` — Deps.Catalog nil.
  - `TestApply_EmptyEntries_NoOp` — empty list passes; catalog
    unchanged.
  - `TestApply_UnknownTool_FailsLoud` — `ErrToolNotRegistered`.
  - `TestApply_UnknownPolicy_FailsLoud` —
    `ErrUnknownApprovalPolicy`.
  - `TestApply_UnknownOAuthProvider_FailsLoud` —
    `ErrUnknownOAuthProvider`.
  - `TestApply_MissingApprovalDeps_FailsLoud` —
    Coordinator / Bus / Redactor required for approval entries.
  - `TestApply_ApprovalWrapper_ApproveCycle` — gate APPROVE fires;
    inner tool runs with original args.
  - `TestApply_ApprovalWrapper_RejectCycle` — gate REJECT;
    `*ErrToolRejected` propagates.
  - `TestApply_OAuthWrapper_HappyPath` — token resolved; inner tool
    runs.
  - `TestApply_OAuthWrapper_ErrAuthRequiredPropagates` —
    `*ErrAuthRequired` propagates verbatim.
  - `TestApply_OAuthWrapper_NoIdentity_FailsLoud` — missing identity.
  - `TestApply_ApprovalWrapper_NoIdentity_FailsLoud` — same.
  - `TestApply_BothMiddleware_ApprovalIsOutermost` — D-090 order pin.
  - `TestApply_AlreadyApplied_FailsLoud` — Builder is one-shot.
  - `TestValidateTools_PolicyAllowlistMirrors_ApprovalPackage` — the
    config's allowlist names a policy the builder can resolve.
  - `TestValidateTools_BindingScopeAllowlistMirrors_AuthPackage` —
    same for binding scopes.
  - Config-level: `TestValidateTools_Entries` exercises every
    invariant the validator enforces.
- **Integration:**
  - `test/integration/phase64a_catalog_wiring_test.go` —
    `TestE2E_Phase64a_FullApproveCycle`,
    `TestE2E_Phase64a_FullRejectCycle`,
    `TestE2E_Phase64a_OAuth_ErrAuthRequiredPropagates`,
    `TestE2E_Phase64a_OAuth_HappyPath`,
    `TestE2E_Phase64a_FailureMode_UnknownTool`,
    `TestE2E_Phase64a_ConcurrencyStress`,
    `TestE2E_Phase64a_GoroutineLeak_InitiateThenCancel`,
    `TestE2E_Phase64a_BothMiddleware_ApprovalIsOutermost`.
- **Conformance:** N/A — Phase 64a is a wiring phase, not a multi-
  driver subsystem.
- **Concurrency / leak:**
  - `TestConcurrent_OAuthWrapper_Reuse` — N=128 concurrent under
    `-race`.
  - `TestConcurrent_OAuthWrapper_CancellationCrossTalk` — cancel A;
    B unaffected.
  - `TestConcurrent_ApprovalWrapper_Reuse` — N=128 concurrent under
    `-race`.
  - `TestConcurrent_OAuthWrapper_ErrAuthRequiredUnderRace` — typed
    sentinel propagation under load.
  - `TestE2E_Phase64a_GoroutineLeak_InitiateThenCancel` — gate leak
    check.

## Smoke script additions

- `scripts/smoke/phase-64a.sh` — 7 assertions:
  1. `internal/tools/catalog` tests under `-race`.
  2. Phase 64a integration test under `-race`.
  3. `harbor validate` accepts a config with valid `tools.entries`.
  4. `harbor validate` rejects an unknown approval policy with a
     named-field error.
  5. Static guard: `Builder` + `WrapWithApproval` + `WrapWithOAuth`
     declared.
  6. Static guard: §13 fail-loud sentinels declared.
  7. Static guard: D-090 composition order documented.

## Coverage target

- `internal/tools/catalog`: 80% (achieved: 89.3%).

## Dependencies

- Phase 26 (tool catalog core) — owns the `ToolCatalog` interface +
  in-memory catalog; we extend it with `CatalogReplacer`.
- Phase 30 (tool OAuth, D-083) — `auth.OAuthProvider` consumed by the
  wrapper.
- Phase 31 (tool approval gates, D-086) — `approval.ApprovalGate`
  consumed by the wrapper.
- Phase 50 (pause/resume coordinator, D-067) — the unified primitive
  both wrappers converge on.
- Phase 64 (`harbor dev` v1, D-089) — the boot-stack consumer.
- Phase 68 (`harbor validate`, D-088) — inherits the new validation
  rules through `internal/config`.

## Risks / open questions

- **Wire-side APPROVE / REJECT bridge.** The Protocol `approve` /
  `reject` methods route through steering → Coordinator.Resume;
  bridging that into the gate's `pending` map needs a steering-aware
  gate subscriber OR a dispatcher-side `ApprovalDispatcher`. That
  bridge is the Wave 11 wave-end E2E concern (Stage 4) — issue #104's
  Protocol-wire half. Phase 64a closes the catalog-wiring half;
  the test exercises the gate's in-process `ResolveApproval` to
  prove the wrapper composition works end-to-end.
- **OAuth provider construction.** Phase 64a wires the BINDING side
  (`tools.entries[].oauth.provider` resolves to a provider in the
  `Deps.OAuthProviders` map). The actual `tools.oauth_providers[]`
  block + the OAuth-provider-per-source construction lands in a later
  phase. For now, the dev cmd builds an empty map; an entry that
  declares `oauth` will fail closed at boot (the §13 fail-loud is
  the design).
- **Future hot reload.** `tools.entries` is restart-required for
  Phase 64a. Phase 65 (`harbor dev` hot-reload) extends to support
  reload; the builder's one-shot Apply needs an UnApply path.

## Glossary additions

None — the existing terms `tool catalog`, `approval gate`, `OAuth
provider`, `binding scope`, `composition order`, `pause/resume
primitive` already cover the surface.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (catalog: 89.3% vs
      80% target)
- [x] If multi-isolation paths changed: cross-session isolation test
      passes — the wrappers propagate identity via ctx; the per-tenant
      concurrent stress in `concurrent_test.go` covers it.
- [x] **Concurrent-reuse test passes** — N=128 concurrent invocations
      against a single shared wrapped descriptor under `-race`,
      asserting no data races, no context bleed, no cancellation
      cross-talk, no goroutine leaks. See
      `internal/tools/catalog/concurrent_test.go`.
- [x] **Integration test exists** —
      `test/integration/phase64a_catalog_wiring_test.go` wires real
      Coordinator + Bus + Redactor + catalog; identity propagates;
      ≥1 failure mode (`TestE2E_Phase64a_FailureMode_UnknownTool`);
      `-race` clean.
- [x] No new vocabulary — N/A.
- [x] No brief findings departed from.
