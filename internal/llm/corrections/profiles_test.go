package corrections_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/corrections"
)

func TestProfileFor_OperatorOverrideWins(t *testing.T) {
	// Operator-supplied profile (even zero-valued Corrections) wins
	// over the per-prefix defaults. This preserves operator intent.
	cfg := llm.ConfigSnapshot{
		ModelProfiles: map[string]llm.ModelProfile{
			"openai/o1-preview": {
				ContextWindowTokens: 128_000,
				// Corrections is intentionally zero — operator says
				// "no quirks needed for this model in my setup."
			},
		},
	}
	got := corrections.ProfileFor(cfg, "openai/o1-preview")
	if got.ReasoningEffortRouting != llm.ReasoningRouteDefault {
		t.Errorf("operator-zero override lost: got %q want %q",
			got.ReasoningEffortRouting, llm.ReasoningRouteDefault)
	}
}

func TestProfileFor_OpenAIO1_DefaultIsThinking(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		ModelProfiles: map[string]llm.ModelProfile{},
	}
	got := corrections.ProfileFor(cfg, "openai/o1-preview")
	if got.ReasoningEffortRouting != llm.ReasoningRouteThinking {
		t.Errorf("o1 default routing: got %q want %q",
			got.ReasoningEffortRouting, llm.ReasoningRouteThinking)
	}
	if got.SchemaMode != llm.SchemaOpenAIStrict {
		t.Errorf("o1 default schema mode: got %q want %q",
			got.SchemaMode, llm.SchemaOpenAIStrict)
	}
}

func TestProfileFor_DeepSeekReasoner_DefaultIsThinking(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		ModelProfiles: map[string]llm.ModelProfile{},
	}
	got := corrections.ProfileFor(cfg, "deepseek/deepseek-reasoner")
	if got.ReasoningEffortRouting != llm.ReasoningRouteThinking {
		t.Errorf("deepseek-reasoner routing: got %q want %q",
			got.ReasoningEffortRouting, llm.ReasoningRouteThinking)
	}
}

func TestProfileFor_NIM_DefaultIsSystemFirstStrict(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		ModelProfiles: map[string]llm.ModelProfile{},
	}
	got := corrections.ProfileFor(cfg, "nim/llama-3.1-70b")
	if got.MessageOrdering != llm.OrderingSystemFirstStrict {
		t.Errorf("nim ordering: got %q want %q",
			got.MessageOrdering, llm.OrderingSystemFirstStrict)
	}
}

func TestProfileFor_Anthropic_DefaultIsEnvelope(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		ModelProfiles: map[string]llm.ModelProfile{},
	}
	got := corrections.ProfileFor(cfg, "anthropic/claude-sonnet-4")
	if got.ResponseFormatShape != llm.ResponseFormatAnthropic {
		t.Errorf("anthropic shape: got %q want %q",
			got.ResponseFormatShape, llm.ResponseFormatAnthropic)
	}
}

func TestProfileFor_UnknownProvider_IsZeroValue(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		ModelProfiles: map[string]llm.ModelProfile{},
	}
	got := corrections.ProfileFor(cfg, "vendor/unknown-7b")
	if got != (llm.CorrectionsProfile{}) {
		t.Errorf("unknown provider: got %+v want zero", got)
	}
}
