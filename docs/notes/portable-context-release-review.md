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

## Stream fixture registration and retained decoder work

The TUI fixture flushes SSE response headers before registering its synthetic
stream. Tests that emit immediately after attach/switch/token replacement can
race that registration. Commit `13ef778` waits on the existing per-session
registration signal at each of those boundaries; it changes no runtime behavior,
assertion, sleep, or timeout. The full conversation race suite passed 100
consecutive runs on Go 1.26.4. A 20-run local baseline did not reproduce the
earlier hosted failure, so the local result is not presented as a reproduction.

Hosted run `35654750952` exposed retained-context deadline failures in the Linux
embedded N=128 test and the macOS served N=128 test. Profiling the embedded path
found the strict host-shape validator repeatedly decoding and copying nested
opaque payloads at each enclosing object and array. It now traverses typed host
containers with one decoder. Opaque leaves remain raw JSON; exact numbers,
canonical/unique host fields, semantic validation, source digests, and all
persistence deadlines remain unchanged. No cache or schema registry is added.

`BenchmarkRetainedDecode_Window` uses one exact 14,660-byte receipt per turn.
Median results from three runs per size on the same Linux host and Go 1.26.4,
with `-benchtime=300ms` and no race instrumentation:

| Turns | Before ns/op | After ns/op | Before bytes/op | After bytes/op |
| --- | ---: | ---: | ---: | ---: |
| 1 | 575,791 | 223,902 | 179,384 | 97,584 |
| 16 | 8,835,661 | 3,277,804 | 2,880,043 | 1,108,242 |
| 32 | 17,613,796 | 6,749,948 | 5,762,854 | 2,186,646 |

These measurements establish reduced decoder work, not task latency, model
quality, cache hits, or proof that every hosted deadline failure is resolved.
The complete runctx/assembly/SDK assembly race suites pass after the change
(86.0%, 83.7%, 100% measured statement coverage). Both embedded and served
128-session concurrency cases pass three repeated race runs without changing
the workload or timeouts. Additional host-field regressions after large opaque
values and between sibling objects preserve rejection and exact payload bytes.
The scoped pinned lint gate reports zero issues. Whole-final-tree coverage and
release acceptance still require their independent gates.

## CI fixture and temporary transport cleanup

The S3 conformance job failed before testing because Docker Hub denied the pull
of `minio/minio:RELEASE.2024-12-18T13-15-44Z`. The fixture now uses the same
release tag from `quay.io/minio/minio`, the registry documented in
[that release's official README](https://github.com/minio/minio/blob/RELEASE.2024-12-18T13-15-44Z/README.md#stable).
Its test port is bound to loopback. The real S3 driver suite, credentials, bucket
checks, teardown, timeouts and every other CI job remain unchanged. This is a
disposable test fixture, not a production deployment recommendation or a claim
that the two registry manifests were independently compared.

Local validation parses the workflow, checks the setup shell syntax, and compares
all other jobs and S3 steps structurally with the preceding tree. A Docker daemon
is unavailable locally; the real image pull and S3 suite still require hosted
validation. Neither a skipped test nor a successful transport job closes this
acceptance gate.

Removed both temporary PR-local publication/source-recovery workflows. No encoded
patch manifest remains tracked. Implementation publication now uses ordinary
authorized repository writes; no test or approval gate is replaced by transport.
The complete release checklist remains open until the final tree is validated.

## Final hardening and coverage closure

The September 22 hardening sweep published five additional commits:

- `30bcf7b` validates the content-stripped identity of custom-redacted actions
  before external dispatch. Tool target, call identity and control targets cannot
  be rewritten while arguments, descriptions and result content remain redactable.
  In-memory, SQLite and real `RunOnce` regressions assert zero external calls on
  refusal.
- `2d0f41c` adds served authority and failure-path tests through existing
  production seams, moving statement coverage from 74.3% to 81.9%.
- `2cfc99f` adds real PostgreSQL boot composition plus MCP rollback/ownership,
  rendering, provider rotation/shutdown and run-loop authority tests. The
  canonical PostgreSQL-backed race/coverage process measures
  `internal/runtime/serve` at **85.1%**, meeting the binding 85% target. The same
  service enablement on the unchanged preceding tree remained at 81.9%.
- `ce684fc4` updates the scaffold fallback from v1.31.6 to the published v1.31.9
  baseline and regenerates its goldens. The canonical drift audit then reports
  1,592 OK / zero warnings / zero failures.
- `cea93340` applies the same action-identity boundary when a custom redactor
  transforms a terminal retained turn. Regressions cover tool/call identity,
  parallel and batch shape, task-control targets and accepted content redaction
  on both in-memory and SQLite stores.

No production five-second deadline, N=128 workload, race instrumentation,
coverage target, production-file inclusion or assertion was weakened. Focused
runctx race/vet/lint and served PostgreSQL race/coverage checks pass. On the
release-candidate documentation tree, terminal-redactor envelope/body refusal
regressions bring the PostgreSQL-backed runctx race profile to 87.0%; runtime
assembly and SDK assembly retain their measured 83.7% and 100% statement
coverage, and served remains at 85.1%.

Two independent adversarial reviews reported no P0. Their P1 findings were the
terminal-redactor identity gap fixed by `cea93340` and stale release evidence
corrected by this documentation increment. Narrow diff-only re-review, the
remaining exact-head repository gates and exact-head hosted CI are still pending.
Run `35697496723` was in progress at the evidence check; running work is not
counted as green.

The owner explicitly waived local and hosted preflight for this RC effort. Those
checks are skipped, not passed, and their absence remains visible in the tracker.
The RC tag, sample-agent deployment and live head-to-head evaluation have not yet
occurred.
