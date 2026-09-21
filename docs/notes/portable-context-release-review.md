# Portable context release review

PR #779 remains a draft. These are incremental fixes and measured validation,
not an RC approval or a claim that all repository gates have passed.

## Console wire-type parity

The final-tree Console lockstep check failed because the canonical
`SessionsReconcileContextRequest` and `SessionsReconcileContextResponse` had no
hand-maintained TypeScript mirror. The generated manifest and Go clients already
contained both types. Added the missing exported interfaces with the existing
session identity shape. No allowlist exception or weakened guard was introduced.

Validation: the actual Node lockstep command failed before the correction and
passes afterward; strict standalone TypeScript checking and the chat-module
encapsulation guard pass. Full Console lint/test/build remains a separate gate.

## Protocol conformance inventory

The public fork conformance example stopped at its stale 152-method count after
`sessions.reconcile_context` joined the canonical 153-method surface. The same
independent suite also omitted the two retained-context refusal codes from its
error inventory and HTTP 409 table. Added the named method and both named codes,
keeping explicit independent expectations and exhaustive membership checks. The
existing served recovery integration exercises real pending and unavailable
evidence through the authenticated HTTP boundary; no fake recovery port or
allowlist exception replaces that proof.

Fresh validation on Go 1.26.4: complete conformance and public fork race suites
pass; scoped pinned Go lint reports zero issues. Phase 269 smoke, including
the disposable PostgreSQL service, passes 14 checks with no skips or failures.
The broad repository run remains a separate incomplete gate: it reproduced the
old fork inventory failure and exhausted the local memory limit in the served
package. No test, coverage target or production timeout was weakened.

## Ambiguous retained host metadata

The journal/window decoder accepted repeated host fields and case aliases through
`encoding/json`'s last-value and case-insensitive matching. Reconciliation then
accepted and mutated corrupted retained state. Fourteen regressions reproduced
this on in-memory and SQLite stores before the correction; they cover pending
status, admission identity, window version and frame settlement. The frame case
keeps byte accounting consistent so it cannot pass only because of an unrelated
size error.

Typed persisted host envelopes now require canonical, unique field names,
including nested admissions, checkpoints and summary metadata. The check stays
private to retained-state decoding; untyped tool results, maps and raw payloads
remain opaque and preserve exact numeric identifiers. Existing semantic,
version, lifetime, scope and digest checks still follow. Rejection returns a
fixed content-free error and does not publish or repair a corrupted window.

The pending transport payload did not match its checksum and contained an
incorrect hunk count. Its source changes were recovered and independently
reviewed; the retained-state regressions reproduced the defect on the published
base before this recovery was applied. Publication uses the reviewed file tree
directly, not the failed encoded-patch workflow.

Fresh Go 1.26.4 full race suites pass for runctx, assembly and SDK assembly;
measured statement coverage is 86.2%, 83.7% and 100% respectively. Additional
PostgreSQL regressions use independent production pools and reject duplicate or
aliased pending/admission metadata without mutating the retained window. These
are storage-corruption checks, not a claim that an unprivileged user can write
internal StateStore records. Full final-tree release acceptance is still separate.

Expanded phase 269 smoke includes these decoder regressions and passes
14 checks with zero skips or failures on the corrected tree, with PostgreSQL
enabled. Scoped pinned lint reports zero issues; changed Markdown and whitespace
checks pass. No persisted format, dependency or default retention changes.
