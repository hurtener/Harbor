// cmd/harbor/cmd_inspect_runs.go — `harbor inspect-runs` (Phase 69,
// D-101).
//
// Two modes:
//
//   - `harbor inspect-runs` (no arg): list recent runs visible on the
//     connected Runtime by replaying the Phase 60 SSE stream from
//     cursor=0 (or --since), aggregating task.spawned /
//     task.completed / task.failed / task.cancelled into one row per
//     Run.
//
//   - `harbor inspect-runs <run-id>`: replay events filtered to a
//     single run id and present them as a typed trajectory (one
//     event per step, in arrival order).
//
// Why SSE replay rather than a new `runs.list` / `runs.trajectory`
// Protocol method (D-101): both queries are derived projections over
// the canonical event stream the Console / third-party Protocol
// clients are ALREADY consuming. Inventing a new method would
// duplicate that surface and add a primitive the Console does not yet
// have a consumer for — the §13 primitive-with-consumer rule reads
// backwards: don't ship a primitive without a consumer in the same
// wave. The CLI is the consumer; the SSE stream is the primitive
// Phase 60 already shipped. When Phase 72+ (Console subscription
// protocol surface) ships richer per-run query methods, the CLI will
// migrate to them — but no sooner.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/spf13/cobra"
)

// newInspectRunsCmd builds the `inspect-runs` cobra subcommand.
func newInspectRunsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect-runs [run-id]",
		Short: "list recent runs and inspect a run's trajectory",
		Long: `List recent runs visible on the connected Harbor Runtime, or
inspect a single run's trajectory.

Modes:

  harbor inspect-runs                       list recent runs in the session
  harbor inspect-runs <run-id>              show one run's event trajectory

Both modes consume the Phase 60 SSE event stream over /v1/events.
The list-mode summary aggregates task.* events keyed by Run ID; the
single-run mode replays the events whose identity.run matches.

Identity (--tenant / --user / --session) is mandatory — the Runtime
rejects requests with an incomplete scope (CLAUDE.md §6). The Bearer
JWT is read from HARBOR_TOKEN (preferred) or ~/.harbor/token.

Examples:
  harbor inspect-runs \\
    --tenant dev --user dev --session dev

  harbor inspect-runs r-abc123 \\
    --tenant dev --user dev --session dev --json
`,
		Args: cobra.MaximumNArgs(1),
		RunE: runInspectRuns,
	}
	cmd.Flags().String(flagBind, DefaultBind, "Runtime bind (host:port or full URL)")
	cmd.Flags().String(flagTenant, "", "tenant id (required)")
	cmd.Flags().String(flagUser, "", "user id (required)")
	cmd.Flags().String(flagSession, "", "session id (required)")
	cmd.Flags().String(flagSince, "0", "replay cursor (default \"0\" — start of retained window)")
	return cmd
}

