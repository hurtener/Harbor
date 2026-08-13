package turns

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/identity"
)

// Projector is the runtime-owned turn-projection core: it validates
// and applies the mutation DTOs (Append / Update / Seal), drives the
// two separately named component channels (AttachReasoning /
// AttachAppRefs), serves the consumer-safe reads (List / Get) and the
// operations-safe read (OpsTurn), and owns the restart / reconcile /
// erasure-fence contracts over a conformance-ready Store.
//
// Concurrent reuse (the mandatory concurrent-reuse contract): a
// constructed *Projector is immutable
// after New — it holds only the Store reference (itself safe for N
// concurrent goroutines per the Store contract) and immutable options.
// Every method's per-call state lives in the call's arguments and
// locals; per-session write serialization is the STORE's job (the
// version checks make concurrent writers safe).
//
// This is NOT Protocol wiring and NOT a driver: it is the domain core
// the future `sessions.turns.list` / `sessions.turns.get` Protocol
// surface and the runtime event→observation mapping will call.
type Projector struct {
	store Store
	clock Clock
	// inlineLimit is the maximum inline-answer byte length
	// (MaxInlineAnswerBytes by default); an inline answer at or above
	// it is refused with ErrContextLeak.
	inlineLimit int
	// activityLimit is the configured inline activity window: it MUST
	// cover the configured per-turn tool-call budget while that budget
	// is at or below the Protocol ceiling, and is capped at the
	// absolute Protocol ceiling MaxActivityRows.
	activityLimit int
	// toolBudget is the runtime's configured per-turn tool-call
	// budget. New fails loud when activityLimit < toolBudget AND
	// toolBudget <= MaxActivityRows; a budget ABOVE the Protocol
	// ceiling is served by the capped inline window plus the named
	// bounded ActivityReader fallback (activityReader) — construction
	// never fails on an over-ceiling budget.
	toolBudget int
	// activityReader is the runtime-wired bounded activity-read
	// fallback (PageActivity delegates to it). Nil when none was wired:
	// an over-budget turn's full activity is then honestly unreadable
	// (the row still carries the More / Dropped lower-bound marker).
	activityReader ActivityReader
	// probe is the runtime's durable erasure authority consulted by
	// Reconcile when the store-local fence may have been lost (an
	// in-memory-backed store after a process restart). Nil means the
	// runtime declared none — an honest availability gap, never a
	// silent claim.
	probe ErasureProbe
}

// ProjectorOption configures New.
type ProjectorOption func(*Projector)

// WithClock injects a controllable clock for deterministic tests.
// Production code uses the real clock.
func WithClock(c Clock) ProjectorOption {
	return func(p *Projector) { p.clock = c }
}

// WithInlineAnswerLimit overrides the inline-answer byte bound
// (default MaxInlineAnswerBytes). A value <= 0 makes New fail loud at
// construction — a broken bound is never silently ignored.
func WithInlineAnswerLimit(n int) ProjectorOption {
	return func(p *Projector) { p.inlineLimit = n }
}

// WithActivityLimit configures the inline activity window of a row
// (default DefaultActivityLimit). New fails loud when the limit is
// below the configured per-turn tool-call budget (WithToolBudget)
// while that budget is at or below the Protocol ceiling — inline
// capacity must cover the budget — or above the absolute Protocol
// ceiling MaxActivityRows. A turn whose actual tool calls exceed the
// window overflows honestly (More + Dropped + Partial) and the full
// activity is read through the named bounded ActivityReader
// (WithActivityReader).
func WithActivityLimit(n int) ProjectorOption {
	return func(p *Projector) { p.activityLimit = n }
}

// WithToolBudget declares the runtime's configured per-turn tool-call
// budget (default DefaultToolBudget). New fails loud when the
// configured inline activity limit (WithActivityLimit) is below it —
// UNLESS the budget exceeds the absolute Protocol ceiling
// MaxActivityRows: then construction NEVER fails (the inline window is
// capped at the ceiling and the row overflows honestly to the named
// bounded ActivityReader fallback).
func WithToolBudget(n int) ProjectorOption {
	return func(p *Projector) { p.toolBudget = n }
}

// WithActivityReader wires the named bounded activity-read fallback
// the row's explicit lower-bound (Activity.More) points at. It is the
// ONLY subresource read the projection exposes (no generic subresource
// framework). Required for over-ceiling tool budgets: when the
// configured per-turn budget exceeds MaxActivityRows the runtime MUST
// wire the reader so an over-budget turn's full activity stays
// readable. PageActivity refuses loudly when none is wired.
func WithActivityReader(r ActivityReader) ProjectorOption {
	return func(p *Projector) { p.activityReader = r }
}

// WithErasureProbe wires the runtime's DURABLE erasure authority the
// projector consults during restart reconciliation (Reconcile). An
// in-memory-backed store loses its store-local fence on restart; the
// probe (wired over the runtime's own erasure cascade — pending ledger
// / tombstone) is what tells "erased" from "never existed" so an
// erased session is never rebuilt from sequence zero merely because
// the in-memory store restarted. Runtimes with a durable erasure
// cascade MUST wire it; a nil probe is an honest availability gap.
func WithErasureProbe(pb ErasureProbe) ProjectorOption {
	return func(p *Projector) { p.probe = pb }
}

