// Package output is Harbor's structured-output strategy + downgrade
// chain (Phase 35 — RFC §6.5).
//
// The wrapper sits OUTSIDE the Phase 34 corrections layer:
//
//	Open() → retry(downgrade(corrections(safety(driver))))
//
// — settled by D-043. Reasoning: a downgrade rewrites the request's
// `ResponseFormat` (e.g. `json_schema` → `json_object`); the
// corrections layer must re-apply its per-provider envelope shaping
// on the rewritten request, so corrections compose INSIDE downgrade.
// The safety net sees the final outgoing payload (post-corrections,
// post-downgrade) on each attempt.
//
// Three Harbor-side strategies make up `OutputMode`:
//
//  1. `OutputModeNative` — pass `FormatJSONSchema` through. The
//     provider enforces strict schema mode (OpenAI / Anthropic / etc).
//  2. `OutputModeTools` — encode the schema into a *Harbor-side
//     prompted* envelope `{"name":"respond_with","arguments":{...}}`.
//     The runtime parses the response locally. **This is NOT
//     provider-native tool-calling** — the static guard against
//     bifrost's provider tool-call API symbols extends to this
//     package (see scripts/smoke/phase-35.sh).
//  3. `OutputModePrompted` — coerce `FormatJSONObject` and append the
//     schema as a system-prompt instruction.
//
// The downgrade chain is `current → next` on
// `llm.IsInvalidJSONSchemaError(err)`. Order:
//
//	Native → Prompted → Text   (max 3 attempts including the initial)
//	Tools → Prompted → Text
//	Prompted → Text            (already 2-step)
//
// Each downgrade emits `llm.mode_downgraded` with identity + From /
// To / Reason; exhausting the chain surfaces `ErrDowngradeExhausted`
// wrapping the underlying failure.
//
// Concurrent-reuse (D-025): the wrapper is stateless across calls. A
// `Wrap` returns a value holding the inner `LLMClient`, the snapshot,
// and the bus reference; all are read-only after construction.
package output

import (
	"github.com/hurtener/Harbor/internal/llm"
)

// init registers `Wrap` as the downgrade-wrapper hook in the `llm`
// package. The production binary blank-imports this package; the
// registration fires at process boot.
func init() {
	llm.RegisterDowngradeWrapper(Wrap)
}

// Wrap composes the downgrade chain on top of `inner`. The returned
// client wraps each `Complete` invocation in a per-attempt
// `ResponseFormat` rewrite + classifier check.
//
// Nil `inner` panics — composition error caught at boot.
func Wrap(inner llm.LLMClient, cfg llm.ConfigSnapshot, deps llm.Deps) llm.LLMClient {
	if inner == nil {
		panic("output.Wrap: inner is nil")
	}
	return newDowngradeClient(inner, cfg, deps)
}
