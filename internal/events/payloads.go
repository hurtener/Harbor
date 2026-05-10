package events

// BusDroppedPayload reports a bounded burst of dropped events that
// were silently lost before the bus emitted this notification. The
// dropped sequence range is closed/closed: [FromSeq, ToSeq] inclusive.
//
// The bus emits at most one BusDroppedPayload per DropWindow per
// subscriber; the range covers every event lost since the last
// emit.
type BusDroppedPayload struct {
	SafeSealed
	FromSeq      uint64
	ToSeq        uint64
	DroppedCount uint64
	SubscriberID uint64
}

// SubscriptionIdleClosedPayload reports that the reaper cancelled a
// subscription that had not drained its channel within IdleTimeout.
type SubscriptionIdleClosedPayload struct {
	SafeSealed
	SubscriberID uint64
	IdleSeconds  float64
}

// AuditRedactionFailedPayload reports a Publish call whose payload
// the audit redactor refused. Carries the failing event's type and
// identity but NO original payload bytes — the failure is observable
// without leaking the bytes the redactor refused.
type AuditRedactionFailedPayload struct {
	SafeSealed
	OriginalType EventType
	Reason       string
}

// AdminScopeUsedPayload is emitted whenever Subscribe is called with
// Filter.Admin=true, regardless of whether the triple is empty or
// partially specified. Surfaces admin-scope use for after-the-fact
// auditability — Phase 61 will additionally enforce a cryptographic
// scope claim, but the audit emit itself is Phase 05's contribution.
type AdminScopeUsedPayload struct {
	SafeSealed
	Tenant       string
	User         string
	Session      string
	SubscriberID uint64
}

// RuntimeErrorPayload is the bus-side projection of a Logger.Error
// call. The telemetry/eventbus adapter constructs one of these from
// the redacted (msg, attrs) the Logger handed to its BusEmitter
// seam, so RuntimeErrorPayload arrives at the bus pre-redacted.
//
// Even so, it is NOT marked SafePayload: a defensive contributor
// might later construct a RuntimeErrorPayload outside the Logger
// path (e.g. emitting a runtime error directly from a handler that
// bypassed the redactor). Running this payload through the bus
// redactor on every Publish is an extra walk per error event, but
// it preserves the audit-redactor-as-bus-boundary contract (D-020).
//
// Fields is the slog.Attr key/value map after Logger redaction, in
// `map[string]any` shape so the audit redactor's reflective walk
// is deterministic.
type RuntimeErrorPayload struct {
	Sealed
	Message string
	Fields  map[string]any
}

// RunCancelledPayload is emitted by Engine.Cancel(runID) when the
// cancellation was observed for an active run. Carries the RunID,
// the wall-clock CancelledAt timestamp (unix nanoseconds), and the
// number of envelopes the cancellation drained from the engine's
// channels (a coarse "how loaded was the run" metric).
//
// SafePayload by construction — every field is internal bookkeeping
// (no caller-controlled bytes). Subscribers consume the typed shape
// directly without an audit-redactor walk. Phase 13.
type RunCancelledPayload struct {
	SafeSealed
	RunID                string
	CancelledAt          int64
	DroppedEnvelopeCount int64
}