// New constructs a Projector over a mandatory Store. A nil store, a
// non-positive inline-answer bound, an inline activity limit outside
// [1, MaxActivityRows], or an inline activity limit below the
// configured tool budget (while that budget is at or below the
// Protocol ceiling) fails loud at construction — never a nil-panicking
// or silently-defaulted projector. A tool budget ABOVE the ceiling is
// NOT a construction failure: the inline window is capped at the
// ceiling, the row overflows honestly, and the full activity is read
// through the wired ActivityReader.
func New(store Store, opts ...ProjectorOption) (*Projector, error) {
	if store == nil {
		return nil, fmt.Errorf("turns: New requires a non-nil Store")
	}
	p := &Projector{
		store:         store,
		clock:         realClock{},
		inlineLimit:   MaxInlineAnswerBytes,
		activityLimit: DefaultActivityLimit,
		toolBudget:    DefaultToolBudget,
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.inlineLimit <= 0 {
		return nil, fmt.Errorf("turns: New requires a positive inline-answer limit, got %d", p.inlineLimit)
	}
	if p.activityLimit < 1 || p.activityLimit > MaxActivityRows {
		return nil, fmt.Errorf("turns: New requires an inline activity limit in [1, %d], got %d", MaxActivityRows, p.activityLimit)
	}
	if p.toolBudget < 0 {
		return nil, fmt.Errorf("turns: New requires a non-negative tool budget, got %d", p.toolBudget)
	}
	if p.toolBudget <= MaxActivityRows && p.activityLimit < p.toolBudget {
		return nil, fmt.Errorf("turns: inline activity limit %d is below the configured per-turn tool budget %d — inline capacity must cover the budget (ceiling %d)",
			p.activityLimit, p.toolBudget, MaxActivityRows)
	}
	return p, nil
}

// ActivityReader returns the wired bounded activity-read fallback
// (nil when none was wired). PageActivity refuses loudly on a nil
// reader.
func (p *Projector) ActivityReader() ActivityReader { return p.activityReader }

// Append creates the mutable row for a root foreground run (mutation
// DTO Append — structurally free of transcript / reasoning /
// App-correlation / pause tokens). The Store mints the immutable
// per-session sequence atomically; an idempotent re-append of an
// existing turn id returns the existing row unchanged (a replay
// no-op). The write is erasure-fenced: an erased session refuses it
// with ErrErasureFenced.
func (p *Projector) Append(ctx context.Context, id identity.Identity, a Append) (TurnRow, error) {
	if err := validateIdentity(id); err != nil {
		return TurnRow{}, err
	}
	if a.TurnID == "" {
		return TurnRow{}, fmt.Errorf("%w: turn id is empty", ErrInvalidInput)
	}
	if strings.Contains(string(a.TurnID), "|") {
		// The turn id rides the opaque page cursor encoding (a "|"
		// separated field); a pipe in the id would make its own cursor
		// undecodable. Fail loud at creation rather than mint a row
		// whose key breaks the paging surface.
		return TurnRow{}, fmt.Errorf("%w: turn id contains the reserved cursor separator '|'", ErrInvalidInput)
	}
	status := a.Status
	if status == "" {
		status = StatusRunning
	}
	if !status.Valid() {
		return TurnRow{}, fmt.Errorf("%w: status %q", ErrInvalidStatus, status)
	}
	if status.Terminal() {
		return TurnRow{}, fmt.Errorf("%w: append cannot create a terminal row — use Seal", ErrInvalidStatus)
	}
	if utf8.RuneCountInString(a.Query) > MaxQueryRunes {
		return TurnRow{}, fmt.Errorf("%w: query exceeds %d runes", ErrInvalidInput, MaxQueryRunes)
	}
	if err := validateActivity(a.Activity, p.activityLimit); err != nil {
		return TurnRow{}, fmt.Errorf("turns: append activity: %w", err)
	}
	inputs, err := normalizeAttachments(a.Inputs)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: append inputs: %w", err)
	}
	outputs, err := normalizeAttachments(a.Outputs)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: append outputs: %w", err)
	}
	pause, err := normalizePause(a.Pause)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: append pause: %w", err)
	}
	bindingSource, err := deriveBindingSource(a.AgentID, a.AgentBindingSource)
	if err != nil {
		return TurnRow{}, err
	}
	taskID := a.TaskID
	if taskID == "" {
		// The runtime did not report the authoritative task id
		// separately: the row key IS the task id (TaskID-derived).
		taskID = string(a.TurnID)
	}

	now := p.clock.Now()
	started := a.StartedAt
	if started.IsZero() {
		started = now
	}
	queryAt := a.QueryAt
	if queryAt.IsZero() {
		queryAt = started
	}
	row := TurnRow{
		TurnID:              a.TurnID,
		TaskID:              taskID,
		RunID:               a.RunID, // empty = unavailable, never equated with TaskID
		SessionID:           id.SessionID,
		Sequence:            0, // minted by the Store
		TieBreaker:          a.TurnID,
		Status:              status,
		Sealed:              false,
		Version:             1,
		LastAppliedEventSeq: a.EventSeq,
		StartedAt:           started,
		UpdatedAt:           started,
		Agent: Agent{
			ID:            a.AgentID,
			Name:          a.AgentName,
			BindingSource: bindingSource,
			Complete:      agentCompleteness(a.AgentID, a.AgentName),
		},
		Query: Query{
			Text:     a.Query,
			At:       queryAt,
			Complete: queryCompleteness(a.Query),
		},
		Answer:    Answer{State: AnswerStateUnavailable, Complete: CompletenessUnavailable},
		Pause:     pause,
		Usage:     Usage{Complete: CompletenessUnavailable},
		Reasoning: Reasoning{Complete: CompletenessUnavailable},
		Inputs:    inputs,
		Outputs:   outputs,
		Apps:      nil,
	}
	row.Activity = clampActivity(assignActivityPositions(a.Activity), p.activityLimit)
	stored, err := p.store.AppendTurnIf(ctx, id, row)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: append %q: %w", a.TurnID, err)
	}
	return stored, nil
}

