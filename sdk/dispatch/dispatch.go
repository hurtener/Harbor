// Package dispatch is the public SDK facade over Harbor's
// internal/runtime/dispatch package — the production ToolExecutor
// that dispatches the planner's CallTool / CallParallel / SpawnTask /
// AwaitTask decisions against the catalog with the D-026
// heavy-result→artifact promotion (RFC §3.6, §6.10; D-204).
// Alias-based re-exports only: no behavior lives here. Truncation
// summary helpers are deliberately private.
package dispatch

import (
	internal "github.com/hurtener/Harbor/internal/runtime/dispatch"
)

// Option customises NewToolExecutor.
type Option = internal.Option

// NewToolExecutor binds the catalog + artifact store + task registry
// into the steering.ToolExecutor the run loop dispatches against.
// Construct once per stack and share across runs (D-025).
var NewToolExecutor = internal.NewToolExecutor

// Constructor options (see internal/runtime/dispatch Option docs).
var (
	// WithHeavyThreshold overrides the heavy-result promotion threshold.
	WithHeavyThreshold = internal.WithHeavyThreshold
	// WithLogger threads a logger into the executor.
	WithLogger = internal.WithLogger
	// WithMaxSpawnDepth caps the SpawnTask recursion depth.
	WithMaxSpawnDepth = internal.WithMaxSpawnDepth
)
