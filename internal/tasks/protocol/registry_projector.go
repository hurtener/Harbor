package protocol

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

// RegistryProjector is the V1 production Projector — a read-only
// projection over a `tasks.TaskRegistry`. It maps the runtime-internal
// `tasks.Task` record onto the flat Protocol wire shapes the Console
// Tasks page renders.
//
// # Scope
//
// `tasks.TaskRegistry.List` is session-scoped: it returns the task
// summaries for one `(tenant, user, session)` identity. RegistryProjector
// projects the caller's own session — the realistic V1 surface, matching
// the high-cardinality runtime-side posture. A cross-tenant
// fan-in is gated by the Service (admin scope); the projector
// honours whatever identity the Service passes it. A future
// cross-runtime aggregating projector slots in behind the Projector
// interface without reshaping the Service.
//
// # Enrichment seam
//
// The TaskRegistry record carries lifecycle + identity + parent-task
// data, but NOT the parent-session metadata, the per-step cost rollup,
// or the planner-checkpoint reference. RegistryProjector reads those
// through the optional Enricher interface. When no Enricher is wired,
// `tasks.get` returns conservative zero-valued enrichment cards (an
// empty parent-session ref, a zero cost rollup, a nil planner snapshot)
// so a partial-build Console still renders the detail rather than
// failing — the zeros are honest ("we don't have this data"), not
// silent degradation of a known value.
//
// # Concurrent reuse
//
// RegistryProjector is immutable after NewRegistryProjector: it holds
// the registry + enricher references. The registry is itself safe for concurrent reuse;
// the projector adds no mutable state.
type RegistryProjector struct {
	registry  tasks.TaskRegistry
	enricher  Enricher
	approvals ApprovalChecker
}

// ApprovalChecker is the optional list-time seam RegistryProjector reads
// the per-row `HasPendingApproval` facet from. The task-registry record is
// a lifecycle record — it does NOT itself model "has an open approval
// gate"; that derived signal is owned by the pause/approval registry one
// package over (the tasks lifecycle-record model). Production wiring supplies an implementation
// backed by the pause coordinator scoped to the task's run; tests and
// partial builds run without one.
//
// A projector with no ApprovalChecker wired leaves `HasPendingApproval`
// at its honest false — the projection-completeness gate's Half-B
// prod-wiring test proves production ALWAYS wires it (a forgotten
// `WithApprovalChecker` in mux.go would ship a permanently-false facet, the
// exact never-wired variant the gate closes).
type ApprovalChecker interface {
	// HasPendingApproval reports whether the task's run (scoped to the
	// task's own identity triple + run id) holds at least one open HITL /
	// tool-approval gate. Identity-scoped: session A's open gate never
	// bleeds into session B's row.
	HasPendingApproval(ctx context.Context, taskIdentity identity.Identity, runID string) bool
}

// Enricher is the optional per-task enrichment backend
// RegistryProjector reads parent-session / cost / planner-snapshot data
// through. Production wiring supplies an implementation backed by the
// sessions subsystem + the `llm.cost.recorded` event stream + the
// planner-checkpoint store; tests and partial-builds run without one.
type Enricher interface {
	// ParentSession returns the parent-session reference card for the
	// task. A zero-valued ref is acceptable ("we don't have this data").
	ParentSession(ctx context.Context, id identity.Identity, taskID string) prototypes.TaskParentSessionRef
	// Cost returns the per-task cost rollup aggregated from
	// `llm.cost.recorded` events scoped to the task.
	Cost(ctx context.Context, id identity.Identity, taskID string) prototypes.TaskCostRollup
	// PlannerSnapshot returns the planner-checkpoint reference at task
	// spawn time, or nil when no checkpoint exists.
	PlannerSnapshot(ctx context.Context, id identity.Identity, taskID string) *prototypes.TaskPlannerSnapshotRef
	// Trajectory returns the projected reasoning-trace snapshot for the
	// task, or nil when the trajectory is unavailable (evicted, not
	// captured, or the run-loop has not wired a trajectory source).
	Trajectory(ctx context.Context, id identity.Identity, taskID string) *prototypes.TaskTrajectoryRef
}

// RegistryProjectorOption configures NewRegistryProjector.
type RegistryProjectorOption func(*RegistryProjector)

