// Package noop is a pass-through audit redactor for tests that want
// to bypass redaction. It is intentionally NOT blank-imported by
// cmd/harbor — production binaries never include a no-op redactor —
// and CODEOWNERS plus the §13 forbidden-practice ("Importing a
// concrete driver package from anywhere except cmd/harbor") gate
// catches any accidental inclusion in production code.
//
// Tests that need a redactor without redaction (e.g. event-bus
// plumbing tests where the rule pipeline isn't under test) import
// this package directly and call New().
package noop

import (
	"context"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
)

// Driver is the no-op Redactor. Redact returns its input unchanged.
type Driver struct{}

// New returns a no-op Driver.
func New() *Driver { return &Driver{} }

// Redact returns payload unchanged. Always nil error.
func (Driver) Redact(_ context.Context, payload any) (any, error) {
	return payload, nil
}

func init() {
	audit.Register("noop", func(_ config.AuditConfig) (audit.Redactor, error) {
		return New(), nil
	})
}