// runInspectRuns is the cobra RunE entry. It selects list or trajectory
// mode based on positional args, opens a snapshot SSE stream against
// /v1/events with replay-from-cursor, and renders the result.
func runInspectRuns(cmd *cobra.Command, args []string) error {
	// Every flag below is statically registered on this command, so the
	// GetX lookups cannot fail; the blank-error discards are intentional.
	bind, _ := cmd.Flags().GetString(flagBind) //nolint:errcheck // flag statically registered; lookup cannot fail
	jsonMode := resolveJSONMode(cmd)

	filter := inspectFilter{}
	filter.Tenant, _ = cmd.Flags().GetString(flagTenant) //nolint:errcheck // flag statically registered; lookup cannot fail
	filter.User, _ = cmd.Flags().GetString(flagUser)     //nolint:errcheck // flag statically registered; lookup cannot fail
	filter.Sess, _ = cmd.Flags().GetString(flagSession)  //nolint:errcheck // flag statically registered; lookup cannot fail
	filter.Since, _ = cmd.Flags().GetString(flagSince)   //nolint:errcheck // flag statically registered; lookup cannot fail
	// NOTE: we deliberately do NOT set filter.Run from the positional
	// arg. The SSE handler filters server-side by Event.Identity.RunID,
	// which is empty on `task.spawned` (the `start` Protocol method
	// dispatches Quadruple{Identity: id} — RunID is set later by the
	// per-task RunLoop driver). Setting X-Harbor-Run server-side would
	// drop the spawn event from the stream and break list/trajectory
	// modes. The CLI filters client-side via runIDFromEvent().
	//
	// Tracked as a follow-up to extend the Phase 60 stream's run-filter
	// to fall back to payload TaskID for task.spawned events.

	if err := filter.validate(); err != nil {
		return emitCLIError(cmd, asCLIErrorOr(err, "inspect-runs"))
	}

	endpoint, err := inspectEndpoint(bind)
	if err != nil {
		return emitCLIError(cmd, asCLIErrorOr(err, "inspect-runs"))
	}

	auth, err := resolveTokenFromOS()
	if err != nil {
		return emitCLIError(cmd, asCLIErrorOr(err, "inspect-runs"))
	}

	var targetRun string
	if len(args) == 1 {
		targetRun = args[0]
	}
	opts := inspectRunsOpts{
		Endpoint:   endpoint,
		Filter:     filter,
		Auth:       auth,
		JSON:       jsonMode,
		Client:     defaultInspectClient(),
		IdleCutoff: snapshotIdleTimeout,
		TargetRun:  targetRun,
	}
	if opts.TargetRun != "" {
		return runInspectRunsTrajectory(cmd.Context(), cmd.OutOrStdout(), opts,
			func(cli CLIError) error { return emitCLIError(cmd, cli) })
	}
	return runInspectRunsList(cmd.Context(), cmd.OutOrStdout(), opts,
		func(cli CLIError) error { return emitCLIError(cmd, cli) })
}

// inspectRunsOpts bundles inputs runInspectRuns{List,Trajectory}
// consume. Same testable-core pattern as inspectEventsOpts.
type inspectRunsOpts struct {
	Endpoint   string
	Filter     inspectFilter
	Auth       inspectAuth
	JSON       bool
	Client     *http.Client
	IdleCutoff time.Duration
	TargetRun  string // empty = list mode
}

// runIDFromEvent extracts the run identifier from a wireEvent. The
// per-task RunLoop driver sets `Event.Identity.RunID` to the TaskID
// once the task starts running — but the `task.spawned` event that
// FIRST attaches a task to a session is emitted from the Spawn path
// with an EMPTY RunID (the `start` Protocol method dispatches
// `Quadruple{Identity: id}`). For task.spawned we fall back to the
// payload's `TaskID` field, which the inprocess driver populates on
// every spawn. Other task.* events that DO carry RunID on the
// identity tuple win immediately. Returns "" when no run identifier
// can be derived.
//
// This is the same "where does the run id live?" projection logic
// the Console will perform in its Live Runtime / Sessions pages once
// they're built (RFC §7) — exporting the projection here documents
// the contract so the future Console phase can borrow the shape.
func runIDFromEvent(ev wireEvent) string {
	if ev.Run != "" {
		return ev.Run
	}
	if m, ok := ev.Payload.(map[string]any); ok {
		if id, ok := m["TaskID"].(string); ok && id != "" {
			return id
		}
		if id, ok := m["task_id"].(string); ok && id != "" {
			return id
		}
	}
	return ""
}

// runSummary is the projection one row of the list-mode table renders
// from. Populated by replaying events with a matching identity triple
// and aggregating by RunID.
//
// The shape is deliberately FLAT — it is the wire shape downstream
// consumers (e.g. a Console "Sessions" page that future-shells through
// the CLI) will rely on. Adding a field is additive; changing a field
// name breaks the wire contract.
type runSummary struct {
	RunID       string `json:"run_id"`
	Status      string `json:"status"`
	StartedAt   string `json:"started_at"`
	LastEventAt string `json:"last_event_at"`
	EventCount  int    `json:"event_count"`
	// FailureCode is non-empty when Status == "failed"; surfaced for
	// quick triage so an operator does not have to drill into
	// inspect-runs <id> for the common case.
	FailureCode string `json:"failure_code,omitempty"`
}

