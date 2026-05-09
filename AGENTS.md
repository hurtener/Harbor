# Harbor — Contributor & Agent Normatives

> This file is **binding** for anyone (human or AI) modifying this repository. It is mirrored verbatim in `CLAUDE.md` so Claude Code picks it up automatically. If the two files diverge, the most recent commit timestamp wins; flag the drift in your PR.

If a rule below conflicts with a section of the RFC or a phase plan, the **RFC wins**, then the **phase plan**, then this file. Update whichever artifact is wrong; do not silently ignore the conflict.

---

## Starting a new session — orientation (READ THIS FIRST)

If this is the start of a fresh Claude Code session (or your first time touching this repo), the design surface is large and intentionally so — Harbor is an ~80-phase, multi-month solo build that uses doc-driven hygiene to prevent drift. **Skim these in order before substantive work:**

1. **§1 ("What Harbor is")** — the four-layer architecture and the three non-negotiable product properties.
2. **§2 ("Authoritative sources")** — the priority chain: RFC > phase plans > this file > research briefs > code comments. When two artifacts disagree, the higher-priority one wins.
3. **§16 ("Authoring a phase plan")** — the binding workflow for any contributor (human or AI) touching a phase plan. **Skipping this is the single largest source of design drift.** The workflow's brief-reading and template-copying steps are forcing functions, not suggestions.

**Drift hygiene artifacts (live references, all under `docs/` and `scripts/`):**
- `docs/decisions.md` — append-only log of settled architectural decisions (D-001..D-NNN). When tempted to re-litigate something, grep here first.
- `docs/glossary.md` — Harbor vocabulary. New terms land here in the same PR.
- `docs/research/INDEX.md` — subsystem → research-briefs reverse index. Tells you which briefs to read for the subsystem you're touching.
- `docs/plans/_template.md` — phase plan template. New phases start as `cp _template.md phase-NN-slug.md`.
- `scripts/drift-audit.sh` — mechanical drift checks. Run via `make drift-audit` (also runs as part of `make preflight`).

**If asked to do something that doesn't fit any of those frames** (a one-off bug fix, a question, a small doc edit) you can proceed without the full §16 ritual — but mention any drift risk you noticed and offer to file a follow-up if needed.

**The naming rule** (§13) is hard: never name the predecessor project — its actual name, abbreviations, "the prior project", author names, or any synonym — anywhere in committed text. The drift-audit enforces this on rule files, the master plan, and phase plans. Internal context in chat is fine; the repo is Harbor-only.

---

## 1. What Harbor is

Harbor is a Go-native runtime SDK for durable, steerable, event-driven AI agents. It ships as a runtime library plus a single static binary (`harbor`), with a four-layer architecture:

1. **Harbor Runtime** — orchestration kernel. Owns: tasks, planner runtime, tools, memory, sessions, events, skills, artifacts, the unified pause/resume primitive.
2. **Harbor Protocol** — the canonical event/state contract. Streaming events, task control surface, observability APIs. Versioned independently of the Runtime.
3. **Harbor Console** — observability and control-plane UI. Ships with the ecosystem; architecturally a Protocol client.
4. **Harbor CLI** — the `harbor` binary. Drives `harbor dev` (local Runtime + Console + hot reload + dynamic agent scaffolding with draft saving), plus deploy/scaffold/validate.

Three product properties are non-negotiable:

1. **Multi-isolation from V1.** Every layer carries the `(tenant, user, session)` identity triple. One user can be in multiple sessions concurrently and they must remain isolated. No code is allowed that assumes a single tenant, user, or session.
2. **The Console is a Protocol client.** The Runtime is headless and emits canonical events. The Runtime never imports Console code. The Console never reads internal Runtime objects — only canonical Protocol events, state snapshots, topology, artifacts, traces, metrics. A "shortcut" debug endpoint that exposes raw internals is a violation, even if it's "only for dev."
3. **The Planner is swappable.** The Runtime owns mechanism (events, tasks, tools, memory, artifacts, pause/resume); the Planner owns reasoning policy. The contract is one `Planner` interface; concrete planners (ReAct first; Plan-Execute, Workflow, Graph, Deterministic, Supervisor, MultiAgent, HumanApproval over time) all sit on the same Runtime primitives.

If a change you're about to make would weaken any of these three, stop and reach for the RFC instead of the keyboard.

---

## 2. Authoritative sources (in priority order)

