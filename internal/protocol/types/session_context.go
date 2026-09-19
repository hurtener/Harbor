package types

// SessionsReconcileContextRequest explicitly seals a fully settled retained run
// as interrupted evidence for later turns in the caller's own session. It does
// not resume execution or resolve an uncertain external operation.
type SessionsReconcileContextRequest struct {
	Identity    IdentityScope `json:"identity"`
	SourceRunID string        `json:"source_run_id"`
}

// SessionsReconcileContextResponse acknowledges durable context reconciliation,
// not execution success. No private transcript, source content or journal is
// exposed. Repeated requests are idempotent while the sealed turn is retained.
type SessionsReconcileContextResponse struct {
	SessionID   string `json:"session_id"`
	SourceRunID string `json:"source_run_id"`
	Reconciled  bool   `json:"reconciled"`
}
