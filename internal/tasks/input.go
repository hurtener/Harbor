package tasks

import "errors"

// MaxInputReceipts bounds per-task clarification state. Receipts are never
// evicted: forgetting a caller key would permit an old retry to apply twice.
const MaxInputReceipts = 256

// InputStatus distinguishes durable admission from actual planning consumption.
type InputStatus string

const (
	// InputAccepted is admitted but not yet proven consumed by planning.
	InputAccepted InputStatus = "accepted"
	// InputApplied was consumed by planning before decision execution.
	InputApplied InputStatus = "applied"
	// InputDeclined was not admitted to the live execution.
	InputDeclined InputStatus = "declined"
	// InputTerminal can no longer be consumed by this task.
	InputTerminal InputStatus = "terminal"
)

var (
	// ErrInputRevisionConflict refuses stale caller intent before admission.
	ErrInputRevisionConflict = errors.New("tasks: accepted input revision changed")
	ErrInputReceiptNotFound  = errors.New("tasks: input receipt not found")
	ErrInputReceiptCapacity  = errors.New("tasks: input receipt capacity reached")
)

// InputReceipt is content-free evidence for one caller event on one exact task.
// Revision is allocated only on acceptance. Accepted is not proof that a model
// consumed the input; Applied is committed after a planner invocation receives it.
type InputReceipt struct {
	EventID     string
	TaskID      TaskID
	PayloadHash string
	Revision    uint64
	Status      InputStatus
	Reason      string
	AcceptedAt  int64
	AppliedAt   int64
	TerminalAt  int64
}

// InputRecord retains bounded text as task input, not as an executable control.
// The content hash binds the original text; Message is the canonical redacted
// text used by the runtime. Neither field is included in the receipt projection.
type InputRecord struct {
	Receipt InputReceipt
	Message string
}
