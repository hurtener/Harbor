package turns

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/identity"
)

// Projector is the runtime-owned turn-projection core: it validates
// and applies the operations-safe mutations (Append / Update / Seal),
// drives the two separately named component channels (AttachReasoning
// / AttachAppRef), serves the consumer-safe reads (List / Get), and
// owns the restart / reconcile / erasure-fence contracts over a
// conformance-ready Store.
//
// Concurrent reuse (D-025): a constructed *Projector is immutable
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

// New constructs a Projector over a mandatory Store. A nil store (or a
// non-positive inline-answer bound from WithInlineAnswerLimit) fails
// loud at construction — never a nil-panicking or silently-defaulted
// projector.
func New(store Store, opts ...ProjectorOption) (*Projector, error) {
	if store == nil {
		return nil, fmt.Errorf("turns: New requires a non-nil Store")
	}
	p := &Projector{store: store, clock: realClock{}, inlineLimit: MaxInlineAnswerBytes}
	for _, opt := range opts {
		opt(p)
	}
	if p.inlineLimit <= 0 {
		return nil, fmt.Errorf("turns: New requires a positive inline-answer limit, got %d", p.inlineLimit)
	}
	return p, nil
}

// Append creates the mutable row for a root foreground run (ops
// Append — structurally free of transcript / reasoning /
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
	if err := validateActivity(a.Activity); err != nil {
		return TurnRow{}, fmt.Errorf("turns: append activity: %w", err)
	}
	if err := validateAttachments(a.Inputs); err != nil {
		return TurnRow{}, fmt.Errorf("turns: append inputs: %w", err)
	}
	if err := validateAttachments(a.Outputs); err != nil {
		return TurnRow{}, fmt.Errorf("turns: append outputs: %w", err)
	}

	now := p.clock.Now()
	started := a.StartedAt
	if started.IsZero() {
		started = now
	}
	row := TurnRow{
		TurnID:     a.TurnID,
		SessionID:  id.SessionID,
		Sequence:   0, // minted by the Store
		TieBreaker: a.TurnID,
		Status:     status,
		Sealed:     false,
		Version:    1,
		StartedAt:  started,
		UpdatedAt:  started,
		Agent: Agent{
			ID:       a.AgentID,
			Name:     a.AgentName,
			Complete: agentCompleteness(a.AgentID, a.AgentName),
		},
		Query: Query{
			Text:     a.Query,
			Complete: queryCompleteness(a.Query),
		},
		Answer:    Answer{Complete: CompletenessUnavailable},
		Usage:     Usage{Complete: CompletenessUnavailable},
		Reasoning: Reasoning{Complete: CompletenessUnavailable},
		Inputs:    a.Inputs,
		Outputs:   a.Outputs,
		App:       nil,
	}
	row.Activity = clampActivity(a.Activity)
	fence, err := FenceFor(id)
	if err != nil {
		return TurnRow{}, err
	}
	stored, err := p.store.AppendTurnIf(ctx, id, row, fence)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: append %q: %w", a.TurnID, err)
	}
	return stored, nil
}

// Update mutates a MUTABLE row in place (ops Update — structurally
// free of transcript / reasoning / App-correlation / pause tokens).
// Non-nil components replace the stored component wholesale (usage is
// cumulative totals; activity is the cumulative feed). expectedVersion
// must match the stored row's current Version; a concurrent write wins
// with ErrStaleVersion and the caller reloads and retries (a replay
// of an already-applied update treats it as "already applied").
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
	if err := validateActivity(u.Activity); err != nil {
		return TurnRow{}, fmt.Errorf("turns: update activity: %w", err)
	}
	if err := validateAttachments(u.Inputs); err != nil {
		return TurnRow{}, fmt.Errorf("turns: update inputs: %w", err)
	}
	if err := validateAttachments(u.Outputs); err != nil {
		return TurnRow{}, fmt.Errorf("turns: update outputs: %w", err)
	}

	current, err := p.store.GetTurn(ctx, id, turnID)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: update %q: %w", turnID, err)
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
		merged.Answer = *u.Answer
	}
	if u.Usage != nil {
		merged.Usage = *u.Usage
	}
	if u.Activity != nil {
		merged.Activity = clampActivity(u.Activity)
	}
	if u.Inputs != nil {
		merged.Inputs = u.Inputs
	}
	if u.Outputs != nil {
		merged.Outputs = u.Outputs
	}
	fence, err := FenceFor(id)
	if err != nil {
		return TurnRow{}, err
	}
	stored, err := p.store.UpdateTurnIf(ctx, id, turnID, expectedVersion, merged, fence)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: update %q: %w", turnID, err)
	}
	return stored, nil
}

