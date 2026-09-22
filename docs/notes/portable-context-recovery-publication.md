# Portable context recovery and publication evidence

September 22, 2026 / PR #779. This note records recovery of the preserved
`d42423a4928fd0477f3a8194b0f6a8cbc6580cc1` checkpoint based on `ac48de6`.
Its archive checksums were verified before publication. GitHub object writes and
ordinary fast-forward ref updates were used; no encoded execution workflow,
force push, approval bypass, merge or release was involved.

## Recovered changes

`4f7d302` retains the published `ac48de6` prepared-intent checks and adds two
settlement guards: reject an oversized stored intent before parsing and require
its expiry to match the admitted journal. In-memory and SQLite regressions
assert that refusal leaves the pending journal unchanged. Corrupt storage is
not treated as evidence that an external action failed or can be replayed.

`c50ba57` preserves typed `agent_retired` during configuration-read failures at
run start. The saved five-stage real-driver regression injects retirement at
LLM overrides, agent/user prompt, prompt-block and completion-hook reads. The
ordinary-error controls retain original mappings. Additional tests distinguish
wrapped/joined sentinels from an unrelated error with identical text. No planner
or tool executes after the failed configuration projection.

`b6ceb97` sets package-level `GOFLAGS=-p=1` only on the existing platform matrix's
`make test` step. A saved controlled Linux experiment with Go 1.26.4, two CPUs
and `GOMAXPROCS=2` reproduced cleanup failure with `-p 2`; the identical tests
with `-p 1` passed both packages twice. The correction does not reduce any
128-session workload, change production five-second deadlines, disable race
instrumentation or skip a test. It still requires hosted macOS validation.

The served-concurrency fixture now identifies the synthetic scope, both terminal
statuses and errors, and whether source/identity evidence reached the next
request. This replaces an opaque failure line, not an assertion or timeout.

## Exact recovered source verification

| File | Verified Git blob |
| --- | --- |
| `internal/runtime/runctx/retained_journal.go` | `298901c0780161b1d5e6f3f608bb6d12557ee428` |
| `internal/runtime/runctx/retained_settlement_validation_test.go` | `e6b6855780e2d3ead8b8557dd93dbbb1ea86556d` |
| `.github/workflows/ci.yml` | `73825f3e61c4b61f6669273c6224b4ec4f116518` |
| `internal/runtime/serve/runloop.go` | `b581aad09d354e442cc183fcf28606bf007d3c2e` |
| `internal/runtime/serve/runloop_retirement_error_test.go` | `18f82df6957053e85a05fb0f607bb27ccd192716` |

The remaining diagnostic test's expected checkpoint blob is
`7b8d0bc8c1dcea8c2aeea6fac32e81863fedb514`. Repository and PR tracker prose is
updated for publication rather than retaining obsolete claims of blocked writes.

## Preserved validation, not a new final-head run

The saved combined-tree results include full runctx/assembly/SDK assembly race
passes (86.9% / 83.7% / 100% statement coverage), phase 269 with PostgreSQL
16.15 and independent pools (14 OK, 0 SKIP, 0 FAIL), full vendor-mode Go lint,
full vet, and a CGo-free static CLI build. Markdown covered 598 files with no
errors; drift reported 1,590 successful checks and one unavailable offline
published-release-reference check.

The canonical local served race/coverage process exceeded the 4 GiB container
limit. A complete fixed inventory of 280 named roots then passed once each in
19 sequential process groups, with no skipped, duplicated or missing tests.
Aggregate served statement coverage was 74.3%, below the documented 85% floor.
Neither grouped execution nor existing earlier-head CI success replaces full
canonical final-head acceptance. The coverage target remains open.

## Outstanding acceptance

Canonical platform tests, coverage remediation, full preflight, Protocol and
applicable frontend/E2E checks, and complete adversarial review remain required.
Previously observed generated-agent module-fetch failures were not waived.
The sample and RC instructions exist but no RC has been published.

[The tracker](portable-context-tracker.md) separates published implementation
from these release gates. Long-term memory remains external to Harbor.
