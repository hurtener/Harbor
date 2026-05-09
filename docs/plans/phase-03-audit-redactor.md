# Phase 03 — Audit redactor

## Summary
Deliver Harbor's central `audit.Redactor` — a single deep-redaction pass that every emit path (event bus, logger, future Governance LLM-boundary hook) must run payloads through before persistence or transmission. Ships the `Redactor` interface, a default pattern-driver covering the canonical secret shapes (`api_key`, `bearer`, `authorization`, `password`, `secret`, `token`, `cookie`), the §4.4 driver registry, and a fail-loudly contract: a rule that errors propagates the error, never silently emits unredacted payload.

## RFC anchor
- RFC §6.14
- RFC §6.4
- RFC §6.15

## Briefs informing this phase
- brief 03
- brief 06

## Brief findings incorporated
- **brief 03 §9 (Q5) — settled at audit subsystem.** "Where does the redactor live — tool subsystem (per-descriptor `Redact` hook) or audit subsystem (a single redactor over the event stream)? The latter is cleaner if the event payload is the canonical record." Harbor adopts the latter: ONE redactor; per-descriptor hooks are NOT the model.
- **brief 06 §1 — event payload is the canonical record.** Redaction must run BEFORE the event hits the bus, not after. Persisted/replayed events must already be redacted; there is no "raw" tier.
- **decisions D-020 — Audit owns redaction; Governance owns thresholds.** Don't conflate. Phase 03 ships the redactor; the Governance subsystem (phases 36a/36b/96) consumes it without re-implementing redaction.

## Findings I'm departing from (if any)
- None.

## Goals
- Deliver a singleton-shaped `audit.Redactor` reachable from any emit path with `audit.MustFrom(ctx)` once the runtime wires it; before the runtime exists (this phase ships only the package), construction is via `audit.Open(ctx, cfg)`.
- Establish the fail-loudly contract for redaction errors: caller MUST treat error as "do not emit" — documented on the interface, enforced by tests.
- Establish the §4.4 driver-registry seam so future redaction strategies (e.g. LLM-boundary semantic redactor, hash-based PII tokenizer) can plug in without changing callers.
- Provide golden-file coverage so the canonical secret shapes are pinned across releases.

## Non-goals
- No wiring into the event bus or logger — those land in Phase 05 (event bus) and Phase 04 (slog logger), which depend on this package.
- No PII redactor at the LLM boundary — that is Phase 96 (post-V1) which hooks the Governance subsystem into this redactor; the contract is preserved here so the post-V1 wiring is mechanical.
- No semantic / embedding-based redaction. V1 is pattern-based.
- No persistence of `audit.redacted` events emitted — the event taxonomy and emit machinery live in Phase 05.

## Acceptance criteria
- [ ] `Redactor` interface defined at `internal/audit/redactor.go` with the documented signature and "must not emit on error" doc-contract.
- [ ] Default `patterns` driver implemented at `internal/audit/drivers/patterns/`; registered via `audit.Register("patterns", ...)`.
- [ ] Built-in rules cover at minimum: `api_key`, `bearer`, `authorization`, `password`, `secret`, `token`, `cookie`. Rule names enumerable.
- [ ] Redaction is deep — nested `map[string]any`, `[]any`, byte slices, strings, and reflective struct walking all redacted consistently.
- [ ] On rule failure (`Apply` returns non-nil error), `Redact` returns the wrapped error and does NOT return a partial payload as fallback. Test asserts no payload bytes leak on error.
- [ ] `noop` driver at `internal/audit/drivers/noop/` exists for tests; lint rule (in `.golangci.yml` exclude path or a `// +build harbor_test` build tag) prevents it from compiling into production binaries.
- [ ] Driver registry uses the §4.4 pattern: drivers self-register from `init()`; `cmd/harbor` blank-imports the production driver only.
- [ ] Coverage on `internal/audit` ≥ 90%.
- [ ] Golden-file tests in `internal/audit/testdata/golden/*.json` exercise the combined rule set against representative event payload shapes.
- [ ] No package-level mutable redactor state (`audit.MustFrom(ctx)` is the only access pattern; no global `audit.Default`).
- [ ] **Multimodal-aware redaction (D-021 / D-022).** A `multimodal` rule group recognizes inline base64 / `DataURL` content in payloads (image / audio / file MIME signatures); rewrites such fields to `[redacted: <MIME> of <N> bytes]` placeholders or, when an `ArtifactRef` is available alongside, swaps to the ref. `ArtifactRef` values pass through unredacted (they are references, not bytes). Golden-file fixture asserts a payload containing a 64 KB inline base64 image redacts cleanly.
- [ ] `make drift-audit` and `make preflight` pass.