// Seal transitions a MUTABLE row to its SEALED terminal form, but
// only once the terminal status's REQUIRED sources are present on the
// current row: a complete seal requires the Answer component Complete
// (ErrSealIncomplete naming "answer" otherwise); a failed seal
// requires a non-empty ErrorClass ("error_class"). A same-status
// re-seal of an already-sealed row is an idempotent no-op that
// returns the sealed row (replay-friendly); a conflicting re-seal
// fails with ErrTurnSealed. After a successful seal the row is
// immutable.
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
	if current.Sealed {
		if current.Status == s.Status {
			// Same-status re-seal: idempotent replay no-op.
			return current, nil
		}
		return TurnRow{}, fmt.Errorf("%w: %q is already sealed as %q", ErrTurnSealed, turnID, current.Status)
	}
	switch s.Status {
	case StatusComplete:
		if current.Answer.Complete != CompletenessComplete {
			return TurnRow{}, fmt.Errorf("%w: answer is %q", ErrSealIncomplete, current.Answer.Complete)
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
	fence, err := FenceFor(id)
	if err != nil {
		return TurnRow{}, err
	}
	stored, err := p.store.SealTurnIf(ctx, id, turnID, expectedVersion, sealed, fence)
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
// the component Unavailable. Replaces any prior reasoning wholesale.
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
	if current.Sealed {
		return TurnRow{}, fmt.Errorf("%w: %q", ErrTurnSealed, turnID)
	}
	merged := current
	merged.Reasoning = clampReasoning(r.Steps)
	merged.UpdatedAt = p.clock.Now()
	fence, err := FenceFor(id)
	if err != nil {
		return TurnRow{}, err
	}
	stored, err := p.store.UpdateTurnIf(ctx, id, turnID, expectedVersion, merged, fence)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: attach reasoning %q: %w", turnID, err)
	}
	return stored, nil
}

// AttachAppRef attaches the App reference component through its
// separately named channel (NOT the generic ops). The ref carries
// render metadata plus availability and NEVER a correlation token.
// A ref missing ServerID or ResourceURI fails loud (a ref that could
// only mount broken is refused, mirroring the console render guard).
// Replaces any prior ref (last-wins within a turn, mirroring the live
// discovery reducer).
func (p *Projector) AttachAppRef(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, a AppRefInput) (TurnRow, error) {
	if err := validateIdentity(id); err != nil {
		return TurnRow{}, err
	}
	if turnID == "" {
		return TurnRow{}, fmt.Errorf("%w: turn id is empty", ErrInvalidInput)
	}
	ref := a.Ref
	if ref.ServerID == "" || ref.ResourceURI == "" {
		return TurnRow{}, fmt.Errorf("%w: app ref requires ServerID and ResourceURI", ErrInvalidInput)
	}
	if ref.Availability == "" {
		ref.Availability = AppAvailable
	}
	if !ref.Availability.Valid() {
		return TurnRow{}, fmt.Errorf("%w: app availability %q", ErrInvalidInput, ref.Availability)
	}
	if ref.Complete == "" {
		ref.Complete = CompletenessComplete
	}
	if !ref.Complete.Valid() {
		return TurnRow{}, fmt.Errorf("%w: app completeness %q", ErrInvalidInput, ref.Complete)
	}

	current, err := p.store.GetTurn(ctx, id, turnID)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: attach app ref %q: %w", turnID, err)
	}
	if current.Sealed {
		return TurnRow{}, fmt.Errorf("%w: %q", ErrTurnSealed, turnID)
	}
	merged := current
	app := ref
	merged.App = &app
	merged.UpdatedAt = p.clock.Now()
	fence, err := FenceFor(id)
	if err != nil {
		return TurnRow{}, err
	}
	stored, err := p.store.UpdateTurnIf(ctx, id, turnID, expectedVersion, merged, fence)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: attach app ref %q: %w", turnID, err)
	}
	return stored, nil
}

