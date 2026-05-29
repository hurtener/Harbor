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

```text
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
│   │   ├── registry/           # Agent Registry — registration identity + agent.* events
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
│   │   └── drivers/{localdb,portico,...}/  # §4.4 seam — `SkillStore` drivers
│   ├── governance/             # cost ceilings + rate limits + MaxTokens (V1); key rotation + model swap + failover (post-V1)
│   ├── tasks/                  # unified foreground/background TaskService
│   ├── sessions/               # SessionManager + lifecycle
│   ├── artifacts/              # artifact store
│   │   ├── ifaces/
│   │   └── drivers/{inmem,fs,sqlite,postgres}/
│   ├── state/                  # StateStore
│   │   ├── ifaces/
│   │   └── drivers/{inmem,sqlite,postgres}/
│   ├── events/                 # typed event bus
│   ├── distributed/            # MessageBus + RemoteTransport (A2A) contracts
│   │   ├── a2a/                # A2A v1 Go shapes (hand-transcribed from proto)
│   │   ├── conformancetest/    # driver conformance suites
│   │   └── drivers/{loopback}/ # V1 in-process driver (post-V1: durable bus; Phase 29: A2A wire)
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
├── harbortest/                 # public test kit — operator-importable from outside the module
├── test/integration/
├── scripts/
│   ├── preflight.sh            # the preflight gate
│   ├── smoke/                  # per-phase smoke scripts
│   ├── hooks/pre-commit        # pre-commit hook
│   └── install-hooks.sh
└── docs/
    ├── plans/                  # phase implementation plans
    ├── rfc/                    # merged RFCs
    ├── research/               # phase-planning research briefs
    └── skills/                 # operator skills — Claude-Code-style playbooks for building agents (V1.1.5+; see §18 drift rule)
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
make lint            # requires golangci-lint v2.x (config is v2 schema)

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
10. **Keep `README.md` current.** When a phase ships, update the Status table in the root `README.md` so a fresh visitor sees what's landed. The table flips that phase's status from "Pending" to "Shipped" and, if the phase introduced a new reader-facing surface (a CLI subcommand, an example config, an installable package), adds a one-line pointer in the relevant section. README updates ride in the same PR as the phase work — not as a follow-up.
11. **Keep `docs/plans/README.md` current.** The master phase plan acts as the canonical execution index. When a phase ships, flip its row's `Status` column from `Pending` to `Shipped` in the same PR. If a phase plan deviated permanently (per §4.3), reflect the deviation in the master plan's detail block too — not just the per-phase plan file. Stale `Pending` rows for shipped phases are a drift signal.

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

The Console is a SvelteKit SPA built with `@sveltejs/adapter-static` (default; alternates require RFC update). It is a **Protocol client** of the Harbor Runtime, deployed via the `harbor console` subcommand (D-091) — never embedded in `harbor dev`.

Binding conventions:

1. **Stack: SvelteKit + Vite + TypeScript + Svelte 5 (runes mode).** Not React, not Next, not Vue. Not Svelte 4. The choice is settled. `web/console/svelte.config.js` ships with `compilerOptions: { runes: true }`; `package.json` pins `"svelte": "^5.0.0"`. Legacy Svelte 4 reactivity (`$:`, top-level `let` as reactive state, `export let` props, store auto-subscription via `$store` in scripts) is rejected by `svelte-check --fail-on-warnings`. See D-092.
2. **Decoupled deployment via `harbor console` (D-091).** The Runtime ships headless. The full Console is served by the `harbor console` subcommand, which bakes the static SvelteKit build into `cmd/harbor` via `embed.FS`. The Console can also run on a different machine, in a browser tab attached to a remote Runtime, or as a third-party implementation. The Runtime binary's `harbor dev` subcommand does NOT serve the Console. A future packed single-agent dev UI in `harbor dev` (post-V1) reuses the Console's chat/playground components via a shared library; the full Console stays on `harbor console`.
3. **Design tokens live in one place, mechanically enforced.** `web/console/src/lib/tokens.css` defines the full token surface as CSS custom properties (colors, spacings, type scale, radii, motion). Components reference tokens, not raw values. PRs that introduce raw color/spacing literals in `.svelte` files are rejected (see §13). Enforcement is mechanical: `web/console/.stylelintrc.cjs` (lands with the first Console phase that creates `web/console/`) disallows hex / rgb() / named colors and arbitrary `px` / `rem` / `em` values outside the token surface. `npm run lint` fails CI on raw literals — no reviewer hunting needed.
4. **Lean on a component library; do not rebuild from scratch.** Default: Skeleton (`@skeletonlabs/skeleton`). Alternates that satisfy the same constraints are acceptable when justified in the PR (Flowbite-Svelte, shadcn-svelte). The project anchors on **one** library; pick once and don't fragment.
5. **Typed Protocol client, generated from `CanonicalWireTypes` (D-093).** The Console talks to the Runtime through a typed client at `web/console/src/lib/protocol.ts`. This file is **generated** by `cmd/harbor-gen-protocol-ts/` from `internal/protocol/singlesource.CanonicalWireTypes` — never hand-edited (the generated header carries `// CODE GENERATED BY cmd/harbor-gen-protocol-ts. DO NOT EDIT.`). Hand-rolled `fetch` calls in components are not allowed. `make protocol-ts-gen-check` (run in the `frontend` CI job) asserts `git diff --exit-code` is clean; a Go-side wire-type change without a corresponding TS regeneration fails the build.
6. **`svelte-check` is part of CI with `--fail-on-warnings`.** A `frontend` CI job runs `npm ci && npm run check && npm run lint && npm run build` in `web/console/` (when present). The strict-mode flag catches Svelte 4 syntax drift early.
7. **Routing**: SvelteKit file-based routes under `src/routes/`. Client-side; no SSR.
8. **Package manager: `npm`.** Lockfile committed.
9. **No build artifacts in git.**
10. **Never read internal Runtime objects.** All data flows through the Protocol's canonical events/state. A Console component that imports a Runtime Go type is a bug.
11. **Shared chat/playground module — encapsulate first, extract on second consumer (D-091).** The chat + playground + MCP-Apps content renderer + file-upload + trace-toggle components ship as a self-contained module at `web/console/src/lib/chat/`. The introducing phase enforces: (a) no imports of other Console internals from the chat module; (b) a typed `ProtocolClient` interface the caller injects, never a Console-specific singleton; (c) the MCP-Apps renderer registry lives at `web/console/src/lib/chat/renderers/`. When a second consumer (the future packed dev UI in `harbor dev`) lands, the extraction to `web/shared/chat/` is mechanical (`git mv`).
12. **Console design-system conventions are binding — `docs/design/console/CONVENTIONS.md` (D-121).** Every Console page is built against the shared foundation: one SvelteKit route group `web/console/src/routes/(console)/` (no `/console/` URL prefix), one app shell, the shared `web/console/src/lib/components/ui/` inventory, the four-state `<PageState>` async contract, the unified `HarborClient` + `connection.ts`, and the one reconciled `tokens.css` scale. `CONVENTIONS.md` is the authority; every Console page phase plan MUST carry a "Console consistency" section citing it, and a page PR that diverges from a convention is rejected on sight.

