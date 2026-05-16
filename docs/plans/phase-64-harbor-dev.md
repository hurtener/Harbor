# Phase 64 — `harbor dev` v1

## Summary

Phase 64 turns `cmd/harbor` from a driver-registration stub into a
real `harbor dev` server. It boots an embedded Runtime, mounts the
Phase 60 Protocol transports (REST + SSE) behind the Phase 61 JWT
auth middleware, mints an ephemeral ES256 dev-token for local
operators, fails loud at boot when no LLM provider is configured,
flips the LLM default driver from `mock` to `bifrost`, and wires an
LLM-backed `Summarizer` so `memory.strategy: rolling_summary`
resolves through production code paths. The §13 "test stubs as
production defaults on operator-facing seams" amendment closes for
the LLM seam in this phase.

## RFC anchor

- RFC §8
- RFC §5.2 (consumed via the Phase 60 mux mount)
- RFC §5.5 (consumed via the Phase 61 validator)

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §"DevX expectations": `harbor dev` exists as a single
  command that boots the whole runtime; identity injection via
  dev-token is operator-facing not buried; an opinionated default
  port (18080) lets operators avoid flag soup on the first clone.
- brief 06 §"Fail-loud at boot": missing-config is a top-level
  exit-non-zero with a named-field error, not a silent fallback.
- brief 06 §"Observability surface": every boot emits a structured
  slog line naming the resolved drivers + the LLM driver + the
  memory strategy, so an operator's terminal shows the substantive
  configuration at a glance.

## Findings I'm departing from (if any)

None. The §13 amendment recorded in the Phase 64 pre-plan note
supersedes any earlier brief stance on "operators wire the LLM
driver themselves"; this phase makes the LLM seam a first-class
operator-facing surface and treats a missing provider as a boot-time
fail-loud.

## Goals

- Boot a local Runtime + Protocol server on `127.0.0.1:18080` with
  zero further configuration when a real LLM provider is configured.
