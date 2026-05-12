package tools

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/skills"
)

// Fit applies the tiered budgeter ladder to `in` and returns the
// first slice that fits within `maxTokens`. The ladder (brief 04
// §4.5):
//
//  1. Full — every field intact.
//  2. Drop optional — clear `Preconditions` and `FailureModes`.
//  3. Cap steps — truncate `Steps` to 3 (and drop optional as in
//     step 2).
//  4. Fail loud — return `ErrSkillTooLarge`.
//
// Returns:
//   - `out` — the fit slice (may be empty if `in` was empty).
//   - `summarized` — true when step 1 (drop optional) fired.
//   - `droppedSteps` — true when step 2 (cap steps) fired.
//   - `err` — non-nil iff the budget was exhausted after step 3.
//
// Token estimation is chars/4 (matches the §6.5 LLM safety net's
// estimator at V1). A tokenizer-backed estimator is a post-V1
// swap-in via this single function.
//
// Concurrent-safe by construction: pure function over value inputs.
// `in` is not mutated; the returned slice is a fresh allocation when
// the ladder steps fire.
func Fit(in []skills.Skill, maxTokens int) (out []skills.Skill, summarized bool, droppedSteps bool, err error) {
	if len(in) == 0 {
		return nil, false, false, nil
	}
	if maxTokens <= 0 {
		// Caller passed an invalid budget — fail loud rather than
		// silently fitting something arbitrarily.
		return nil, false, false, fmt.Errorf("%w: maxTokens=%d (must be > 0)", ErrSkillTooLarge, maxTokens)
	}

	// Step 0 — full.
	if tokensFor(in) <= maxTokens {
		return cloneSkills(in), false, false, nil
	}

	// Step 1 — drop optional fields.
	step1 := dropOptional(in)
	if tokensFor(step1) <= maxTokens {
		return step1, true, false, nil
	}

	// Step 2 — cap steps to 3 (on top of dropped optional).
	step2 := capSteps(step1, 3)
	if tokensFor(step2) <= maxTokens {
		return step2, true, true, nil
	}

	// Step 3 — still over budget. Fail loud per CLAUDE.md §5.
	return nil, false, false, fmt.Errorf("%w: maxTokens=%d, estimated=%d after full ladder (%d skill(s))",
		ErrSkillTooLarge, maxTokens, tokensFor(step2), len(in))
}

// dropOptional returns a fresh slice with `Preconditions` and
// `FailureModes` cleared on every entry. Steps, tags, required-*
// stay intact.
func dropOptional(in []skills.Skill) []skills.Skill {
	out := make([]skills.Skill, len(in))
	for i, s := range in {
		s.Preconditions = nil
		s.FailureModes = nil
		out[i] = s
	}
	return out
}

// capSteps returns a fresh slice with `Steps` truncated to `max`
// entries per skill. When the source slice has ≤ max entries, the
// skill is returned unchanged (so the function is safe to call
// repeatedly without re-allocating in the common case).
func capSteps(in []skills.Skill, max int) []skills.Skill {
	out := make([]skills.Skill, len(in))
	for i, s := range in {
		if len(s.Steps) > max {
			capped := make([]string, max)
			copy(capped, s.Steps[:max])
			s.Steps = capped
		}
		out[i] = s
	}
	return out
}

// cloneSkills returns a fresh slice with copy-by-value of every
// entry. Slice fields on `Skill` are shared with the caller's input
// (deep copy is unnecessary at the budgeter — Redact has already
// produced fresh allocations for the text-bearing slices).
func cloneSkills(in []skills.Skill) []skills.Skill {
	out := make([]skills.Skill, len(in))
	copy(out, in)
	return out
}

// tokensFor estimates the total token count of `in` via the chars/4
// envelope. The envelope sums every text-bearing field that the
// planner-prompt renderer will surface:
//
//   - Name + Title + Description + Trigger + TaskType (scalar)
//   - Tags (joined with spaces)
//   - Steps + Preconditions + FailureModes (entries joined with newlines)
//   - RequiredTools + RequiredNS + RequiredTags (joined with spaces)
//
// Per skill, plus a per-skill framing overhead of 8 tokens.
//
// Cost: O(text-bytes-per-skill * N). For the typical N ≤ 20 SkillStore
// limit and skill bodies in the kilobyte range, the estimate is
// O(microseconds).
func tokensFor(in []skills.Skill) int {
	total := 0
	for _, s := range in {
		total += charsEstimate(s.Name)
		total += charsEstimate(s.Title)
		total += charsEstimate(s.Description)
		total += charsEstimate(s.Trigger)
		total += charsEstimate(s.TaskType)
		for _, t := range s.Tags {
			total += charsEstimate(t)
		}
		for _, step := range s.Steps {
			total += charsEstimate(step)
		}
		for _, p := range s.Preconditions {
			total += charsEstimate(p)
		}
		for _, f := range s.FailureModes {
			total += charsEstimate(f)
		}
		for _, t := range s.RequiredTools {
			total += charsEstimate(t)
		}
		for _, n := range s.RequiredNS {
			total += charsEstimate(n)
		}
		for _, t := range s.RequiredTags {
			total += charsEstimate(t)
		}
		// Per-skill framing — section headers + structure overhead
		// the planner adds when rendering. Conservative 8 tokens.
		total += 8
	}
	return total
}

// charsEstimate returns the chars/4 token estimate for `s`. Matches
// the §6.5 LLM safety net's estimator (chars/4) so the budgeter and
// the safety net agree on token cost at V1.
func charsEstimate(s string) int {
	// Integer division by 4 — the canonical chars-per-token
	// heuristic used across the LLM industry as a low-precision
	// envelope. The ceiling adjustment ensures a 1-3 char field
	// still counts as 1 token (not 0) so the budgeter doesn't
	// underestimate trivially-small fields.
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}
