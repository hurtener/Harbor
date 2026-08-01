# Phase 225 — Run and prompt fidelity

## Summary

Supersedes the affected semantics of phases 219, 220, and 222: run requests are one strict JSON document, caller-memory telemetry reports admitted wire bytes, normal users retain a contained personalization tier, and valid extra system blocks are byte-faithful.

## RFC anchor

- RFC §5.2.
- RFC §6.2.
- RFC §6.5.

## Briefs informing this phase

- brief 13
- brief 02
- brief 03

## Brief findings incorporated

- brief 13 §2.3: untrusted prompt contributions need fixed runtime-owned framing.
- brief 02: run-specific state belongs in the run context, not shared planner state.
- brief 03 §5: one mechanism must own each prompt contribution; parallel modes drift.

## Findings I'm departing from (if any)

- Phase 220 placed caller-authored `extra_instructions` in the operator guidance block. This phase supersedes that placement while preserving the public field and non-admin access.

## Goals

- Preserve normal-user personalization without representing prompt order as an authorization boundary.
- Make decoding, byte accounting, redaction, and extra-block rendering truthful.

## Non-goals

- No admin-only gate for `extra_instructions`; no Protocol version change; no filtering of personalization prose.

## Acceptance criteria

- [x] A second JSON value after a valid run request is rejected; trailing whitespace is accepted.
- [x] `memory.caller_block_admitted.bytes` equals pre-redaction wire bytes and no raw secret reaches events or state.
- [x] The existing 32 KiB caller-memory and 256 KiB control-request bounds remain effective and documented.
- [x] Normal users can set one-run personalization, tenant guidance survives unchanged, and runtime authority is unaffected.
- [x] Nonblank extra system blocks preserve exact bytes; whitespace-only blocks are rejected or ignored for legacy records.
- [x] Identity, cancellation, and N≥100 concurrent reuse show no cross-talk or leak.

## Files added or changed

- `internal/protocol/transports/stream/`
- `internal/runtime/runctx/`, `internal/planner/react/`, `internal/agentcfg/`
- Protocol/operator documentation and `scripts/smoke/phase-225.sh`

## Public API surface

- Existing wire fields remain unchanged. Internal prompt composition distinguishes operator guidance from caller personalization.

## Test plan

- **Unit:** decoder EOF, byte accounting, prompt escaping, byte-faithful blocks.
- **Integration:** real Protocol start/override path with redaction and authority assertions.
- **Conformance:** N/A — no driver interface change.
- **Concurrency / leak:** N≥100 shared planner/run-context compositions under `-race`.

## Smoke script additions

- Execute the named strict-decoding, telemetry, personalization, and block-fidelity tests and require real PASS markers.

## Coverage target

- Touched Go packages do not fall below their measured v1.25 floor.

## Dependencies

- 219, 220, 222.

## Risks / open questions

- Prompt delimiters mitigate accidental instruction blending but do not create a model-enforced security boundary.

## Glossary additions

- User personalization.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] CI preflight passes (local preflight prohibited for this review/remediation mandate)
- [x] `make check-mirror` passes
- [x] All cross-references resolve; coverage, isolation, concurrent-reuse, integration, and glossary gates pass
