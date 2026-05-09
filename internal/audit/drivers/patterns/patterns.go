// Package patterns is Harbor's V1 audit redactor driver. It composes
// the canonical rule set from internal/audit (key-based redaction for
// the seven secret shapes + bearer-in-value regex + multimodal
// detection) and applies every rule in deterministic order on every
// Redact call.
//
// The driver self-registers under name "patterns" via init(); the
// runtime entry point cmd/harbor/main.go blank-imports this package
// to trigger registration. Other drivers (PII tokenizer, semantic
// redactor) plug in via the same registry seam without changing
// callers.
package patterns

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
)

// Driver is the patterns Redactor. Built once at boot via Open and
// shared across every emit path; D-025 concurrent-reuse contract
// is enforced by the test suite. The rule slice is immutable after
// construction.
type Driver struct {
	rules []audit.Rule
}

// New constructs a Driver with the canonical V1 rule set. Exposed
// for tests that want to drive the redactor without round-tripping
// through the registry.
func New() *Driver {
	return &Driver{rules: audit.CanonicalRules()}
}

// NewWithRules constructs a Driver from an explicit rule set. Useful
// for tests that want to assert behaviour against a single rule.
func NewWithRules(rules []audit.Rule) *Driver {
	clone := make([]audit.Rule, len(rules))
	copy(clone, rules)
	return &Driver{rules: clone}
}

// Names returns the deterministic order in which this driver applies
// rules. Used by boot-log emission and by golden-file tests.
func (d *Driver) Names() []string {
	out := make([]string, len(d.rules))
	for i, r := range d.rules {
		out[i] = r.Name()
	}
	return out
}

// Redact applies every rule in order. On the first error it returns
// (nil, wrapped error) — fail-loudly per audit.Redactor's contract.
// No partial payload is returned to the caller; the caller MUST NOT
// emit on error.
func (d *Driver) Redact(ctx context.Context, payload any) (any, error) {
	cur := payload
	for _, rule := range d.rules {
		next, err := rule.Apply(ctx, cur)
		if err != nil {
			return nil, fmt.Errorf("%w: rule %s: %w", audit.ErrRedactionFailed, rule.Name(), err)
		}
		cur = next
	}
	return cur, nil
}

func init() {
	audit.Register("patterns", func(_ config.AuditConfig) (audit.Redactor, error) {
		return New(), nil
	})
}
