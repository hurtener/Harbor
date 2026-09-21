package llm

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// ContextHistory is a detached runtime-owned trajectory coordinate snapshot.
// ReplayEnd is exclusive. It describes selected runtime history, not proof of
// provider receipt, inspection, or unchanged formatting by a custom planner.
// No narrative, source identifier, digest or tool content is included.
type ContextHistory struct {
	CheckpointVersion    int
	CheckpointGeneration uint64
	ReplayStart          int
	ReplayEnd            int
	UnseenFrom           int
	UnseenKnown          bool
}

// ContextPreparedPayload reports one structurally valid, materialized request
// at the mandatory leaf capacity check, including capacity rejection. Requests
// rejected earlier (identity, authorization, malformed input or a content leak)
// do not emit this event. Ready-for-capacity is not a provider-success receipt.
// Fields are fixed-size counts/flags/coordinates only. Arbitrary request model
// strings, prompt bytes, call IDs, nonces, extras and errors are never copied.
type ContextPreparedPayload struct {
	events.SafeSealed
	Identity            identity.Quadruple
	OccurredAt          time.Time
	Sections            RequestTokenSections
	EstimatedTokens     int
	ContextWindowTokens int
	InputLimitExclusive int
	OutputReserved      int
	OutputLimitKnown    bool
	CapacityExceeded    bool
	MessageCount        int
	ToolCount           int
	WorkingInputTarget  int
	PlannerStep         int
	Attempt             int
	Retry               int
	Downgrade           int
	FallbackHop         int
	AttemptKnown        bool
	MaintenanceOrdinal  int
	History             *ContextHistory `json:",omitempty"`
}

func emitContextPrepared(ctx context.Context, bus events.EventBus, id identity.Quadruple, req CompleteRequest, profile ModelProfile, sections RequestTokenSections, limit, output int) {
	if bus == nil {
		return
	}
	p := ContextPreparedPayload{
		Identity: id, OccurredAt: time.Now(), Sections: sections,
		EstimatedTokens: sections.Total(), ContextWindowTokens: profile.ContextWindowTokens,
		InputLimitExclusive: limit, OutputReserved: output,
		OutputLimitKnown: req.MaxTokens != nil || profile.DefaultMaxTokens != nil,
		CapacityExceeded: sections.Total() >= limit,
		MessageCount:     len(req.Messages), ToolCount: len(req.Tools),
	}
	if ordinal, ok := ctx.Value(compactionAttemptKey{}).(int); ok {
		p.MaintenanceOrdinal = ordinal
	}
	if step, ok := attemptStepFrom(ctx); ok {
		p.PlannerStep = step
	}
	if scope, ok := AttemptScopeFrom(ctx); ok && scope != nil {
		p.AttemptKnown = true
		if scope.PlannerStep > 0 {
			p.PlannerStep = scope.PlannerStep
		}
		p.Attempt, p.Retry, p.Downgrade, p.FallbackHop = scope.Attempt, scope.Retry, scope.Downgrade, scope.FallbackHop
	}
	// A maintenance completion has its own input and no parent replay range.
	if preparation, ok := ctx.Value(contextPreparationKey{}).(ContextPreparation); ok && p.MaintenanceOrdinal == 0 {
		p.WorkingInputTarget = preparation.InputTarget
		if preparation.History != nil {
			if history := preparation.History(); history != nil {
				copy := *history
				p.History = &copy
			}
		}
	}
	events.PublishAsyncObserved(ctx, bus, events.Event{
		Type: EventTypeContextPrepared, Identity: id, OccurredAt: p.OccurredAt, Payload: p,
	})
}
