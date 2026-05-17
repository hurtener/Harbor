# Phase 66 — `harbor dev` draft-save scaffolding

## Summary

Phase 66 ships the `harbor dev` draft-save surface. Operators iterate on an
agent skeleton under a project-local `.harbor/drafts/` scratchpad without
committing the scaffold; a "save" promotes the draft to a `harbor scaffold`-
emitted layout under an operator-supplied output dir, and refuses promotion
when the rendered `harbor.yaml` fails `internal/config.Load + Validate`. The
surface is HTTP-only at V1 (mounted on the existing `harbor dev` mux at
`/v1/dev/drafts/`, behind the Phase 61 JWT validator); the Console is the
intended consumer once it lands. Identity scoping is enforced on disk via a
`<root>/<tenant>/<user>/<session>/<draft_id>/` subpath so concurrent operators
sharing the same `.harbor/drafts/` root cannot collide.

## RFC anchor

- RFC §8

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §"DevX expectations": the dev loop should let an operator
  iterate without committing scaffold output to the working tree —
  Phase 66 closes that with a per-operator scratchpad whose lifecycle
  is operator-visible (`.harbor/drafts/`) but separately disposable.
- brief 06 §"Fail-loud at boot": fail-loud at the seam, not at the
  next operator command — `Store.Save` refuses to promote a draft
  whose `harbor.yaml` does not pass `internal/config.Load + Validate`.

## Findings I'm departing from (if any)

None.

## Goals

- Project-local `.harbor/drafts/<tenant>/<user>/<session>/<draft_id>/`
  scratchpad mirroring the scaffold engine's output tree.
- HTTP round-trip: `create → get → patch → preview → save → delete`
  under `/v1/dev/drafts/` on the existing `harbor dev` mux.
- Save promotes the draft through the same engine `harbor scaffold`
  invokes, after running the rendered config through
  `internal/config.Load + Validate` as a fail-loud gate.
- Identity-scoped on disk: concurrent operators sharing the
  `.harbor/drafts/` root cannot collide; cross-identity reads return
  ErrNotFound.
- Lifecycle observable on the canonical event bus: `dev.draft.created`,
  `dev.draft.updated`, `dev.draft.previewed`, `dev.draft.saved`,
  `dev.draft.discarded`.

## Non-goals

- Real preview run (a dry-run that boots the draft against a
  sandboxed runtime). The V1 preview is a validation-only pass.
  A follow-up phase upgrades this with the existing event shape.
- A `harbor dev draft ...` CLI sub-CLI. The Console + scripted
  clients are the intended consumers; a CLI surface adds operator
  friction without solving a load-bearing use case at V1.
- Draft listing (`GET /v1/dev/drafts/`). Reserved for a follow-up;
  the V1 acceptance criterion does not require it.
- Cross-machine draft replication. The Store is a single-host
  filesystem-backed component by design.

## Acceptance criteria

- [x] `POST /v1/dev/drafts/` materialises a draft tree under
  `<root>/<tenant>/<user>/<session>/<draft_id>/` and returns
  `{draft_id, files[]}`.
- [x] `GET /v1/dev/drafts/{id}` lists files + content for the Console
  editor.
- [x] `PATCH /v1/dev/drafts/{id}/files/{path}` writes a file's content;
  path-traversal-safe per CLAUDE.md §7 rule 5.
- [x] `POST /v1/dev/drafts/{id}/preview` runs `internal/config.Load`
  against the rendered `harbor.yaml`; returns `{ok, errors[]}`.
- [x] `POST /v1/dev/drafts/{id}/save` promotes the draft to an
  operator-supplied output dir. Refuses with `ErrValidationFailed`
  when the rendered config fails `internal/config.Load`.
- [x] `DELETE /v1/dev/drafts/{id}` discards the draft (idempotent).
- [x] Cross-identity reads return `ErrNotFound`.
- [x] Missing-identity requests return HTTP 401 with the
  `identity_required` error code.
- [x] Five lifecycle events land on the bus per round-trip
  (`created`, `updated`, `previewed`, `saved`, `discarded`).
- [x] `scripts/smoke/phase-66.sh` exercises the round-trip against the
  live `bin/harbor dev`; the 404/405/501 → SKIP convention keeps the
  smoke harmless on builds that pre-date Phase 66.
- [x] `test/integration/phase66_draft_save_test.go` runs the round-trip
  through the devstack helper with real drivers, asserts the bus
  events, exercises the path-traversal failure mode, the missing-
  bearer failure mode, and an N=10 concurrency stress under `-race`.

## Files added or changed

```text
internal/
├── devdraft/                # NEW package
│   ├── doc.go
│   ├── devdraft.go          # Store + types + sentinels
│   ├── events.go            # 5 EventTypes + SafePayload structs
│   ├── http.go              # Handler + wire shapes + error mapping
│   ├── path_safety.go       # §7 rule 5 helper (mirror of importer/path_safety.go)
│   ├── devdraft_test.go     # unit tests (Store happy + failure modes)
│   ├── http_test.go         # handler tests (per-endpoint + auth)
│   └── concurrent_test.go   # D-025 N≥100 concurrent reuse test

cmd/harbor/
└── cmd_dev.go               # wire draftStore + handler into bootDevStack,
                             # register `/v1/dev/drafts/` under auth.Middleware

harbortest/devstack/
└── devstack.go              # D-094 mirror: always construct DraftStore,
                             # mount handler when transports are enabled

test/integration/
└── phase66_draft_save_test.go  # cross-subsystem E2E

scripts/smoke/
└── phase-66.sh              # smoke (round-trip through the live binary)

docs/
├── decisions.md             # +D-100
├── glossary.md              # +.harbor/drafts/
└── plans/
    ├── README.md            # Phase 66 row → Shipped
    └── phase-66-harbor-dev-draft-save.md  # this file

README.md                    # Status row Phase 66 → Shipped
```

