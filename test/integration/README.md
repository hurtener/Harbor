# Harbor — integration tests

This directory holds cross-subsystem end-to-end tests.
**Per AGENTS.md §17, every phase that closes a cross-subsystem seam ships an integration test.** When the test spans more than two subsystems or has no natural single-package home, it lives here. Same-package wiring tests (e.g. `internal/telemetry/eventbus/eventbus_test.go`) belong with their package.

## Conventions

- **Package name**: `integration_test` (every file). The directory is `_test`-only — no production code lives here.
- **Test names**: `TestE2E_<topic>_<scenario>` per AGENTS.md §5.
- **File naming**: `wave2_*.go` for wave-2-surface tests; `<subsystem>_<other-subsystem>_test.go` for two-system seams; topical names (e.g. `runtime_lifecycle_test.go`) for broader flows.
- **Real drivers everywhere**: `audit/drivers/patterns`, `events/drivers/inmem`, `state/drivers/inmem`, etc. No mocks at the boundary.
- **Identity propagation**: every test wires `identity.With` / `identity.WithRun` and asserts the triple flows through.
- **Race detector**: every test runs under `go test -race ./test/integration/...`.

## What an integration test must include

Mandatory:

1. Construct real drivers via their `Open(...)` factories.
2. Wire the seam under test end-to-end.
3. Assert the happy-path round trip.
4. Assert at least one failure mode (closed bus, redactor error, missing identity).
5. For long-lived components (bus, store): a concurrency stress run with N≥10 goroutines.

Forbidden:

- Mocks at the boundary.
- `time.Sleep` for synchronisation (use channels, controllable clocks, bounded real-time deadlines).
- Importing unexported state.

## Why this directory exists

The Wave 2 checkpoint audit (PR #11) found that Phase 04 and Phase 05 each shipped their `BusEmitter` ↔ `EventBus` seam but no test wired them together — the runtime.error event documented in RFC §6.14 never reached the bus. This directory is the mechanical hygiene response: a wave-end smoke test would have caught the gap before Wave 3 planning started.

When the next wave-end audit runs, this directory should hold one or more tests that demonstrate every shipped subsystem's seams are alive.