- Make `mock` LLM unreachable in a production binary path; the only
  surface that exposes it is the `HARBOR_DEV_ALLOW_MOCK=1` env-var
  escape hatch (banner'd on every boot).
- Wire the LLM-backed default `Summarizer` for `memory.strategy:
  rolling_summary` (`internal/llm/summarizer`) so a Phase 64 dev
  loop with rolling-summary configured resolves a real summariser
  through `inmem.New + Options{Summarizer: summarizer.New(client)}`.
- Print a default-identity dev-token at startup so a fresh-clone
  operator can `curl -H "Authorization: Bearer ${TOKEN}"` the
  Protocol surface without writing JWT-signing code.
- Provide a fail-loud-no-config path: an empty `llm:` block (or
  `driver: bifrost` without an api_key) exits non-zero with a
  one-line error naming the missing knob and pointing to
  `examples/dev.yaml`.

## Non-goals

- Hot-reload — that's Phase 65.
- Draft-save scaffolding — that's Phase 66.
- Console embedding — phases 72–75 ship the Console behind the
  Protocol; `harbor dev` only opens the Protocol surface.
- Tool-catalog OAuth + approval wiring from operator config —
  scoped out to Phase 64a (D-090), shipping in the same wave.
- Real OIDC JWKS wiring — the dev-token surface is in-memory ES256;
  production OIDC wiring lands in a later release-engineering phase.

## Acceptance criteria

- [ ] `harbor dev` returns `/healthz` 200 against a default config
  (the binding acceptance criterion).
- [ ] `harbor dev` against an empty `llm:` block (or `driver: mock`
  without `HARBOR_DEV_ALLOW_MOCK=1`) exits non-zero with a one-line
  error message that names the failing config key and points to
  `examples/dev.yaml`.
- [ ] `HARBOR_DEV_ALLOW_MOCK=1 harbor dev` boots against the same
  config (override-to-mock) and prints `[DEV-ONLY MOCK LLM — DO NOT
  USE IN PRODUCTION]` to stderr on every boot.
- [ ] `harbor dev` mints + prints a default-identity ES256 dev-token
  (`HARBOR_DEV_TOKEN=…`) on stderr at boot. The token carries
  `(tenant=dev, user=dev, session=dev)` plus `admin` scope.
- [ ] `llm.DefaultDriver` is `"bifrost"`. The default loader value
  (`config.defaults()`) matches.
- [ ] `internal/llm/summarizer` ships a production LLM-backed
  Summarizer; `harbor dev` wires it through
  `inmem.New(..., Options{Summarizer: ...})` when the operator
  configured `memory.strategy: rolling_summary`.
- [ ] The Phase 60 transport mux is mounted under `/v1/`. A POST to
  `/v1/control/start` with a valid Bearer token succeeds and returns
  a task id; without a Bearer token returns 401.
- [ ] `scripts/smoke/phase-64.sh` shows OK ≥ 5 and FAIL = 0.
- [ ] `test/integration/phase64_harbor_dev_test.go` covers the
  end-to-end boot + REST control + SSE event round-trip + failure
  modes + N≥16 concurrency stress under `-race`.

## Files added or changed

```text
cmd/harbor/
├── cmd_dev.go              # rewritten: real harbor dev (vs. Phase 63 stub)
├── cmd_dev_test.go         # added: unit tests for the dev cmd
├── devauth.go              # added: ES256 ephemeral signer + KeySet
├── devmock.go              # added: conditional mock blank-import + banner
├── cmd_stub_test.go        # updated: `dev` graduated out of stub table
└── testdata/golden/help.txt # updated: dev short description

internal/
├── config/loader.go        # default llm.driver flipped to "bifrost"
├── config/validate.go      # validateLLM comment updated for the flip
├── llm/registry.go         # DefaultDriver flipped to "bifrost"
├── llm/coverage_test.go    # test pins Driver="mock" (the flip §17.6 fix)
└── llm/summarizer/         # added: production LLM-backed Summarizer

examples/
├── dev.yaml                # added: canonical harbor dev config
└── harbor.yaml             # updated: driver: mock → bifrost

scripts/
├── preflight.sh            # exports HARBOR_DATA_DIR; passes --config
└── smoke/
    ├── phase-63.sh         # dev removed from the stub-list
    └── phase-64.sh         # added: LLM-seam + fail-loud smoke

test/integration/
├── phase64_harbor_dev_test.go        # added
└── phase64_harbor_dev_helpers_test.go # added

docs/
├── decisions.md            # +D-089
├── glossary.md             # +harbor dev, +dev token, +HARBOR_DEV_ALLOW_MOCK
└── plans/
    ├── README.md           # Phase 64 row → Shipped
    └── phase-64-harbor-dev.md # this file

README.md                   # Status row Phase 64 → Shipped
```

## Public API surface

- `cmd/harbor.runDev(cmd, args) error` — the cobra RunE for `dev`.
  Indirect surface — operators call `harbor dev`, not Go code.
- `internal/llm/summarizer.Summarizer` — implements
  `memory.Summarizer`; constructed via `summarizer.New(llmClient)`.

## Test plan

- **Unit:** `cmd/harbor/cmd_dev_test.go` covers `validateLLMProvider`
  (constraint #2 fail-loud + happy path + mock-escape short-circuit),
  `parsePortFromBind` (host:port + IPv6 + malformed), `newDevSigner`
  (fresh keys per call), `SignDevToken` (parseable JWT + identity
  triple mandatory), `bootErrorToCLIError` (sentinel mapping).
  `internal/llm/summarizer/summarizer_test.go` covers Summarize
  round-trip, identity propagation, LLM-error pass-through, ctx
  cancellation, model pinning, D-025 N=100 concurrent reuse.
- **Integration:**
  `test/integration/phase64_harbor_dev_test.go` boots the assembled
  dev stack (real audit, events, state, artifacts, LLM, memory,
  tasks, steering, protocol, auth, transports), drives a `start`
  over REST with a Bearer token, observes `task.spawned` on the SSE
  stream, exercises the unauthenticated-rejection failure mode, runs
  N=16 concurrency stress. The fail-loud-no-config path is exercised
  by the smoke script's assertion 6.
- **Conformance:** N/A — the dev cmd is a binary, not an interface.
- **Concurrency / leak:** the integration test's concurrency stress
  is D-025-checked at the transports mux boundary; the summarizer's
  D-025 N=100 test pins the per-package surface; the auth Validator
  and the transports mux already ship their own D-025 tests in their
  respective phase PRs (Phase 60 / Phase 61).

## Smoke script additions

- `scripts/smoke/phase-64.sh` assertions:
  1. `/healthz` returns 200.
  2. `/healthz` body `.status == "ok"`.
  3. `/readyz` returns 200.
  4. POST `/v1/control/start` without Bearer → 401.
  5. POST `/v1/control/start` with the dev token (parsed out of
     `${HARBOR_DATA_DIR}/server.log`) → 200 + non-empty task_id (the
     LLM-seam round-trip, constraint #5).
  6. Boot in a tmp dir with no config → exits non-zero with a
     named-field error (constraint #5 fail-loud half).

## Coverage target

- `cmd/harbor`: 75% (Phase 64 increment; the helpers + the
  validateLLMProvider gate are pinned by cmd_dev_test.go + the
  Phase 60 / 61 / 63 surfaces this phase consumes already meet their
  own targets).
- `internal/llm/summarizer`: 80% (new package; the test suite covers
  every path).

## Dependencies

- Phase 02 (config)
- Phase 03 (audit)
- Phase 05 (events)
- Phase 07 (state)
- Phase 17 (artifacts)
- Phase 20 (tasks)
- Phase 23 (memory)
- Phase 32 (llm)
- Phase 33 (bifrost driver)
- Phase 52/53 (steering)
- Phase 54 (protocol)
- Phase 60 (protocol/transports)
- Phase 61 (protocol/auth)
- Phase 63 (CLI skeleton)

## Risks / open questions

- The mock LLM driver is still blank-imported in `cmd/harbor/devmock.go`
  so it CAN be linked into a production binary, but it is unreachable
  unless `HARBOR_DEV_ALLOW_MOCK=1` is set AND the banner fires. A
  future refactor that wants stricter unreachability uses a
  `harbor_testfixtures` build tag; the trade-off is every test
  importing the mock then needs the same tag.
- The dev token's identity triple is fixed (`tenant=dev, user=dev,
  session=dev`). An operator who wants to test multi-tenant
  isolation against the dev server needs a more general token-mint
  surface (deferred; the dev-token surface is intentionally minimal —
  operators wanting multi-tenant test tokens use the public Phase 61
  `auth.Validator` directly).
- The dev signer's keypair is in-memory and regenerated per boot.
  Operators relying on stable token values across restarts will
  notice; document in the README's quickstart.

## Glossary additions

- `harbor dev` — the `cmd/harbor` subcommand that boots a local
  Runtime + Protocol server. Phase 64 / D-089.
- `HARBOR_DEV_ALLOW_MOCK` — the env-var escape hatch that allows the
  `harbor dev` subcommand to route the LLM seam through the mock
  driver. Banner'd on every boot. Phase 64 / D-089.
- `HARBOR_DEV_TOKEN` — the named stderr line `harbor dev` prints at
  boot carrying the ES256 dev-token operators use to authenticate
  Protocol requests. Phase 64 / D-089.
- `dev signer` — the ephemeral ES256 keypair `harbor dev` mints at
  boot to sign dev tokens. In-memory; regenerated per boot.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test
  passes — N/A; identity flows through unchanged surfaces.
- [ ] **Concurrent-reuse test passes** — the integration test runs
  N=16 concurrent requests against one shared dev stack under
  -race; the summarizer test runs N=100.
- [ ] **Integration test exists** — `phase64_harbor_dev_test.go`.
- [ ] If new vocabulary: glossary updated ✓
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — D-089.