// WithEnricher wires the per-task enrichment backend. A nil enricher is
// treated as "WithEnricher not supplied" — `tasks.get` returns
// conservative zero-valued enrichment cards.
func WithEnricher(e Enricher) RegistryProjectorOption {
	return func(p *RegistryProjector) {
		if e != nil {
			p.enricher = e
		}
	}
}

// WithApprovalChecker wires the list-time approval-gate seam the
// `HasPendingApproval` facet reads from. A nil checker is treated as
// "WithApprovalChecker not supplied" — the row's `HasPendingApproval` stays
// at its honest false (the gate's Half-B prod-wiring test proves production
// wires it).
func WithApprovalChecker(c ApprovalChecker) RegistryProjectorOption {
	return func(p *RegistryProjector) {
		if c != nil {
			p.approvals = c
		}
	}
}

// NewRegistryProjector builds the V1 production Projector over a
// `tasks.TaskRegistry`. The registry is mandatory — a nil fails loud
// with ErrMisconfigured. The returned *RegistryProjector is safe for concurrent reuse.
func NewRegistryProjector(registry tasks.TaskRegistry, opts ...RegistryProjectorOption) (*RegistryProjector, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: tasks.TaskRegistry is nil", ErrMisconfigured)
	}
	p := &RegistryProjector{registry: registry}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// ListTasks returns every task-row projection visible to id — the
// tasks in id's session, newest-first. The Service applies the facet
// filter + pagination on top.
//
// The TaskRegistry reads the identity triple from the request context
// (CLAUDE.md §6 rule 3 — identity flows through ctx). The projector
// folds the verified identity into the context before every registry
// call so a registry built from an identity-free context (the wire
// handler's `r.Context()` once auth is satisfied) still scopes its
// reads. A folding failure is an incomplete triple — fail loud.
func (p *RegistryProjector) ListTasks(ctx context.Context, id identity.Identity) ([]prototypes.TaskRow, error) {
	idCtx, err := identity.With(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("tasks/protocol: identity scope incomplete: %w", err)
	}
	ctx = idCtx
	summaries, err := p.registry.List(ctx, id, tasks.TaskFilter{})
	if err != nil {
		return nil, fmt.Errorf("tasks/protocol: registry list: %w", err)
	}
	// build a task → TaskGroup reverse index so the
	// projected rows carry their `GroupID`. The Background Jobs page's
	// per-job "Related Sessions" tab issues a `tasks.list?group_id=…`
	// drill-in; the Service-layer filterMatches pass narrows on the
	// `TaskRow.GroupID` this index populates. ListGroups is
	// identity-scoped; a registry without group support returns an
	// empty slice — the index is then empty and every row's GroupID is
	// "" (the honest "not a group member" default), never a silent
	// degradation of a known value.
	groupOf := map[tasks.TaskID]tasks.TaskGroupID{}
	groups, gerr := p.registry.ListGroups(ctx, id, nil)
	if gerr != nil {
		return nil, fmt.Errorf("tasks/protocol: registry list groups: %w", gerr)
	}
	for _, g := range groups {
		for _, member := range g.Members {
			groupOf[member] = g.ID
		}
	}
	rows := make([]prototypes.TaskRow, 0, len(summaries))
	for _, sum := range summaries {
		task, terr := p.registry.Get(ctx, sum.ID)
		if terr != nil {
			// A task that vanished between List and Get (a concurrent
			// terminal GC) is skipped, not fatal — the list is a
			// best-effort snapshot. A genuine error other than
			// not-found is propagated.
			if errors.Is(terr, tasks.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("tasks/protocol: registry get %q: %w", sum.ID, terr)
		}
		row := projectRow(task)
		applyGroup(&row, groupOf, task.ID)
		p.applyApproval(ctx, task, &row)
		rows = append(rows, row)
	}
	return rows, nil
}

// applyGroup overlays the row's `group_id` from the task→group reverse
// index (the conditionally-assigned Background-Jobs facet the `group_id`
// filter narrows on). A task with no group membership keeps the honest ""
// default. The list projector and the projection-completeness probe share
// this one assignment site so the probe exercises the real overlay (a
// regression that drops the overlay is caught).
func applyGroup(row *prototypes.TaskRow, groupOf map[tasks.TaskID]tasks.TaskGroupID, taskID tasks.TaskID) {
	if gid, ok := groupOf[taskID]; ok {
		row.GroupID = string(gid)
	}
}

// applyApproval populates the row's `has_pending_approval` facet from the
// approval seam, scoped to the task's OWN identity + run (never the
// caller's) so an admin fan-in reads each row's real gate state and session
// A's open gate never appears on session B's row. An unwired checker leaves
// the honest false — the projection-completeness gate's Half-B prod-wiring
// test proves production wires it. The list projector and the fleet
// projector share this one assignment site.
func (p *RegistryProjector) applyApproval(ctx context.Context, task *tasks.Task, row *prototypes.TaskRow) {
	if p.approvals == nil {
		return
	}
	row.HasPendingApproval = p.approvals.HasPendingApproval(ctx, task.Identity.Identity, task.Identity.RunID)
}

// GetTask returns the enriched detail for taskID. A task not visible to
// id (genuine absence or a cross-tenant lookup) returns ErrTaskNotFound
// — existence is never revealed across tenants.
func (p *RegistryProjector) GetTask(ctx context.Context, id identity.Identity, taskID string) (prototypes.TaskDetail, error) {
	idCtx, err := identity.With(ctx, id)
	if err != nil {
		return prototypes.TaskDetail{}, fmt.Errorf("tasks/protocol: identity scope incomplete: %w", err)
	}
	ctx = idCtx
	task, err := p.registry.Get(ctx, tasks.TaskID(taskID))
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return prototypes.TaskDetail{}, ErrTaskNotFound
		}
		return prototypes.TaskDetail{}, fmt.Errorf("tasks/protocol: registry get: %w", err)
	}
	// Defence-in-depth: the registry's Get already scopes by the ctx
	// identity; assert the projected row's tenant matches the caller's
	// so a mis-scoped registry can never leak a cross-tenant task.
	if task.Identity.TenantID != id.TenantID {
		return prototypes.TaskDetail{}, ErrTaskNotFound
	}

	detail := prototypes.TaskDetail{
		Task: projectRow(task),
	}
	// The parent-session card always carries the session ID from the task
	// identity — it is the one field that is always known regardless of
	// whether an enricher is wired. The enricher overlays AgentName +
	// Status when it returns them non-zero.
	detail.ParentSession = prototypes.TaskParentSessionRef{
		SessionID: task.Identity.SessionID,
	}
	if p.enricher != nil {
		enriched := p.enricher.ParentSession(ctx, id, taskID)
		if enriched.AgentName != "" {
			detail.ParentSession.AgentName = enriched.AgentName
		}
		if enriched.Status != "" {
			detail.ParentSession.Status = enriched.Status
		}
		if !enriched.StartedAt.IsZero() {
			detail.ParentSession.StartedAt = enriched.StartedAt
		}
		if !enriched.LatestEventAt.IsZero() {
			detail.ParentSession.LatestEventAt = enriched.LatestEventAt
		}
		detail.Cost = p.enricher.Cost(ctx, id, taskID)
		detail.PlannerSnapshot = p.enricher.PlannerSnapshot(ctx, id, taskID)
		detail.Trajectory = p.enricher.Trajectory(ctx, id, taskID)
	}
	// No enricher → cost rollup + planner snapshot stay zero-valued; the
	// parent-session baseline (SessionID from identity) was set above.
	// fix: the TS contract declares cost.per_step as a non-null
	// array (TaskCostStep[]); a Go nil slice JSON-marshals to `null` and
	// the Console null-derefs on `.length`. Normalize to an empty slice
	// so the wire honours the contract — failing loud is the rule
	// (CLAUDE.md §5), and a missing-per-step rollup is honestly "no
	// per-step rows yet," not "null."
	if detail.Cost.PerStep == nil {
		detail.Cost.PerStep = []prototypes.TaskCostStep{}
	}
	if task.Result != nil && len(task.Result.Value) > 0 {
		detail.ResultInline = string(task.Result.Value)
	}
	// surface the operator-attached input
	// artifacts with their per-attachment disposition hints, in spawn
	// order, so a client can verify its `start` hint round-tripped.
	if len(task.InputArtifactIDs) > 0 {
		detail.InputArtifacts = make([]prototypes.TaskInputArtifact, 0, len(task.InputArtifactIDs))
		for _, artID := range task.InputArtifactIDs {
			detail.InputArtifacts = append(detail.InputArtifacts, prototypes.TaskInputArtifact{
				ID:          artID,
				Disposition: task.InputArtifactDispositions[artID],
			})
		}
	}
	return detail, nil
}

// projectRow maps a runtime-internal *tasks.Task onto the flat
// TaskRow wire shape. Time fields convert from the registry's unix-nano
// convention; DurationMS is the elapsed wall-clock from CreatedAt to
// UpdatedAt.
func projectRow(t *tasks.Task) prototypes.TaskRow {
	started := time.Unix(0, t.CreatedAt).UTC()
	updated := time.Unix(0, t.UpdatedAt).UTC()
	kind := projectKind(t.Kind)
	row := prototypes.TaskRow{
		ID:       string(t.ID),
		Kind:     kind,
		Status:   projectStatus(t.Status),
		Priority: t.Priority,
		Identity: prototypes.IdentityScope{
			Tenant:  t.Identity.TenantID,
			User:    t.Identity.UserID,
			Session: t.Identity.SessionID,
		},
		ParentSessionID: t.Identity.SessionID,
		Description:     t.Description,
		Query:           t.Query,
		StartedAt:       started,
		UpdatedAt:       updated,
		DurationMS:      updated.Sub(started).Milliseconds(),
		// IsBackground mirrors Kind so a Console
		// row-renderer (the Background Jobs queue) branches without
		// re-comparing the enum. LastActivityAt defaults to UpdatedAt —
		// the registry record carries no separate event timestamp; a
		// future Enricher seam can advance it from the run's event
		// stream without reshaping this projection.
		IsBackground:   kind == prototypes.TaskKindBackground,
		LastActivityAt: updated,
		// item 7: the registry-side ToolCount counter is the
		// running count of tool dispatches the runloop has performed
		// against this task; mirrored to the wire so the Console Tasks
		// page renders the count without subscribing to the per-tool
		// event stream.
		ToolCount: t.ToolCount,
		// The caller-named agent this task's run executes under. Empty
		// (elided by omitempty) means the caller named none and the run
		// bound the runtime's configured default — "defaulted", not
		// "unknown", since every row without it bound the default by
		// construction.
		AgentID: t.AgentID,
	}
	if t.ParentTaskID != nil {
		row.ParentTaskID = string(*t.ParentTaskID)
	}
	if t.Status == tasks.StatusFailed && t.Error != nil {
		row.ErrorClass = t.Error.Code
	}
	// The task's LATEST durable progress snapshot projects onto the
	// row's `progress` + `tags` wire fields. Nil snapshot → both stay
	// absent (the honest "no progress reported" — the Console renders
	// an indeterminate bar instead of a fabricated 0.0). The projector
	// never fabricates a progress value: only what ReportProgress
	// durably recorded appears on the wire.
	if t.Progress != nil {
		if t.Progress.Fraction != nil {
			f := *t.Progress.Fraction
			row.Progress = &f
		}
		if len(t.Progress.Tags) > 0 {
			row.Tags = append([]string(nil), t.Progress.Tags...)
		}
	}
	return row
}

// projectKind maps the runtime-internal task kind onto the wire enum.
func projectKind(k tasks.TaskKind) prototypes.TaskKind {
	if k == tasks.KindBackground {
		return prototypes.TaskKindBackground
	}
	return prototypes.TaskKindForeground
}

// projectStatus maps the runtime-internal task status onto the wire
// enum. The two FSM vocabularies are kept distinct on purpose — the
// Protocol owns its own wire vocabulary (CLAUDE.md §8).
func projectStatus(s tasks.TaskStatus) prototypes.TaskStatus {
	switch s {
	case tasks.StatusPending:
		return prototypes.TaskStatusPending
	case tasks.StatusRunning:
		return prototypes.TaskStatusRunning
	case tasks.StatusPaused:
		return prototypes.TaskStatusPaused
	case tasks.StatusComplete:
		return prototypes.TaskStatusComplete
	case tasks.StatusFailed:
		return prototypes.TaskStatusFailed
	case tasks.StatusCancelled:
		return prototypes.TaskStatusCancelled
	default:
		return prototypes.TaskStatusPending
	}
}