// runInspectRunsList replays the SSE stream from --since (default 0,
// i.e. the start of the retained window), groups by RunID, and emits
// one row per run. Renders as a human table or as a JSON array
// (`--json`).
func runInspectRunsList(
	ctx context.Context,
	out io.Writer,
	opts inspectRunsOpts,
	emit func(CLIError) error,
) error {
	streamCtx := ctx
	if opts.IdleCutoff > 0 {
		var cancel context.CancelFunc
		streamCtx, cancel = context.WithTimeout(ctx, opts.IdleCutoff)
		defer cancel()
	}

	runs := map[string]*runSummary{}
	err := inspectSSE(streamCtx, opts.Client, opts.Endpoint, opts.Filter, opts.Auth, func(frame sseFrame) (bool, error) {
		if frame.Data == "" {
			return false, nil
		}
		var ev wireEvent
		if dErr := json.Unmarshal([]byte(frame.Data), &ev); dErr != nil {
			// A malformed frame is a Runtime bug, not a CLI bug; surface
			// it and keep aggregating rather than tearing down the tail.
			fmt.Fprintf(out, "# decode error: %v\n", dErr)
			return false, nil
		}
		runID := runIDFromEvent(ev)
		if runID == "" {
			return false, nil // not associable with a run
		}
		s, ok := runs[runID]
		if !ok {
			s = &runSummary{RunID: runID, Status: "running"}
			runs[runID] = s
		}
		s.EventCount++
		if s.StartedAt == "" {
			s.StartedAt = ev.OccurredAt
		}
		s.LastEventAt = ev.OccurredAt
		switch ev.Type {
		case "task.spawned":
			// Already running by default — no-op.
		case "task.completed":
			s.Status = "completed"
		case "task.failed":
			s.Status = "failed"
			if m, mok := ev.Payload.(map[string]any); mok {
				if code, cOK := m["ErrorCode"].(string); cOK {
					s.FailureCode = code
				} else if code, cOK := m["error_code"].(string); cOK {
					s.FailureCode = code
				}
			}
		case "task.cancelled":
			s.Status = "cancelled"
		case "task.paused":
			s.Status = "paused"
		case "task.resumed":
			s.Status = "running"
		}
		return false, nil
	})
	if err != nil {
		var cli CLIError
		if errors.As(err, &cli) {
			cli.Subcommand = "inspect-runs"
			return emit(cli)
		}
		return emit(CLIError{
			Subcommand: "inspect-runs",
			Code:       CodeStreamFailed,
			Message:    err.Error(),
		})
	}

	sorted := make([]*runSummary, 0, len(runs))
	for _, r := range runs {
		sorted = append(sorted, r)
	}
	// Stable order: by StartedAt then RunID — golden-friendly.
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].StartedAt == sorted[j].StartedAt {
			return sorted[i].RunID < sorted[j].RunID
		}
		return sorted[i].StartedAt < sorted[j].StartedAt
	})

	if opts.JSON {
		buf, mErr := json.Marshal(sorted)
		if mErr != nil {
			return emit(CLIError{
				Subcommand: "inspect-runs",
				Code:       CodeStreamFailed,
				Message:    fmt.Sprintf("marshal runs list: %v", mErr),
			})
		}
		if _, wErr := out.Write(append(buf, '\n')); wErr != nil {
			return emit(CLIError{
				Subcommand: "inspect-runs",
				Code:       CodeStreamFailed,
				Message:    fmt.Sprintf("write runs list: %v", wErr),
			})
		}
		return nil
	}

	if len(sorted) == 0 {
		fmt.Fprintln(out, "no runs visible in the replay window")
		return nil
	}
	fmt.Fprintln(out, "RUN_ID                 STATUS     EVENTS  STARTED_AT                          LAST_EVENT_AT                       FAILURE")
	for _, r := range sorted {
		fc := r.FailureCode
		if fc == "" {
			fc = "-"
		}
		fmt.Fprintf(out, "%-22s %-10s %-7d %-35s %-35s %s\n",
			abbreviate(r.RunID, 22),
			r.Status,
			r.EventCount,
			r.StartedAt,
			r.LastEventAt,
			fc,
		)
	}
	return nil
}