1. `RFC-001-Harbor.md` — product intent and design decisions.
2. `docs/plans/phase-NN-*.md` — implementation specifications. Acceptance criteria are binding.
3. `docs/plans/README.md` — cross-cutting conventions and phase index.
4. This file (`AGENTS.md` / `CLAUDE.md`) — operational rules.
5. `docs/research/*.md` — phase-planning research briefs. Authoritative for context, not for design — the RFC and phase plans are where decisions land.
6. Code comments and godoc — last and least authoritative; if a comment disagrees with the RFC, the RFC wins.

When a phase plan and the RFC drift, the RFC wins. File a follow-up to update the plan.

---

## 3. Repository layout

```
.
├── RFC-001-Harbor.md          # design RFC
├── README.md                   # quickstart + pointers
├── AGENTS.md / CLAUDE.md       # this file (verbatim copies)
├── Makefile                    # canonical build / test / lint commands
├── .github/                    # workflows, dependabot, codeowners, PR template
├── .golangci.yml
├── .markdownlint.yaml
├── .editorconfig
├── .gitignore
├── go.mod / go.sum             # added in Phase 0
├── cmd/harbor/                 # main binary, subcommands
├── internal/                   # production code
│   ├── identity/               # (tenant, user, session) triple
│   ├── runtime/                # orchestration kernel
│   │   ├── engine/             # node graph + executor
│   │   ├── messages/           # envelopes, headers, trace_id
│   │   ├── streaming/          # chunked outputs + parent-trace correlation
│   │   ├── routers/            # routing + retry/timeout policies
│   │   ├── concurrency/        # map_concurrent, join_k, etc.
│   │   ├── playbooks/          # composable subflows
│   │   ├── pauseresume/        # the unified pause/resume primitive
│   │   ├── steering/           # cancel/redirect/inject/pause/resume control
│   │   └── ...
│   ├── planner/                # Planner interface + concrete planners
│   │   ├── ifaces/
│   │   ├── react/              # reference ReAct implementation
│   │   └── ...                 # future: planexecute, workflow, graph
│   ├── tools/                  # transport-agnostic tool catalog
│   │   ├── catalog/
│   │   ├── inproc/             # in-process tool driver
│   │   ├── http/               # HTTP tool driver
│   │   ├── mcp/                # MCP southbound driver
│   │   └── a2a/                # A2A southbound driver
│   ├── llm/                    # LLM client + provider correction
│   ├── memory/                 # memory subsystem
│   │   ├── ifaces/
│   │   └── drivers/{inmem,sqlite,postgres}/
│   ├── skills/                 # token-savvy skill subsystem
│   │   ├── catalog/
│   │   ├── importer/           # Skills.md importer
│   │   ├── generator/          # in-runtime skill generator
│   │   └── providers/{localdb,portico,...}/
│   ├── tasks/                  # unified foreground/background TaskService
│   ├── sessions/               # SessionManager + lifecycle
│   ├── artifacts/              # artifact store
│   │   ├── ifaces/
│   │   └── drivers/{inmem,fs,sqlite,postgres}/
│   ├── state/                  # StateStore
│   │   ├── ifaces/
│   │   └── drivers/{inmem,sqlite,postgres}/
│   ├── events/                 # typed event bus
│   ├── protocol/               # Harbor Protocol — wire types, methods, errors, transports
│   │   ├── types/
│   │   ├── methods/
│   │   ├── errors/
│   │   └── transports/{stream,control}/
│   ├── server/                 # protocol server (Runtime's network surface)
│   ├── telemetry/              # slog + OTel
│   ├── config/                 # configuration loader
│   └── audit/                  # audit redaction + emit
├── examples/
│   ├── tools/                  # example tools (in-proc, HTTP, MCP, A2A)
│   └── *.yaml                  # example configs
├── test/integration/
├── scripts/
│   ├── preflight.sh            # the preflight gate
│   ├── smoke/                  # per-phase smoke scripts
│   ├── hooks/pre-commit        # pre-commit hook
│   └── install-hooks.sh
└── docs/
    ├── plans/                  # phase implementation plans
    ├── rfc/                    # merged RFCs
    └── research/               # phase-planning research briefs
```

The Console (a separate product in the Harbor ecosystem) lives in its own repo. If it later monorepos into `web/console/`, the binding rules in §4.5 still apply.

Anything that doesn't have a home above is wrong. If you need a new top-level directory, propose it in the RFC first.

---

## 4. Build, test, lint, run

All targets are canonical and run by CI.