// Update mutates a MUTABLE row in place (mutation DTO Update —
// structurally free of transcript / reasoning / App-correlation /
// pause tokens). Non-nil components replace the stored component
// wholesale (usage is cumulative totals; activity is the cumulative
// feed). expectedVersion must match the stored row's current Version;
// a concurrent write wins with ErrStaleVersion and the caller reloads
// and retries (a replay of an already-applied update treats it as
// "already applied").
func (p *Projector) Update(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, u Update) (TurnRow, error) {
	if err := validateIdentity(id); err != nil {
		return TurnRow{}, err
	}
	if turnID == "" {
		return TurnRow{}, fmt.Errorf("%w: turn id is empty", ErrInvalidInput)
	}
	if u.Status != "" {
		if !u.Status.Valid() {
			return TurnRow{}, fmt.Errorf("%w: status %q", ErrInvalidStatus, u.Status)
		}
		if u.Status.Terminal() {
			return TurnRow{}, fmt.Errorf("%w: update cannot set a terminal status — use Seal", ErrInvalidStatus)
		}
	}
	if u.Answer != nil {
		if err := validateAnswer(*u.Answer, p.inlineLimit); err != nil {
			return TurnRow{}, fmt.Errorf("turns: update answer: %w", err)
		}
	}
	if u.Usage != nil {
		if err := validateUsage(*u.Usage); err != nil {
			return TurnRow{}, fmt.Errorf("turns: update usage: %w", err)
		}
	}
	if err := validateActivity(u.Activity, p.activityLimit); err != nil {
		return TurnRow{}, fmt.Errorf("turns: update activity: %w", err)
	}
	inputs, err := normalizeAttachments(u.Inputs)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: update inputs: %w", err)
	}
	outputs, err := normalizeAttachments(u.Outputs)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: update outputs: %w", err)
	}
	pause, err := normalizePause(u.Pause)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: update pause: %w", err)
	}

	current, err := p.store.GetTurn(ctx, id, turnID)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: update %q: %w", turnID, err)
	}
	// Monotonic / idempotent event-sequence application: an observation
	// at or below the row's last-applied sequence has ALREADY been
	// applied (a response-loss replay, or an out-of-order feed) and is a
	// NO-OP — returned unchanged, no version bump, no content mutation,
	// and NO version expectation required (a replay never needs a lucky
	// expected version). The row's LastAppliedEventSeq and the
	// accumulated Answer/Reasoning snapshot sequences never regress.
	if u.EventSeq > 0 && u.EventSeq <= current.LastAppliedEventSeq {
		return current, nil
	}
	if current.Sealed {
		return TurnRow{}, fmt.Errorf("%w: %q", ErrTurnSealed, turnID)
	}
	merged := current
	merged.UpdatedAt = p.clock.Now()
	if u.Status != "" {
		merged.Status = u.Status
	}
	if u.Answer != nil {
		ans := *u.Answer
		ans.Complete = answerCompleteness(ans.State) // derive the uniform honesty from the closed union
		// Component/version consistency anchor: the accumulated answer
		// snapshot's Seq must never regress. The effective sequence is
		// the observation's EventSeq when recorded, else the
		// caller-supplied component Seq; when neither is recorded the
		// prior anchor is preserved.
		cand := u.EventSeq
		if cand == 0 {
			cand = ans.Seq
		}
		if cand > 0 && cand < current.Answer.Seq {
			cand = current.Answer.Seq // never regress the accumulated snapshot
		}
		if cand > 0 {
			ans.Seq = cand
		} else {
			ans.Seq = current.Answer.Seq
		}
		merged.Answer = ans
	}
	if u.Usage != nil {
		merged.Usage = *u.Usage
	}
	if u.Activity != nil {
		merged.Activity = clampActivity(assignActivityPositions(u.Activity), p.activityLimit)
	}
	if u.Inputs != nil {
		merged.Inputs = inputs
	}
	if u.Outputs != nil {
		merged.Outputs = outputs
	}
	if u.Pause != nil {
		merged.Pause = pause
	}
	if u.EventSeq > current.LastAppliedEventSeq {
		merged.LastAppliedEventSeq = u.EventSeq // the guard above guarantees > 0 here
	}
	stored, err := p.store.UpdateTurnIf(ctx, id, turnID, expectedVersion, merged)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: update %q: %w", turnID, err)
	}
	return stored, nil
}

// Seal transitions a MUTABLE row to its SEALED terminal form, but
// only once the terminal status's REQUIRED sources are present on the
// current row: a complete seal requires the Answer component in a
// definite state — inline / artifact_ref / empty (ErrSealIncomplete
// naming "answer" otherwise; an evicted or unavailable answer is NOT a
// complete seal); a failed seal requires a non-empty ErrorClass
// ("error_class"). A same-status re-seal of an already-sealed row is
// an idempotent no-op that returns the sealed row (replay-friendly); a
// conflicting re-seal fails with ErrTurnSealed. After a successful
// seal the row is immutable.
func (p *Projector) Seal(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, s Seal) (TurnRow, error) {
	if err := validateIdentity(id); err != nil {
		return TurnRow{}, err
	}
	if turnID == "" {
		return TurnRow{}, fmt.Errorf("%w: turn id is empty", ErrInvalidInput)
	}
	if !s.Status.Valid() {
		return TurnRow{}, fmt.Errorf("%w: status %q", ErrInvalidStatus, s.Status)
	}
	if !s.Status.Terminal() {
		return TurnRow{}, fmt.Errorf("%w: status %q is not terminal", ErrNotTerminal, s.Status)
	}

	current, err := p.store.GetTurn(ctx, id, turnID)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: seal %q: %w", turnID, err)
	}
	// Monotonic / idempotent event-sequence application: a seal
	// observation at or below the row's last-applied sequence was
	// already applied (replay / out-of-order) — return the row
	// unchanged, no version bump, no version expectation required.
	if s.EventSeq > 0 && s.EventSeq <= current.LastAppliedEventSeq {
		return current, nil
	}
	if current.Sealed {
		if current.Status == s.Status {
			// Same-status re-seal: idempotent replay no-op.
			return current, nil
		}
		return TurnRow{}, fmt.Errorf("%w: %q is already sealed as %q", ErrTurnSealed, turnID, current.Status)
	}
	switch s.Status {
	case StatusComplete:
		if !definiteAnswer(current.Answer) {
			return TurnRow{}, fmt.Errorf("%w: answer is %q (state %q)", ErrSealIncomplete, current.Answer.Complete, current.Answer.State)
		}
	case StatusFailed:
		if s.ErrorClass == "" {
			return TurnRow{}, fmt.Errorf("%w: error_class is empty", ErrSealIncomplete)
		}
	}

	now := p.clock.Now()
	finished := s.FinishedAt
	if finished.IsZero() {
		finished = now
	}
	sealed := current
	sealed.Status = s.Status
	sealed.Sealed = true
	sealed.FinishReason = s.FinishReason
	sealed.ErrorClass = s.ErrorClass
	sealed.FinishedAt = finished
	sealed.UpdatedAt = now
	if s.EventSeq > current.LastAppliedEventSeq {
		sealed.LastAppliedEventSeq = s.EventSeq // the guard above guarantees > 0 here
	}
	stored, err := p.store.SealTurnIf(ctx, id, turnID, expectedVersion, sealed)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: seal %q: %w", turnID, err)
	}
	return stored, nil
}

