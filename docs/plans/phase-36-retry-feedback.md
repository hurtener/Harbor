# Phase 36 — Retry with feedback

## Summary

Validation/parse failures of an LLM response feed back into the same LLM as a
corrective sub-prompt; the runtime re-asks the model bounded by
`ModelProfile.MaxRetries`. Each retry emits `llm.retry_with_feedback`. The
seam is a `Validator func(CompleteResponse) error` field on `CompleteRequest`
— callers provide a checker; the wrapper runs the loop. The retry wrapper
composes OUTSIDE downgrade so a fresh corrective turn flows through downgrade
+ corrections + safety on each attempt (D-043).

## RFC anchor

- RFC §6.5

## Briefs informing this phase

- brief 03
- brief 07
- brief 08

## Brief findings incorporated

- brief 03 §6 ("retry / repair loops"): validation failures should re-engage the LLM with a corrective sub-prompt. Bounded by `MaxRetries` per planner step. The runtime owns the bound; the planner owns the validator.
- brief 07 ("the elegance principle"): the retry surface is a single field (`Validator`) on the existing request, not a parallel API. The caller stays in control of validation policy; the wrapper handles the mechanics.
- brief 08 §5 ("real-world drift"): JSON-schema responses occasionally fail validation despite a "valid" provider response — operators need a fast, observable corrective loop. Phase 36 ships exactly this.

## Findings I'm departing from (if any)

None.

## Goals

- Add `Validator func(CompleteResponse) error` field on `CompleteRequest`.
- Ship the `retryWithFeedbackClient` wrapper that runs the validator after each successful `Complete`, builds a corrective sub-prompt on failure, and re-asks the inner client up to `ModelProfile.MaxRetries` times.
- Emit `llm.retry_with_feedback` event per retry (identity quadruple + attempt index + truncated failure reason).
- Surface `ErrRetryExhausted` (wrapped) with the chain of failure reasons when the bound is exceeded.
- Compose ABOVE downgrade so retry attempts get fresh downgrade chains: `retry(downgrade(corrections(safetyClient(driver))))`.
- Per-known-provider default `MaxRetries`: 1 across the board (bounded, conservative). Operator-tunable.

## Non-goals

- Provider-side automatic retry (HTTP / RPC retries live inside the bifrost driver per Phase 33a's NetworkDefaults; the Phase 36 wrapper is application-level).
- Multi-validator chains. One validator per request; the caller composes upstream.
- Retry telemetry beyond the event emit (Phase 36a / governance subscribes; per-call dashboards are post-V1).

## Acceptance criteria

- [ ] `CompleteRequest.Validator` field defined.
- [ ] Wrapper runs validator → on fail, builds corrective sub-prompt with (rejected content, validator complaint, retry instruction) and re-calls inner.
- [ ] Retry count bounded by `ModelProfile.MaxRetries` (default 1).
- [ ] `llm.retry_with_feedback` event emits per retry attempt with identity + attempt + truncated reason.
- [ ] `ErrRetryExhausted` wraps the chain of validator failures.
- [ ] **D-025 concurrent-reuse** test: N≥100 concurrent `Complete` calls (with validator) against ONE shared wrapper.
- [ ] Identity propagation through retry loop.
- [ ] Coverage on `internal/llm/retry`: ≥ 85%.
- [ ] `scripts/smoke/phase-36.sh` green; `make preflight` green.

## Files added or changed

- `internal/llm/retry/retry.go` — NEW: `retryWithFeedbackClient` wrapper.
- `internal/llm/retry/retry_test.go`, `d025_test.go` — NEW.
- `internal/llm/llm.go` — MODIFIED: add `Validator` field + `MaxRetries` on `ModelProfile`.
- `internal/llm/errors.go` — MODIFIED: add `ErrRetryExhausted` + `ErrValidationFailed`.
- `internal/llm/events.go` — MODIFIED: register `llm.retry_with_feedback` + `RetryWithFeedbackPayload`.
- `internal/llm/registry.go` — MODIFIED: add `RegisterRetryWrapper` + compose.
- `internal/config/config.go` — MODIFIED: `LLMModelProfileConfig.MaxRetries`.
- `internal/config/validate.go` — MODIFIED.
- `cmd/harbor/main.go` — MODIFIED: blank-import `internal/llm/retry`.
- `examples/harbor.yaml` — MODIFIED: comment `max_retries` knob.
- `docs/plans/phase-36-retry-feedback.md` — NEW (this file).
- `docs/plans/README.md` — MODIFIED: flip Phase 36 row to `Shipped`.
- `README.md` — MODIFIED.
- `docs/decisions.md` — D-043 covers the compose order (shared with Phase 35); D-043 mentions Phase 36 inline.
- `docs/glossary.md` — NEW entries: `RetryWithFeedback`, `Validator`.
- `scripts/smoke/phase-36.sh` — NEW.

## Public API surface

- `llm.CompleteRequest.Validator` field.
- `llm.ModelProfile.MaxRetries` field.
- `llm.ErrRetryExhausted`, `llm.ErrValidationFailed` sentinels.
- `llm.RegisterRetryWrapper(fn func(LLMClient, ConfigSnapshot, Deps) LLMClient)` hook.
- `llm.EventTypeRetryWithFeedback`, `llm.RetryWithFeedbackPayload`.
- `retry.Wrap(inner, cfg, deps) LLMClient`.

## Test plan

- **Unit:** happy path (validator passes — no retry); validator fails once then passes; bounded retry (validator fails N+1 times → `ErrRetryExhausted` chain).
- **Integration:** retry + downgrade composed (validator fail forces retry; if validator complaint signals schema-error class, the inner downgrade can also fire).
- **Conformance:** N/A.
- **Concurrency / leak:** D-025 stress.

## Smoke script additions

- Build clean.
- Run `internal/llm/retry/...` tests under `-race`.
- Assert event registry contains `llm.retry_with_feedback`.

## Coverage target

- `internal/llm/retry`: 85%.

## Dependencies

- 35 (downgrade chain — retry composes OUTSIDE it per D-043).

## Risks / open questions

- A validator that raises a transient/IO error (rather than a deterministic "your output is wrong") could cause needless retry burn. The wrapper does NOT retry when the validator returns nil; it only retries on validator's non-nil error. Operators write validators that classify their own errors.
- The corrective-sub-prompt template ships fixed. Tuning is post-V1.

## Glossary additions

- `RetryWithFeedback` — the validator-driven LLM retry primitive.
- `Validator` — a `func(CompleteResponse) error` Phase 36 reads off the request.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] **Concurrent-reuse test passes** — YES.
- [ ] **Integration test exists** — YES (combined with Phase 35 in `output_integration_test.go`).
- [ ] Glossary updated — YES.