The Console is its own repo (or `web/console/` monorepo) and its own product. Forbidden practices added (see §13): hand-rolled component primitives that the chosen library already provides; raw color or spacing values in `.svelte` files; mixing package managers; build artifacts committed to git; React/Vue/etc. dependencies in the Console; **the Runtime importing the Console package, in any direction**; Svelte 4 reactivity syntax in `web/console/`; hand-edited `protocol.ts` (which is generated); embedding the Console build into any Runtime subcommand other than `harbor console`.

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

### Concurrent reuse contract — non-negotiable (D-025)

**Compiled artifacts are immutable after construction. Per-run state lives in `ctx` + `RunContext`, never on the artifact.**

A "compiled artifact" is anything built once and called many times: a `flow.Engine`, a `Tool` (any transport), a `Planner` instance, a `MemoryStore` driver, a `Redactor`, a `LLMClient`, a `ToolCatalog`. The artifact MUST be safe to share across N concurrent goroutines without:

- **Data races** (the race detector is the gate; CI runs `go test -race ./...`).
- **Context bleed** (run A's input/state never reaches run B; verified by per-run identity assertions in the test).
- **Cancellation cross-talk** (cancelling run A's `ctx` MUST NOT affect run B; verified by parallel-cancel tests).
- **Goroutine leaks** (each invocation's goroutines are joined before the invocation returns; baseline `runtime.NumGoroutine()` test asserts this).

Concretely:

1. Mutable fields on a compiled-artifact struct that change *after* construction are forbidden, except where guarded by `sync.RWMutex` (or atomic primitives) AND documented as "internally synchronized." A bare `count int` field on `Engine` is a bug; an `atomic.Int64` is OK; a `map[string]X` is a bug unless behind a mutex with documented invariants.
2. Each invocation of a compiled artifact (`Engine.Run(ctx, ...)`, `Tool.Invoke(ctx, args, rc)`, `Planner.Next(ctx, rc)`) creates its own per-run scope. The artifact reads from `ctx` / `rc`, never from itself, for run-specific data.
3. Per-run goroutines are cancelled via the run's `ctx`, not via a shared engine-level `ctx`.
4. Subflow / spawned tasks inherit the run's identity quadruple via `ctx`; they do not read identity from the parent artifact.
5. Package-level mutable state outside `init()` is forbidden except for: driver registries (write-once, read-many; §4.4 seam pattern) and metric definitions.

**Every phase that builds a reusable artifact MUST ship a concurrent-reuse test** that runs N=100+ concurrent invocations against a single shared instance with `-race`, asserting all four guarantees above. This is part of §11 Testing rules (now explicit).

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

**Clarifying note — `agent_id` is NOT part of the isolation tuple.** Agents are runtime *entities* with a registration identity (`agent_id`, minted and persisted by the Agent Registry — RFC §6.16, D-059). That registration identity is **not** an isolation principal: the isolation boundary is and stays the tuple `(tenant, user, session)` (+ `run` for the quadruple). An agent runs *within* `(tenant, user, session)`; it does not widen the boundary. Storage methods, event filters, and memory/state drivers scope by the tuple, never by `agent_id`. Do not add `agent_id` to `WHERE` clauses as an isolation filter.

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
- **Concurrent-reuse tests are mandatory** for any phase that builds a reusable artifact (engines, tools, planners, drivers, redactors, clients, catalogs). Test N≥100 concurrent invocations against a single shared instance under `-race`, asserting: no data races, no context bleed, no cross-cancellation, no goroutine leaks (baseline-restored after all runs return). See §5 "Concurrent reuse contract" for the full rule and D-025 for the rationale.

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
- ❌ **A Console DB used as a shadow source of truth for runtime entities** (agents, sessions, tasks, tools, events, artifacts). A Console-side datastore holds Console-local state only — saved views, layouts, preferences, annotations. Runtime entities flow exclusively through the Protocol. See RFC §7, D-061.
- ❌ **A Console page phase shipping without its feeding Protocol-surface phase** landing first or in the same wave. This is the "no primitive without its consumer" rule read backwards — it keeps the Console honest as a Protocol client instead of letting it grow private hooks. See RFC §7, D-062.
- ❌ **Two parallel implementations of the same conceptual feature** (e.g. "with-flag-X / without-flag-X" toggles for the same purpose). Pick one and deepen it.
- ❌ **Shipping a primitive without its first consumer in the same wave.** A primitive (interface, control instruction, decision shape, runtime mechanism) that lands without a concrete that exercises it will bit-rot, drift from the design that motivated it, or be silently dropped at the next refactor. **The rule is binary:** the wave that introduces a primitive MUST also introduce at least one consumer that exercises the primitive end-to-end with a test. If a primitive lands in V1, its first consumer lands in V1 — not "later." Two concrete consequences of this rule, called out so they don't get re-litigated:
  - **`SpawnTask` and `AwaitTask` emission MUST land in the same phase.** A planner that can spawn a background task but cannot join it produces orphan work the runtime cannot recover. The pair is the unit of value; splitting them across phases violates this rule. The Decision sum already pins both shapes (Phase 42 / D-047) — the emission paths are what must twin.
  - **The unified pause/resume primitive requires a `RequestPause`-emitting consumer in the same wave.** Phase 50 (the primitive) cannot ship without at least one planner (or planner upgrade) emitting `RequestPause` for a real reason — HITL approval, tool-side OAuth, or A2A `AUTH_REQUIRED`. A pause primitive with no producer is dead code; a pause primitive whose first producer lands "in the next wave" routinely drifts because the primitive's design was never validated against a real call site.
- ❌ **Silent degradation.** No `try { ... } catch { return nil }`-shaped patterns. Errors are explicit; capabilities are mandatory; identity is mandatory.
- ❌ **Test stubs as production defaults on operator-facing seams.** Stubs (`EchoSummarizer`, `staticSummariser`, the `mock` LLM driver, and future equivalents — anything whose godoc carries phrases like "test-grade," "canned responses," or "deterministic for tests") live in `*_test.go` files or a `testfixtures` subpackage gated by a build tag. They are NEVER a registry's `DefaultDriver`, and they are never the only shipped implementation of an interface the binary will resolve at boot. A V1 binary must produce real behavior on the golden path with zero configuration ceremony — operators get a working agent runtime out of the box, not a kit of seams to wire themselves. Two concrete consequences:
  - **Fail loudly at boot when a required external dependency is missing.** Missing LLM provider / API key / external store → the binary exits non-zero with an error message that names the missing config key and points to the relevant `examples/` file. Silent fallback to a stub when nothing is configured is forbidden.
  - **Dev-only escape hatches are explicit, never the default.** A `--mock` CLI flag (or a single, prominently-banner'd env var like `HARBOR_DEV_ALLOW_MOCK=1`) is acceptable for first-clone convenience and CI smoke; the default path with no flag set must demand a real provider. When the escape hatch fires, every boot prints a stderr banner like `[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]`.

  This rule closes the same failure mode the primitive-with-consumer rule (above) closes, one layer up: there the concern is library primitives that never get exercised under real call sites; here the concern is operator-facing seams that never get exercised under real workloads because the binary defaults to a stub. A phase plan that ships an operator-facing seam without a non-stub default violates this rule, even if a test stub satisfies the primitive-with-consumer rule. When a deliberate carve-out is required (e.g. a subsystem whose real implementation genuinely cannot land in V1), file an RFC PR + a new `docs/decisions.md` entry — never quietly ship a stub as the default and rationalize it as "fail-safe."
- ❌ **Naming the predecessor project anywhere in this repo** — neither the predecessor's project name nor any synonym ("the prior project", abbreviations, author names) appears in committed text. Internal context is fine in chat; the repo is Harbor-only.
- ❌ Optional `Supports*` capability protocols when all V1 drivers implement everything (see §4.4).
- ❌ Adding identity-downgrading knobs (`require_explicit_key`-style flags that allow missing tenant/user/session). Identity is mandatory.
- ❌ Mutable state on compiled artifacts that crosses run boundaries. A `count int` field on `Engine` / `Tool` / `Planner` / etc. is a bug. Use `atomic.*` primitives for genuinely shared counters, or move per-run state into `ctx` / `RunContext`. See §5 "Concurrent reuse contract" + D-025.
- ❌ Shipping a reusable artifact phase without a concurrent-reuse test (N≥100 invocations against a single instance under `-race`). See §11.
- ❌ Raw heavy content in a message reaching the `LLMClient`. Any string / byte slice / `DataURL` ≥ heavy-output threshold that is not already an `ArtifactStub` is a leak. The runtime's LLM-edge enforcement pass fails loudly with `ErrContextLeak` and emits `llm.context_leak`. See RFC §6.5 "Context-window safety net" + D-026.
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

## 17. End-to-end + cross-subsystem integration testing

Per-package unit tests catch most bugs but miss two classes the Wave 2 checkpoint audit (PR #11) pinned:

1. **Cross-package wiring gaps** — two phases each ship their seam, neither connects them. The Wave 2 instance was the `BusEmitter` ↔ `EventBus` wiring; both Phase 04 and Phase 05 plans assumed the OTHER would close the seam.
2. **Cross-subsystem concurrency interactions** — boundary-level races (e.g. close-during-publish on the bus) that don't surface inside one package's tests.

The hygiene response below is binding. Skipping it is the same kind of drift signal as skipping §16's brief-reading.

### 17.1 When an integration test is required

A phase ships an integration test whenever ANY of these is true:

- Its plan's `Deps` lists a different subsystem's already-shipped phase (the new phase consumes that subsystem's surface — prove the consumption works).
- It introduces a new adapter / wrapper / driver that closes a seam another phase opened (the wiring test belongs in this PR, not in a follow-up).
- It introduces a new public interface that other phases will Publish/Subscribe/Open against (cover at least one round-trip end-to-end).

A plan that has `Deps: 00` only (the skeleton) is exempt — there's no seam to test yet.

### 17.2 Where integration tests live

Two acceptable shapes:

- **In-package**: when the package itself IS the wiring boundary — e.g. `internal/telemetry/eventbus/eventbus_test.go` ships `TestEndToEnd_Logger_Bus_Subscribe`. The package was created specifically to bridge two subsystems; testing it locally is appropriate.
- **`test/integration/<topic>_test.go`**: for tests that span >2 subsystems, prove a wave-level surface is alive end-to-end, or have no natural single-package home. The wave-end smoke test (`test/integration/wave2_test.go`) is the canonical example.

When in doubt, prefer `test/integration/` — it's easier to find, doesn't bloat the per-package coverage report, and serves as a wave-boundary regression gate.

### 17.3 What an integration test must cover

Mandatory:

1. **Real drivers everywhere on the seam.** No mocks at the boundary — use the production drivers (`audit/drivers/patterns`, `events/drivers/inmem`, `state/drivers/inmem`, etc.). A mock at the seam defeats the purpose of the test.
2. **Identity propagation**: prove the multi-isolation triple flows through every layer the test wires up. Cross-tenant isolation lives or dies at this boundary.
3. **At least one failure mode**: a forced redactor error, a closed bus, a missing identity. Not just the happy path.
4. **`-race` is the gate**: integration tests run under the race detector. CI fails on a hit.

Required when the wiring is long-lived (server, bus, store):

1. **Concurrency stress run**: N≥10 concurrent producers/consumers exercise the boundary; assert no cross-talk, no goroutine leak after teardown. (Per-package `D-025` tests assert intra-package; the integration stress proves cross-package.)

### 17.4 Forbidden in integration tests

- Mocks or in-test fakes that re-implement subsystem behavior.
- `time.Sleep` used as a synchronisation primitive (sleeps for "wait for async event"). Use channels, controllable clocks, or `eventually`-style assertions with bounded real-time timeouts.
- Skipping the test on platform / CI mismatch — integration tests run on every supported OS.
- Using internal/unexported state — test the public surface only.

### 17.5 Wave-end checkpoint audit

At every wave boundary (every 2-4 phases), a checkpoint audit runs BEFORE the next wave's planning starts. The audit:

1. Reads each shipped phase's source + tests + plan + RFC reference.
2. Hunts for: wiring gaps, RFC drift, depth issues, weak tests, hygiene regressions.
3. Produces a categorised punch list (FAIL / WARN / NIT) with file:line refs and one-line fix directives.
4. Lands as a single `chore(checkpoint): wave-N audit fixes` PR.

The audit is a forking task — the auditor runs read-only, the operator fixes from the punch list. PR #11 (the Wave 2 audit) is the reference.

The audit is mandatory at wave boundaries; it is also acceptable to trigger one ad-hoc when scope drift is suspected.

### 17.6 Fix what the integration test finds — no matter where the bug lives

When an integration test (especially a wave-end smoke or checkpoint audit) surfaces a bug, **fix it in the same PR — even when the root cause is in a previously-shipped phase's code.** Examples that this rule covers, regardless of which phase originally shipped the surface:

- Test-time non-idempotency that surfaces under `go test -count=N` (e.g. PR #16's `TestOpen_HonoursCfgDriver` flake — registered a process-wide driver name without cleanup; lived since Phase 05; surfaced when Wave 3's stress run flushed it out).
- Cross-package wiring gaps where each phase's tests pass in isolation but the seam between them is dead (PR #11's `BusEmitter` ↔ `EventBus` gap).
- Validator regressions where a previous phase's test config helper stops validating after a later phase tightens a rule (Wave 2's `wave2Config()` becoming stale once Phase 08's `validateSessions` required non-zero fields — fixed in PR #15).
- Race conditions, goroutine leaks, or stale-doc references that are only visible when the larger surface composes.

**When the test fixture's bug shape mirrors a latent production bug, fix BOTH in the same PR.** A common failure mode: an integration test surfaces a bug that the test fixture itself reproduces (e.g. a missing constructor option, a misconfigured driver). The temptation is to patch the fixture alone and call it done — the test goes green. But if the production code has the same omission, the fixture-only fix silently perpetuates the test↔production divergence; the test no longer guards what it was meant to guard. The Wave 11.5 §17.5 closeout audit pinned this in F1: PR #121 patched the bus-wiring omission in `harbortest/devstack.Assemble` but missed the same omission at `cmd/harbor/cmd_dev.go::bootDevStack`. The wave-end E2E "passed" only because devstack carried the fix; production silently emitted no `pause.resumed` events on the bus. **Whenever you fix a bug shape on the test side, grep production for the same call site and fix it too.** If you can't fix both (because production's fix has a larger blast radius), the test-side patch must include a top-of-test comment naming the unfixed production gap and a tracking issue.

**Don't defer with "this is a Phase N issue, file a follow-up."** That defeats the gate's purpose: the test exists to catch drift, and drift in old phases is just as load-bearing as drift in new ones. A wave-end smoke that "passes" only by ignoring the issues it surfaced is not a gate, it's noise.

The PR's title and body should call out the cross-phase fix (e.g. `feat(...) wave-3 + fix Phase 05 driver-registration flake`) so reviewers see what's bundled. Fixing across phase boundaries is **expected**, not exceptional, when integration tests do their job.

If a discovered bug is genuinely too large for the current PR (full subsystem rewrite, new RFC required), the PR description must (a) name the bug with file:line precision, (b) link a tracking issue, and (c) explain why it's deferred — *and* the integration test that surfaced it must `t.Skip` with the issue link, never silently mask the failure.

### 17.7 Wave delivery cadence (the repeatable six-step process)

Harbor is built in **waves** — a wave is ~3–8 phases that form a coherent subsystem slice. §17.5 documents the wave-end audit; this section documents the full cadence the audit sits inside. The six steps are binding for any contributor (human or AI) coordinating a wave.

1. **Scope the wave.** Open `docs/plans/README.md`, read the next contiguous block of `Pending` phases plus the critical-path ordering near the bottom. A wave is the next coherent subsystem slice — not an arbitrary count.
2. **Stage it.** Group the wave's phases into **stages** — bundles that can be built in parallel because they have no inter-dependencies. A phase whose `Deps` list points only at already-shipped phases can go in the current stage; one that depends on a sibling waits for the next stage. Confirm the staging with the user before dispatching — staging is a judgment call they sign off on. **Apply the §13 primitive-with-consumer rule here:** if a stage introduces a primitive (interface, decision shape, runtime mechanism), the same stage MUST include a consumer that exercises it end-to-end with a test.
3. **Dispatch each stage as parallel worktree agents** — one phase per agent, isolated worktree, one general-purpose agent each. Every dispatch prompt MUST include: the master-plan detail block, the mandatory reading list (the relevant CLAUDE.md sections, RFC sections, predecessor phase plans, briefs), the §16 phase-plan workflow, the validation gate, a **pre-assigned `D-NNN` number** (so parallel agents don't collide in `docs/decisions.md`), an explicit **workspace warning** (operate only inside the worktree; `pwd` first; STOP if a path resolves outside it), and the **markdownlint hygiene reminder** (blank lines around `---` and `## D-NNN` headings in `docs/decisions.md`; run `markdownlint-cli2` repo-wide before committing).
4. **Drain merges, then dispatch the next stage.** Wait for the user to merge a stage's PRs before dispatching the dependent stage. A cleanup `chore` PR may run as background work between stages if audit WARNs/NITs have accumulated.
5. **Wave-end E2E.** The final stage includes a `test/integration/waveN_test.go` — real drivers across the wave's surface, identity propagation, ≥1 failure mode, N≥10 concurrency stress. It is bundled with the final phase's PR.
6. **Wave-end checkpoint audit (§17.5).** A read-only fork audits every phase in the wave; the coordinator lands the punch list as one `chore(checkpoint): wave-N audit fixes` PR. **This gates the next wave's planning** — do not start Wave N+1 scoping until Wave N's audit PR is merged.

**Recurring failure modes to pre-empt** (the reason each guard in step 3 exists): agents drifting out of their worktree into the main checkout; latent `docs/decisions.md` markdownlint breakage that surfaces one PR late (CI lints repo-wide); committed merge-conflict markers (`<<<<<<<` / `=======` / `>>>>>>>`) from an agent that ran `git merge main` mid-build; and the occasional agent dying on an API-overload error mid-run — recover by picking up that worktree's in-progress work and finishing it as coordinator rather than re-dispatching from scratch.

---

## 18. Operator-skill hygiene — same-PR drift prevention (effective V1.1.5)

`docs/skills/<slug>/SKILL.md` is Harbor's operator-facing adoption surface — Claude-Code-style playbooks for the activities Harbor's CLI / Console / Protocol expose. The skills only earn operator trust when they STAY in sync with the surface they document. Effective V1.1.5 (the first cut that ships the skills), the §17.6-style "fix what the test finds" rule extends here:

**A change that mutates a documented surface MUST update the matching skill in the SAME PR.** "Documented surface" includes any of:

- A `harbor` CLI verb (its flags, output, exit codes, posture). Skills tying to that verb live by it.
- A Harbor Protocol method, wire-shape field, capability advertisement, or event payload key — the `use-the-harbor-protocol` skill (and any skill that demonstrates a Protocol call) consumes them.
- A Console route, page, or `<PageState>` branch the operator reads.
- A `harbor.yaml` config field (added, renamed, removed, semantically changed).
- A canonical artifact a skill quotes verbatim (e.g. the `harbor init` template's structure).

**How to know which skill is affected.** Every skill carries a frontmatter `metadata.surface` value (`cli` / `agent-yaml` / `tools` / `mcp` / `llm` / `memory` / `playground` / `console` / `tasks` / `protocol`). When a PR touches one of these surfaces, grep `docs/skills/` for matching `surface:` lines and read the SKILL.md bodies — the affected skill is usually obvious in <60 seconds.

**Failure mode this closes.** Without this rule, skills drift silently: the surface evolves, the skill doesn't, an operator follows a stale step, hits a wall, and abandons Harbor. The first-five-minutes adoption guarantee (`scaffold-a-harbor-agent` → `run-the-dev-loop` → `drive-the-playground` in <5 min) is only meaningful if every operator who follows it today gets the same five-minute experience tomorrow.

**What the drift-audit catches mechanically.** `scripts/skills/check-frontmatter.sh` (invoked by `make drift-audit`) verifies every `SKILL.md` has a well-formed frontmatter (`name` / `description` / `license` / `metadata.framework: harbor` / `metadata.surface` in the recognised set / `metadata.verbs`). A skill with a removed-from-the-codebase surface keyword in its `verbs:` still passes the audit — drift-audit cannot read prose. The human-review side of this rule is therefore the LOAD-BEARING gate; the audit is the trip-wire for the trivial regressions.

**When a surface change genuinely doesn't require a skill update.** Internal refactors, perf optimisations, lint cleanups, test-only changes — these don't touch operator surfaces by definition and are exempt. The rule is "if you change the surface AN OPERATOR FOLLOWS, update the playbook." If you're unsure: grep the skills, then read the body of the closest match; if no skill mentions the surface you're changing, you're exempt.

**When two surfaces compete for one skill update.** A change that affects two skills (e.g. a `harbor.yaml` config field renamed AND a CLI flag that reads it) updates BOTH in the same PR. The skill-frontmatter helper lists every `SKILL.md` that names the affected surface; touch them all.

**Dockyard precedent.** Dockyard's sibling skills (`~/Repos/Dockyard/skills/`) carry the same drift discipline. The cross-references between Harbor and Dockyard skills work because both repos enforce same-PR updates on their respective surfaces.

---

## 19. Mirroring

`AGENTS.md` and `CLAUDE.md` are kept verbatim identical. After any edit, run:

```bash
diff -q AGENTS.md CLAUDE.md
# expected: no output (files identical)
```

CI enforces this invariant. The `mirror` job in `.github/workflows/ci.yml` runs `diff -q AGENTS.md CLAUDE.md` and fails the build if they differ. If they drift in a PR, the contributor must reconcile before merge.
