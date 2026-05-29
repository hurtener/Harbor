package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/planner/repair"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// ErrDeclarativeActionMissingTool is the sentinel a declarative_action
// invocation returns when the operator-supplied envelope omits the
// `tool` discriminator. The LLM sees the structured error observation
// and corrects on the next turn.
var ErrDeclarativeActionMissingTool = errors.New("declarative_action: envelope missing `tool` discriminator")

// ErrDeclarativeActionReservedName is the sentinel a declarative_action
// invocation returns when the LLM tried to use the escape-hatch tool
// to invoke a planner-reserved name (`_finish` / `_spawn_task` /
// `_await_task`). These names are NOT in the catalog — they are
// planner-level primitives the projector handles natively. Emitting
// them through `declarative_action` is a model-side mistake; the
// planner surfaces the error observation + escalates `FinishRepair`
// (for `_finish`) so the next turn's prompt nudges the LLM toward the
// right channel.
var ErrDeclarativeActionReservedName = errors.New("declarative_action: planner-reserved tool name (use the native call shape instead)")

// declarativeActionFinishToolName / SpawnTaskToolName / AwaitTaskToolName
// mirror the React planner's reserved-name constants. Duplicated here
// (not imported from `internal/planner/react`) to avoid an
// `internal/tools/builtin` → `internal/planner/react` import edge that
// would create a cycle. The CLAUDE.md §13 import-graph contract keeps
// the planner subtree free of `internal/tools/builtin` references; the
// reverse is also clean. A drift test in `declarative_action_test.go`
// pins the constants against the React-side authoritative values.
const (
	declarativeActionFinishToolName = "_finish"
	declarativeActionSpawnToolName  = "_spawn_task"
	declarativeActionAwaitToolName  = "_await_task"
)

func registerDeclarativeAction(cat tools.ToolCatalog) error {
	return inproc.RegisterFunc[DeclarativeActionArgs, DeclarativeActionOut](
		cat, "declarative_action",
		func(ctx context.Context, args DeclarativeActionArgs) (DeclarativeActionOut, error) {
			return declarativeAction(ctx, cat, args)
		},
		tools.WithDescription(
			"Escape-hatch: dispatch any catalog tool by name + JSON args. "+
				"Use ONLY when native tool-calling is unavailable (weaker LLMs). "+
				"Provider-native tool calls remain the primary path; this exists "+
				"so operators with prompt-engineered-tool-calling models stay "+
				"functional.",
		),
		tools.WithSideEffect(tools.SideEffectStateful),
		tools.WithLoading(tools.LoadingDeferred),
		tools.WithTags("builtin", "meta", "escape_hatch"),
	)
}

// DeclarativeActionArgs is the meta-tool's input envelope. Two
// canonical shapes the LLM can emit:
//
//   - **Typed**: `{tool: "<catalog name>", args: {...}}`. Direct
//     dispatch — the most common case.
//   - **Salvage**: `{body: "<raw text>"}`. The body is fed through
//     `repair.ActionParser`, which tolerates fenced JSON / prose-
//     wrapped JSON / multi-action arrays. Used by LLMs whose
//     instruction-following produces messier output shapes.
//
// When both are supplied, `Tool`/`Args` win. When neither is supplied,
// the meta-tool returns `ErrDeclarativeActionMissingTool`.
type DeclarativeActionArgs struct {
	// Tool is the catalog name to dispatch. Reserved names (`_finish`,
	// `_spawn_task`, `_await_task`) return ErrDeclarativeActionReservedName
	// — they are planner-level, not catalog entries.
	Tool string `json:"tool,omitempty"`
	// Args is the JSON arguments to pass to Tool. Validated against
	// the tool's args schema before dispatch; an invalid shape returns
	// a structured `repair_outcome.args_repaired=true` observation so
	// the next planner step's prompt escalates ArgsRepair guidance.
	Args json.RawMessage `json:"args,omitempty"`
	// Body is the alternate salvage input: a raw JSON envelope (or
	// array of envelopes) that the `repair.ActionParser` parses. A
	// multi-action array trips MultiAction; a parse failure trips
	// ArgsRepair.
	Body json.RawMessage `json:"body,omitempty"`
}

// DeclarativeActionOut is the meta-tool's structured observation
// shape. The planner walks the trajectory at the start of its next
// step (see `internal/planner/react/declarative_outcomes.go`) and reads
// `RepairOutcome` to drive the per-run RepairCounters — closing the
// across-step repair-escalation loop that the native main path no
// longer touches.
type DeclarativeActionOut struct {
	// Dispatched is true when the inner tool's Invoke returned without
	// error. The planner / LLM treat this as a successful dispatch.
	Dispatched bool `json:"dispatched"`
	// Tool is the resolved inner-tool name (echoed for observability —
	// the trajectory step's Action carries declarative_action, not the
	// inner name, so this field surfaces the actual call target).
	Tool string `json:"tool,omitempty"`
	// Observation is the inner tool's typed result, JSON-encoded. The
	// LLM consumes this as the next turn's tool-result content.
	Observation json.RawMessage `json:"observation,omitempty"`
	// Error is the human-readable error message when Dispatched=false.
	Error string `json:"error,omitempty"`
	// RepairOutcome carries the across-step repair classification the
	// React planner reads on the next step (Phase 107c step 10 — D-167).
	// Nil means "no repair signal" (a clean dispatch resets counters
	// the same way a clean native step does).
	RepairOutcome *DeclarativeRepairOutcome `json:"repair_outcome,omitempty"`
}

