package builtin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// ErrDeclarativeActionNotWired is the explicit fail-loud sentinel a
// caller sees when `declarative_action` is invoked before the
// implementing-agent's React-planner cutover (Phase 107c step 9) wires
// the actual dispatch path. Returning a "dispatched: true" stub here
// would teach the LLM a lie (CLAUDE.md §13 — "test stubs as
// production defaults" forbidden); we fail loudly instead.
var ErrDeclarativeActionNotWired = errors.New(
	"declarative_action: not yet wired — the planner's dispatch path is delivered in Phase 107c step 9. " +
		"For V1.3 pre-step-9 builds, use native tool-calling instead.")

func registerDeclarativeAction(cat tools.ToolCatalog) error {
	return inproc.RegisterFunc[DeclarativeActionArgs, DeclarativeActionOut](
		cat, "declarative_action",
		func(ctx context.Context, args DeclarativeActionArgs) (DeclarativeActionOut, error) {
			return declarativeAction(ctx, cat, args)
		},
		tools.WithDescription("Escape-hatch: call any tool by name + JSON args. Use when native tool-calling is unavailable."),
		tools.WithSideEffect(tools.SideEffectPure),
		tools.WithLoading(tools.LoadingDeferred),
		tools.WithTags("builtin", "meta", "escape_hatch"),
	)
}

type DeclarativeActionArgs struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type DeclarativeActionOut struct {
	Dispatched  bool   `json:"dispatched"`
	Observation string `json:"observation,omitempty"`
	Error       string `json:"error,omitempty"`
}

// declarativeAction is the V1.3 placeholder body. Step 9 of Phase 107c
// (React planner Next() cutover) replaces this with a real dispatch
// through the runtime's tool executor + `repair.ActionParser`. Until
// then, calling this tool returns ErrDeclarativeActionNotWired so the
// planner / operator sees the gap instead of silently believing the
// dispatch happened.
func declarativeAction(_ context.Context, _ tools.ToolCatalog, _ DeclarativeActionArgs) (DeclarativeActionOut, error) {
	return DeclarativeActionOut{}, ErrDeclarativeActionNotWired
}
