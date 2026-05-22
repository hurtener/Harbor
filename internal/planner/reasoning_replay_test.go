package planner_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

// TestIsValidReasoningReplayMode covers the Phase 83e (D-148) replay-
// mode enum validation: the empty string (unset sentinel), `never`,
// and `text` are valid; anything else is rejected.
func TestIsValidReasoningReplayMode_AcceptsCanonicalAndEmpty(t *testing.T) {
	t.Parallel()
	for _, m := range []planner.ReasoningReplayMode{
		"", planner.ReasoningReplayNever, planner.ReasoningReplayText,
	} {
		if !planner.IsValidReasoningReplayMode(m) {
			t.Errorf("IsValidReasoningReplayMode(%q) = false, want true", m)
		}
	}
}

func TestIsValidReasoningReplayMode_RejectsUnknown(t *testing.T) {
	t.Parallel()
	for _, m := range []planner.ReasoningReplayMode{
		"provider_native", "always", "Never", "TEXT", "yes",
	} {
		if planner.IsValidReasoningReplayMode(m) {
			t.Errorf("IsValidReasoningReplayMode(%q) = true, want false", m)
		}
	}
}

// TestReasoningReplayMode_ZeroValueResolvesToNever asserts the
// critical default contract (Phase 83e care point): the zero value of
// the enum resolves to `never`, NOT an accidental opt-in.
func TestReasoningReplayMode_ZeroValueResolvesToNever(t *testing.T) {
	t.Parallel()
	var zero planner.ReasoningReplayMode
	rc := planner.RunContext{}
	if got := planner.EffectiveReasoningReplay(rc, zero); got != planner.ReasoningReplayNever {
		t.Errorf("EffectiveReasoningReplay(zero-config) = %q, want %q — replay must be OFF by default",
			got, planner.ReasoningReplayNever)
	}
}

// TestEffectiveReasoningReplay_ConfiguredValue asserts the agent-
// configured value applies when there is no per-run override.
func TestEffectiveReasoningReplay_ConfiguredValue(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{} // no override

	if got := planner.EffectiveReasoningReplay(rc, planner.ReasoningReplayText); got != planner.ReasoningReplayText {
		t.Errorf("configured=text → %q, want text", got)
	}
	if got := planner.EffectiveReasoningReplay(rc, planner.ReasoningReplayNever); got != planner.ReasoningReplayNever {
		t.Errorf("configured=never → %q, want never", got)
	}
}

// TestEffectiveReasoningReplay_RunOverrideWins asserts the per-run
// RunContext.ReasoningReplay override beats the agent-configured value
// in both directions.
func TestEffectiveReasoningReplay_RunOverrideWins(t *testing.T) {
	t.Parallel()

	text := planner.ReasoningReplayText
	never := planner.ReasoningReplayNever

	// Configured never, override text → text.
	rcText := planner.RunContext{ReasoningReplay: &text}
	if got := planner.EffectiveReasoningReplay(rcText, planner.ReasoningReplayNever); got != planner.ReasoningReplayText {
		t.Errorf("override=text over configured=never → %q, want text", got)
	}

	// Configured text, override never → never.
	rcNever := planner.RunContext{ReasoningReplay: &never}
	if got := planner.EffectiveReasoningReplay(rcNever, planner.ReasoningReplayText); got != planner.ReasoningReplayNever {
		t.Errorf("override=never over configured=text → %q, want never", got)
	}
}

// TestEffectiveReasoningReplay_NonCanonicalFailsClosed asserts a
// non-canonical mode (defence in depth — config validation rejects
// these pre-boot) resolves to `never`, never to an accidental opt-in.
func TestEffectiveReasoningReplay_NonCanonicalFailsClosed(t *testing.T) {
	t.Parallel()
	bogus := planner.ReasoningReplayMode("provider_native")
	rc := planner.RunContext{ReasoningReplay: &bogus}
	if got := planner.EffectiveReasoningReplay(rc, planner.ReasoningReplayText); got != planner.ReasoningReplayNever {
		t.Errorf("non-canonical override → %q, want never (fail-closed)", got)
	}
	if got := planner.EffectiveReasoningReplay(planner.RunContext{}, bogus); got != planner.ReasoningReplayNever {
		t.Errorf("non-canonical configured → %q, want never (fail-closed)", got)
	}
}