// DeclarativeRepairOutcome maps onto the per-run `planner.RepairCounters`
// (Phase 83c — D-145). The React planner reads it at the start of the
// step that follows a declarative_action dispatch and bumps the matching
// counter; on a clean step it stays nil so the planner resets all three
// counters per the existing semantics.
type DeclarativeRepairOutcome struct {
	// ArgsRepaired is true when the inner tool's args failed schema
	// validation OR when the salvage parser could not extract an
	// envelope. Drives planner.RepairCounters.ArgsRepair.
	ArgsRepaired bool `json:"args_repaired,omitempty"`
	// MultiAction is true when the salvage parser returned more than
	// one envelope in a single body. Drives
	// planner.RepairCounters.MultiAction.
	MultiAction bool `json:"multi_action,omitempty"`
	// FinishRepair is true when the LLM tried to invoke a planner-
	// reserved finish marker (`_finish`) through declarative_action.
	// Drives planner.RepairCounters.FinishRepair so the next turn's
	// prompt nudges the LLM toward issuing a content-only terminal.
	FinishRepair bool `json:"finish_repair,omitempty"`
}

// declarativeAction is the meta-tool's real V1.3 body (Phase 107c
// step 10 — AC-13). The flow:
//
//  1. Resolve the action envelope. The typed `Tool`/`Args` shape wins;
//     when only `Body` is supplied, the body is parsed via
//     `repair.ActionParser` (the existing Phase 44 salvage parser).
//     Parse failures and empty results surface
//     ErrDeclarativeActionMissingTool with `repair_outcome.args_repaired=true`.
//  2. Detect reserved names (`_finish` / `_spawn_task` / `_await_task`)
//     and surface ErrDeclarativeActionReservedName. The `_finish` case
//     additionally sets `repair_outcome.finish_repair=true` so the
//     planner escalates FinishRepair guidance on the next turn.
//  3. Detect multi-action emissions (Body parsed to >1 envelope) and
//     surface a structured error with `repair_outcome.multi_action=true`.
//     The meta-tool dispatches AT MOST ONE inner tool per invocation;
//     multi-action through declarative_action is a model-side mistake
//     (the LLM should issue N native tool_calls or N declarative_action
//     calls in one response, not pack them into one body).
//  4. Resolve the inner tool via the catalog. A missing tool returns a
//     structured error (not args-repair — the LLM emitted a bad name,
//     not bad args).
//  5. Validate the args against the tool's schema (when a Validate
//     function is bound). A schema failure surfaces
//     `repair_outcome.args_repaired=true`.
//  6. Invoke the inner tool under the meta-tool's ctx (identity flows
//     through naturally — every meta-tool checks `requireIdentity`).
//     Invoke errors propagate as the meta-tool's Error field with no
//     repair outcome — invocation errors are not a planner-layer
//     repair signal.
//
// Identity is mandatory (§6 rule 9 + D-001): a missing tenant / user /
// session triple returns `ErrIdentityRequired` with NO repair outcome
// (identity failures are policy violations, not LLM-output-format
// failures the planner can repair).
func declarativeAction(ctx context.Context, cat tools.ToolCatalog, args DeclarativeActionArgs) (DeclarativeActionOut, error) {
	if _, err := requireIdentity(ctx); err != nil {
		return DeclarativeActionOut{}, err
	}

	envelope, salvageOutcome, err := resolveEnvelope(args)
	if err != nil {
		//nolint:nilerr // intentional: a malformed envelope is surfaced in-band as a structured repair outcome (Error + RepairOutcome) so the planner repairs on the observation, not as a hard Go error
		return DeclarativeActionOut{
			Dispatched:    false,
			Error:         err.Error(),
			RepairOutcome: salvageOutcome,
		}, nil
	}

	switch envelope.Tool {
	case declarativeActionFinishToolName:
		return DeclarativeActionOut{
			Dispatched: false,
			Tool:       envelope.Tool,
			Error: fmt.Sprintf("%s — issue a tool-free response (Content only, ToolCalls empty) to finish.",
				ErrDeclarativeActionReservedName),
			RepairOutcome: &DeclarativeRepairOutcome{FinishRepair: true},
		}, nil
	case declarativeActionSpawnToolName, declarativeActionAwaitToolName:
		return DeclarativeActionOut{
			Dispatched: false,
			Tool:       envelope.Tool,
			Error: fmt.Sprintf("%s — invoke %q natively (the planner declares it as a built-in native call).",
				ErrDeclarativeActionReservedName, envelope.Tool),
			// No repair outcome — the LLM should re-emit through the
			// native channel; bumping ArgsRepair here would conflate the
			// signal with actual args-shape failures.
		}, nil
	}

	desc, ok := cat.Resolve(envelope.Tool)
	if !ok {
		return DeclarativeActionOut{
			Dispatched: false,
			Tool:       envelope.Tool,
			Error:      fmt.Sprintf("tool %q not found in catalog", envelope.Tool),
			// No repair outcome — name miss is not an args-shape failure;
			// the LLM should consult `tool_search` / `tool_get` instead.
		}, nil
	}

	// Validate the args BEFORE Invoke so a schema failure surfaces
	// as the args-repair signal, not as a generic Invoke error. The
	// inproc driver also validates during the policy shell, but
	// running it here lets us classify the failure precisely.
	if desc.Validate != nil {
		if vErr := desc.Validate(envelope.Args); vErr != nil {
			return DeclarativeActionOut{
				Dispatched:    false,
				Tool:          envelope.Tool,
				Error:         fmt.Sprintf("args validation failed: %v", vErr),
				RepairOutcome: &DeclarativeRepairOutcome{ArgsRepaired: true},
			}, nil
		}
	}

	result, invokeErr := desc.Invoke(ctx, envelope.Args)
	if invokeErr != nil {
		// `tools.ErrToolInvalidArgs` wrapping means the policy shell's
		// args validator rejected the input. Classify as ArgsRepaired so
		// the next planner step's prompt escalates the args-repair
		// guidance — same as the pre-check path above. Drivers without
		// a bound `Validate` function (HTTP / MCP) only surface schema
		// failures via this path.
		out := DeclarativeActionOut{
			Dispatched: false,
			Tool:       envelope.Tool,
			Error:      fmt.Sprintf("invoke %q failed: %v", envelope.Tool, invokeErr),
		}
		if errors.Is(invokeErr, tools.ErrToolInvalidArgs) {
			out.RepairOutcome = &DeclarativeRepairOutcome{ArgsRepaired: true}
		}
		return out, nil
	}

	encoded, encErr := json.Marshal(result.Value)
	if encErr != nil {
		return DeclarativeActionOut{
			Dispatched: true,
			Tool:       envelope.Tool,
			Error:      fmt.Sprintf("invoke succeeded but result not JSON-encodable: %v", encErr),
		}, nil
	}

	return DeclarativeActionOut{
		Dispatched:  true,
		Tool:        envelope.Tool,
		Observation: encoded,
	}, nil
}