```bash
# Build the binary (CGo-free, single static binary — once Phase 1 lands)
make build

# Run the test suite with the race detector
make test

# Static analysis
make vet
make lint            # requires golangci-lint v1.61+

# Mirror check: AGENTS.md ↔ CLAUDE.md verbatim
make check-mirror

# Live preflight: build, boot dev server, run smoke checks, tear down.
# This is the SAME gate the pre-commit hook and CI enforce.
make preflight

# Install the git hooks (one-time, per-clone)
make install-hooks
```

Coverage targets per phase plan are non-negotiable. If a PR drops coverage below the target for a touched package, it is rejected.

### 4.1 Preflight gate — non-negotiable

Static checks (vet, lint, tests) catch a lot but **not** "the binary boots and the Protocol surface still works." Harbor's pre-commit hook and CI both run a live preflight that:

1. Builds `./bin/harbor` (no-op if `cmd/harbor/main.go` absent).
2. Boots `./bin/harbor dev` on `127.0.0.1:18080` with a temp data dir (no-op until Phase 1 lands).
3. Waits for `/healthz` to return 200.
4. Runs each `scripts/smoke/phase-NN.sh` against the running server.
5. Tears down (graceful TERM, then KILL, then cleanup).

Each phase smoke script auto-skips its surface if the endpoint returns 404/405/501 — so the gate works at every phase: surfaces that exist must work, surfaces that don't yet are fine. **When you ship Phase N, the corresponding `scripts/smoke/phase-NN.sh` must pass before you commit, because the pre-commit hook will run it.**

When you add a feature, extend the relevant phase smoke script so the new surface is covered. PRs that introduce a new endpoint or Protocol method without a smoke check are rejected.

To install the pre-commit hook locally:
```bash
make install-hooks
```

To bypass in an actual emergency:
```bash
HARBOR_PREFLIGHT_SKIP=1 git commit -m '...'
```
The PR description must justify the skip. CI still runs the gate; an emergency local skip never reaches `main`.

### 4.2 Phase implementor contract

When implementing **Phase N**, the following are part of the work — not optional follow-ups:

1. **`scripts/smoke/phase-NN.sh` must pass against your build before you commit.** The skeleton already exists. Add real assertions as you implement.
2. **A new Protocol method, REST endpoint, or MCP/A2A surface = a new smoke check in the same PR.** Reviewers look for this. Forgetting it is a rejection-on-sight reason (see §13).
3. **Use `scripts/smoke/common.sh` helpers.** Don't roll new curl wrappers — `assert_status`, `skip_if_404`, `assert_json_path`, `assert_json_truthy`, `protocol_call`, `api_url` are the vocabulary. New helpers go in `common.sh` with a one-line docstring.
4. **The 404/405/501 → SKIP convention is sacred.** It's how phase-N+1 scripts coexist with phase-N builds.
5. **A SKIP that should be an OK is a bug.** When a phase's surface lands, its smoke counters in preflight should show `OK > 0` for that phase.
6. **A FAIL is never acceptable on `main`.** Pre-commit + CI both gate this.
7. **New env vars or config keys**: document in the relevant phase plan AND the example config AND, if the smoke script needs them, the smoke script itself.
8. **New CLI subcommands**: include a degradation path so the smoke still works on builds that don't yet have the subcommand.
9. **Done definition**: a phase is done when (a) all phase plan acceptance criteria pass, (b) coverage targets met, (c) `scripts/smoke/phase-NN.sh` shows OK ≥ the count of acceptance criteria it covers and FAIL = 0, (d) prior phases' smoke scripts still pass against the new build (no regressions).

### 4.3 Reasonable plan deviations

A phase plan describes the intended path. Deviations are allowed when justified:

- Implementor finds a simpler approach that still satisfies acceptance criteria.
- A library named in the plan is missing/abandoned — pick a like-for-like swap and document it.
- A speculative interface in the plan turns out wrong once code lands — refactor and update the plan in the same PR.

Document any deviation in the PR description (Phase / RFC reference section). Update the plan file if the deviation is permanent. **Never** silently violate the RFC; if the deviation reaches into RFC territory, that's an RFC PR first.

### 4.4 Extensibility seams (project-wide policy)

Any subsystem with **plausible alternate backends or strategies** must live behind an interface, not a concrete type. SQLite vs Postgres vs in-memory is the canonical example; the same pattern applies to memory drivers, skill providers, tool transports (in-proc / HTTP / MCP / A2A), LLM providers, artifact stores, planner concretes.

The shape:

