package distributed

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/distributed/a2a"
)

// RemoteCallKind discriminates the three SendMessage-shaped A2A
// dispatch modes a `RemoteTransport.Stream` call can take. `Send` is
// unary (mapped to A2A SendMessage); `Stream` is the streaming send
// (A2A SendStreamingMessage); `Subscribe` is the streaming-subscribe
// shape (A2A SubscribeToTask, when the caller wants live updates for
// a task it didn't initiate).
//
// Drivers route by RemoteCallRequest.Kind. Empty Kind defaults to
// Send.
type RemoteCallKind string

// RemoteCallKind values.
const (
	// RemoteCallKindSend — A2A SendMessage (unary request/reply).
	RemoteCallKindSend RemoteCallKind = "send"
	// RemoteCallKindStream — A2A SendStreamingMessage (streaming reply).
	RemoteCallKindStream RemoteCallKind = "stream"
	// RemoteCallKindSubscribe — A2A SubscribeToTask (live updates for an existing task).
	RemoteCallKindSubscribe RemoteCallKind = "subscribe"
)

// RemoteCallRequest carries the inputs to a `RemoteTransport.Send` or
// `RemoteTransport.Stream` invocation. The shape is the V1 wire-neutral
// envelope; Phase 29's A2A driver translates this into the on-the-wire
// gRPC / JSON-RPC / HTTP+JSON request as configured.
type RemoteCallRequest struct {
	// AgentURL is the target A2A agent's interface URL (matches an
	// AgentInterface.URL value from a discovered AgentCard).
	AgentURL string
	// Kind selects the dispatch shape. Empty defaults to Send.
	Kind RemoteCallKind
	// ContextID is the A2A context_id (the conversation grouping).
	// Empty for new contexts; the target may assign one in the reply.
	ContextID string
	// TaskID is the A2A task_id. Empty for new tasks; set when the
	// caller is continuing an existing task (Send with a follow-up
	// message, Stream with Kind=Subscribe for an existing task).
	TaskID string
	// Message is the A2A message payload. Caller-side audit redaction
	// (D-020) MUST have already run.
	Message a2a.Message
	// Config carries A2A send configuration (accepted output modes,
	// push-notification config, history length, return-immediately).
	Config a2a.SendMessageConfiguration
	// Timeout is the per-call deadline. Zero means no timeout; drivers
	// SHOULD treat zero as "honour the parent ctx deadline."
	Timeout time.Duration
}

// RemoteCallResult is the unary-reply form returned by
// `RemoteTransport.Send`. It carries the resulting A2A Task plus an
// optional HTTP status (populated by HTTP-shaped transports;
// zero-valued for gRPC / loopback).
type RemoteCallResult struct {
	// Task is the A2A Task returned by the agent.
	Task a2a.Task
	// HTTPStatus is populated by transports that carry HTTP semantics
	// (the JSON-RPC over HTTP and HTTP+JSON bindings of A2A). Zero
	// when the transport does not carry HTTP status.
	HTTPStatus int
}

// RemoteTaskSnapshot is the value returned by
// `RemoteTransport.GetTask` and the element type of `ListTasks`. The
// underlying type is an A2A Task; the named alias exists so the
// distributed contract can evolve independently of the wire shape
// without breaking callers.
type RemoteTaskSnapshot a2a.Task

// RemoteTaskFilter narrows `RemoteTransport.ListTasks`. Mirrors the
// proto ListTasksRequest filter shape — Tenant, ContextID, Status
// (zero-value = "any"), pagination, history length, status timestamp
// filter, and the artifact-inclusion flag.
type RemoteTaskFilter struct {
	// Tenant filters by the agent-side tenant scope (matches the
	// proto's `string tenant = 1` path parameter).
	Tenant string
	// ContextID filters by conversation grouping.
	ContextID string
	// Status filters by task status; TaskStateUnspecified means "any".
	Status a2a.TaskState
	// PageSize bounds the response size; drivers SHOULD cap at 100
	// per the proto contract.
	PageSize int32
	// PageToken carries the continuation token from a prior call's
	// NextPageToken. Empty for the first call.
	PageToken string
	// HistoryLength bounds per-task history depth in the response;
	// zero means "no limit imposed by the caller."
	HistoryLength int32
	// StatusTimestampAfter filters to tasks whose status was updated
	// at or after this instant. Zero means "no time filter."
	StatusTimestampAfter time.Time
	// IncludeArtifacts asks the driver to populate Artifacts on each
	// returned Task. False by default to reduce payload size.
	IncludeArtifacts bool
}

