package planner_test

import (
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// Compile-time assertion that the promoted `tools.PlannerView`
// satisfies `planner.ToolCatalogView` STRUCTURALLY (Phase 110a —
// D-194). The assertion lives HERE — not in `internal/tools` — because
// `internal/planner` imports `internal/tools` (ToolCatalogView's
// methods return tools.Tool), so the tools package cannot name the
// interface without an import cycle.
var _ planner.ToolCatalogView = tools.PlannerView{}