// AttachReasoning attaches the bounded ORDERED reasoning component
// through its separately named channel (NOT the generic ops — the ops
// are structurally unable to contain reasoning). Steps are fed in
// chronological trajectory order with strictly increasing (gap-
// tolerant) indices; the projector retains the first MaxReasoningSteps
// and reports the tail drop as Partial + Dropped. An empty feed marks
// the component Unavailable. Replaces any prior reasoning wholesale
// and stamps the observation's EventSeq on the accumulated snapshot
// (Reasoning.Seq).
func (p *Projector) AttachReasoning(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, r ReasoningInput) (TurnRow, error) {
	if err := validateIdentity(id); err != nil {
		return TurnRow{}, err
	}
	if turnID == "" {
		return TurnRow{}, fmt.Errorf("%w: turn id is empty", ErrInvalidInput)
	}
	if err := validateReasoningSteps(r.Steps); err != nil {
		return TurnRow{}, fmt.Errorf("turns: attach reasoning: %w", err)
	}

	current, err := p.store.GetTurn(ctx, id, turnID)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: attach reasoning %q: %w", turnID, err)
	}
	// Monotonic / idempotent event-sequence application: a reasoning
	// observation at or below the row's last-applied sequence was
	// already applied — no-op, no version bump, no version expectation
	// required. Reasoning.Seq never regresses.
	if r.EventSeq > 0 && r.EventSeq <= current.LastAppliedEventSeq {
		return current, nil
	}
	if current.Sealed {
		return TurnRow{}, fmt.Errorf("%w: %q", ErrTurnSealed, turnID)
	}
	merged := current
	merged.Reasoning = clampReasoning(r.Steps)
	if r.EventSeq > 0 {
		// Component/version consistency anchor: never regress the
		// accumulated reasoning snapshot's Seq.
		if r.EventSeq > current.Reasoning.Seq {
			merged.Reasoning.Seq = r.EventSeq
		} else {
			merged.Reasoning.Seq = current.Reasoning.Seq
		}
		merged.LastAppliedEventSeq = r.EventSeq // the guard above guarantees > 0 here
	} else {
		merged.Reasoning.Seq = current.Reasoning.Seq // no seq recorded: preserve the anchor
	}
	merged.UpdatedAt = p.clock.Now()
	stored, err := p.store.UpdateTurnIf(ctx, id, turnID, expectedVersion, merged)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: attach reasoning %q: %w", turnID, err)
	}
	return stored, nil
}

// AttachAppRefs upserts the turn's ORDERED App reference collection
// through its separately named channel (NOT the generic ops). The
// replacement identity is exactly (EffectiveAgentID, ServerID,
// ResourceURI): a ref whose identity is already on the row replaces it
// IN PLACE (position fixed by the FIRST declaration) with the latest
// correlation metadata (ToolCallID / ToolName / DisplayMode /
// RawHTMLTrusted / Availability); a new identity appends at the end.
// Each ref carries render metadata plus availability; ToolCallID is
// optional correlation metadata for the identity-scoped
// `mcp.apps.tool_context` lazy delivery — never authority. A ref
// missing ServerID or ResourceURI fails loud (a ref that could only
// mount broken is refused, mirroring the console render guard).
func (p *Projector) AttachAppRefs(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, a AppRefInput) (TurnRow, error) {
	if err := validateIdentity(id); err != nil {
		return TurnRow{}, err
	}
	if turnID == "" {
		return TurnRow{}, fmt.Errorf("%w: turn id is empty", ErrInvalidInput)
	}
	// Deep-copy the fed refs: the stored row must never alias the
	// caller's backing array (concurrent reuse — a caller
	// mutating its input must not corrupt the projection).
	refs := make([]AppRef, len(a.Refs))
	for i, ref := range a.Refs {
		normalized, err := normalizeAppRef(ref, i)
		if err != nil {
			return TurnRow{}, err
		}
		refs[i] = normalized
	}

	current, err := p.store.GetTurn(ctx, id, turnID)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: attach app refs %q: %w", turnID, err)
	}
	// Monotonic / idempotent event-sequence application: an App-ref
	// observation at or below the row's last-applied sequence was
	// already applied — no-op, no version bump, no version expectation
	// required.
	if a.EventSeq > 0 && a.EventSeq <= current.LastAppliedEventSeq {
		return current, nil
	}
	if current.Sealed {
		return TurnRow{}, fmt.Errorf("%w: %q", ErrTurnSealed, turnID)
	}
	merged := current
	merged.Apps = upsertAppRefs(merged.Apps, refs)
	if len(merged.Apps) > MaxAppsPerTurn {
		return TurnRow{}, fmt.Errorf("%w: app ref collection exceeds %d entries — Apps are a bounded declaration, never silently dropped",
			ErrInvalidInput, MaxAppsPerTurn)
	}
	if a.EventSeq > current.LastAppliedEventSeq {
		merged.LastAppliedEventSeq = a.EventSeq // the guard above guarantees > 0 here
	}
	merged.UpdatedAt = p.clock.Now()
	stored, err := p.store.UpdateTurnIf(ctx, id, turnID, expectedVersion, merged)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: attach app refs %q: %w", turnID, err)
	}
	return stored, nil
}

// List serves one newest-first keyset page of the consumer-safe
// projection — the backing of the future `sessions.turns.list`. Pages
// are ordered by the immutable keys (Sequence DESC, TurnID DESC) and
// are stable under concurrent appends (no skips, no duplicates; a
// newly appended turn can never satisfy an already-issued cursor). A
// Limit above MaxListLimit fails loudly (ErrInvalidInput) — bounded
// paging, never an accidental dump. The page exposes its snapshot
// as-of (AsOf / Snapshot), the next older cursor, HasMore, the exact
// older-row Remaining when the store knows it (CountExact), the
// explicit completeness / partial reason, and the live-resume sequence
// (LiveResumeSeq) a consumer composes subscribe-before-page with.
// Foreign-session / stale-snapshot / expired cursors are rejected by
// the store with their distinct domain errors.
func (p *Projector) List(ctx context.Context, id identity.Identity, opts ListOptions) (Page, error) {
	if err := validateIdentity(id); err != nil {
		return Page{}, err
	}
	limit := opts.Limit
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 0 || limit > MaxListLimit {
		return Page{}, fmt.Errorf("%w: limit %d outside [1, %d]", ErrInvalidInput, limit, MaxListLimit)
	}
	rows, next, info, err := p.store.ListTurns(ctx, id, opts.Before, limit)
	if err != nil {
		return Page{}, fmt.Errorf("turns: list: %w", err)
	}
	var liveResumeSeq uint64
	for _, row := range rows {
		if row.LastAppliedEventSeq > liveResumeSeq {
			liveResumeSeq = row.LastAppliedEventSeq
		}
	}
	partialReason := ""
	if info.Truncated {
		partialReason = "retention_eviction"
	}
	return Page{
		Rows:          rows,
		NextCursor:    next,
		HasMore:       next != nil,
		AsOf:          p.clock.Now(),
		Snapshot:      info.Snapshot,
		Remaining:     info.Remaining,
		CountExact:    info.CountExact,
		Complete:      !info.Truncated,
		PartialReason: partialReason,
		LiveResumeSeq: liveResumeSeq,
	}, nil
}

// Get serves one consumer-safe row — the backing of the future
// `sessions.turns.get`. ErrTurnNotFound when the projection does not
// retain the turn (never created, evicted past the retention bound, or
// erased — the caller maps it onto the honest wire not-found).
func (p *Projector) Get(ctx context.Context, id identity.Identity, turnID TurnID) (TurnRow, error) {
	if err := validateIdentity(id); err != nil {
		return TurnRow{}, err
	}
	if turnID == "" {
		return TurnRow{}, fmt.Errorf("%w: turn id is empty", ErrInvalidInput)
	}
	row, err := p.store.GetTurn(ctx, id, turnID)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: get %q: %w", turnID, err)
	}
	return row, nil
}

