package types

// ControlReceipt is durable, content-free acknowledgement for a text input on
// one exact task. Accepted is not applied; terminal never promises incorporation.
type ControlReceipt struct {
	EventID       string `json:"event_id"`
	TaskID        string `json:"task_id"`
	InputRevision uint64 `json:"input_revision"`
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	AcceptedAt    int64  `json:"accepted_at,omitempty"`
	AppliedAt     int64  `json:"applied_at,omitempty"`
	TerminalAt    int64  `json:"terminal_at,omitempty"`
}

// ControlReceiptRequest looks up an exact caller event, including after restart
// or task termination. It does not enqueue, resume, or start execution.
type ControlReceiptRequest struct {
	Identity IdentityScope `json:"identity"`
	EventID  string        `json:"event_id"`
}

// ControlReceiptResponse carries retained input evidence without executing work.
type ControlReceiptResponse struct {
	Receipt         ControlReceipt `json:"receipt"`
	ProtocolVersion string         `json:"protocol_version"`
}