1. Interface in `internal/<area>/ifaces/` (or `internal/<area>/<area>.go` when more natural).
2. Concrete implementations in `internal/<area>/drivers/<driver>/` — one driver per subdirectory.
3. A factory + registry at `internal/<area>/<area>.go` that dispatches by name.
4. Drivers self-register from their `init()` and are pulled in via blank import (`_ "github.com/.../drivers/<driver>"`) at the binary entry point.
5. Callers depend ONLY on the interface package. Nothing else imports a concrete driver except `cmd/harbor` and tests scoped to that driver.
6. The factory's error message lists registered drivers so misconfigurations are obvious.

**No optional-capability ceremony when all V1 drivers will implement everything.** A subsystem with three drivers (in-mem / SQLite / Postgres) all of which implement everything has one mandatory interface — no `Supports*` capability protocols, no `hasattr`-style duck-typing. Optional capabilities are a smell when they map to "everyone implements everything anyway."

### 4.5 Console / Protocol-client conventions (binding)

The Console is a SvelteKit SPA built with `@sveltejs/adapter-static` (default; alternates require RFC update). It is a **Protocol client** of the Harbor Runtime.

Binding conventions:

1. **Stack: SvelteKit + Vite, TypeScript.** Not React, not Next, not Vue. The choice is settled.
2. **Decoupled deployment.** The Runtime ships headless. The Console can run on the same machine, on a different machine, in a browser tab attached to a remote Runtime, or as a third-party implementation. The Runtime binary does not embed the Console; if the Console is later monorepo'd into `web/console/`, embedding is done via a thin static-file handler that still talks to the Runtime via the Protocol.
3. **Design tokens live in one place.** `web/console/src/lib/tokens.css` defines the full token surface as CSS custom properties (colors, spacings, type scale, radii, motion). Components reference tokens, not raw values. PRs that introduce raw color/spacing literals in `.svelte` files are rejected (see §13).
4. **Lean on a component library; do not rebuild from scratch.** Default: Skeleton (`@skeletonlabs/skeleton`). Alternates that satisfy the same constraints are acceptable when justified in the PR (Flowbite-Svelte, shadcn-svelte). The project anchors on **one** library; pick once and don't fragment.
5. **Typed Protocol client.** The Console talks to the Runtime through a typed client (`web/console/src/lib/protocol.ts`). Hand-rolled `fetch` calls in components are not allowed.
6. **`svelte-check` is part of CI.** A `frontend` CI job runs `npm ci && npm run check && npm run lint && npm run build` in `web/console/` (when present).
7. **Routing**: SvelteKit file-based routes under `src/routes/`. Client-side; no SSR.
8. **Package manager: `npm`.** Lockfile committed.
9. **No build artifacts in git.**
10. **Never read internal Runtime objects.** All data flows through the Protocol's canonical events/state. A Console component that imports a Runtime Go type is a bug.

The Console is its own repo (or `web/console/` monorepo) and its own product. Forbidden practices added (see §13): hand-rolled component primitives that the chosen library already provides; raw color or spacing values in `.svelte` files; mixing package managers; build artifacts committed to git; React/Vue/etc. dependencies in the Console; **the Runtime importing the Console package, in any direction**.

---

## 5. Code conventions (Go)

### Language and tooling

- **Go 1.26+.** No earlier. (Bumped from 1.22 in 2026-05-08 to match the bifrost dependency floor.)
- **Module path:** `github.com/hurtener/Harbor`.
- **CGo is forbidden.** `CGO_ENABLED=0` is enforced in CI build. SQLite uses `modernc.org/sqlite`.
- **Static binary.** `go build -ldflags='-s -w'`. Verified by CI on Linux.

### Style

- `gofmt -s` clean. CI fails otherwise.
- `goimports` with local prefix `github.com/hurtener/Harbor`.
- All exported identifiers documented with godoc comments. Package-level doc comment in every package.
- File naming: lowercase, underscore-separated, no `util.go` / `helpers.go`.
- One package per directory.

### Errors

- Wrap with `fmt.Errorf("context: %w", err)`. Never bare-return upstream errors that originated below.
- Sentinel errors (`var ErrFoo = errors.New("...")`) for cases callers compare against. Use `errors.Is` / `errors.As`.
- `errcheck` and `errorlint` are part of `golangci-lint`. Don't suppress without a one-line `//nolint:` comment **with reason**.
- **Never** use `panic` in production code paths except for "this is impossible by construction" cases.
- **Fail loudly.** Errors are explicit; capabilities are mandatory; identity is mandatory. Patterns like `try { ... } catch { return None }` (silently degrading on serialization failure, missing identity, missing capability) are forbidden — they were the source of the silent-context-loss bug we are explicitly closing. Pause/resume serialization MUST raise `ErrUnserializable` rather than silently dropping context.