// OpsTurn serves the OPERATIONS-SAFE READ projection of one turn — a
// pure, STRUCTURALLY DISTINCT DTO (OpsTurnRow) that omits the consumer
// fields the operations surface must never see (query, answer,
// reasoning traces, pause tokens, App resource URI / tool_call_id, App
// context/input/result) while retaining lifecycle / agent binding /
// timing / usage / cost / tool-name / status / counts / availability.
// It is the read shape the future operations Protocol lane consumes;
// the mutation DTOs are NOT that surface.
func (p *Projector) OpsTurn(ctx context.Context, id identity.Identity, turnID TurnID) (OpsTurnRow, error) {
	row, err := p.Get(ctx, id, turnID)
	if err != nil {
		return OpsTurnRow{}, err
	}
	return projectOps(row), nil
}

// Checkpoint returns the session's last-applied runtime event
// sequence (0 when none was ever saved — a fresh session, an erased
// session, or an in-memory-backed projection after a restart, which is
// EXPLICIT restart loss, not silent retention).
func (p *Projector) Checkpoint(ctx context.Context, id identity.Identity) (uint64, error) {
	if err := validateIdentity(id); err != nil {
		return 0, err
	}
	seq, err := p.store.LoadCheckpoint(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("turns: checkpoint: %w", err)
	}
	return seq, nil
}

// AdvanceCheckpoint records the session's last-applied runtime event
// sequence. Monotonic and idempotent: a sequence at or below the
// stored checkpoint is a no-op, so a reconcile retry can never rewind
// the checkpoint. Refused with ErrErasureFenced on an erased session —
// a rebuild must never advance an erased session's checkpoint.
func (p *Projector) AdvanceCheckpoint(ctx context.Context, id identity.Identity, seq uint64) error {
	if err := validateIdentity(id); err != nil {
		return fmt.Errorf("turns: advance checkpoint: %w", err)
	}
	if err := p.store.SaveCheckpoint(ctx, id, seq); err != nil {
		return fmt.Errorf("turns: advance checkpoint: %w", err)
	}
	return nil
}

// Reconcile drives a restart-recovery pass: the runtime's `apply`
// closure replays the durable observations with sequence greater than
// the session's checkpoint — re-applying an already-applied
// observation is a no-op (appends are idempotent on the turn id; a
// stale-version update/seal is "already applied") — and returns the
// new high-water sequence. The projector advances the checkpoint
// monotonically ONLY after apply returns, so a failure mid-replay
// leaves the checkpoint at the last success and a retry resumes
// without double-applying the tail. Returns the session's checkpoint
// after the pass (max of the prior checkpoint and the reported high
// water).
//
// Restart contract: after a process restart the runtime calls
// Reconcile with the session's checkpoint as the resume point. For an
// IN-MEMORY-backed store (Durable() == false) the checkpoint reads 0
// and the projection is rebuilt from sequence zero — explicit restart
// loss, never a silent claim of durability. An ERASED session stays
// fenced: the replay's writes fail with ErrErasureFenced, so a replay
// can never resurrect an erased session.
//
// THE ERASURE-PROBE GATE (P6 / G4): an in-memory-backed store loses
// its STORE-LOCAL fence on a process restart — rows, checkpoint, AND
// fence are gone — so the runtime's durable erasure cascade (the
// ErasureProbe wired via WithErasureProbe) is the ONLY authority that
// can still distinguish "this session was erased" from "this session
// never existed". Before ANY replay, Reconcile consults the probe: a
// session the runtime's erasure fence reports erased is NEVER rebuilt
// from sequence zero merely because the in-memory store restarted —
// Reconcile restores the store-local fence and refuses loudly with
// ErrErasureFenced. A nil probe (the runtime declared no durable
// erasure authority) is an HONEST availability gap, never a silent
// claim: an erased session could then be rebuilt, and the runtime is
// responsible for that posture.
func (p *Projector) Reconcile(ctx context.Context, id identity.Identity, apply func(ctx context.Context, id identity.Identity, from uint64) (uint64, error)) (uint64, error) {
	if err := validateIdentity(id); err != nil {
		return 0, err
	}
	if apply == nil {
		return 0, fmt.Errorf("%w: reconcile apply callback is nil", ErrInvalidInput)
	}
	if p.probe != nil {
		erased, err := p.probe.Erased(ctx, id)
		if err != nil {
			return 0, fmt.Errorf("turns: reconcile erasure probe: %w", err)
		}
		if erased {
			// The store-local fence was lost (in-memory-backed restart).
			// Restore it so the projection stays honest for this
			// process lifetime, then refuse the rebuild loudly — an
			// erased session never rebuilds from sequence zero.
			if err := p.store.FenceSession(ctx, id); err != nil {
				return 0, fmt.Errorf("turns: reconcile: erased session refused but store-local fence restore failed: %w", err)
			}
			return 0, fmt.Errorf("%w: session is erased (runtime erasure fence) — rebuild from sequence zero refused", ErrErasureFenced)
		}
	}
	from, err := p.store.LoadCheckpoint(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("turns: reconcile: %w", err)
	}
	high, err := apply(ctx, id, from)
	if err != nil {
		return 0, fmt.Errorf("turns: reconcile: %w", err)
	}
	if high > from {
		if err := p.store.SaveCheckpoint(ctx, id, high); err != nil {
			return 0, fmt.Errorf("turns: reconcile checkpoint: %w", err)
		}
		return high, nil
	}
	return from, nil
}

// Erase removes every retained turn row and the session checkpoint
// under id — the projection leg of the runtime's session-erasure
// cascade. The STORE-LOCAL erasure fence is set FIRST (FenceSession)
// so no write can slip in between the fence and the deletion, and the
// fence is NEVER removed by the erasure: an erased session stays
// fenced across replay and restart (no resurrection), and re-erase is
// idempotent (FenceSession is a no-op on a fenced session;
// DeleteScope returns (0, nil) on an absent scope).
func (p *Projector) Erase(ctx context.Context, id identity.Identity) (int, error) {
	if err := validateIdentity(id); err != nil {
		return 0, err
	}
	if err := p.store.FenceSession(ctx, id); err != nil {
		return 0, fmt.Errorf("turns: erase fence: %w", err)
	}
	n, err := p.store.DeleteScope(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("turns: erase: %w", err)
	}
	return n, nil
}

