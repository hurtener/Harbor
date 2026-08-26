package protocolclient

import (
	"net/http"

	internal "github.com/hurtener/Harbor/internal/protocol/client"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

type (
	// Client is the curated concurrent-safe REST/SSE Protocol client interface.
	Client = internal.Client
	// RuntimeClient is the additive Runtime inspection/control client,
	// including HA-68 skill-publication methods.
	RuntimeClient = internal.RuntimeClient
	// Connection describes one authenticated Runtime attachment.
	Connection = internal.Connection
	// TokenSource resolves a bearer token for each request.
	TokenSource = internal.TokenSource
	// TokenSourceFunc adapts a function to TokenSource.
	TokenSourceFunc = internal.TokenSourceFunc
	// Option configures a Client at construction.
	Option = internal.Option
	// Error is a typed HTTP plus canonical Protocol failure.
	Error = internal.ProtocolError
	// StreamOptions selects an event subscription and reconnect cursor.
	StreamOptions = internal.StreamOptions
	// SSEFrame is one decoded Server-Sent Events frame.
	SSEFrame = internal.SSEFrame
	// EventStream owns one cancellable event subscription.
	EventStream = internal.EventStream

	// Method is a canonical Harbor Protocol method name.
	Method = methods.Method
	// ErrorCode is a canonical Harbor Protocol error code.
	ErrorCode = protoerrors.Code

	// IdentityScope is the Protocol wire identity tuple.
	IdentityScope = types.IdentityScope
	// RuntimeInfo is the runtime.info response.
	RuntimeInfo = types.RuntimeInfo
	// ExternalGrantReadiness is runtime.info's content-free external-grant
	// enforcement and coordinator-transport readiness projection.
	ExternalGrantReadiness = types.ExternalGrantReadiness
	// RuntimeHealth is the runtime.health response.
	RuntimeHealth = types.RuntimeHealth
	// StartRequest is the start request.
	StartRequest = types.StartRequest
	// LLMProviderRouteSelector is the opaque provider-route selector carried by
	// an optional start request.
	LLMProviderRouteSelector = types.LLMProviderRouteSelector
	// StartResponse is the start response.
	StartResponse = types.StartResponse
	// TaskListRequest is the tasks.list request.
	TaskListRequest = types.TaskListRequest
	// TaskListResponse is the tasks.list response.
	TaskListResponse = types.TaskListResponse
	// TaskGetRequest is the tasks.get request.
	TaskGetRequest = types.TaskGetRequest
	// TaskDetail is the tasks.get response.
	TaskDetail = types.TaskDetail
	// SessionsListRequest is the sessions.list request.
	SessionsListRequest = types.SessionsListRequest
	// SessionsListResponse is the sessions.list response.
	SessionsListResponse = types.SessionsListResponse
	// SessionsInspectRequest is the sessions.inspect request.
	SessionsInspectRequest = types.SessionsInspectRequest
	// SessionsInspectResponse is the sessions.inspect response.
	SessionsInspectResponse = types.SessionsInspectResponse
	// SessionsSetTitleRequest is the sessions.set_title request.
	SessionsSetTitleRequest = types.SessionsSetTitleRequest
	// SessionsSetTitleResponse is the sessions.set_title response.
	SessionsSetTitleResponse = types.SessionsSetTitleResponse
	// SessionsDeleteResponse is the sessions.delete response.
	SessionsDeleteResponse = types.SessionsDeleteResponse
	// StateHistoryRequest is the state.history request.
	StateHistoryRequest = types.StateHistoryRequest
	// StateHistoryResponse is the state.history response.
	StateHistoryResponse = types.StateHistoryResponse
	// PauseListRequest is the pause.list request.
	PauseListRequest = types.PauseListRequest
	// PauseListResponse is the pause.list response.
	PauseListResponse = types.PauseListResponse
	// ControlRequest is the shared steering-control request.
	ControlRequest = types.ControlRequest
	// ControlResponse is the shared steering-control response.
	ControlResponse = types.ControlResponse
	// ArtifactsPutRequest is the artifacts.put request.
	ArtifactsPutRequest = types.ArtifactsPutRequest
	// ArtifactsPutResponse is the artifacts.put response.
	ArtifactsPutResponse = types.ArtifactsPutResponse
	// ArtifactsListRequest is the artifacts.list request.
	ArtifactsListRequest = types.ArtifactsListRequest
	// ArtifactsListResponse is the artifacts.list response.
	ArtifactsListResponse = types.ArtifactsListResponse
	// SkillPublicationSkill is the reviewed publication body.
	SkillPublicationSkill = types.SkillPublicationSkill
	// SkillPublicationMetadata is the content-free publication projection.
	SkillPublicationMetadata = types.SkillPublicationMetadata
	// SkillPublicationReference is an exact Agent publication pin.
	SkillPublicationReference = types.SkillPublicationReference
	// SkillPublicationReceipt is a replay-safe mutation receipt.
	SkillPublicationReceipt                = types.SkillPublicationReceipt
	SkillPublicationPublishRequest         = types.SkillPublicationPublishRequest
	SkillPublicationPublishResponse        = types.SkillPublicationPublishResponse
	SkillPublicationListRequest            = types.SkillPublicationListRequest
	SkillPublicationListResponse           = types.SkillPublicationListResponse
	SkillPublicationGetRequest             = types.SkillPublicationGetRequest
	SkillPublicationGetResponse            = types.SkillPublicationGetResponse
	SkillPublicationSuccessorRequest       = types.SkillPublicationSuccessorRequest
	SkillPublicationSuccessorResponse      = types.SkillPublicationSuccessorResponse
	SkillPublicationRetireRequest          = types.SkillPublicationRetireRequest
	SkillPublicationRetireResponse         = types.SkillPublicationRetireResponse
	SkillPublicationAvailableRequest       = types.SkillPublicationAvailableRequest
	SkillPublicationAvailableResponse      = types.SkillPublicationAvailableResponse
	SkillPublicationInstallRequest         = types.SkillPublicationInstallRequest
	SkillPublicationInstallResponse        = types.SkillPublicationInstallResponse
	SkillPublicationUpdateRequest          = types.SkillPublicationUpdateRequest
	SkillPublicationUpdateResponse         = types.SkillPublicationUpdateResponse
	SkillPublicationRemoveRequest          = types.SkillPublicationRemoveRequest
	SkillPublicationRemoveResponse         = types.SkillPublicationRemoveResponse
	SkillPublicationReferencesListRequest  = types.SkillPublicationReferencesListRequest
	SkillPublicationReferencesListResponse = types.SkillPublicationReferencesListResponse
)

const (
	// ProtocolVersion is the pinned Harbor Protocol version.
	ProtocolVersion = types.ProtocolVersion

	// MethodCancel cancels a run.
	MethodCancel = methods.MethodCancel
	// MethodPause pauses a run.
	MethodPause = methods.MethodPause
	// MethodResume resumes a run.
	MethodResume = methods.MethodResume
	// MethodRedirect redirects a run.
	MethodRedirect = methods.MethodRedirect
	// MethodInjectContext injects context into a run.
	MethodInjectContext = methods.MethodInjectContext
	// MethodApprove approves a paused operation.
	MethodApprove = methods.MethodApprove
	// MethodReject rejects a paused operation.
	MethodReject = methods.MethodReject
	// MethodPrioritize changes task priority.
	MethodPrioritize = methods.MethodPrioritize
	// MethodUserMessage injects a user message.
	MethodUserMessage = methods.MethodUserMessage
)

var (
	// ErrInvalidConnection reports invalid construction inputs.
	ErrInvalidConnection = internal.ErrInvalidConnection
	// ErrTokenRequired reports an empty token result.
	ErrTokenRequired = internal.ErrTokenRequired
	// ErrTokenIdentityMismatch reports static-token use for another principal.
	ErrTokenIdentityMismatch = internal.ErrTokenIdentityMismatch
	// ErrIdentityRequired reports an incomplete isolation identity.
	ErrIdentityRequired = internal.ErrIdentityRequired
	// ErrResponseTooLarge reports a bounded-body violation.
	ErrResponseTooLarge = internal.ErrResponseTooLarge
	// ErrMalformedResponse reports strict JSON failure.
	ErrMalformedResponse = internal.ErrMalformedResponse
	// ErrIncompatibleProtocol reports a Protocol-major mismatch.
	ErrIncompatibleProtocol = internal.ErrIncompatibleProtocol
	// ErrMalformedSSE reports invalid event-stream framing.
	ErrMalformedSSE = internal.ErrMalformedSSE
	// ErrSSELineTooLarge reports a line bound violation.
	ErrSSELineTooLarge = internal.ErrSSELineTooLarge
	// ErrSSEFrameTooLarge reports a frame bound violation.
	ErrSSEFrameTooLarge = internal.ErrSSEFrameTooLarge
	// ErrStreamClosed reports receive after explicit close.
	ErrStreamClosed = internal.ErrStreamClosed
)

// New constructs a Protocol client.
func New(connection Connection, options ...Option) (Client, error) {
	client, err := internal.New(connection, options...)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// StaticToken returns a fixed TokenSource bound to principal.
func StaticToken(token string, principal IdentityScope) TokenSource {
	return internal.StaticToken(token, principal)
}

// WithHTTPClient supplies the HTTP client used for REST and SSE calls.
func WithHTTPClient(client *http.Client) Option { return internal.WithHTTPClient(client) }
