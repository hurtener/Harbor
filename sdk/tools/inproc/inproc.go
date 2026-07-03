// Package inproc is the public SDK facade over Harbor's
// internal/tools/drivers/inproc package — the in-process tool driver
// that registers plain Go functions as Tools with reflection-derived
// schemas (RFC §3.6, §6.10). RegisterFunc is generic, so it is
// forwarded as a wrapper (Go has no generic function values); the
// wrapper adds no behavior. Schema-derivation internals are
// deliberately private.
package inproc

import (
	"context"

	internal "github.com/hurtener/Harbor/internal/tools/drivers/inproc"
	"github.com/hurtener/Harbor/sdk/tools"
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrSchemaBuild — JSON-schema derivation failed for I or O.
	ErrSchemaBuild = internal.ErrSchemaBuild
	// ErrUnsupportedType — I or O is not schema-derivable.
	ErrUnsupportedType = internal.ErrUnsupportedType
)

// RegisterFunc registers a Go function as a Tool on the catalog.
// Input and output schemas are derived from the type parameters I and
// O via reflection; opts configure the descriptor (policy, scopes,
// description, examples — see the sdk/tools DescriptorOption set).
// `fn` must be safe for concurrent invocation. Thin generic
// forward over the internal driver; no behavior is added.
func RegisterFunc[I any, O any](
	cat tools.ToolCatalog,
	name string,
	fn func(ctx context.Context, in I) (O, error),
	opts ...tools.DescriptorOption,
) error {
	return internal.RegisterFunc(cat, name, fn, opts...)
}