## Files added or changed
- `internal/audit/redactor.go` — `Redactor` interface, `Rule` interface, `Open(ctx, cfg)`, `MustFrom(ctx)`, `WithRedactor(ctx, r)`.
- `internal/audit/registry.go` — `Register(name, factory)`, `factories` map, `errFactoryUnknown` listing registered drivers.
- `internal/audit/rules.go` — `Rule` implementations for the canonical secret shapes; helpers (`KeyMatcher`, `RegexMatcher`).
- `internal/audit/redactor_test.go` — interface + registry tests.
- `internal/audit/rules_test.go` — per-rule unit tests.
- `internal/audit/golden_test.go` — golden-file harness reading `testdata/golden/*.json`.
- `internal/audit/drivers/patterns/patterns.go` — default driver; `init()` registers; defaults match the canonical rule set.
- `internal/audit/drivers/patterns/patterns_test.go` — driver-level tests + fuzz.
- `internal/audit/drivers/noop/noop.go` — `noop` driver behind `//go:build harbor_test_only` (or equivalent guard).
- `internal/audit/testdata/golden/{secrets,headers,nested,oversize}.json` — golden inputs and `*.expected.json` outputs.
- `cmd/harbor/main.go` — blank-imports `_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"` (this file does not yet exist; this phase introduces a stub `cmd/harbor/main.go` that exists solely to host the blank-import + `func main(){}` placeholder; phase 09+ replaces it with the real entry point).

## Public API surface
```go
package audit

type Redactor interface {
    // Redact returns a deep-redacted copy of payload. Returns an error if a rule fails;
    // callers MUST treat the error as "do not emit" — never persist or transmit on error.
    Redact(ctx context.Context, payload any) (any, error)
}

type Rule interface {
    Apply(ctx context.Context, payload any) (any, error)
    Name() string
}

func Open(ctx context.Context, cfg config.AuditConfig) (Redactor, error)
func Register(name string, factory func(config.AuditConfig) (Redactor, error))

// Context propagation:
func WithRedactor(ctx context.Context, r Redactor) context.Context
func MustFrom(ctx context.Context) Redactor // panics if absent — fail-loudly
```

## Test plan
- **Unit:** each built-in rule (`api_key`, `bearer`, `authorization`, `password`, `secret`, `token`, `cookie`) — positive (matches), negative (does not over-match plain words), and edge (mixed case, surrounding whitespace, JSON-quoted).
- **Integration:** the `patterns` driver applies the rule set in deterministic order over composite payloads (nested maps, slices, structs); golden-file tests assert byte-equal output.
- **Conformance:** N/A this phase (single driver in V1; the seam exists for future drivers — when a second driver lands, a conformance harness joins them).
- **Concurrency / leak (D-025 concurrent-reuse contract):** `Redactor` is a canonical reusable artifact — built once at boot, shared across every event emitter / logger. Test runs N≥100 goroutines redacting independent payloads against a single shared `Redactor` instance under `-race`, asserting the four guarantees: no data races, no context bleed (each goroutine reads back its own redacted payload), no cancellation cross-talk, no goroutine leaks (baseline-restored). Per AGENTS.md §5 + §11 + RFC §3.5.
- **Fuzz:** `FuzzRedactor` walks malformed inputs (truncated UTF-8, deeply nested maps, cyclic references via `any`-pointer); does not panic; either redacts or returns an error.
- **Failure-mode:** a `Rule.Apply` that returns an error produces a wrapped error from `Redact`; the returned payload is `nil`; no partial leakage.

## Smoke script additions
Phase 03 has no HTTP / Protocol surface — this is a Go-package phase. `scripts/smoke/phase-03.sh` records the surface state explicitly:
- `skip "phase 03: audit redactor — Go package only; validated by go test ./internal/audit/..."`

## Coverage target
- `internal/audit`: 90%
- `internal/audit/drivers/patterns`: 90%

## Dependencies
- 00 (skeleton — shipped). Wave 1: parallelizable with phases 01 (identity) and 02 (config).

## Risks / open questions
- The reflective deep-walk over arbitrary `any` payloads has performance implications. Risk: hot-path callers (logger, event bus) may need a typed fast-path. Mitigation: add a benchmark in this phase; if pathological, design a typed-payload protocol in Phase 05 (event bus) where the canonical event shape is fixed.
- Cyclic `any`-graph inputs are pathological — the fuzz test plus an explicit cycle-detector (or a conservative depth cap with a documented `ErrRedactionDepthExceeded`) prevent infinite recursion. Settle the choice during implementation.
- The `noop` driver guard mechanism (build tag vs lint rule) needs a final pick during implementation; both are acceptable. RFC §11 has no Q-N for this.

## Glossary additions
- **Audit redactor** — Harbor's single deep-redaction pass. The runtime singleton accessed via `audit.MustFrom(ctx)` that every emit path runs payloads through before persistence or transmission. Fail-loudly contract: a rule error means "do not emit." RFC §6.14, brief 03 §9, decisions D-020.

## Pre-merge checklist
- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on `internal/audit` ≥ 90%; on `internal/audit/drivers/patterns` ≥ 90%
- [ ] If multi-isolation paths changed: cross-session isolation test passes (N/A this phase — Redactor is identity-agnostic)
- [ ] If new vocabulary: glossary updated (yes — `Audit redactor` term added)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none)