### Context

- `context.Context` is the **first parameter** of every function that does I/O, waits, or wants to be cancellable. Never store it in a struct.
- Pass `ctx` through; never call `context.Background()` inside business code unless explicitly bridging an unmanaged async boundary, and document why.
- Honor `ctx.Err()` between long phases of work.

### Logging

- **One logger:** `log/slog`. JSON handler in production, text handler in dev.
- Loggers carry these attributes when present: `tenant_id`, `user_id`, `session_id`, `run_id`, `task_id`, `trace_id`, `span_id`, `tool`. Build a request-scoped child via `logger.With(...)` once per request.
- Severity:
  - `Debug` — useful only when debugging.
  - `Info` — lifecycle events worth telling an operator.
  - `Warn` — unexpected but recovered.
  - `Error` — the request/operation failed.
- **Never log secrets.** Don't log raw tool arguments or results — they routinely contain secrets. Pass through the audit redactor for anything sensitive.
- Don't `fmt.Println`, `log.Print*`, or write to `os.Stdout` directly outside CLI command output.

### Concurrency

- Goroutines started by long-lived components must be cancellable by a `ctx` and joined on shutdown.
- Bounded channels with explicit drop policies on backpressure. Default: drop-oldest, emit a `dropped` event on first drop in a window.
- `sync.Mutex` is the default. Use `sync.RWMutex` only when contention is measured, not assumed.
- No `goto`. No `runtime.Goexit`. No global state mutation outside `init` and (registered) metric definitions.

### Tests

- Unit tests next to the source: `foo.go` ↔ `foo_test.go`.
- Integration tests under `test/integration/`.
- `t.TempDir()` for any filesystem fixture. Never write to the working tree from tests.
- Test names: `TestXxx_Behavior_Scenario`. Examples: `TestEngine_AcquireStartsRun`, `TestPause_FailsLoudlyOnUnserializableContext`.
- Integration tests beginning with `TestE2E_` are required where listed in phase plans.
- `go test -race ./...` is the gate.
- Skipped tests: `t.Skip("reason: <one line>")`. CI fails on a Skip with reason ending `TODO`.

### Linting

- Don't add `//nolint:` without a comment explaining the reason in the same line.
- Prefer fixing the root cause over silencing.
- New linters added to `.golangci.yml` only via PR with rationale.

---

## 6. Multi-isolation — non-negotiable rules

These rules are integrity-critical. A violation is a security bug, not a style nit.

1. **Identity is the triple `(tenant_id, user_id, session_id)`.** The session is the innermost scope and the most active concurrency boundary.
2. **Every storage method that touches an identity-scoped table takes the triple and filters with the appropriate `WHERE` clause.** No "current identity from a global." No "fetch all then filter in Go."
3. **Identity flows from the Protocol through the request `context.Context`.** Read it via `identity.MustFrom(ctx)` in handlers; pass identity into storage explicitly.
4. **Memory is session-scoped by default.** Cross-session promotion (user-level, tenant-level) requires an explicit declared policy with audit. No global memory.
5. **Event bus subscriptions filter by identity.** Cross-session/cross-tenant observers (admin, Console fleet view) require an elevated subscription with the matching scope claim.
6. **Tool execution captures the full triple in provenance.** Tool implementations that cache must key by the triple.
7. **Planner state is per-session.** Sharing a Planner instance across sessions is a bug.
8. **No package-level mutable identity state.**
9. **Identity is mandatory.** Memory drivers, state drivers, event subscribers — all reject requests with a missing identity component. There is no opt-out knob: the runtime fails closed.
10. **Concurrency-leak tests are mandatory.** Any new code path touching identity has a test running N concurrent sessions and asserting no cross-talk.

If a change cannot satisfy these without contortion, the design is wrong — propose a fix in the RFC first.

---

## 7. Security — non-negotiable rules