## Public API surface

- `internal/devdraft.Store` — the filesystem-backed draft store. Methods:
  - `NewStore(Options) (*Store, error)`
  - `Create(ctx, CreateOptions) (*Draft, error)`
  - `Get(ctx, draftID) (*Draft, error)`
  - `WriteFile(ctx, draftID, relPath, content) error`
  - `Preview(ctx, draftID) (*PreviewResult, error)`
  - `Save(ctx, draftID, SaveOptions) (*SaveResult, error)`
  - `Discard(ctx, draftID) error`
  - `Root() string` (test-only inspection helper)
- `internal/devdraft.NewHandler(*Store, *slog.Logger) (*Handler, error)`
  — the `http.Handler` `harbor dev` mounts at `RoutePrefix`.
- `internal/devdraft.RoutePrefix` (`= "/v1/dev/drafts"`) — the mount
  point.
- `internal/devdraft.EventType{Created,Updated,Previewed,Saved,Discarded}`
  — registered with `internal/events` at init().
- Typed sentinels: `ErrIdentityMissing`, `ErrNotFound`, `ErrUnsafePath`,
  `ErrUnknownTemplate`, `ErrInvalidName`, `ErrOutputDirExists`,
  `ErrValidationFailed`, `ErrIO`.

## Test plan

- **Unit:** `internal/devdraft/devdraft_test.go` covers Store happy
  path (create / get / write / preview / save / discard), every
  failure mode (missing identity, invalid name, unknown template,
  path traversal, oversize file, broken yaml on preview + save,
  pre-existing output dir), cross-identity isolation, idempotent
  discard, and the lifecycle-event emit shape.
  `internal/devdraft/http_test.go` covers the handler per-endpoint
  with raw httptest (create, patch, preview, save, delete, missing
  identity → 401, path traversal → 400/CodeUnsafePath, broken yaml
  → 400/CodeValidationFailed, full round-trip).
- **Integration:** `test/integration/phase66_draft_save_test.go`
  wires the devstack helper, drives the round-trip over HTTP with a
  real Bearer token, observes the five bus events, exercises path-
  traversal + missing-bearer failure modes, and runs an N=10
  concurrency stress under `-race`.
- **Conformance:** N/A — Store has no driver registry.
- **Concurrency / leak:** `internal/devdraft/concurrent_test.go`
  pins the D-025 obligation — N=128 concurrent invocations against
  one shared Store under `-race`, asserting no goroutine leak
  (baseline restored after every invocation returns).

## Smoke script additions

- `scripts/smoke/phase-66.sh`:
  1. Unauthenticated POST → 401.
  2. With dev token: POST create → 201 + non-empty `draft_id`.
  3. PATCH file → 200.
  4. Preview → 200 + `ok=true`.
  5. Save → 200 + the promoted `harbor.yaml` is on disk.
  6. DELETE → 200.
  7. GET after DELETE → 404.

  The script SKIPs the authenticated path when `HARBOR_DEV_TOKEN`
  cannot be parsed out of the preflight server log — the smoke must
  still pass against builds that do not yet print the token under
  this exact prefix.

## Coverage target

- `internal/devdraft`: 80%

## Dependencies

- Phase 02 (config) — `internal/config.Load` is the validator the
  save path runs.
- Phase 05 (events) — the lifecycle event bus surface.
- Phase 60 (protocol/transports) — the draft handler mounts under
  the same router the Protocol mux already lives on.
- Phase 61 (protocol/auth) — the same `auth.Middleware` wraps the
  draft handler.
- Phase 64 (`harbor dev` v1) — the binary the draft handler ships
  in.
- Phase 67 (`harbor scaffold`) — the engine `Store.Create` and
  `Store.Save` invoke for the seed + promoted shapes.

## Risks / open questions

- The Store is filesystem-backed; multi-process operators sharing
  the same `.harbor/drafts/` root rely on the identity-scoped
  subpath to avoid collisions but get no cross-process locking. A
  future phase that needs cross-process semantics (rare — drafts
  are per-operator) extends the Store with a file-lock or a
  durable backing store.
- The V1 preview path is a config-validation pass, not a real dry-
  run that boots the draft. A follow-up phase upgrades the preview
  surface in place (the bus event + the wire shape are stable
  across that upgrade).
- The handler does not list drafts (`GET /v1/dev/drafts/`). A
  follow-up phase adds it; the V1 acceptance criterion does not
  require it.

## Glossary additions

- `.harbor/drafts/` — the per-operator filesystem scratchpad the
  Phase 66 / D-100 draft store materialises agent skeletons under.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation
  test passes — the cross-identity `Get` test pins it.
- [x] **Concurrent-reuse test passes** —
  `internal/devdraft/concurrent_test.go::TestStore_ConcurrentReuse_NoRaceUnderLoad`
  runs N=128 concurrent invocations against one shared Store under
  `-race`.
- [x] **Integration test exists** —
  `test/integration/phase66_draft_save_test.go`.
- [x] If new vocabulary: glossary updated.
- [x] If a brief finding was departed from: justified above +
  decisions.md entry filed — D-100.