// Durable reports whether the backing store survives a process
// restart. An in-memory-backed projector reports false — its rows,
// checkpoints, and erasure fences are GONE after a restart (explicit
// loss) and the runtime rebuilds via Reconcile against the durable
// event log, gated on the runtime's durable erasure probe
// (WithErasureProbe) so an erased session is never rebuilt from
// sequence zero merely because the in-memory store restarted.
func (p *Projector) Durable() bool { return p.store.Durable() }

// Close closes the backing store. Subsequent operations fail with
// ErrStoreClosed (wrapped). Idempotent.
func (p *Projector) Close(ctx context.Context) error {
	return p.store.Close(ctx)
}

// validateIdentity maps an identity.Validate failure onto the turns
// package's own mandatory-identity sentinel so every projector method
// fails closed with ErrIdentityRequired (CLAUDE.md §6), regardless of
// which identity-layer error shape fired.
func validateIdentity(id identity.Identity) error {
	if err := identity.Validate(id); err != nil {
		return fmt.Errorf("turns: %w", ErrIdentityRequired)
	}
	return nil
}

// projectOps builds the pure operations-safe READ DTO from a consumer
// row. It is the ONLY place the consumer row is reduced to the
// operations shape; the reduction is structural (the DTO has no fields
// for the omitted content).
func projectOps(row TurnRow) OpsTurnRow {
	out := OpsTurnRow{
		TurnID:              row.TurnID,
		TaskID:              row.TaskID,
		RunID:               row.RunID,
		SessionID:           row.SessionID,
		Sequence:            row.Sequence,
		TieBreaker:          row.TieBreaker,
		Status:              row.Status,
		Sealed:              row.Sealed,
		Version:             row.Version,
		StartedAt:           row.StartedAt,
		UpdatedAt:           row.UpdatedAt,
		FinishedAt:          row.FinishedAt,
		FinishReason:        row.FinishReason,
		ErrorClass:          row.ErrorClass,
		AgentID:             row.Agent.ID,
		AgentName:           row.Agent.Name,
		AgentBindingSource:  row.Agent.BindingSource,
		Usage:               row.Usage,
		Activity:            row.Activity.Rows,
		ReasoningSteps:      len(row.Reasoning.Steps),
		Inputs:              len(row.Inputs),
		Outputs:             len(row.Outputs),
		Pause:               row.Pause,
		LastAppliedEventSeq: row.LastAppliedEventSeq,
	}
	for _, app := range row.Apps {
		out.Apps = append(out.Apps, AppOpsRef{
			EffectiveAgentID: app.EffectiveAgentID,
			ServerID:         app.ServerID,
			ToolName:         app.ToolName,
			Availability:     app.Availability,
		})
	}
	return out
}

// --- validation helpers -------------------------------------------------

func agentCompleteness(id, name string) Completeness {
	if id == "" && name == "" {
		return CompletenessUnavailable
	}
	return CompletenessComplete
}

func queryCompleteness(q string) Completeness {
	if q == "" {
		return CompletenessUnavailable
	}
	return CompletenessComplete
}

// deriveBindingSource resolves the append's honest binding provenance:
// an explicitly reported source is validated and kept; an empty source
// derives explicit from a non-empty agent id and unknown otherwise
// (defaulted must be reported explicitly — the projector never
// invents a defaulted claim).
func deriveBindingSource(agentID string, reported AgentBindingSource) (AgentBindingSource, error) {
	if reported == "" {
		if agentID != "" {
			return AgentBindingExplicit, nil
		}
		return AgentBindingUnknown, nil
	}
	if !reported.Valid() {
		return "", fmt.Errorf("%w: agent binding source %q", ErrInvalidInput, reported)
	}
	return reported, nil
}

// answerCompleteness derives the uniform honesty from the closed union
// answer state: inline / artifact_ref / empty are Complete; evicted /
// unavailable are Unavailable.
func answerCompleteness(s AnswerState) Completeness {
	switch s {
	case AnswerStateInline, AnswerStateArtifactRef, AnswerStateEmpty:
		return CompletenessComplete
	default: // evicted, unavailable
		return CompletenessUnavailable
	}
}

// definiteAnswer reports whether the answer is in a state that
// satisfies a complete seal: inline / artifact_ref / empty. An
// evicted or unavailable answer is NOT definite — the answer component
// is honestly not present.
func definiteAnswer(a Answer) bool {
	switch a.State {
	case AnswerStateInline, AnswerStateArtifactRef, AnswerStateEmpty:
		return true
	}
	return false
}

// validateActivity validates the per-row shape of a fed activity list
// (position ordinal, tool non-empty + bounded, status valid, summary
// bounded) against the configured inline activity window. The window
// clamp (overflow → explicit lower-bound) happens separately in
// clampActivity. Feeds are the CUMULATIVE activity of the turn and may
// legitimately exceed the window by any margin (an over-budget turn,
// or a replay of one) — overflow is the honest Partial/More/Dropped
// contract, never a validation failure; only the tool-name bound and
// per-row shape fail loud.
func validateActivity(fed []ActivityRow, limit int) error {
	for i, row := range fed {
		if row.Tool == "" {
			return fmt.Errorf("%w: activity row %d has an empty tool name", ErrInvalidInput, i)
		}
		if err := validateText(row.Tool, "activity tool", MaxToolNameRunes); err != nil {
			return fmt.Errorf("%w: activity row %d: %v", ErrInvalidInput, i, err)
		}
		status := row.Status
		if status == "" {
			status = ActivityInvoked
		}
		if !status.Valid() {
			return fmt.Errorf("%w: activity row %d status %q", ErrInvalidInput, i, row.Status)
		}
		if utf8.RuneCountInString(row.Summary) > MaxActivitySummaryRunes {
			return fmt.Errorf("%w: activity row %d summary exceeds %d runes", ErrInvalidInput, i, MaxActivitySummaryRunes)
		}
	}
	return nil
}

// assignActivityPositions stamps each fed row's IMMUTABLE position
// ordinal (the feed index). Feeds are cumulative, so a position never
// changes for a row's lifetime and appends only ever add HIGHER
// positions — the keyset key the ActivityReader pages over. The row
// is deep-copied (the stored projection never aliases the caller's
// backing array — concurrent reuse).
func assignActivityPositions(fed []ActivityRow) []ActivityRow {
	out := make([]ActivityRow, len(fed))
	for i, row := range fed {
		row.Position = i
		out[i] = row
	}
	return out
}

