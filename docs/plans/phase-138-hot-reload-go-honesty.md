# Phase 138 — Hot-reload Go-source honesty

## Summary

`harbor dev`'s fsnotify watcher drove an in-process `bootDevStack` reboot
on *every* watched change — including `.go` source edits — and emitted
`dev.hot_reload.completed{Success=true}` even though an in-process rebuild
never recompiles the binary. The Go change was never picked up, yet the
runtime reported a successful hot-reload: a loud false-success. This phase
makes the watcher honest: a `.go` change routes to a WARN-and-guide path
(`make build` + restart) and does **not** reboot or emit a `completed`
event, while config / YAML / scaffold changes keep the existing in-process
rebuild path unchanged.

## RFC anchor

- RFC §8

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 (devx, "What `harbor dev` exposes"): the dev loop "watches the
  project directory for changes, hot-reloads on Go-source changes (graceful-stop
  in-flight runs first; configurable)". This phase keeps the watcher + graceful
  drain but corrects the brief's aspiration where it does not hold: an
  in-process rebuild cannot recompile Go, so "hot-reloads on Go-source changes"
  is delivered honestly as detect-and-guide, not silently-claim-success.
- brief 06 (devx, hot-reload tests): "file change triggers graceful run drain
  and restart; in-flight runs cancel cleanly; new runs pick up new code." The
  "new runs pick up new code" guarantee is true for config / YAML (the rebuild
  re-reads them) but false for `.go` (no recompile). The phase pins the
  distinction with a live test rather than letting the false guarantee stand.

## Findings I'm departing from (if any)

- brief 06 implies the watcher hot-reloads `.go` source the same way it reloads
  config. The shipped V1 shape (in-process `bootDevStack` rebuild, never a Go
  recompile or binary re-exec) cannot honour that for `.go`. The departure:
  `.go` changes are detected and WARN-guided to a manual rebuild rather than
  driven through a rebuild that reports a success it did not achieve. This is a
  deliberate honesty correction, recorded as D-268.

## Goals

- Detect `.go` changes in the watcher and route them to a WARN + `make build`
  / restart guidance path instead of an in-process `bootDevStack` reboot.
- Guarantee a `.go` change never emits `dev.hot_reload.completed{Success=true}`.
- Keep the config / YAML / scaffold in-process rebuild path unchanged.
- Make the dev-loop recipe state the `.go`-vs-config reload distinction.
- Complete the dangling doc sentence at `cmd_dev_hot_reload.go` ("This is
  documented in." → name the recipe).

## Non-goals

- `policy: rebuild-binary` (auto `go build` + re-exec on a `.go` change) — the
  open question is resolved as **deferred**; this phase ships WARN + guidance
  only.
- Editing `docs/skills/run-the-dev-loop/SKILL.md` — it is already honest about
  `.go` reload.
- Editing the README (it carries no hot-reload claim) or the marketing site
  (`landingSpec.ts:236` is the sibling honesty-sweep phase's target).

## Acceptance criteria

- [ ] A `.go` change classifies to a WARN-and-guide path (`classifyEvent` →
      `reloadGoSourceWarn`); `shouldTrigger` returns false for `.go`.
- [ ] A `.go` change logs the honest WARN ("Go source change detected …") and
      does **not** drive an in-process rebuild — no `dev.hot_reload.completed`
      for that edit.
- [ ] A config / YAML / scaffold change still drives the in-process rebuild and
      emits `dev.hot_reload.completed{Success=true}` (path unchanged).
- [ ] The dangling doc sentence in `cmd_dev_hot_reload.go` is completed and the
      package doc describes the WARN routing; no phase/`D-`/brief refs in the
      non-test godoc (§13).
- [ ] `docs/recipes/run-harbor-dev.md` carries the `.go`-changes caveat.
- [ ] `scripts/smoke/phase-65.sh` performs a live `.go` edit against the running
      preflight dev server and asserts the WARN fires + no rebuild fired.

## Files added or changed

- `cmd/harbor/cmd_dev_hot_reload.go` — classification (`reloadClass` /
  `classifyEvent` / `isGoSource`), `shouldTrigger` re-expressed as the
  rebuild-gate predicate, `warnGoSourceChange`, Run-loop routing, completed doc.
- `cmd/harbor/cmd_dev_hot_reload_test.go` — `classifyEvent` / `isGoSource` unit
  tests; updated `shouldTrigger` `.go` expectation; live
  `TestHotReloadSupervisor_GoSourceChange_WarnsNoRebuild` (WARN fires, no
  stack swap for `.go`; YAML still swaps).
- `scripts/smoke/phase-65.sh` — Assertion 4: live `.go`-edit honesty.
- `docs/recipes/run-harbor-dev.md` — `.go`-changes caveat.
- `docs/plans/phase-138-hot-reload-go-honesty.md` — this plan.
- `docs/plans/README.md` — index row + detail block.
- `docs/decisions.md` — D-268.

## Public API surface

- None. All changes are internal to `package main` (`cmd/harbor`). No Protocol
  method, wire type, error code, config field, or exported API changes.

## Test plan

- **Unit:** `TestClassifyEvent_RoutesGoSourceToWarn`, `TestIsGoSource`, and the
  updated `TestShouldTrigger_SkipsDBSidecars` (`.go` now does not rebuild).
- **Integration:** `TestHotReloadSupervisor_GoSourceChange_WarnsNoRebuild` —
  in-package, real `bootDevStack` + real fsnotify watcher: a live `.go` edit
  logs the honest WARN and does not swap the stack (no rebuild / no `completed`),
  then a YAML edit DOES swap the stack (rebuild path unchanged). The existing
  `TestHotReloadSupervisor_RebuildEmitsCompletedOnNewBus` continues to prove the
  rebuild path emits `completed{Success=true}`.
- **Conformance:** N/A — no multi-driver interface changed.
- **Concurrency / leak:** N/A — no new reusable artifact; the supervisor's
  existing ctx-cancel / leak tests are unchanged and still pass under `-race`.

## Smoke script additions

- `scripts/smoke/phase-65.sh` Assertion 4 (live-server): write a `.go` file
  into the watched `examples/` dir, assert the server log contains
  "hot-reload: Go source change detected", and assert the count of
  "hot-reload: file change observed" (the rebuild-path marker) is unchanged
  across the edit — the observable proxy for "no
  `dev.hot_reload.completed{Success=true}`". A static `strings` grep cannot
  distinguish this (the `completed` string stays in the binary for the YAML
  path), so the assertion is a live edit-and-observe.

## Coverage target

- `cmd/harbor`: ≥ 75% (master-plan target for the dev subsystem); the touched
  hot-reload paths gain direct unit + live coverage.

## Dependencies

- 65 (`harbor dev` hot-reload supervisor — the surface this corrects).

## Risks / open questions

- **Open question resolved:** `policy: rebuild-binary` is deferred (WARN +
  guidance only). Recorded in D-268.
- A live `.go` edit is safe against the long-running preflight server precisely
  because the edit no longer reboots — it only WARNs. The smoke deliberately
  does not perform a live YAML edit (that would reboot and could disturb sibling
  live-server smokes); the YAML rebuild path is covered in-package.

## Glossary additions

- None — no new vocabulary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, no identity-scoped path changed.
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A, no new reusable artifact (the supervisor is unchanged in shape; routing-only edit).
- [ ] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: integration test exists — the in-package `TestHotReloadSupervisor_GoSourceChange_WarnsNoRebuild` wires the real supervisor + real fsnotify + real bus end-to-end under `-race`.
- [ ] If new vocabulary: glossary updated — N/A.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — D-268.
