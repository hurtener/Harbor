package tools

import (
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
)

func TestFit_EmptyInput(t *testing.T) {
	t.Parallel()

	got, sum, dropped, err := Fit(nil, 100)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if got != nil {
		t.Fatalf("got=%v, want nil", got)
	}
	if sum || dropped {
		t.Fatalf("got summarized=%v droppedSteps=%v, want both false", sum, dropped)
	}
}

func TestFit_InvalidMaxTokens(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{{Name: "x"}}
	_, _, _, err := Fit(in, 0)
	if !errors.Is(err, ErrSkillTooLarge) {
		t.Fatalf("err=%v, want wrapped ErrSkillTooLarge for maxTokens=0", err)
	}
}

func TestFit_Step0_Full_Fits(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{
		{
			Name:    "small",
			Title:   "t",
			Steps:   []string{"a"},
			Trigger: "trig",
		},
	}
	got, sum, dropped, err := Fit(in, 1000)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if sum || dropped {
		t.Fatalf("ladder fired at step 0 — sum=%v dropped=%v, want both false", sum, dropped)
	}
	if len(got) != 1 || got[0].Name != "small" {
		t.Fatalf("got=%v, want one entry named small", got)
	}
}

func TestFit_Step1_DropsOptional(t *testing.T) {
	t.Parallel()

	// Build a skill big enough to exceed budget when full but fit
	// when preconditions + failure_modes are dropped.
	in := []skills.Skill{
		{
			Name:          "demo",
			Title:         "concise",
			Trigger:       "concise",
			Steps:         []string{"s1", "s2"},
			Preconditions: []string{strings.Repeat("p", 200)},
			FailureModes:  []string{strings.Repeat("f", 200)},
		},
	}
	// Full estimate: roughly (200+200)/4 + a few small fields ≈ 110+ tokens.
	// Set budget to 50 so step 0 fails and step 1 fits.
	got, sum, dropped, err := Fit(in, 50)
	if err != nil {
		t.Fatalf("err=%v, want nil after step 1 fit", err)
	}
	if !sum {
		t.Fatalf("summarized=false, want true after step 1")
	}
	if dropped {
		t.Fatalf("droppedSteps=true, want false (step 2 should not fire)")
	}
	if len(got[0].Preconditions) != 0 || len(got[0].FailureModes) != 0 {
		t.Fatalf("optional fields not dropped: pre=%v fm=%v", got[0].Preconditions, got[0].FailureModes)
	}
	if len(got[0].Steps) != 2 {
		t.Fatalf("steps capped prematurely: %v", got[0].Steps)
	}
}

func TestFit_Step2_CapsSteps(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{
		{
			Name:          "demo",
			Title:         "t",
			Trigger:       "g",
			Steps:         []string{strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100), strings.Repeat("d", 100), strings.Repeat("e", 100)},
			Preconditions: []string{strings.Repeat("p", 200)},
			FailureModes:  []string{strings.Repeat("f", 200)},
		},
	}
	// After step 1, optional dropped → 5×~25 + framing ≈ 130 tokens.
	// After step 2, 3×~25 + framing ≈ 80 tokens. Pick 100 so step 2 fits.
	got, sum, dropped, err := Fit(in, 100)
	if err != nil {
		t.Fatalf("err=%v, want nil after step 2 fit", err)
	}
	if !sum || !dropped {
		t.Fatalf("got sum=%v dropped=%v, want both true", sum, dropped)
	}
	if len(got[0].Steps) != 3 {
		t.Fatalf("got %d steps, want 3 (capped)", len(got[0].Steps))
	}
}

func TestFit_Step3_FailsLoudly(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{
		{
			Name:    "huge",
			Title:   strings.Repeat("x", 2000),
			Trigger: strings.Repeat("y", 2000),
			Steps:   []string{strings.Repeat("a", 4000), strings.Repeat("b", 4000), strings.Repeat("c", 4000)},
		},
	}
	_, _, _, err := Fit(in, 10) // tiny budget — even capped @ 3 steps won't fit
	if !errors.Is(err, ErrSkillTooLarge) {
		t.Fatalf("err=%v, want wrapped ErrSkillTooLarge", err)
	}
}

func TestFit_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{
		{
			Name:          "demo",
			Steps:         []string{"s1", "s2", "s3", "s4"},
			Preconditions: []string{"p1"},
		},
	}
	// Force step 2 (cap steps) to fire by feeding huge optional.
	in[0].FailureModes = []string{strings.Repeat("f", 1000)}
	_, _, _, err := Fit(in, 50)
	_ = err
	if len(in[0].Steps) != 4 {
		t.Fatalf("input Steps mutated: len=%d, want 4", len(in[0].Steps))
	}
	if len(in[0].Preconditions) != 1 {
		t.Fatalf("input Preconditions mutated: len=%d, want 1", len(in[0].Preconditions))
	}
	if len(in[0].FailureModes) != 1 {
		t.Fatalf("input FailureModes mutated: len=%d, want 1", len(in[0].FailureModes))
	}
}

func TestFit_MultipleSkills(t *testing.T) {
	t.Parallel()

	// Two skills with optional fields — step 1 must drop both.
	in := []skills.Skill{
		{Name: "a", Title: "x", Preconditions: []string{strings.Repeat("p", 200)}, FailureModes: []string{strings.Repeat("f", 200)}, Steps: []string{"s"}},
		{Name: "b", Title: "x", Preconditions: []string{strings.Repeat("p", 200)}, FailureModes: []string{strings.Repeat("f", 200)}, Steps: []string{"s"}},
	}
	got, sum, _, err := Fit(in, 80)
	if err != nil {
		t.Fatalf("err=%v, want nil after step 1", err)
	}
	if !sum {
		t.Fatalf("summarized=false, want true")
	}
	if len(got) != 2 {
		t.Fatalf("got %d skills, want 2", len(got))
	}
	for i, s := range got {
		if len(s.Preconditions) != 0 || len(s.FailureModes) != 0 {
			t.Fatalf("got[%d] optional fields not dropped: pre=%v fm=%v", i, s.Preconditions, s.FailureModes)
		}
	}
}

func TestFit_ErrorMessageMentionsBudget(t *testing.T) {
	t.Parallel()

	in := []skills.Skill{{Name: "x", Title: strings.Repeat("y", 1000), Steps: []string{strings.Repeat("z", 1000), strings.Repeat("z", 1000), strings.Repeat("z", 1000), strings.Repeat("z", 1000)}}}
	_, _, _, err := Fit(in, 5)
	if err == nil {
		t.Fatalf("err=nil, want error")
	}
	if !strings.Contains(err.Error(), "maxTokens=5") {
		t.Fatalf("err=%q, expected to mention maxTokens=5", err.Error())
	}
}