1. **JWT validation: asymmetric algorithms only.** RS256/RS384/RS512/ES256/ES384/ES512 allowlist. Never HS\* or `none`.
2. **No hardcoded secrets**, including in tests. Tests use fixtures from `internal/<area>/testdata/` with documented dummy values.
3. **No credential passthrough by default.** OAuth flows use token exchange (RFC 8693). Passthrough requires explicit configuration AND emits audit events.
4. **The unified pause/resume primitive is the ONE path** for HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED`/`INPUT_REQUIRED`, and operator/Console PAUSE. Don't reinvent pause coordination in any subsystem.
5. **Path traversal**: any code that takes a relative path from a manifest, config, or API input MUST normalize via `filepath.Clean` and verify with `strings.HasPrefix(absPath, allowedRoot)`. Use the helper in `internal/skills/importer/path_safety.go` once it lands; don't reinvent.
6. **Audit redaction**: every payload goes through `audit.Redactor`. Don't write events bypassing it.
7. **No untyped tool arguments in audit payloads.** Summarize, truncate, or redact — full args are not safe to persist.
8. **No `exec.Command` with shell strings.** Always argv-form, never `sh -c "..."`.

---

## 8. Harbor Protocol rules

- The Protocol version is pinned in `internal/protocol/types/version.go`. Bumping the version is an RFC change.
- All wire types live in `internal/protocol/types/`. Other packages import them; nothing else defines Protocol message structs.
- Method names live in `internal/protocol/methods/methods.go`. No hardcoded method strings elsewhere.
- Error codes live in `internal/protocol/errors/errors.go`. Add new codes there and only there.
- Transports (HTTP+SSE, WebSocket, stdio for embedding) live in `internal/protocol/transports/`.
- Trace context propagation: `internal/telemetry/propagation.go`.
- **The Console NEVER reads internal Runtime objects.** A Protocol method that maps 1:1 to an internal Go function signature is a smell — review for leaking internals.
- **Protocol surface is versioned** independently of the Runtime implementation. Breaking changes require a deprecation window so third-party consoles aren't whipsawed.
- **The Protocol exposes:** streaming events, task control surface (start/cancel/pause/resume/redirect/inject), state snapshots, topology, artifacts (by reference), traces, metrics.
- **Authentication**: identity triple via JWT (or operator-configured equivalent); the Protocol never accepts a request without an identity scope.

---

## 9. Persistence — non-negotiable rules

- **Three drivers ship at V1 with conformance parity:** in-memory (dev/embedded), SQLite (`modernc.org/sqlite`, CGo-free), Postgres (`pgx`).
- Each persistence-shaped subsystem (StateStore, ArtifactStore, MemoryStore, ...) defines an interface; drivers self-register; a factory dispatches.
- **Migrations are forward-only.** Per-driver migration directories. Each migration ends with `INSERT OR IGNORE INTO schema_migrations(version) VALUES (N);` (or driver equivalent).
- WAL journal mode for SQLite. Don't change without an RFC update.
- All queries parameterized. No string concatenation into SQL.
- A subsystem with a feature that "only works on Postgres" or "only works on SQLite" is a design smell — promote the interface or split the feature.
- Transactions: `db.BeginTx(ctx, ...)`; defer rollback before commit.
- **No optional-capability ceremony.** All three V1 drivers implement the full interface.

---

## 10. Configuration changes

- Schema lives in `internal/config/config.go`. New fields require:
  1. Backward compatibility (new optional fields with documented defaults), OR
  2. An RFC update with a documented migration path.
- Validation lives in `internal/config/loader.go::Validate`. New fields validated there.
- Hot-reloadable fields documented in the phase plan that introduces them. Default: **not hot-reloadable**; restart-required.
- Example configs in `examples/` updated whenever the schema gains a top-level field.

---

## 11. Testing rules

- Tests named per phase plans MUST exist and pass.
- New code paths require new tests. PRs that add code without tests are rejected.
- Race detector mandatory: `go test -race`. CI matrix runs Linux + macOS.
- Coverage gates per phase plan.
- **Cross-session isolation**: any new code touching multiple sessions must have an integration test asserting isolation.
- **Time-sensitive tests** use a controllable clock. Never `time.Sleep` for synchronization.
- **Goroutine leak tests**: long-lived components have a test that asserts `runtime.NumGoroutine` returns to baseline after shutdown.
- **Conformance suites**: subsystems with multiple drivers have a single conformance test suite that all drivers must pass.
- **Pause/resume serialization tests** are mandatory: assert `ErrUnserializable` is raised loudly when a non-serializable handle is in pause state. No silent `nil`/`None` returns.

---

## 12. Commit and PR conventions

### Commits

- **Conventional Commits**: `<type>(<scope>): <subject>`.
  - Types: `feat`, `fix`, `docs`, `refactor`, `test`, `perf`, `build`, `ci`, `chore`, `deps`.
  - Scope: most-affected package or area: `phase-02`, `runtime/engine`, `protocol`, `tools/mcp`, `ci`, etc.
- Subject in imperative mood, no trailing period, ≤ 72 chars.
- Body explains **why**, not what. Reference RFC sections / phase plan sections / issue numbers.
- One commit = one logical change. Squash WIP locally before push.

### Pull requests

- Use the PR template (`.github/PULL_REQUEST_TEMPLATE.md`).
- Tag the phase being implemented when applicable.
- All checklist items addressed (✅ / ❌ N/A / ⏳ deferred to follow-up issue).
- Self-review before requesting review.

### Merge

- Squash merge by default. Linear history on `main`.
- Force-push to feature branches is fine; force-push to `main` is forbidden.
- Tagged releases (`vX.Y.Z`) made from `main` only.

---

## 13. Forbidden practices

These will cause the PR to be rejected on sight.

- ❌ Hardcoded secrets, including in tests. Use `testdata/` fixtures with documented dummy values.
- ❌ Shell-form `exec.Command("sh", "-c", "...")`.
- ❌ HS\* / `none` JWT algorithms.
- ❌ Storing identity in package-level state.
- ❌ Adding a third place to define Protocol message types (single source: `internal/protocol/types`).
- ❌ Using `panic` for control flow.
- ❌ Adding CGo dependencies. Build is `CGO_ENABLED=0`.
- ❌ Pulling in heavy frameworks. Stdlib + the libraries listed in the RFC are the allowed surface; additions require RFC update.
- ❌ Logging unredacted tool arguments or results.
- ❌ Bypassing the unified pause/resume primitive — HITL, OAuth, A2A AUTH_REQUIRED, steering all use the same primitive.
- ❌ Cross-session queries without an explicit elevated scope claim.
- ❌ Editing migrations after they have merged. Append-only.
- ❌ Adding `//nolint:` without a one-line reason.
- ❌ `go test` without `-race`.
- ❌ `git push --force` to `main`.
- ❌ Committing with `--no-verify` to skip the preflight hook except in a documented emergency.
- ❌ Adding a new Protocol method or REST endpoint without extending the relevant `scripts/smoke/phase-NN.sh`.
- ❌ Importing a concrete driver package from anywhere except `cmd/harbor` (blank import for self-registration) or that driver's own tests.
- ❌ Building a new subsystem with plausible alternate implementations as a single concrete type instead of an interface + factory + registry (see §4.4).
- ❌ **The Console reads or imports any Runtime internal type.** All data flows through the Protocol's canonical events/state.
- ❌ **The Runtime imports the Console package**, in any direction.
- ❌ **Two parallel implementations of the same conceptual feature** (e.g. "with-flag-X / without-flag-X" toggles for the same purpose). Pick one and deepen it.
- ❌ **Silent degradation.** No `try { ... } catch { return nil }`-shaped patterns. Errors are explicit; capabilities are mandatory; identity is mandatory.
- ❌ **Naming the predecessor project anywhere in this repo** — neither the predecessor's project name nor any synonym ("the prior project", abbreviations, author names) appears in committed text. Internal context is fine in chat; the repo is Harbor-only.
- ❌ Optional `Supports*` capability protocols when all V1 drivers implement everything (see §4.4).
- ❌ Adding identity-downgrading knobs (`require_explicit_key`-style flags that allow missing tenant/user/session). Identity is mandatory.
- ❌ Raw color / spacing / type-scale literals in `.svelte` files (when Console code lands).
- ❌ Hand-rolling a component the chosen library (default Skeleton) already provides.
- ❌ Mixing package managers (`pnpm`/`yarn`) inside `web/console/` (when it lands). `npm` only.
- ❌ Committing `web/console/build/` or `web/console/node_modules/`.
- ❌ Adding a non-Svelte frontend dependency (React/Vue/etc.) to `web/console/`.
- ❌ Hand-rolled `fetch` calls in `.svelte` files — go through the typed Protocol client.

