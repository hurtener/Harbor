# Phase 65 — `harbor dev` hot-reload

## Summary

Wires an fsnotify watcher into the `harbor dev` boot stack so file
changes (Go source under `.harbor/agents/`, edits to the loaded
`harbor.yaml`) trigger a graceful-drain in-process restart of the
devStack. Configurable retain-in-flight policy (`drain` / `cancel` /
`disabled`); emits canonical `dev.hot_reload.triggered` /
`dev.hot_reload.completed` events on the bus so wire consumers
(integration tests, the Console, third-party Protocol clients) observe
the restart cycle.

## RFC anchor

- RFC §8 — the CLI layer. `harbor dev` "watches the project directory
  for changes, hot-reloads on Go-source changes (graceful-stop in-flight
  runs first; configurable)".

## Briefs informing this phase

- brief 06 — events / observability / DevX. §7 item 10 names the phase:
  "fsnotify watcher, graceful-drain restart policy, in-flight-run
  handling."

## Brief findings incorporated

- **brief 06 §7 item 10**: fsnotify is the de-facto pure-Go FS-watching
  library for Go and was already the implicit dependency for this
  phase. The §10 deps surface in the RFC allows it.
- **brief 06 §7 item 9**: Phase 64 (`harbor dev` v1) is the predecessor
  — boots an embedded runtime + Protocol on `127.0.0.1:<port>`. This
  phase wraps Phase 64's `bootDevStack` with the restart orchestrator.
- **CLAUDE.md §13 amendment**: "fail loudly at boot when a required
  external dependency is missing." Applied here: fsnotify
  watcher.Add(path) errors fail the boot loud rather than silently
  degrading to a watcher-less binary.

## Findings I'm departing from (if any)

None. The phase as-shipped matches every brief finding cited above.

## Goals

- File change under a configured watch root triggers a graceful-drain
  rebuild of the running devStack.
- In-flight RunLoops drain cleanly up to the configured
  `cli.dev_hot_reload.drain_timeout` before the supervisor forces close.
- The rebuilt devStack picks up the new code (re-reads the config,
  re-opens every subsystem, re-binds the listener).
- Operators have an explicit escape hatch (`--no-hot-reload` flag) and
  a config-driven opt-out (`cli.dev_hot_reload.enabled: false` OR
  `policy: disabled`).
- Bus emission discoverable by wire consumers: `dev.hot_reload.triggered`
  and `dev.hot_reload.completed` land on the canonical event bus with
  the dev identity triple stamped.

## Non-goals

- Binary re-exec on rebuild. The shape is in-process devStack rebuild;
  binary rebuild + re-exec is post-V1 (see "Hot-reload shape decision"
  in D-099 for the rationale).
- Per-language-specific watch filters. The watcher fires on any file
  change under a watch root that isn't a Chmod-only event. Editor
  swap-file events occasionally fire spurious rebuilds; the 250ms
  debounce window collapses bursts to one cycle.
- Reconfiguring the watcher at runtime (changing watch roots without
  a full `harbor dev` restart). The watcher itself is the reload
  mechanism; reconfiguring it live would race the watcher's own
  goroutine.
- Watching paths outside the project tree. The default watch roots are
  `.harbor/agents/` (the Phase 66 drafts directory) plus the loaded
  config file's parent directory. Operators can extend via the config
  block, but the V1 surface is project-local.

## Acceptance criteria

- [x] `cli.dev_hot_reload` config block exists with `Enabled` / `Policy`
  / `DrainTimeout` / `WatchRoots` fields and loader-applied defaults.
- [x] `validateCLI` rejects unknown policy values, negative drain
  timeout, and the enabled-with-no-roots typo.
- [x] `harbor dev --no-hot-reload` overrides `cli.dev_hot_reload.enabled`
  to false for that boot.
- [x] On a file change, the supervisor emits `dev.hot_reload.triggered`
  on the active stack's bus, drains the stack per policy, reboots via
  `bootDevStack`, swaps in the new stack, and emits
  `dev.hot_reload.completed` on the new bus.
- [x] A boot failure on rebuild returns the wrapped error up to
  `runDev`; the operator sees a CLIError with code `boot_internal_error`.
- [x] Unit tests in `cmd/harbor/cmd_dev_hot_reload_test.go` exercise
  the watcher's lifecycle (file mutation → triggered event → new
  stack) under `-race`.
- [x] `harbortest/devstack` package-doc documents the deliberate scope
  choice: the supervisor wraps `bootDevStack` at the runDev layer, not
  inside `bootDevStack` itself, so the helper does not mirror it
  (per D-094's source-of-truth invariant, this is a documented carve-out
  rather than drift).