// clampActivity retains the LAST `limit` rows of the fed sequence
// (the recent window) and reports the overflow as the explicit
// lower-bound: More + Dropped, with the component Partial. An empty
// feed is a complete empty window (the runtime fed the current
// cumulative list and it has no rows). The retained window is
// deep-copied — it never aliases the caller's backing array.
func clampActivity(fed []ActivityRow, limit int) Activity {
	if len(fed) == 0 {
		return Activity{Complete: CompletenessComplete}
	}
	if len(fed) <= limit {
		return Activity{Rows: append([]ActivityRow(nil), fed...), Complete: CompletenessComplete}
	}
	kept := append([]ActivityRow(nil), fed[len(fed)-limit:]...)
	return Activity{
		Rows:     kept,
		Complete: CompletenessPartial,
		More:     true,
		Dropped:  len(fed) - limit,
	}
}

// validateReasoningSteps validates the ordered step sequence: indices
// non-negative and strictly increasing (gap-tolerant — steps without
// reasoning are not fed), traces bounded. The clamp (overflow → first
// N kept, tail dropped) happens in clampReasoning.
func validateReasoningSteps(steps []ReasoningStep) error {
	if len(steps) > MaxReasoningSteps*4 {
		return fmt.Errorf("%w: reasoning feed exceeds %d steps", ErrInvalidInput, MaxReasoningSteps*4)
	}
	prev := -1
	for i, step := range steps {
		if step.Index < 0 {
			return fmt.Errorf("%w: reasoning step %d has a negative index", ErrInvalidInput, i)
		}
		if step.Index <= prev {
			return fmt.Errorf("%w: reasoning step %d index %d is not strictly increasing", ErrInvalidInput, i, step.Index)
		}
		prev = step.Index
		if utf8.RuneCountInString(step.Trace) > MaxStepTraceRunes {
			return fmt.Errorf("%w: reasoning step %d trace exceeds %d runes", ErrInvalidInput, i, MaxStepTraceRunes)
		}
	}
	return nil
}

// clampReasoning retains the FIRST MaxReasoningSteps of the fed
// sequence (the chronological order) and reports the tail drop as
// Partial + Dropped. An empty feed marks the component Unavailable.
// The retained steps are deep-copied — the stored projection never
// aliases the caller's backing array (concurrent reuse).
func clampReasoning(fed []ReasoningStep) Reasoning {
	if len(fed) == 0 {
		return Reasoning{Complete: CompletenessUnavailable}
	}
	if len(fed) <= MaxReasoningSteps {
		return Reasoning{Steps: append([]ReasoningStep(nil), fed...), Complete: CompletenessComplete}
	}
	return Reasoning{
		Steps:    append([]ReasoningStep(nil), fed[:MaxReasoningSteps]...),
		Complete: CompletenessPartial,
		Dropped:  len(fed) - MaxReasoningSteps,
	}
}

// validateAnswer validates the answer component against the CLOSED
// UNION state contract: exactly the content the state declares, an
// inline answer at or above the inline bound is ErrContextLeak (heavy
// answers MUST route by artifact reference), and the derived Complete
// must stay consistent with State. A failed read is NEVER expressed as
// Empty — it is Evicted or Unavailable.
func validateAnswer(a Answer, inlineLimit int) error {
	if !a.State.Valid() {
		return fmt.Errorf("%w: answer state %q", ErrInvalidInput, a.State)
	}
	switch a.State {
	case AnswerStateInline:
		if a.Ref != nil {
			return fmt.Errorf("%w: inline answer carries a ref", ErrInvalidInput)
		}
		if len([]byte(a.Inline)) >= inlineLimit {
			return fmt.Errorf("%w: inline answer is %d bytes (>= %d) — route by artifact reference",
				ErrContextLeak, len([]byte(a.Inline)), inlineLimit)
		}
	case AnswerStateArtifactRef:
		if a.Ref == nil {
			return fmt.Errorf("%w: artifact_ref answer carries no ref", ErrInvalidInput)
		}
		if a.Inline != "" {
			return fmt.Errorf("%w: artifact_ref answer carries inline text", ErrInvalidInput)
		}
		if a.Ref.ID == "" {
			return fmt.Errorf("%w: answer ref id is empty", ErrInvalidInput)
		}
		if a.Ref.SizeBytes < 0 {
			return fmt.Errorf("%w: answer ref size is negative", ErrInvalidInput)
		}
	case AnswerStateEmpty:
		if a.Inline != "" || a.Ref != nil {
			return fmt.Errorf("%w: empty answer cannot carry content", ErrInvalidInput)
		}
	case AnswerStateEvicted:
		if a.Inline != "" || a.Ref != nil {
			return fmt.Errorf("%w: evicted answer cannot carry content (a failed read never becomes empty)", ErrInvalidInput)
		}
	case AnswerStateUnavailable:
		if a.Inline != "" || a.Ref != nil {
			return fmt.Errorf("%w: unavailable answer cannot carry content", ErrInvalidInput)
		}
	}
	// The derived honesty must stay consistent with the closed union.
	want := answerCompleteness(a.State)
	if a.Complete != "" && a.Complete != want {
		return fmt.Errorf("%w: answer completeness %q is inconsistent with state %q (want %q)",
			ErrInvalidInput, a.Complete, a.State, want)
	}
	return nil
}

// validateUsage validates the cumulative usage rollup shape.
func validateUsage(u Usage) error {
	if !u.Complete.Valid() {
		return fmt.Errorf("%w: usage completeness %q", ErrInvalidInput, u.Complete)
	}
	if u.PromptTokens < 0 || u.CompletionTokens < 0 || u.ReasoningTokens < 0 || u.TotalTokens < 0 {
		return fmt.Errorf("%w: usage token counts must be non-negative", ErrInvalidInput)
	}
	if u.CostUSD < 0 {
		return fmt.Errorf("%w: usage cost must be non-negative", ErrInvalidInput)
	}
	if utf8.RuneCountInString(u.Model) > MaxModelRunes {
		return fmt.Errorf("%w: usage model exceeds %d runes", ErrInvalidInput, MaxModelRunes)
	}
	return nil
}