// trajectoryStep is one row of the trajectory rendering. The shape
// follows wireEvent but flattens / strips identity (the request is
// already scoped to one run) so the output is tighter.
type trajectoryStep struct {
	Sequence   uint64 `json:"sequence"`
	OccurredAt string `json:"occurred_at"`
	Type       string `json:"type"`
	Payload    any    `json:"payload,omitempty"`
}

// runInspectRunsTrajectory replays the SSE stream filtered to one run
// and renders the trajectory. Returns CodeRunNotFound when the replay
// drained without yielding a single event with the target Run.
func runInspectRunsTrajectory(
	ctx context.Context,
	out io.Writer,
	opts inspectRunsOpts,
	emit func(CLIError) error,
) error {
	streamCtx := ctx
	if opts.IdleCutoff > 0 {
		var cancel context.CancelFunc
		streamCtx, cancel = context.WithTimeout(ctx, opts.IdleCutoff)
		defer cancel()
	}

	var steps []trajectoryStep
	err := inspectSSE(streamCtx, opts.Client, opts.Endpoint, opts.Filter, opts.Auth, func(frame sseFrame) (bool, error) {
		if frame.Data == "" {
			return false, nil
		}
		var ev wireEvent
		if dErr := json.Unmarshal([]byte(frame.Data), &ev); dErr != nil {
			// A malformed frame is a Runtime bug, not a CLI bug; surface
			// it and keep aggregating rather than tearing down the tail.
			fmt.Fprintf(out, "# decode error: %v\n", dErr)
			return false, nil
		}
		// Defensive: filter.applyHeaders set X-Harbor-Run, but the
		// task.spawned event landing the Run on the bus carries the
		// TaskID in the payload — not on Event.Identity.Run (the
		// `start` Protocol method spawns with RunID=""). We accept
		// EITHER source as the run identifier.
		if runIDFromEvent(ev) != opts.TargetRun {
			return false, nil
		}
		steps = append(steps, trajectoryStep{
			Sequence:   ev.Sequence,
			OccurredAt: ev.OccurredAt,
			Type:       ev.Type,
			Payload:    ev.Payload,
		})
		return false, nil
	})
	if err != nil {
		var cli CLIError
		if errors.As(err, &cli) {
			cli.Subcommand = "inspect-runs"
			return emit(cli)
		}
		return emit(CLIError{
			Subcommand: "inspect-runs",
			Code:       CodeStreamFailed,
			Message:    err.Error(),
		})
	}

	if len(steps) == 0 {
		return emit(CLIError{
			Subcommand: "inspect-runs",
			Code:       CodeRunNotFound,
			Message:    fmt.Sprintf("no events found for run %q in the replay window", opts.TargetRun),
			Hint:       "check the run id; if the run completed before the retained window started, the events have been GC'd",
		})
	}

	// Stable order by sequence — replay arrives in publication order
	// but the assertion makes the golden deterministic against any
	// future bus that re-orders.
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Sequence < steps[j].Sequence })

	if opts.JSON {
		payload := struct {
			RunID string           `json:"run_id"`
			Steps []trajectoryStep `json:"steps"`
		}{RunID: opts.TargetRun, Steps: steps}
		buf, mErr := json.Marshal(payload)
		if mErr != nil {
			return emit(CLIError{
				Subcommand: "inspect-runs",
				Code:       CodeStreamFailed,
				Message:    fmt.Sprintf("marshal trajectory: %v", mErr),
			})
		}
		if _, wErr := out.Write(append(buf, '\n')); wErr != nil {
			return emit(CLIError{
				Subcommand: "inspect-runs",
				Code:       CodeStreamFailed,
				Message:    fmt.Sprintf("write trajectory: %v", wErr),
			})
		}
		return nil
	}

	fmt.Fprintf(out, "run %s — %d events\n", opts.TargetRun, len(steps))
	fmt.Fprintln(out, "SEQ    OCCURRED_AT                         TYPE                              PAYLOAD")
	for _, s := range steps {
		fmt.Fprintf(out, "%-6d %-35s %-33s %s\n",
			s.Sequence,
			s.OccurredAt,
			s.Type,
			payloadSketch(s.Payload),
		)
	}
	return nil
}