## Files added or changed

```text
cmd/harbor/
├── cmd_dev.go                       # +--no-hot-reload flag; runDev hands off to supervisor when enabled
├── cmd_dev_hot_reload.go            # NEW — supervisor + watcher + bus emission
└── cmd_dev_hot_reload_test.go       # NEW — unit + lifecycle tests
internal/config/
├── config.go                        # +DevHotReloadConfig + policy constants
├── loader.go                        # +CLI.DevHotReload defaults
├── validate.go                      # +validateCLI
└── validate_test.go                 # +TestValidateCLI_DevHotReload table
harbortest/devstack/
└── devstack.go                      # +godoc carve-out note (supervisor scope choice)
scripts/smoke/
└── phase-65.sh                      # NEW — three assertions (watcher log, --no-hot-reload flag, bus event strings)
docs/decisions.md                    # +D-099 entry
docs/plans/README.md                 # flip Phase 65 row Pending → Shipped
README.md                            # flip Phase 65 status
go.mod / go.sum                      # +github.com/fsnotify/fsnotify v1.10.1
```

## Public API surface

The Phase 65 surface is `cmd/harbor`-internal (cobra-driven), not a
public-API surface. The package-internal types other Phase-65-extending
code would touch:

```go
// cmd/harbor/cmd_dev_hot_reload.go
type hotReloadSupervisor struct{ ... } // unexported — owned by runDev

func newHotReloadSupervisor(
    logger *slog.Logger,
    bootOpts devBootOptions,
    initialStack *devStack,
    cfg config.DevHotReloadConfig,
    watchRoots []string,
) (*hotReloadSupervisor, error)

func (s *hotReloadSupervisor) Run(ctx context.Context) error
func (s *hotReloadSupervisor) CurrentStack() *devStack
```

Canonical bus event types (public per the events package's
`RegisterEventType` registry):

```go
const (
    EventTypeDevHotReloadTriggered events.EventType = "dev.hot_reload.triggered"
    EventTypeDevHotReloadCompleted events.EventType = "dev.hot_reload.completed"
)

type DevHotReloadTriggeredPayload struct {
    events.SafeSealed
    Path   string
    Op     string
    Policy string
}

type DevHotReloadCompletedPayload struct {
    events.SafeSealed
    Path         string
    Op           string
    Policy       string
    DurationMS   int64
    Success      bool
    ErrorMessage string
}
```

Config surface (public via `internal/config`):

```go
type CLIConfig struct {
    DevHotReload DevHotReloadConfig `yaml:"dev_hot_reload,omitempty"`
}

type DevHotReloadConfig struct {
    Enabled      *bool         `yaml:"enabled,omitempty"`
    Policy       string        `yaml:"policy,omitempty"`
    DrainTimeout time.Duration `yaml:"drain_timeout,omitempty"`
    WatchRoots   []string      `yaml:"watch_roots,omitempty"`
}

const (
    DevHotReloadPolicyDrain    = "drain"
    DevHotReloadPolicyCancel   = "cancel"
    DevHotReloadPolicyDisabled = "disabled"
)
```

## Test plan

- **Unit:**
  - `TestResolveHotReloadWatchRoots_UnionsConfigDirAndDedupes` — the
    helper unions cfg.WatchRoots with the config file's parent dir,
    deduplicates, cleans paths.
  - `TestShouldTrigger_FiltersChmodOnly` — Chmod-only events do not
    warrant a rebuild; Write / Create / Rename / Remove do.
  - `TestNewHotReloadSupervisor_RejectsNilDeps` — constructor invariants.
  - `TestNewHotReloadSupervisor_DefaultsPolicyAndDrainTimeout` —
    empty Policy / non-positive DrainTimeout fall back to defaults.
  - `TestValidateCLI_DevHotReload` — exhaustive table-driven coverage
    of the policy / drain timeout / roots / blank-root rules.
- **Integration:**
  - `TestHotReloadSupervisor_FileChangeTriggersRebuild` — the
    end-to-end shape: real `bootDevStack` + real bus + real fsnotify +
    real file mutation + real subscriber. Asserts the canonical
    `dev.hot_reload.triggered` event lands AND the supervisor's
    `CurrentStack()` swaps to a new stack instance after the rebuild.
    Per CLAUDE.md §17.2, this in-package test IS the integration test
    for the wiring boundary (cmd/harbor is the wiring boundary).
  - `TestHotReloadSupervisor_CtxCancel_ReturnsCleanly` — supervisor
    Run returns nil on ctx-cancel; final `CurrentStack()` is the
    initial stack (no rebuild fired); no goroutine leak.
  - `TestHotReloadSupervisor_MissingWatchRoot_LogsAndSkips` — a
    non-existent watch root is logged and skipped (the default
    `.harbor/agents` root does not exist for first-time projects); the
    supervisor serves the remaining roots.