// normalizeAttachments validates one attachment metadata list (every
// id non-empty, sizes non-negative, count bounded) and normalizes each
// entry's honest reference availability: an unset Availability is
// Unavailable ("no availability was reported"), never a fabricated
// "available" claim. A nil slice passes through as nil.
func normalizeAttachments(atts []Attachment) ([]Attachment, error) {
	if atts == nil {
		return nil, nil
	}
	if len(atts) > MaxAttachmentsPerSide {
		return nil, fmt.Errorf("%w: attachment list exceeds %d entries", ErrInvalidInput, MaxAttachmentsPerSide)
	}
	out := make([]Attachment, len(atts))
	for i, a := range atts {
		if a.ID == "" {
			return nil, fmt.Errorf("%w: attachment %d has an empty id", ErrInvalidInput, i)
		}
		if a.SizeBytes < 0 {
			return nil, fmt.Errorf("%w: attachment %d has a negative size", ErrInvalidInput, i)
		}
		if a.Availability == "" {
			a.Availability = CompletenessUnavailable
		}
		if !a.Availability.Valid() {
			return nil, fmt.Errorf("%w: attachment %d availability %q", ErrInvalidInput, i, a.Availability)
		}
		if a.Availability == CompletenessPartial {
			return nil, fmt.Errorf("%w: attachment %d availability cannot be partial (a reference resolves or it does not)", ErrInvalidInput, i)
		}
		out[i] = a
	}
	return out, nil
}

// normalizePause validates the durable pause component (class /
// reason / lifecycle / availability — never a token). A nil input
// yields the honest Unavailable component. When the component is
// reported (Availability Complete), Class must be set and valid,
// Lifecycle defaults to Active, and Reason stays content-free and
// bounded. Actionability is never stored.
func normalizePause(p *Pause) (Pause, error) {
	if p == nil {
		return Pause{Availability: CompletenessUnavailable}, nil
	}
	out := *p
	if out.Availability == "" {
		out.Availability = CompletenessComplete
	}
	if !out.Availability.Valid() {
		return Pause{}, fmt.Errorf("%w: pause availability %q", ErrInvalidInput, out.Availability)
	}
	if out.Availability == CompletenessPartial {
		return Pause{}, fmt.Errorf("%w: pause availability cannot be partial", ErrInvalidInput)
	}
	if out.Availability == CompletenessUnavailable {
		// Honest "no pause episode recorded": no class/reason/lifecycle
		// may be fabricated alongside it.
		if out.Class != "" || out.Reason != "" || out.Lifecycle != "" {
			return Pause{}, fmt.Errorf("%w: unavailable pause cannot carry class/reason/lifecycle", ErrInvalidInput)
		}
		return out, nil
	}
	if out.Class == "" {
		return Pause{}, fmt.Errorf("%w: pause class is empty", ErrInvalidInput)
	}
	if !out.Class.Valid() {
		return Pause{}, fmt.Errorf("%w: pause class %q", ErrInvalidInput, out.Class)
	}
	if out.Lifecycle == "" {
		out.Lifecycle = PauseLifecycleActive
	}
	if !out.Lifecycle.Valid() {
		return Pause{}, fmt.Errorf("%w: pause lifecycle %q", ErrInvalidInput, out.Lifecycle)
	}
	if utf8.RuneCountInString(out.Reason) > MaxPauseReasonRunes {
		return Pause{}, fmt.Errorf("%w: pause reason exceeds %d runes", ErrInvalidInput, MaxPauseReasonRunes)
	}
	return out, nil
}

// upsertAppRefs applies the ORDERED App-ref upsert semantics: the
// replacement identity is exactly the comparable typed AppRefKey
// (effective agent id, server id, resource uri). A fed ref whose
// identity already exists replaces it IN PLACE (position fixed by the
// first declaration) with the latest correlation metadata; a new
// identity appends at the end. The collection therefore never reorders
// after the first declaration. The returned slice is a deep copy —
// the stored projection never aliases the caller's backing array and
// never mutates the stored row's backing array in place (concurrent
// reuse).
func upsertAppRefs(current []AppRef, fed []AppRef) []AppRef {
	out := append([]AppRef(nil), current...)
	index := make(map[AppRefKey]int, len(out)+len(fed))
	for i, ref := range out {
		index[ref.Key()] = i
	}
	for _, ref := range fed {
		key := ref.Key()
		if i, ok := index[key]; ok {
			out[i] = ref
		} else {
			index[key] = len(out)
			out = append(out, ref)
		}
	}
	return out
}

// validateText validates one free-text / identity / URI / tool field:
// valid UTF-8, bounded in RUNES, and free of NUL / C0-control / DEL
// bytes (0x00-0x1F, 0x7F). Control characters create identity and
// rendering ambiguity (the NUL-concatenated App identity was
// ambiguous exactly when a field contained a NUL byte) and are
// rejected loudly — never silently stripped or escaped.
func validateText(s, name string, maxRunes int) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	if utf8.RuneCountInString(s) > maxRunes {
		return fmt.Errorf("%s exceeds %d runes", name, maxRunes)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains a NUL / control character (U+%04X) — rejected as ambiguous", name, r)
		}
	}
	return nil
}

// normalizeAppRef validates ONE App reference and applies its
// documented defaults: the identity fields (ServerID, ResourceURI) are
// mandatory, every string / URI / tool field is valid UTF-8, bounded,
// and free of NUL / control ambiguity, the availability and
// completeness enums are valid (unset defaults to AppAvailable /
// CompletenessComplete), and the collection count is bounded by
// MaxAppsPerTurn (enforced at the upsert result in AttachAppRefs). The
// returned ref carries the defaults applied.
func normalizeAppRef(ref AppRef, i int) (AppRef, error) {
	if ref.ServerID == "" || ref.ResourceURI == "" {
		return AppRef{}, fmt.Errorf("%w: app ref %d requires ServerID and ResourceURI", ErrInvalidInput, i)
	}
	fields := []struct {
		name  string
		value string
		max   int
	}{
		{"EffectiveAgentID", ref.EffectiveAgentID, MaxAppAgentIDRunes},
		{"ServerID", ref.ServerID, MaxAppServerIDRunes},
		{"ResourceURI", ref.ResourceURI, MaxAppResourceURIRunes},
		{"DisplayMode", ref.DisplayMode, MaxAppDisplayModeRunes},
		{"ToolCallID", ref.ToolCallID, MaxAppToolCallIDRunes},
		{"ToolName", ref.ToolName, MaxToolNameRunes},
	}
	for _, f := range fields {
		if err := validateText(f.value, "app ref "+f.name, f.max); err != nil {
			return AppRef{}, fmt.Errorf("%w: app ref %d: %v", ErrInvalidInput, i, err)
		}
	}
	if ref.Availability == "" {
		ref.Availability = AppAvailable
	}
	if !ref.Availability.Valid() {
		return AppRef{}, fmt.Errorf("%w: app ref %d availability %q", ErrInvalidInput, i, ref.Availability)
	}
	if ref.Complete == "" {
		ref.Complete = CompletenessComplete
	}
	if !ref.Complete.Valid() {
		return AppRef{}, fmt.Errorf("%w: app ref %d completeness %q", ErrInvalidInput, i, ref.Complete)
	}
	return ref, nil
}
