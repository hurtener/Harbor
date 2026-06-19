# Phase 91 — console-key-rotation

## Summary

Ships **Console-driven LLM provider key rotation** — an admin rotates the LLM provider API key **live, with no redeploy**, via the admin-scoped Protocol method `governance.rotate_key`. The mechanism is the one **settled in D-019** (RFC §6.15): the Console pushes a new key value over the Protocol → Harbor's bifrost `Account` swaps the live key **atomically** (an `atomic.Pointer` over the key holder) → bifrost reads it on the **next call** via `Account.GetKeysForProvider(ctx, …)`. No `ReloadConfig` race; the old key is invalidated immediately.

This is the **operational-security** sibling of Phase 92 (tenant-default overrides): same admin-gated `/v1/governance/*` surface, same audit posture, same "no redeploy" goal — but rotation is **immediate**, not next-turn, because a key may be compromised and the old value must stop working at once (Phase 92's parameters are per-run, so they snapshot at run start; a credential is not).

A key is a **secret** (CLAUDE.md §7): the new key value travels only on the request leg, is held only in the atomic key holder, and **never** appears in logs, audit payloads, or responses — the `governance.key_rotated` audit event carries provider + a non-reversible fingerprint (e.g. last-4 / SHA-256 prefix) + actor + timestamp, never the key.

## RFC anchor

- RFC §6.15
- RFC §6.5

## Briefs informing this phase

- brief 03
- brief 08

## Brief findings incorporated

- brief 03 (Tools + integrations + LLM client) + brief 08 (LLM client validation — bifrost): the LLM key reaches bifrost through the `Account.GetKeysForProvider` seam — bifrost calls it per request. Today the bifrost `Account` holds the resolved key as a plain string baked at construction (`internal/llm/drivers/bifrost/account.go`). The seam is already in the right place; Phase 91 makes the value behind it **atomically swappable** rather than introducing a new call path.
- D-019 (key rotation — settled mechanism): atomic key swap behind `GetKeysForProvider`, no `ReloadConfig`, old key invalidated immediately. Phase 91 realises exactly this. The RFC §6.15 `KeyResolver` stub (`per-call key selection (wraps bifrost.KeySelector)`) is the named seam; Phase 91 ships the **rotation** facet (swap the primary key), not the full multi-key resolver (multi-key/failover is Phase 93+).
- D-025 (compiled artifacts immutable; per-run state in `ctx`): the bifrost driver/Account is a compiled artifact. The rotatable key is the ONE sanctioned mutable field — guarded by an `atomic.Pointer` and documented "internally synchronised" per the §5 carve-out (driver registries + atomics are the allowed exceptions). The swap must not introduce any other mutable state, must not block in-flight calls, and must pass an N≥100 concurrent rotate-while-read test under `-race`.
- D-086 / Phase 73m (`auth.rotate_token` precedent): the admin-scoped rotate verb is a settled shape — `RotateSurface` + admin gate + redacted audit emitted only on success + the `POST /v1/<family>/<verb>` stream handler with error classification. `governance.rotate_key` mirrors it, on the `/v1/governance/` family Phase 92 established.
- Phase 26a `Sealer` (AES-256-GCM encryption-at-rest, `internal/tools/auth/sealer.go`): reusable IF Phase 91 persists the rotated key (see the open question on restart behaviour).

## Findings I'm departing from (if any)

None. D-019 settled the mechanism; this phase implements it. The one decision left open by D-019 — whether a rotated key **persists** across runtime restart — is surfaced as a risk/open question below (recommended: in-memory swap for V1 with a loud restart-reverts-to-config caveat, encrypted persistence via the Phase 26a `Sealer` as a gated option), to be pinned in D-233.

## Goals

- A swappable live-key holder behind the bifrost `Account`: the resolved key lives in an `atomic.Pointer` (a shared `llm` key holder injected via `llm.Deps`, read by `Account.GetKeysForProvider`, written by the rotate path). No new per-call code path; no `ReloadConfig`.
- A new admin-scoped Protocol method `governance.rotate_key`: validates identity + the verified `auth.ScopeAdmin` claim (authority from the verified ctx, not the body), swaps the key atomically, emits `governance.key_rotated` (secret-free), returns a non-secret confirmation (provider + fingerprint + rotated-at).
- Immediate effect: the next `Complete` after the rotate uses the new key; the old key never serves another call.
- Secret hygiene: the new key value is accepted on the request body, held only in the atomic holder, and never logged / audited / echoed (CLAUDE.md §7). The request leg is bounded + the body identity is defence-in-depth-checked like the other admin verbs.
- Full identity scoping + admin gate + audit; a non-admin caller is rejected with `CodeScopeMismatch`.

## Non-goals

- **Multi-key per provider / weighted key sets / failover** — bifrost's `[]Key` shape supports it, but Phase 91 rotates the single primary key; multi-key + `FailoverPolicy` + `CircuitBreaker` are Phase 93/94 (RFC §6.15).
- **The full `KeyResolver` interface** (per-call identity-scoped key selection) — Phase 91 ships rotation, not per-identity key routing.
- **Provider swap** (changing the provider, not just its key) — out of scope; Phase 91 swaps the credential for the bound provider.
- **KEK / secret-management infrastructure beyond the existing `Sealer`** — if persistence is chosen, it reuses the Phase 26a `Sealer` + an operator-supplied KEK; Phase 91 does not build a new secrets subsystem.
- **Mid-call rotation** — a rotate takes effect on the NEXT call; an in-flight call completes on the key it started with (no cancellation of in-flight requests).

## Acceptance criteria

- [ ] `governance.rotate_key` swaps the LLM provider key: admin scope required (non-admin → `CodeScopeMismatch`/403); a valid rotate makes the NEXT `Complete` use the new key; the old key is not used again.
- [ ] The new key value never appears in any log, the audit event, or the response — verified by a test asserting the redactor/audit payload + response carry only a fingerprint, and a log-scan test.
- [ ] `governance.key_rotated` audit event emitted on success (provider + fingerprint + actor + rotated-at); a failed rotate emits nothing.
- [ ] **Concurrent-reuse**: N≥100 concurrent `GetKeysForProvider` reads interleaved with rotates against one shared Account holder under `-race` — no torn reads, no race, every read returns a valid (old-or-new) key.
- [ ] Identity isolation + fail-loud: a missing identity fails closed; a rotate with an empty/invalid key value is rejected (`CodeInvalidRequest`) before any swap.
- [ ] Protocol surface: `governance.rotate_key` in `methods.go` (single source) + `IsGovernanceAdminMethod` + the TS lockstep manifest + the regenerated Protocol docs + a `scripts/smoke/phase-91.sh` assertion.
- [ ] Operator skill(s) updated (§18 — the protocol skill's admin-control-surface section).

## Files added or changed

- `internal/llm/drivers/bifrost/account.go` — wrap the resolved key in the atomic holder; `GetKeysForProvider` reads it; a `RotateKey` swap method (or read from an injected shared holder).
- `internal/llm/llm.go` / `internal/llm/deps.go` — the shared live-key holder type (`atomic.Pointer`-backed) on `llm.Deps` (the read+write seam injected at boot), OR a `KeyRotator` capability interface the client surfaces. (Recommended: shared holder via Deps — clean, D-025-safe, no reaching through the wrapper chain.)
- `internal/protocol/methods/methods.go` — `MethodGovernanceRotateKey = "governance.rotate_key"` + add to `canonicalGovernanceAdminMethods`.
- `internal/protocol/types/governance.go` — `GovernanceRotateKeyRequest` (identity + provider + key value) + `GovernanceRotateKeyResponse` (provider + fingerprint + rotated_at; NO key).
- `internal/runtime/governance/protocol/` — the rotate-key service (validate + invoke the holder swap + audit), mirroring the tenant-override service.
- `internal/protocol/transports/stream/governance_handler.go` — route `governance/rotate_key` → admin gate → service (extend the existing handler).
- `internal/governance/events.go` — `governance.key_rotated` event + secret-free payload + register.
- `internal/protocol/singlesource/singlesource.go` + the wire manifest (regen) + `cmd/harbor-*` typeindex/docs (regen) — the new method + types.
- `cmd/harbor/cmd_dev.go` (+ `harbortest/devstack` mirror, D-094) — construct the shared key holder, inject into the bifrost driver + the rotate service, wire the governance service.
- `scripts/smoke/phase-91.sh`; `docs/skills/use-the-harbor-protocol/SKILL.md` (§18); `docs/glossary.md`; `docs/decisions.md` (D-233); `docs/plans/README.md` + `README.md`.

## Public API surface

- New Protocol method `governance.rotate_key` (request: `{identity, provider, key}`; admin-scoped; immediate effect). New canonical event `governance.key_rotated` (secret-free).
- A new `llm` live-key holder type (or `KeyRotator` interface) — internal seam; no change to the `LLMClient` / `Subsystem` interface signatures.
- `RunOverrides` / governance tenant types unchanged.

## Test plan

- **Unit:** the atomic key holder (swap + read; empty-key reject); the rotate service (identity validation, admin gate delegated to handler, secret never on the audit payload); `GetKeysForProvider` returns the swapped value after a rotate.
- **Integration:** `test/integration/key_rotation_test.go` — real bifrost Account (mock provider OR the mock LLM driver) + the rotate service + wire transport: a rotate changes the key the next call uses; non-admin rejected; the audit event fires secret-free; identity isolation. Under `-race`.
- **Concurrency / leak:** N≥100 concurrent reads + rotates against one shared holder under `-race` (the holder is the reusable artifact); no torn read, no leak.
- **Secret hygiene:** a test that captures the audit bus + the slog output during a rotate and asserts the key value appears in NEITHER; the response carries only the fingerprint.
- **Protocol lockstep:** `make protocol-ts-gen-check` + `make protocol-docs-gen-check` green.

## Smoke script additions

`scripts/smoke/phase-91.sh`:

- Static: `methods.go` declares `governance.rotate_key`; the atomic key holder is present in the bifrost account; `governance.key_rotated` event registered.
- Live (preflight dev server): `governance.rotate_key` with admin scope returns 200 + a fingerprint (no key echoed); a non-admin call → 403; an empty-key body → 400. (The next-call-uses-new-key behaviour is covered by the integration test, not the smoke — asserting key identity over the wire would risk leaking it.)

## Coverage target

- Touched governance + llm/bifrost key-holder packages: 85%.
- `internal/protocol/*` touched lines: no regression.

## Dependencies

- 36a (governance subsystem + the `/v1/governance/` admin surface Phase 92 extended)
- 60 (Protocol transport)
- 73 (Console attaching — the operator workflow; RFC §6.15 notes key rotation "needs Console to land first")
- 26a (the `Sealer` — only if persistence is chosen)
- (prerequisite, shipped) the bifrost `Account.GetKeysForProvider` seam; the `auth.rotate_token` + `governance.set_tenant_overrides` admin-verb precedents.

## Risks / open questions

- **Persistence across restart (the one open decision D-019 left).** In-memory atomic swap (D-019's literal mechanism) means a runtime restart reverts to the config/env key — which, if that key was rotated BECAUSE it was compromised, silently re-arms the compromised credential. Options: (a) in-memory only + a LOUD boot/rotate caveat that a restart reverts to the config key (simplest; correct if rotation is "use a fresh key now", and the operator updates config/env out-of-band); (b) persist the rotated key encrypted via the Phase 26a `Sealer` keyed by an operator KEK, so it survives restart (more secure, needs a KEK + a StateStore record). Recommend (a) for V1 with the caveat documented + an explicit stderr warning at rotate, (b) as a gated follow-up; **pin in D-233.**
- **Secret on the wire.** The new key crosses the Protocol on the request leg (TLS-protected in production, but plaintext in the body). This is intrinsic to "Console pushes a new key value" (D-019). Mitigations: bounded body, no logging of the body, the route is admin-only + TLS-required in production. Confirm the dev transport does not log request bodies on this route.
- **Injection seam choice.** Shared key holder via `llm.Deps` (recommended — read by the bifrost driver, written by the rotate service, no wrapper-chain reach) vs a `KeyRotator` capability accessor surfaced through the wrapped client chain. Confirm at implementation; the shared holder keeps the swap point a single internally-synchronised atomic.
- **Provider granularity.** V1 rotates the single bound provider's primary key. The request carries `provider` for forward-compat (multi-provider), but a value other than the bound provider is rejected (`CodeInvalidRequest`) until multi-provider lands (Phase 93+).
- **Custom providers.** Each custom provider resolves its own key at boot; V1 rotation targets the primary. Multi-custom-provider rotation is deferred (documented, not silently dropped).

## Glossary additions

- **Console-driven key rotation** — an admin rotates the LLM provider API key live (no redeploy) via the admin-scoped `governance.rotate_key` Protocol method; the bifrost `Account` swaps the key atomically behind `GetKeysForProvider` so the next call uses it and the old key is invalidated immediately (D-019). The key value is a secret — never logged / audited / echoed; the `governance.key_rotated` event carries only a fingerprint. Phase 91, RFC §6.15, D-233.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes (doc-only planning PR may skip the local preflight via `HARBOR_PREFLIGHT_SKIP=1` with justification; CI gates — this plan PR carries no code)
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (at implementation)
- [ ] If multi-isolation paths changed: cross-session + cross-tenant isolation test passes (binding at implementation)
- [ ] **Concurrent-reuse test passes** — N≥100 rotate-while-read under `-race` (the key holder is the reusable artifact; binding at implementation)
- [ ] **Integration test exists** — `test/integration/key_rotation_test.go`, real Account + rotate service + wire transport, immediate-effect, admin gate, secret-free audit, under `-race` (binding at implementation)
- [ ] **Secret-hygiene test** — the key value appears in no log / audit / response (binding at implementation)
- [ ] Protocol changed → `methods.go` single-source + TS lockstep + `make protocol-ts-gen-check` + `make protocol-docs-gen-check` + a smoke assertion (binding at implementation)
- [ ] Operator skill updated for the changed surface (§18)
- [ ] If new vocabulary: glossary updated (Console-driven key rotation)
- [ ] decisions.md entry filed (D-233 — the rotation mechanism realisation + the persistence-across-restart decision)
