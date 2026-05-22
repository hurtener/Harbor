// Package weather is a worked, runnable example of registering a Go
// function as a Harbor in-process tool.
//
// It shows the canonical pattern: declare typed input/output structs,
// call inproc.RegisterFunc, and let the driver derive JSON Schemas
// from the types via reflection. The registered function is wrapped
// in Harbor's ToolPolicy shell (timeout + retry + validation) so a
// plain registration is production-resilient by construction.
//
// The "lookup" function returns a canned, deterministic result — a
// real tool would call a weather API here. The value of this example
// is the registration SHAPE, not the data source. See docs/recipes/
// define-a-tool.md for the companion how-to.
package weather

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// LookupArgs is the tool's typed input. JSON Schema is derived from
// the struct tags by the inproc driver at registration time.
type LookupArgs struct {
	// City is the city name to look up.
	City string `json:"city"`
}

// LookupResult is the tool's typed output.
type LookupResult struct {
	// City echoes the requested city.
	City string `json:"city"`
	// TempC is the temperature in degrees Celsius.
	TempC float64 `json:"temp_c"`
	// Summary is a short human-readable conditions string.
	Summary string `json:"summary"`
}

// ToolName is the catalog name the example tool registers under.
const ToolName = "weather.lookup"

// Register adds the weather.lookup tool to cat. It returns the error
// from inproc.RegisterFunc unchanged so callers fail loudly on a
// duplicate name or a schema-derivation bug (CLAUDE.md §5).
//
// A real deployment would register tools at boot from cmd/harbor; an
// example or a test calls Register against a tools.NewCatalog().
func Register(cat tools.ToolCatalog) error {
	if err := inproc.RegisterFunc[LookupArgs, LookupResult](
		cat,
		ToolName,
		lookup,
		tools.WithDescription("Look up current weather conditions by city name."),
		tools.WithTags("example", "weather"),
		tools.WithSideEffect(tools.SideEffectExternal),
	); err != nil {
		return fmt.Errorf("register %s: %w", ToolName, err)
	}
	return nil
}

// lookup is the tool body. It is safe to invoke concurrently (D-025) —
// it holds no mutable state and reads only from its arguments.
func lookup(ctx context.Context, in LookupArgs) (LookupResult, error) {
	if err := ctx.Err(); err != nil {
		return LookupResult{}, fmt.Errorf("weather.lookup: %w", err)
	}
	if in.City == "" {
		return LookupResult{}, fmt.Errorf("weather.lookup: city is required")
	}
	// A real tool would call a weather provider here. The canned
	// result keeps the example deterministic and network-free.
	return LookupResult{
		City:    in.City,
		TempC:   21.0,
		Summary: "clear skies",
	}, nil
}