- **Conformance:** N/A — the supervisor is per-boot, not a §4.4 seam.
- **Concurrency / leak:** the supervisor serialises rebuilds (one
  active stack at a time; the supervisor's per-event handler IS the
  serial discipline). Goroutine leak coverage is implicit in the
  CtxCancel test (the test's `t.Cleanup` observes the run-done channel
  and confirms the supervisor's goroutines drain).

## Smoke script additions

`scripts/smoke/phase-65.sh` ships three assertions:

1. The `harbor dev` boot log contains the supervisor's
   `"hot-reload: watcher started"` Info line — confirms the watcher
   wired up at the production boot path.
2. `harbor dev --help` lists the `--no-hot-reload` flag — confirms the
   operator escape hatch is exposed.
3. The binary contains the canonical event-type strings
   `dev.hot_reload.triggered` and `dev.hot_reload.completed` (a
   `strings(1)` probe), confirming the `init()` registration fired.

All three follow the 404/405/501 → SKIP convention: a pre-Phase-65
build skips, a Phase-65 build OKs.

## Coverage target

- `cmd/harbor`: ≥ 75% on touched files (`cmd_dev.go` runDev branch +
  `cmd_dev_hot_reload.go` supervisor + watcher).
- `internal/config`: ≥ 90% on the new validator (table-driven test
  covers every branch).

## Dependencies

- 64 (`harbor dev` v1 / D-089) — the supervisor wraps `bootDevStack`.

## Risks / open questions

- **fsnotify portability.** fsnotify uses platform-specific backends
  (inotify on Linux, FSEvents on macOS, ReadDirectoryChangesW on
  Windows). The supervisor's behaviour is consistent across platforms
  for the Write / Create / Rename / Remove events we filter; Chmod
  semantics differ but we skip Chmod uniformly.
- **Editor swap-file noise.** Some editors (vim, emacs) save via
  swap-file-rename which fires multiple fsnotify events. The 250ms
  debounce window collapses bursts to one rebuild; in practice this is
  the right tradeoff (rare spurious rebuilds vs. lossy events).
- **Burst rebuild cost.** Each rebuild closes every subsystem and
  re-opens them. For the inmem driver stack (the dev-loop default)
  this is fast (~50-200ms); SQLite/Postgres-backed dev stacks would be
  slower. Operators that run hot-reload against a persistent driver
  see the cost; the workaround is `--no-hot-reload` for those flows.
- **Rebuild-during-rebuild.** A second file change during an ongoing
  rebuild does NOT queue a second rebuild — the debounce timer's
  one-shot channel collapses concurrent triggers. This is the correct
  shape: a rebuild already incorporates the file's current state, so
  a second rebuild against the same state is wasted work.

## Glossary additions

- **hot-reload supervisor** — the `cmd/harbor`-internal orchestrator
  that wraps `bootDevStack` with an fsnotify watcher and a debounced
  rebuild loop. Owns the active devStack lifecycle from runDev hand-off
  until ctx-cancel.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test
  passes — N/A: the supervisor stamps the dev identity triple on its
  emitted events; the triple is fixed at compile time (DevTenant /
  DevUser / DevSession), so no cross-session paths change.
- [x] **If this phase builds a reusable artifact: concurrent-reuse
  test passes.** — N/A: the supervisor is per-`harbor dev` boot, not a
  shared concurrent artifact. The underlying `bootDevStack` is the
  reusable artifact and its concurrent-reuse contract is already
  pinned by Phase 64's tests.
- [x] **If this phase consumes a shipped subsystem's surface OR
  closes a cross-subsystem seam: an integration test exists.** — yes,
  see `TestHotReloadSupervisor_FileChangeTriggersRebuild` (real
  bootDevStack + real bus + real fsnotify + real subscriber per
  CLAUDE.md §17.2 in-package shape).
- [x] If new vocabulary: glossary updated (the supervisor term lands
  in `docs/glossary.md` via this plan's Glossary section).
- [x] If a brief finding was departed from: justified above +
  decisions.md entry filed — N/A, no departures.
