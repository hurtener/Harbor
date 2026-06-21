package agentcfg_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
)

func psp(s string) *string   { return &s }
func pfp(f float64) *float64 { return &f }
func pip(i int) *int         { return &i }

// TestContentHash_IncludesLLMParams proves a revision that changes ONLY the
// per-agent LLM-params section produces a distinct content hash (so it is a
// real, diffable revision).
func TestContentHash_IncludesLLMParams(t *testing.T) {
	base := agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a"}}}
	withModel := agentcfg.ConfigPayload{
		Skills:    &agentcfg.SkillsSelection{Names: []string{"a"}},
		LLMParams: &agentcfg.LLMParams{Model: psp("model-x")},
	}
	hb, err := agentcfg.ContentHash(base)
	if err != nil {
		t.Fatalf("hash base: %v", err)
	}
	hm, err := agentcfg.ContentHash(withModel)
	if err != nil {
		t.Fatalf("hash withModel: %v", err)
	}
	if hb == hm {
		t.Fatal("adding an LLM-params section did not change the content hash")
	}
	// A different temperature is a distinct hash too.
	t1, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{LLMParams: &agentcfg.LLMParams{Temperature: pfp(0.2)}})
	t2, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{LLMParams: &agentcfg.LLMParams{Temperature: pfp(0.9)}})
	if t1 == t2 {
		t.Fatal("different temperatures hashed equal")
	}
}

// TestNormalizePayload_LLMParams_EmptyStringsDropToNil proves an empty Model
// / ReasoningEffort string normalises to unset, and an all-empty section
// drops out (so a no-op set does not perturb the hash / create a phantom
// diff).
func TestNormalizePayload_LLMParams_EmptyStringsDropToNil(t *testing.T) {
	got := agentcfg.NormalizePayload(agentcfg.ConfigPayload{
		LLMParams: &agentcfg.LLMParams{Model: psp("  "), ReasoningEffort: psp("")},
	})
	if got.LLMParams != nil {
		t.Fatalf("all-empty LLM-params section should drop to nil, got %+v", got.LLMParams)
	}

	// A section with one real dimension survives; the empty string drops.
	got2 := agentcfg.NormalizePayload(agentcfg.ConfigPayload{
		LLMParams: &agentcfg.LLMParams{Model: psp(""), Temperature: pfp(0.5)},
	})
	if got2.LLMParams == nil || got2.LLMParams.Model != nil {
		t.Fatalf("empty model should drop to nil, surviving section = %+v", got2.LLMParams)
	}
	if got2.LLMParams.Temperature == nil || *got2.LLMParams.Temperature != 0.5 {
		t.Fatalf("temperature should survive: %+v", got2.LLMParams)
	}

	// Hash equality: an all-empty section equals no section.
	hNone, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{})
	hEmpty, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{LLMParams: &agentcfg.LLMParams{Model: psp("")}})
	if hNone != hEmpty {
		t.Fatal("an all-empty LLM-params section must hash equal to no section")
	}
}

// TestDiffLLMParams_PerFieldDelta proves each dimension reports its own
// change + from/to (numeric dimensions formatted canonically; an unset
// dimension is "").
func TestDiffLLMParams_PerFieldDelta(t *testing.T) {
	from := agentcfg.ConfigPayload{LLMParams: &agentcfg.LLMParams{
		Model:       psp("model-a"),
		Temperature: pfp(0.2),
		MaxTokens:   pip(1000),
	}}
	to := agentcfg.ConfigPayload{LLMParams: &agentcfg.LLMParams{
		Model:           psp("model-b"),
		Temperature:     pfp(0.2), // unchanged
		ReasoningEffort: psp("high"),
		// MaxTokens unset → from 1000 to "".
	}}
	d := agentcfg.DiffLLMParams(from, to)
	if !d.Changed() {
		t.Fatal("Changed() = false, want true")
	}
	if !d.ModelChanged || d.ModelFrom != "model-a" || d.ModelTo != "model-b" {
		t.Errorf("model delta = %+v", d)
	}
	if d.TemperatureChanged {
		t.Errorf("temperature should be unchanged (0.2 → 0.2): %+v", d)
	}
	if !d.MaxTokensChanged || d.MaxTokensFrom != "1000" || d.MaxTokensTo != "" {
		t.Errorf("max-tokens delta = from %q to %q (set→unset)", d.MaxTokensFrom, d.MaxTokensTo)
	}
	if !d.ReasoningEffortChanged || d.ReasoningEffortFrom != "" || d.ReasoningEffortTo != "high" {
		t.Errorf("reasoning-effort delta = from %q to %q (unset→set)", d.ReasoningEffortFrom, d.ReasoningEffortTo)
	}
}

// TestDiffLLMParams_NoChange proves two identical sections (and two
// no-LLM-params payloads) report no change.
func TestDiffLLMParams_NoChange(t *testing.T) {
	same := agentcfg.ConfigPayload{LLMParams: &agentcfg.LLMParams{Model: psp("m"), Temperature: pfp(0.5)}}
	if agentcfg.DiffLLMParams(same, same).Changed() {
		t.Error("identical sections reported a change")
	}
	if agentcfg.DiffLLMParams(agentcfg.ConfigPayload{}, agentcfg.ConfigPayload{}).Changed() {
		t.Error("two empty payloads reported a change")
	}
}