// resolveEnvelope produces the single action envelope the meta-tool
// dispatches. Returns one of three outcomes:
//
//   - typed input (`args.Tool != ""`) → return that envelope, no
//     salvage outcome.
//   - salvage input (`args.Body` parses to exactly one envelope) →
//     return the parsed envelope, no salvage outcome.
//   - salvage input parsed to N>1 envelopes → return an error with
//     `repair_outcome.multi_action=true`.
//   - empty input / unparseable Body → return ErrDeclarativeActionMissingTool
//     with `repair_outcome.args_repaired=true`.
func resolveEnvelope(args DeclarativeActionArgs) (repair.ActionEnvelope, *DeclarativeRepairOutcome, error) {
	if args.Tool != "" {
		return repair.ActionEnvelope{Tool: args.Tool, Args: defaultArgs(args.Args)}, nil, nil
	}
	if len(args.Body) > 0 {
		parser := repair.NewParser()
		actions, parseErr := parser.Parse(string(args.Body))
		switch {
		case parseErr != nil:
			return repair.ActionEnvelope{},
				&DeclarativeRepairOutcome{ArgsRepaired: true},
				fmt.Errorf("%w: body did not parse to an envelope: %w", ErrDeclarativeActionMissingTool, parseErr)
		case len(actions) > 1:
			return repair.ActionEnvelope{},
				&DeclarativeRepairOutcome{MultiAction: true},
				fmt.Errorf("declarative_action: body parsed to %d envelopes; dispatch ONE tool per call (multi-action emissions through declarative_action are not supported — emit N native tool_calls instead)", len(actions))
		case len(actions) == 0:
			return repair.ActionEnvelope{},
				&DeclarativeRepairOutcome{ArgsRepaired: true},
				fmt.Errorf("%w: body parsed to zero envelopes", ErrDeclarativeActionMissingTool)
		default:
			a := actions[0]
			return repair.ActionEnvelope{Tool: a.Tool, Args: defaultArgs(a.Args)}, nil, nil
		}
	}
	return repair.ActionEnvelope{},
		&DeclarativeRepairOutcome{ArgsRepaired: true},
		ErrDeclarativeActionMissingTool
}

// defaultArgs canonicalises an empty / nil Args to the JSON empty
// object `{}` so the downstream validator sees a well-shaped input
// rather than `null`. Matches `inproc`'s call-site convention.
func defaultArgs(args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return json.RawMessage("{}")
	}
	return args
}
