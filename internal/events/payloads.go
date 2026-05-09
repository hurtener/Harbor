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
