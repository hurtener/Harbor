package corrections

import (
	"strings"

	"github.com/hurtener/Harbor/internal/llm"
)

// estimateRequestTokens mirrors the safety-pass estimator (chars/4 +
// per-message overhead) so the backfill numbers match Phase 32's
// token-budget guard. Kept inline so the corrections package does
// not depend on `internal/llm`'s unexported `estimateTokens` helper.
//
// The estimator walks the message slice and sums:
//   - Text-mode content: `len(*m.Content.Text)/4 + 1` (the +1 is
//     role overhead).
//   - Multimodal parts: per-part text characters / 4 + 1 per part.
//   - Audio/image/file parts that arrive as `ArtifactStub` JSON: the
//     stub's serialized form is short (well under threshold), so we
//     count a constant 16 tokens — close enough that operator
//     dashboards see consistent numbers without paying for marshal.
//
// Synthetic; not a replacement for a real tokenizer. Phase 33+ may
// register tiktoken-equivalent estimators via
// `ModelProfile.TokenEstimator`; this fallback runs when the named
// estimator is empty or "chars_div_4".
func estimateRequestTokens(req llm.CompleteRequest) int {
	total := 0
	for _, m := range req.Messages {
		total += 1 // role overhead
		if m.Content.Text != nil {
			total += len(*m.Content.Text) / 4
			continue
		}
		for _, p := range m.Content.Parts {
			switch p.Type {
			case llm.PartText:
				total += len(p.Text) / 4
			case llm.PartImage, llm.PartAudio, llm.PartFile:
				total += 16
			}
		}
	}
	return total
}

// estimateStringTokens returns the chars/4 estimate for a free-form
// string (no role overhead). Used for the response content tally in
// `backfillUsage`.
func estimateStringTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s) / 4
}

// ProfileFor returns the `CorrectionsProfile` for the given model
// name. The lookup is operator-config-first; when the operator hasn't
// declared a profile, a per-known-provider default applies based on
// the model name prefix.
//
// The defaults table is intentionally small and conservative: the
// per-provider quirks listed in brief 03 §4 + brief 08 are encoded
// as defaults so an operator who omits the corrections block still
// gets sensible behaviour when running against a known model. An
// operator who overrides the profile in `harbor.yaml` wins.
//
// This function is exported for tests; production code reads
// `cfg.ModelProfiles[req.Model].Corrections` directly. The function
// exists to demonstrate the defaults table and let smoke tests prove
// the wiring works.
func ProfileFor(cfg llm.ConfigSnapshot, model string) llm.CorrectionsProfile {
	if prof, ok := cfg.ModelProfiles[model]; ok {
		// Zero-valued Corrections still wins — operator declared the
		// model and (implicitly) opted for default behaviour.
		return prof.Corrections
	}
	return defaultProfileFor(model)
}

// defaultProfileFor returns a per-prefix default profile. Prefix
// matching is case-insensitive and deliberately broad — the operator
// can always override.
//
//   - `openai/o1*`, `openai/o3*` → ReasoningRouteThinking +
//     SchemaOpenAIStrict.
//   - `deepseek/deepseek-reasoner*` → ReasoningRouteThinking.
//   - `nim/*` → OrderingSystemFirstStrict (brief 03 §4: NIM
//     rejects mid-thread system).
//   - `anthropic/*` → ResponseFormatAnthropic (envelope shape).
//
// Everything else returns the zero-valued profile (no corrections).
func defaultProfileFor(model string) llm.CorrectionsProfile {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "openai/o1"), strings.HasPrefix(m, "openai/o3"),
		strings.HasPrefix(m, "o1-"), strings.HasPrefix(m, "o3-"),
		strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"):
		return llm.CorrectionsProfile{
			SchemaMode:             llm.SchemaOpenAIStrict,
			ReasoningEffortRouting: llm.ReasoningRouteThinking,
		}
	case strings.Contains(m, "deepseek-reasoner"):
		return llm.CorrectionsProfile{
			ReasoningEffortRouting: llm.ReasoningRouteThinking,
		}
	case strings.HasPrefix(m, "nim/"):
		return llm.CorrectionsProfile{
			MessageOrdering: llm.OrderingSystemFirstStrict,
		}
	case strings.HasPrefix(m, "anthropic/"):
		return llm.CorrectionsProfile{
			ResponseFormatShape: llm.ResponseFormatAnthropic,
		}
	}
	return llm.CorrectionsProfile{}
}
