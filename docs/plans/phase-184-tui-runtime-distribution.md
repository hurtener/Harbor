# Phase 184 — TUI Runtime distribution and wave E2E

## Summary

Promote the proven TUI behind `sdk/tui`, add explicit serve readiness, and
expose identical co-launch behavior through `harbor serve --tui` and generated
serving binaries' `--tui`. The wave-end E2E proves standalone attach, stock
co-launch, and scaffolded co-launch retain the full UX quality and Protocol-only
architecture.

## RFC anchor

- RFC §3.1
- RFC §3.6
- RFC §5.1
- RFC §5.4
- RFC §5.5
- RFC §5.6
- RFC §8

## Briefs informing this phase

- brief 06
- brief 11
- brief 12

## Brief findings incorporated

- brief 06 §5 and §8: co-located CLI experiences are ordinary Protocol clients.
- brief 11 §PG-1: direct interaction uses canonical session/task/control state.
- brief 12 §2 and §5: deployment convenience does not weaken headless Runtime
  boundaries; local and remote modes share one client.

## Findings I'm departing from (if any)

Brief 12's packed browser dev UI is not implemented. This phase distributes the
native TUI through production serving and scaffolded binaries; `harbor dev`
remains unchanged.

## Goals

- Make the complete high-fidelity TUI available to stock and external runtimes.
- Provide deterministic readiness and explicit server ownership.
- Prove distribution modes do not regress rendering or lifecycle quality.

## Non-goals

- No anonymous loopback, automatic token minting, or mock fallback.
- No Runtime-to-TUI internal adapter.
- No `harbor dev` behavior change.
- No Console embedding/replacement.

## Acceptance criteria

- [x] Served handles expose race-safe one-shot readiness returning the actual
      bound address or bind/cancellation error, through `sdk/server`.
- [x] `harbor serve --tui` launches the ordinary authenticated server, waits for
      readiness, and attaches the complete TUI through REST/SSE.
- [x] `sdk/tui.Run(ctx, Options)` is a curated connection-only facade with no
      Runtime/stack/event-bus handle.
- [x] `harbor scaffold --with-server --with-tui` generates a binary whose
      opt-in `--tui` uses `sdk/server` plus `sdk/tui`; flagless behavior remains
      headless and unchanged.
- [x] Attach quit leaves a remote server alive; explicit co-launch quit drains
      its owned server. Server failure visibly exits after terminal restoration.
- [x] Runtime logs never overwrite Bubble Tea frames; configured sink/captured
      stderr preserves operator access.
- [x] Wave-end PTY E2E covers attach, stock, and generated modes with identity,
      session isolation, conversation, control/intervention, reconnect, resize,
      shutdown ordering, and goroutine baseline.
- [x] The wave-end capture matrix proves distribution modes render equivalent
      frames for the same fixtures; co-launch may not ship a reduced-quality UI.
- [x] README, CHANGELOG, skills, scaffold docs, CLI help, and status tables
      document v1.15 auth, ownership, and launch behavior.

## Files added or changed

- `internal/runtime/serve/serve.go`
- `internal/runtime/serve/external/external.go`
- `sdk/server/`
- `sdk/tui/`
- `cmd/harbor/cmd_serve.go`
- `cmd/harbor/cmd_scaffold.go`
- scaffold templates and goldens
- `test/integration/wave_v115_tui_test.go`
- TUI/scaffold skills and matching docs-site stubs/navigation
- `README.md`
- `CHANGELOG.md`
- `scripts/smoke/phase-184.sh`

## Public API surface

```go
type Options struct {
    BaseURL string
    Token   protocolclient.TokenSource
    Session string
}

func Run(ctx context.Context, opts Options) error
func (h *server.Handle) WaitReady(ctx context.Context) (string, error)
```

CLI additions: `harbor serve --tui` and generated serving-binary `--tui`.

## Test plan

- **Unit:** readiness, ownership shutdown, facade compile, scaffold goldens.
- **Integration:** real stock/generated servers and auth, compiled tool observed
  through Protocol, all three launch modes.
- **Conformance:** cross-mode request and terminal-frame equivalence matrix.
- **Concurrency / leak:** readiness waiters, N≥10 co-launch cycles, PTY/server
  cleanup under `-race`.

## Smoke script additions

- Assert serve/scaffold flags and absence from `harbor dev`.
- Compile generated external module.
- Run `TestE2E_WaveV115TUI` with a no-match-fails guard under `-race`.

## Coverage target

- touched `internal/runtime/serve`: 85%
- `sdk/tui`: 80%
- touched scaffold/CLI paths: 80%

## Dependencies

- 183
- 159
- 160

## Risks / open questions

- Readiness must not add a second listener lifecycle or polling contract.
- Bubble Tea terminal ownership and Runtime logging require strict output
  separation in co-launch modes.

## Glossary additions

- co-launched TUI
- owned server

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] Cross-session isolation test passes
- [x] Readiness/co-launch reuse tests pass under `-race` with no leak
- [x] Wave-end real-driver PTY E2E covers all modes and quality matrix
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed
