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
// brief 11 §CC-4's high-cardinality runtime-side posture. A cross-tenant
// fan-in is gated by the Service (admin scope, D-079); the projector
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
// # Concurrent reuse (D-025)
//
// RegistryProjector is immutable after NewRegistryProjector: it holds
// the registry + enricher references. The registry is itself D-025-safe;
// the projector adds no mutable state.
type RegistryProjector struct {
	registry tasks.TaskRegistry
	enricher Enricher
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

// NewRegistryProjector builds the V1 production Projector over a
// `tasks.TaskRegistry`. The registry is mandatory — a nil fails loud
// with ErrMisconfigured. The returned *RegistryProjector is D-025-safe.
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
	// Phase 73h (D-128): build a task → TaskGroup reverse index so the
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
		if gid, ok := groupOf[task.ID]; ok {
			row.GroupID = string(gid)
		}
		rows = append(rows, row)
	}
	return rows, nil
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
	if p.enricher != nil {
		detail.ParentSession = p.enricher.ParentSession(ctx, id, taskID)
		detail.Cost = p.enricher.Cost(ctx, id, taskID)
		detail.PlannerSnapshot = p.enricher.PlannerSnapshot(ctx, id, taskID)
	} else {
		// No enricher — the parent-session card carries the session ID
		// the task runs within (always known from the task identity);
		// the cost rollup + planner snapshot stay zero-valued.
		detail.ParentSession = prototypes.TaskParentSessionRef{
			SessionID: task.Identity.SessionID,
		}
	}
	if task.Result != nil && len(task.Result.Value) > 0 {
		detail.ResultInline = string(task.Result.Value)
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
		// Phase 73h (D-128): IsBackground mirrors Kind so a Console
		// row-renderer (the Background Jobs queue) branches without
		// re-comparing the enum. LastActivityAt defaults to UpdatedAt —
		// the registry record carries no separate event timestamp; a
		// future Enricher seam can advance it from the run's event
		// stream without reshaping this projection.
		IsBackground:   kind == prototypes.TaskKindBackground,
		LastActivityAt: updated,
		// Phase 83m item 7: the registry-side ToolCount counter is the
		// running count of tool dispatches the runloop has performed
		// against this task; mirrored to the wire so the Console Tasks
		// page renders the count without subscribing to the per-tool
		// event stream.
		ToolCount: t.ToolCount,
	}
	if t.ParentTaskID != nil {
		row.ParentTaskID = string(*t.ParentTaskID)
	}
	if t.Status == tasks.StatusFailed && t.Error != nil {
		row.ErrorClass = t.Error.Code
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