---

## 14. Pre-merge checklist

Before requesting review, run through this:

- [ ] `make vet test build` passes locally (once Go code exists).
- [ ] `make lint` is clean.
- [ ] `make preflight` passes locally (build + boot + smoke against impacted/new surfaces).
- [ ] If you added an endpoint or Protocol method, the relevant `scripts/smoke/phase-NN.sh` exercises it and asserts response shape.
- [ ] Coverage on touched packages is ≥ phase target (or this PR explicitly improves it toward the target).
- [ ] If multi-isolation code paths changed: cross-session isolation test passes.
- [ ] If Protocol types changed: every reference still compiles, including Runtime, Console (if monorepo'd), and any third-party clients we know about.
- [ ] If config schema changed: example configs updated; backward compatibility verified.
- [ ] If migrations added: clean DB starts cleanly; existing DB runs the new migration; both via tests.
- [ ] AGENTS.md and CLAUDE.md still verbatim identical (`make check-mirror`).
- [ ] No new TODO comments without an issue link.
- [ ] No leftover `fmt.Println` / `log.Print*`.
- [ ] No new dependencies without one-liner rationale in the PR description.
- [ ] PR description references RFC section / phase plan section.

If you are an AI agent: **do not claim a task is done until every applicable checklist item is verified.** "I think tests pass" is not verification. Run the commands; show the output.

---

## 15. When in doubt

- If a rule contradicts a phase plan, the **plan wins** (this file is operational, the plan is design).
- If a plan contradicts the RFC, the **RFC wins**.
- If something is unspecified, propose the smallest change that solves the problem and link to it in the PR description.
- If you discover a rule that should exist but doesn't, add it to this file and `CLAUDE.md` (verbatim copy) in your PR.

---

## 16. Authoring a phase plan (workflow)

This is the canonical workflow for any contributor (human or AI) starting work on a phase. The drift-audit gate (`make drift-audit`) enforces what it can; this workflow covers what it can't.

1. **Read the master plan entry.** Open `docs/plans/README.md`, find the Phase N detail block. Note: owning subsystem, RFC sections cited, dependencies, risks/open questions.
2. **Read the cited RFC sections** in `RFC-001-Harbor.md`. The RFC is the design source of truth.
3. **Read the relevant briefs** per `docs/research/INDEX.md` for this subsystem. **A phase plan that doesn't list at least one informing brief is a drift signal.** Briefs encode hard-won lessons; skipping them re-introduces drift the project explicitly closed.
4. **Read the glossary** (`docs/glossary.md`) for any term you're unsure about. If you're about to introduce a new term, pre-write its glossary entry.
5. **Read the decisions log** (`docs/decisions.md`) for entries that mention this subsystem. Settled decisions don't get re-litigated; if you think one needs to change, that's an RFC PR + new decisions entry, not silent drift.
6. **Copy the template:** `cp docs/plans/_template.md docs/plans/phase-NN-slug.md`. Fill every section. The template's "Brief findings incorporated" and "Findings I'm departing from" sections are forcing functions — they make the inheritance from briefs visible.
7. **Author the smoke skeleton:** `cp scripts/smoke/_template.sh scripts/smoke/phase-NN.sh && chmod +x scripts/smoke/phase-NN.sh`. Add real assertions as the surface lands.
8. **Run `make drift-audit`** before commit. It checks: required headings, RFC `§N.M` references resolve, `brief NN` references resolve, smoke script exists, mirror invariant, forbidden-name scan.
9. **Run `make preflight`** before commit. It runs drift-audit + every phase smoke against the live build (no-ops where the surface isn't built yet).
10. **Commit only when both audit and preflight pass.** PR description must reference the RFC section and any superseded prior decision.

### Common drift patterns to watch for

- A subsystem grows new vocabulary not in `docs/glossary.md` — add the term in the same PR.
- A phase plan's design diverges from the RFC — that's RFC drift; either revise the plan or open an RFC PR (don't silently depart).
- A brief finding contradicts what your phase does — re-read the brief; either follow it or document the departure in the template's "Findings I'm departing from" section AND file a new entry in `docs/decisions.md`.
- A phase plan introduces a dependency not in RFC §10 — that's an RFC PR first.
- A phase plan deletes or renames a heading another phase plan references — drift-audit will catch it on the dependent plan, but the originating PR is responsible for migrating references.
- A phase plan adds a new top-level directory not in §3 — RFC PR first; §3 is the binding layout.

This workflow exists because the volume (~80 phase plans across months) makes drift cumulative. Hygiene up front is cheaper than retrofit.

---

## 17. Mirroring

`AGENTS.md` and `CLAUDE.md` are kept verbatim identical. After any edit, run:

```bash
diff -q AGENTS.md CLAUDE.md
# expected: no output (files identical)
```

CI enforces this invariant. The `mirror` job in `.github/workflows/ci.yml` runs `diff -q AGENTS.md CLAUDE.md` and fails the build if they differ. If they drift in a PR, the contributor must reconcile before merge.