// RemoteEventStream is the streaming-reply interface returned by
// `RemoteTransport.Stream`. Each Recv yields the next event in the
// stream; the stream terminates with a `RemoteStreamDone` error
// (typically wrapping `io.EOF` or `ctx.Err()`).
//
// Callers MUST call Close to release the stream's resources (channel
// + goroutine). The conformance suite's `Stream_RespectsClose` covers
// the contract.
type RemoteEventStream interface {
	// Recv returns the next StreamResponse or an error. Returns an
	// error wrapping io.EOF when the stream completes normally (the
	// "done" condition for A2A's final event).
	Recv(ctx context.Context) (a2a.StreamResponse, error)
	// Close releases the stream. Idempotent.
	Close() error
}

// RemoteTaskEventStream is the streaming-reply interface returned by
// `RemoteTransport.Subscribe`. Identical surface to RemoteEventStream
// — the named alias makes the call-site intent (Subscribe vs Stream)
// readable without a documentation hop.
type RemoteTaskEventStream = RemoteEventStream

// RemoteTransport is Harbor's cross-process / cross-host call surface.
// Every method maps 1:1 to an A2A v1 RPC from the vendored
// `docs/specifications/a2a.proto`. The mapping is verbatim so Phase 29
// (the southbound A2A driver) consumes the surface without churn.
//
// Implementations MUST be safe for concurrent use by N goroutines
// against a single shared instance (D-025).
//
// All methods receive identity via `ctx`. Calls without a complete
// identity triple are rejected with `ErrIdentityRequired` at the
// caller-side boundary (drivers SHOULD NOT need to re-validate when
// the runtime owns the ctx).
type RemoteTransport interface {
	// Send maps to A2A `SendMessage`. Unary request/reply.
	Send(ctx context.Context, req RemoteCallRequest) (RemoteCallResult, error)
	// Stream maps to A2A `SendStreamingMessage` (when req.Kind ==
	// RemoteCallKindStream) or `SubscribeToTask` (when req.Kind ==
	// RemoteCallKindSubscribe AND req.TaskID is set). Drivers route
	// by req.Kind.
	Stream(ctx context.Context, req RemoteCallRequest) (RemoteEventStream, error)
	// GetTask maps to A2A `GetTask`. Returns ErrTaskNotFound when the
	// task is not registered with the target agent.
	GetTask(ctx context.Context, taskID, contextID string) (*RemoteTaskSnapshot, error)
	// ListTasks maps to A2A `ListTasks`. Returns a snapshot per the
	// filter; pagination handled via filter.PageToken.
	ListTasks(ctx context.Context, filter RemoteTaskFilter) ([]RemoteTaskSnapshot, error)
	// Cancel maps to A2A `CancelTask`. Returns nil when the task
	// reached a terminal state (Canceled).
	Cancel(ctx context.Context, taskID, contextID string) error
	// Subscribe maps to A2A `SubscribeToTask` for the caller wanting
	// live updates on a task it did NOT initiate. Symmetric with
	// Stream(Kind=Subscribe); exposed as a separate method so the
	// call-site intent reads cleanly.
	Subscribe(ctx context.Context, taskID, contextID string) (RemoteTaskEventStream, error)
	// CreateTaskPushNotificationConfig maps to A2A
	// `CreateTaskPushNotificationConfig`. Stores a push-notification
	// config for a task; V1 drivers store in memory.
	CreateTaskPushNotificationConfig(ctx context.Context, cfg a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error)
	// GetTaskPushNotificationConfig maps to A2A
	// `GetTaskPushNotificationConfig`. Returns the stored config.
	GetTaskPushNotificationConfig(ctx context.Context, taskID, configID string) (a2a.TaskPushNotificationConfig, error)
	// ListTaskPushNotificationConfigs maps to A2A
	// `ListTaskPushNotificationConfigs`. Lists configs for a task.
	ListTaskPushNotificationConfigs(ctx context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error)
	// DeleteTaskPushNotificationConfig maps to A2A
	// `DeleteTaskPushNotificationConfig`. Deletes a stored config.
	DeleteTaskPushNotificationConfig(ctx context.Context, taskID, configID string) error
	// GetExtendedAgentCard maps to A2A `GetExtendedAgentCard`. Returns
	// the agent's self-describing manifest.
	GetExtendedAgentCard(ctx context.Context) (*a2a.AgentCard, error)
	// Close releases driver-owned resources. Idempotent.
	Close(ctx context.Context) error
}
