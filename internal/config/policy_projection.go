package config

import (
	"fmt"
	"time"
)

// ProjectedToolPolicy is the cycle-free, primitive-only image of a
// `ToolPolicyConfig` after projection. Its fields map 1:1 onto
// `tools.ToolPolicy`; the binary entry point (`cmd/harbor`) performs
// the trivial final copy into the runtime type. Splitting the
// projection this way keeps `internal/config` free of an import cycle
// (`tools` → `events` → `config`) while preserving CLAUDE.md §13's
// single-source rule: there is exactly ONE place that interprets the
// operator-facing `ToolPolicyConfig` fields — `ToToolPolicy` below —
// and the `tools.ToolPolicy` STRUCT remains the single definition of
// the policy shape.
//
// Per-field zero-value semantics are preserved: a field that the
// operator omitted (its YAML value is the Go zero value) is left at
// the zero value here, so `tools.ToolPolicy`'s own per-field
// `resolved()` fall-through fills it with the package default at
// dispatch time. The projection NEVER substitutes a default itself —
// the single exception is the `max_attempts → MaxRetries` arithmetic,
// which only fires when `max_attempts >= 1` (an omitted `max_attempts`
// leaves `MaxRetries` zero, inheriting the default attempt count).
type ProjectedToolPolicy struct {
	// TimeoutMS is the per-attempt deadline in milliseconds; 0 means
	// "inherit the default" at the runtime layer.
	TimeoutMS int
	// MaxRetries is `max_attempts - 1` when `max_attempts >= 1`, else
	// 0 (inherit the default total attempt count).
	MaxRetries int
	// BackoffBase / BackoffMax are the projected backoff base / cap; a
	// zero Duration inherits the default.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// BackoffMult is the per-retry multiplier; 0 inherits the default.
	BackoffMult float64
	// RetryOn carries the validated error-class strings (`transient` /
	// `timeout` / `5xx` / `permanent`). nil/empty inherits the default
	// allowlist. The strings are validated against the allowlist by
	// ToToolPolicy; the runtime copy turns them into `tools.ErrorClass`.
	RetryOn []string
	// RetryOnEmpty, when true, instructs the runtime copy to build an
	// EXPLICIT empty (non-nil) `RetryOn` slice — the policy shell reads
	// that as "retry on nothing" (exactly one attempt). Set by
	// ToToolPolicy when `max_attempts == 1` and the operator named no
	// `retry_on` allowlist; see the comment in ToToolPolicy for why the
	// MaxRetries:0 fall-through alone is insufficient. Mutually
	// exclusive with a populated RetryOn.
	RetryOnEmpty bool
}

// validToolPolicyErrorClasses is the operator-facing allowlist for the
// `retry_on` field. It MUST stay in lock-step with the `tools.ErrorClass`
// constants (`transient` / `timeout` / `5xx` / `permanent`). A drift
// here is caught by the cross-check assertion in the mcp driver's
// tests (which compare against the `tools` package constants directly)
// and by config validation rejecting any value not in this set.
var validToolPolicyErrorClasses = map[string]struct{}{
	"transient": {},
	"timeout":   {},
	"5xx":       {},
	"permanent": {},
}

// ToToolPolicy projects the operator-facing `ToolPolicyConfig` onto the
// cycle-free `ProjectedToolPolicy` image. It is the single config→policy
// translation seam (CLAUDE.md §13).
//
// Mapping:
//   - `max_attempts` (TOTAL attempts incl. the first) → `MaxRetries =
//     max_attempts - 1` when `max_attempts >= 1`; an omitted/zero
//     `max_attempts` leaves `MaxRetries` zero so the runtime inherits
//     the default total attempt count (per-field fall-through).
//   - `timeout_ms` → `TimeoutMS` directly.
//   - `backoff_base_ms` / `backoff_max_ms` → `time.Duration`.
//   - `backoff_mult` → `BackoffMult` directly.
//   - `retry_on` strings are validated against the error-class
//     allowlist; an unknown value is a hard error (fail loud — no
//     silent drop, CLAUDE.md §5).
//
// A negative `max_attempts` or `timeout_ms` is rejected here as a
// belt-and-braces guard; config validation rejects them earlier with a
// field-scoped error.
func (c ToolPolicyConfig) ToToolPolicy() (ProjectedToolPolicy, error) {
	out := ProjectedToolPolicy{}

	if c.MaxAttempts < 0 {
		return ProjectedToolPolicy{}, fmt.Errorf("tool policy: max_attempts must be >= 0, got %d", c.MaxAttempts)
	}
	if c.MaxAttempts >= 1 {
		out.MaxRetries = c.MaxAttempts - 1
	}

	if c.TimeoutMS < 0 {
		return ProjectedToolPolicy{}, fmt.Errorf("tool policy: timeout_ms must be >= 0, got %d", c.TimeoutMS)
	}
	out.TimeoutMS = c.TimeoutMS

	if c.BackoffBaseMS < 0 {
		return ProjectedToolPolicy{}, fmt.Errorf("tool policy: backoff_base_ms must be >= 0, got %d", c.BackoffBaseMS)
	}
	if c.BackoffBaseMS > 0 {
		out.BackoffBase = time.Duration(c.BackoffBaseMS) * time.Millisecond
	}

	if c.BackoffMaxMS < 0 {
		return ProjectedToolPolicy{}, fmt.Errorf("tool policy: backoff_max_ms must be >= 0, got %d", c.BackoffMaxMS)
	}
	if c.BackoffMaxMS > 0 {
		out.BackoffMax = time.Duration(c.BackoffMaxMS) * time.Millisecond
	}

	if c.BackoffMult < 0 {
		return ProjectedToolPolicy{}, fmt.Errorf("tool policy: backoff_mult must be >= 0, got %v", c.BackoffMult)
	}
	out.BackoffMult = c.BackoffMult

	if len(c.RetryOn) > 0 {
		retryOn := make([]string, 0, len(c.RetryOn))
		for _, class := range c.RetryOn {
			if _, ok := validToolPolicyErrorClasses[class]; !ok {
				return ProjectedToolPolicy{}, fmt.Errorf(
					"tool policy: unknown retry_on error class %q (allowed: 5xx, permanent, timeout, transient)", class)
			}
			retryOn = append(retryOn, class)
		}
		out.RetryOn = retryOn
	} else if c.MaxAttempts == 1 {
		// max_attempts:1 means "exactly one attempt, no retry". Because
		// the runtime tools.ToolPolicy.resolved() treats a zero
		// MaxRetries on an otherwise-set policy as "inherit the default
		// 3 retries" (per-field fall-through), MaxRetries:0 alone is
		// NOT enough to pin a single attempt. The policy shell honours
		// an EXPLICIT empty (non-nil) RetryOn as "retry on nothing"
		// (one attempt only). So when the operator asked for a single
		// attempt and did not name a retry_on allowlist, we project an
		// explicit-empty RetryOn — RetryOnEmpty signals the runtime
		// copy to build an empty, non-nil slice. This makes
		// max_attempts:1 mean exactly one attempt regardless of the
		// MaxRetries fall-through.
		out.RetryOnEmpty = true
	}

	return out, nil
}
