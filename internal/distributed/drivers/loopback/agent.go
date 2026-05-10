// Package loopback ships Harbor's V1 in-process drivers for both
// `distributed.MessageBus` and `distributed.RemoteTransport`. It is
// the test reference for the conformance suite — every later driver
// (durable bus at phase 86, A2A wire RemoteTransport at phase 29)
// inherits the same suite verbatim.
//
// The MessageBus loopback projects each published envelope as a typed
// event on the configured `events.EventBus` (registering a synthetic
// EventType once at init). The RemoteTransport loopback maintains an
// in-memory `map[agentURL]Agent` registered via the
// `RegisterAgent` sidecar API; every RemoteTransport call dispatches
// synchronously to the registered Agent.
//
// The Agent abstraction (see agent.go) is conformance-suite-facing
// only — production callers depend on `RemoteTransport`. The Agent
// exists so the loopback driver can simulate a remote A2A endpoint
// without leaving the process.
package loopback

import (
	"context"

	"github.com/hurtener/Harbor/internal/distributed/a2a"
)

// Agent is the in-process simulation of a remote A2A peer. The
// loopback RemoteTransport dispatches each method one-to-one to the
// registered Agent for the target URL. One method per A2A RPC.
//
// Conformance-suite-facing only. Production callers depend on
// `distributed.RemoteTransport`.
type Agent interface {
	// SendMessage maps to A2A SendMessage.
	SendMessage(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (a2a.Task, error)
	// SendStreamingMessage maps to A2A SendStreamingMessage. The
	// returned channel is owned by the Agent; the Agent MUST close it
	// when the stream is complete (the loopback transport's Recv
	// observes the close as the end-of-stream signal).
	SendStreamingMessage(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (<-chan a2a.StreamResponse, error)
	// GetTask maps to A2A GetTask. Returns the Task or
	// `distributed.ErrTaskNotFound` if absent.
	GetTask(ctx context.Context, taskID, contextID string) (a2a.Task, error)
	// ListTasks maps to A2A ListTasks.
	ListTasks(ctx context.Context, filter ListTasksFilter) ([]a2a.Task, error)
	// CancelTask maps to A2A CancelTask. Returns the terminal Task.
	CancelTask(ctx context.Context, taskID, contextID string) (a2a.Task, error)
	// SubscribeToTask maps to A2A SubscribeToTask. Channel ownership
	// rules identical to SendStreamingMessage.
	SubscribeToTask(ctx context.Context, taskID, contextID string) (<-chan a2a.StreamResponse, error)
	// CreateTaskPushNotificationConfig maps to A2A
	// CreateTaskPushNotificationConfig.
	CreateTaskPushNotificationConfig(ctx context.Context, cfg a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error)
	// GetTaskPushNotificationConfig maps to A2A
	// GetTaskPushNotificationConfig.
	GetTaskPushNotificationConfig(ctx context.Context, taskID, configID string) (a2a.TaskPushNotificationConfig, error)
	// ListTaskPushNotificationConfigs maps to A2A
	// ListTaskPushNotificationConfigs.
	ListTaskPushNotificationConfigs(ctx context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error)
	// DeleteTaskPushNotificationConfig maps to A2A
	// DeleteTaskPushNotificationConfig.
	DeleteTaskPushNotificationConfig(ctx context.Context, taskID, configID string) error
	// GetExtendedAgentCard maps to A2A GetExtendedAgentCard.
	GetExtendedAgentCard(ctx context.Context) (a2a.AgentCard, error)
}

// ListTasksFilter is the Agent-side view of the
// `distributed.RemoteTaskFilter` (the loopback driver translates one
// to the other). Kept separate so the Agent surface tracks the proto
// `ListTasksRequest` shape directly without leaking the distributed
// package's wrapping types.
type ListTasksFilter struct {
	Tenant               string
	ContextID            string
	Status               a2a.TaskState
	PageSize             int32
	PageToken            string
	HistoryLength        int32
	StatusTimestampAfter int64 // Unix nanos; 0 = "no filter"
	IncludeArtifacts     bool
}