// List serves one newest-first keyset page of the consumer-safe
// projection — the backing of the future `sessions.turns.list`. Pages
// are ordered by the immutable keys (Sequence DESC, TurnID DESC) and
// are stable under concurrent appends (no skips, no duplicates). A
// Limit above MaxListLimit fails loudly (ErrInvalidInput) — bounded
// paging, never an accidental dump.
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
	rows, next, truncated, err := p.store.ListTurns(ctx, id, opts.Before, limit)
	if err != nil {
		return Page{}, fmt.Errorf("turns: list: %w", err)
	}
	return Page{
		Rows:       rows,
		NextCursor: next,
		HasMore:    next != nil,
		Truncated:  truncated,
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

// Checkpoint returns the session's last-applied runtime event
// sequence (0 when none was ever saved — a fresh session, or an
// in-memory-backed projection after a restart, which is EXPLICIT
// restart loss, not silent retention).
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
// the checkpoint.
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
// loss, never a silent claim of durability.
func (p *Projector) Reconcile(ctx context.Context, id identity.Identity, apply func(ctx context.Context, id identity.Identity, from uint64) (uint64, error)) (uint64, error) {
	if err := validateIdentity(id); err != nil {
		return 0, err
	}
	if apply == nil {
		return 0, fmt.Errorf("%w: reconcile apply callback is nil", ErrInvalidInput)
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
// cascade (the cascade's pending ledger + tombstone fence slots are
// the separate concern that makes later writes fail with
// ErrErasureFenced). Idempotent.
func (p *Projector) Erase(ctx context.Context, id identity.Identity) (int, error) {
	if err := validateIdentity(id); err != nil {
		return 0, err
	}
	n, err := p.store.DeleteScope(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("turns: erase: %w", err)
	}
	return n, nil
}

// Durable reports whether the backing store survives a process
// restart. An in-memory-backed projector reports false — its rows and
// checkpoints are GONE after a restart (explicit loss) and the runtime
// rebuilds via Reconcile against the durable event log.
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

// validateActivity validates the per-row shape of a fed activity list
// (tool non-empty, status valid, summary bounded). The window clamp
// (overflow → explicit lower-bound) happens separately in clampActivity.
func validateActivity(fed []ActivityRow) error {
	if len(fed) > MaxActivityRows*4 {
		// A feed 4x the window is already absurd; bound the validation
		// walk without special-casing the overflow path.
		return fmt.Errorf("%w: activity feed exceeds %d rows", ErrInvalidInput, MaxActivityRows*4)
	}
	for i, row := range fed {
		if row.Tool == "" {
			return fmt.Errorf("%w: activity row %d has an empty tool name", ErrInvalidInput, i)
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

// clampActivity retains the LAST MaxActivityRows of the fed sequence
// (the recent window) and reports the overflow as the explicit
// lower-bound: More + Dropped, with the component Partial. An empty
// feed is a complete empty window (the runtime fed the current
// cumulative list and it has no rows).
func clampActivity(fed []ActivityRow) Activity {
	if len(fed) == 0 {
		return Activity{Complete: CompletenessComplete}
	}
	if len(fed) <= MaxActivityRows {
		return Activity{Rows: fed, Complete: CompletenessComplete}
	}
	kept := fed[len(fed)-MaxActivityRows:]
	return Activity{
		Rows:     kept,
		Complete: CompletenessPartial,
		More:     true,
		Dropped:  len(fed) - MaxActivityRows,
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
// Partial + Dropped. An empty feed marks the component Unavailable
// (no reasoning source reported).
func clampReasoning(fed []ReasoningStep) Reasoning {
	if len(fed) == 0 {
		return Reasoning{Complete: CompletenessUnavailable}
	}
	if len(fed) <= MaxReasoningSteps {
		return Reasoning{Steps: fed, Complete: CompletenessComplete}
	}
	return Reasoning{
		Steps:    fed[:MaxReasoningSteps],
		Complete: CompletenessPartial,
		Dropped:  len(fed) - MaxReasoningSteps,
	}
}

// validateAnswer validates the answer component shape: exactly one of
// Inline / Ref when Complete (an empty inline answer is a legitimate
// complete answer); no content in a non-complete answer; an inline
// answer at or above the inline bound is ErrContextLeak (heavy answers
// MUST route by artifact reference).
func validateAnswer(a Answer, inlineLimit int) error {
	if !a.Complete.Valid() {
		return fmt.Errorf("%w: answer completeness %q", ErrInvalidInput, a.Complete)
	}
	switch a.Complete {
	case CompletenessComplete:
		if a.Ref != nil {
			if a.Inline != "" {
				return fmt.Errorf("%w: answer carries both Inline and Ref", ErrInvalidInput)
			}
			if a.Ref.ID == "" {
				return fmt.Errorf("%w: answer ref id is empty", ErrInvalidInput)
			}
			if a.Ref.SizeBytes < 0 {
				return fmt.Errorf("%w: answer ref size is negative", ErrInvalidInput)
			}
			return nil
		}
		if len([]byte(a.Inline)) >= inlineLimit {
			return fmt.Errorf("%w: inline answer is %d bytes (>= %d) — route by artifact reference",
				ErrContextLeak, len([]byte(a.Inline)), inlineLimit)
		}
		return nil
	default: // partial / unavailable answers carry no content
		if a.Inline != "" || a.Ref != nil {
			return fmt.Errorf("%w: a %q answer cannot carry content", ErrInvalidInput, a.Complete)
		}
		return nil
	}
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

// validateAttachments validates one attachment metadata list: every id
// non-empty, sizes non-negative, count bounded.
func validateAttachments(atts []Attachment) error {
	if len(atts) > MaxAttachmentsPerSide {
		return fmt.Errorf("%w: attachment list exceeds %d entries", ErrInvalidInput, MaxAttachmentsPerSide)
	}
	for i, a := range atts {
		if a.ID == "" {
			return fmt.Errorf("%w: attachment %d has an empty id", ErrInvalidInput, i)
		}
		if a.SizeBytes < 0 {
			return fmt.Errorf("%w: attachment %d has a negative size", ErrInvalidInput, i)
		}
	}
	return nil
}
