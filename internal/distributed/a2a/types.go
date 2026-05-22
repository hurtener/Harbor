package a2a

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidPart is returned when a Part oneof is empty / contains no
// recognised variant. Surfaced at unmarshal time.
var ErrInvalidPart = errors.New("a2a: invalid Part: no recognised variant")

// ErrInvalidOneof is returned when a discriminated-union JSON payload
// matches no known variant.
var ErrInvalidOneof = errors.New("a2a: invalid oneof: no recognised variant")

// -----------------------------------------------------------------------------
// Enums
// -----------------------------------------------------------------------------

// TaskState defines the possible lifecycle states of a Task.
//
// Proto: enum TaskState (`docs/specifications/a2a.proto` §enum TaskState).
type TaskState int32

// TaskState values mirror the proto's `TASK_STATE_*` constants.
const (
	// TaskStateUnspecified — the task is in an unknown or indeterminate state.
	TaskStateUnspecified TaskState = 0
	// TaskStateSubmitted — task has been successfully submitted and acknowledged.
	TaskStateSubmitted TaskState = 1
	// TaskStateWorking — task is actively being processed by the agent.
	TaskStateWorking TaskState = 2
	// TaskStateCompleted — task has finished successfully. Terminal.
	TaskStateCompleted TaskState = 3
	// TaskStateFailed — task has finished with an error. Terminal.
	TaskStateFailed TaskState = 4
	// TaskStateCanceled — task was canceled before completion. Terminal.
	TaskStateCanceled TaskState = 5
	// TaskStateInputRequired — agent requires additional user input. Interrupted.
	TaskStateInputRequired TaskState = 6
	// TaskStateRejected — agent has decided to not perform the task. Terminal.
	TaskStateRejected TaskState = 7
	// TaskStateAuthRequired — authentication is required to proceed. Interrupted.
	TaskStateAuthRequired TaskState = 8
)

// taskStateNames is the canonical name table mirroring the proto enum's
// uppercase wire form. Used for JSON marshalling so the Go types
// round-trip against either an integer or a string form.
var taskStateNames = map[TaskState]string{
	TaskStateUnspecified:   "TASK_STATE_UNSPECIFIED",
	TaskStateSubmitted:     "TASK_STATE_SUBMITTED",
	TaskStateWorking:       "TASK_STATE_WORKING",
	TaskStateCompleted:     "TASK_STATE_COMPLETED",
	TaskStateFailed:        "TASK_STATE_FAILED",
	TaskStateCanceled:      "TASK_STATE_CANCELED",
	TaskStateInputRequired: "TASK_STATE_INPUT_REQUIRED",
	TaskStateRejected:      "TASK_STATE_REJECTED",
	TaskStateAuthRequired:  "TASK_STATE_AUTH_REQUIRED",
}

var taskStateValues = func() map[string]TaskState {
	m := make(map[string]TaskState, len(taskStateNames))
	for k, v := range taskStateNames {
		m[v] = k
	}
	return m
}()

// String returns the canonical wire name (TASK_STATE_*).
func (s TaskState) String() string {
	if name, ok := taskStateNames[s]; ok {
		return name
	}
	return fmt.Sprintf("TaskState(%d)", int32(s))
}

// IsValid reports whether s is one of the 9 canonical values.
func (s TaskState) IsValid() bool {
	_, ok := taskStateNames[s]
	return ok
}

// MarshalJSON emits the canonical wire string.
func (s TaskState) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON accepts either the canonical wire string or the
// integer form (proto3 JSON allows both).
func (s *TaskState) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		v, ok := taskStateValues[str]
		if !ok {
			return fmt.Errorf("a2a: unknown TaskState %q", str)
		}
		*s = v
		return nil
	}
	var n int32
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("a2a: TaskState: %w", err)
	}
	*s = TaskState(n)
	if !s.IsValid() {
		return fmt.Errorf("a2a: unknown TaskState %d", n)
	}
	return nil
}

// Role identifies the sender of a Message in A2A communication.
//
// Proto: enum Role.
type Role int32

// Role values mirror the proto's ROLE_* constants.
const (
	// RoleUnspecified — the role is unspecified.
	RoleUnspecified Role = 0
	// RoleUser — message is from the client to the server.
	RoleUser Role = 1
	// RoleAgent — message is from the server to the client.
	RoleAgent Role = 2
)

var roleNames = map[Role]string{
	RoleUnspecified: "ROLE_UNSPECIFIED",
	RoleUser:        "ROLE_USER",
	RoleAgent:       "ROLE_AGENT",
}

var roleValues = func() map[string]Role {
	m := make(map[string]Role, len(roleNames))
	for k, v := range roleNames {
		m[v] = k
	}
	return m
}()

// String returns the canonical wire name.
func (r Role) String() string {
	if name, ok := roleNames[r]; ok {
		return name
	}
	return fmt.Sprintf("Role(%d)", int32(r))
}

// IsValid reports whether r is one of the 3 canonical values.
func (r Role) IsValid() bool {
	_, ok := roleNames[r]
	return ok
}

// MarshalJSON emits the canonical wire string.
func (r Role) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

// UnmarshalJSON accepts string or integer form.
func (r *Role) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		v, ok := roleValues[str]
		if !ok {
			return fmt.Errorf("a2a: unknown Role %q", str)
		}
		*r = v
		return nil
	}
	var n int32
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("a2a: Role: %w", err)
	}
	*r = Role(n)
	if !r.IsValid() {
		return fmt.Errorf("a2a: unknown Role %d", n)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Part — discriminated union (proto: message Part { oneof content {...} })
// -----------------------------------------------------------------------------

// Part is the proto Part oneof Go interface. Each concrete variant
// (TextPart, RawPart, URLPart, DataPart) satisfies this interface via
// its Kind() method and an unexported seal. Callers discriminate via
// `Kind()` or a type switch:
//
//	switch p := part.(type) {
//	case *TextPart: …
//	case *RawPart:  …
//	}
//
// Proto: message Part (`docs/specifications/a2a.proto` §message Part).
type Part interface {
	// Kind returns the variant discriminator: "text" | "raw" | "url" | "data".
	Kind() string
	isPart()
}

// PartKind values are the canonical variant discriminators.
const (
	// PartKindText — proto: `string text = 1`.
	PartKindText = "text"
	// PartKindRaw — proto: `bytes raw = 2`. Base64-encoded on the wire.
	PartKindRaw = "raw"
	// PartKindURL — proto: `string url = 3`.
	PartKindURL = "url"
	// PartKindData — proto: `google.protobuf.Value data = 4`.
	PartKindData = "data"
)

// partCommon holds the proto fields shared across every Part variant
// (Filename, MediaType, Metadata). Embedded into each concrete Part.
type partCommon struct {
	// Filename — proto: `string filename = 6` ("e.g., 'document.pdf'").
	Filename string `json:"filename,omitempty"`
	// MediaType — proto: `string media_type = 7` (MIME type).
	MediaType string `json:"media_type,omitempty"`
	// Metadata — proto: `google.protobuf.Struct metadata = 5`.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// TextPart is the `text` variant of Part.
//
// Proto: `string text = 1` ("The string content of the `text` part.").
type TextPart struct {
	Text string `json:"text"`
	partCommon
}

// Kind returns PartKindText.
func (p *TextPart) Kind() string { return PartKindText }
func (p *TextPart) isPart()      {}

// RawPart is the `raw` variant of Part — raw bytes.
//
// Proto: `bytes raw = 2` ("The raw byte content of a file. In JSON
// serialization, this is encoded as a base64 string.").
type RawPart struct {
	// Raw is the raw bytes. JSON-encoded as a base64 string (stdlib default).
	Raw []byte `json:"raw"`
	partCommon
}

// Kind returns PartKindRaw.
func (p *RawPart) Kind() string { return PartKindRaw }
func (p *RawPart) isPart()      {}

// URLPart is the `url` variant of Part.
//
// Proto: `string url = 3` ("A url pointing to the file's content.").
type URLPart struct {
	URL string `json:"url"`
	partCommon
}

// Kind returns PartKindURL.
func (p *URLPart) Kind() string { return PartKindURL }
func (p *URLPart) isPart()      {}

// DataPart is the `data` variant of Part.
//
// Proto: `google.protobuf.Value data = 4` ("Arbitrary structured data
// as a JSON value (object, array, string, number, boolean, or null).").
type DataPart struct {
	// Data is the structured JSON value. Any Go value that marshals to JSON.
	Data any `json:"data"`
	partCommon
}

// Kind returns PartKindData.
func (p *DataPart) Kind() string { return PartKindData }
func (p *DataPart) isPart()      {}

// partWire is the on-the-wire representation of Part — a flat object
// with the oneof variant inlined alongside the shared (Filename,
// MediaType, Metadata) fields. We use this both for marshal and
// unmarshal so the JSON round-trip is symmetric.
type partWire struct {
	Text *string         `json:"text,omitempty"`
	Raw  []byte          `json:"raw,omitempty"`
	URL  *string         `json:"url,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`

	Filename  string         `json:"filename,omitempty"`
	MediaType string         `json:"media_type,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// MarshalPart encodes any Part variant into the proto's wire form.
// Exposed for callers that need to marshal a `Part` interface field
// without an enclosing struct.
func MarshalPart(p Part) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("a2a: %w: nil Part", ErrInvalidPart)
	}
	switch v := p.(type) {
	case *TextPart:
		return json.Marshal(partWire{Text: &v.Text, Filename: v.Filename, MediaType: v.MediaType, Metadata: v.Metadata})
	case *RawPart:
		return json.Marshal(partWire{Raw: v.Raw, Filename: v.Filename, MediaType: v.MediaType, Metadata: v.Metadata})
	case *URLPart:
		return json.Marshal(partWire{URL: &v.URL, Filename: v.Filename, MediaType: v.MediaType, Metadata: v.Metadata})
	case *DataPart:
		raw, err := json.Marshal(v.Data)
		if err != nil {
			return nil, fmt.Errorf("a2a: DataPart: %w", err)
		}
		return json.Marshal(partWire{Data: raw, Filename: v.Filename, MediaType: v.MediaType, Metadata: v.Metadata})
	default:
		return nil, fmt.Errorf("a2a: %w: unknown concrete Part type %T", ErrInvalidPart, p)
	}
}

// UnmarshalPart decodes a JSON payload into a concrete Part variant.
// Probes the payload in declaration order (text, raw, url, data) and
// returns the first match. Returns ErrInvalidPart when no variant
// fields are present.
func UnmarshalPart(data []byte) (Part, error) {
	var w partWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("a2a: Part: %w", err)
	}
	common := partCommon{Filename: w.Filename, MediaType: w.MediaType, Metadata: w.Metadata}
	switch {
	case w.Text != nil:
		return &TextPart{Text: *w.Text, partCommon: common}, nil
	case w.Raw != nil:
		return &RawPart{Raw: w.Raw, partCommon: common}, nil
	case w.URL != nil:
		return &URLPart{URL: *w.URL, partCommon: common}, nil
	case len(w.Data) > 0:
		var v any
		if err := json.Unmarshal(w.Data, &v); err != nil {
			return nil, fmt.Errorf("a2a: DataPart: %w", err)
		}
		return &DataPart{Data: v, partCommon: common}, nil
	}
	return nil, ErrInvalidPart
}

// Parts is a slice of Part values with a custom JSON encoding so the
// slice round-trips through MarshalPart / UnmarshalPart. Use as the
// type of any `repeated Part` proto field.
type Parts []Part

// MarshalJSON marshals each Part via MarshalPart and emits a JSON array.
func (ps Parts) MarshalJSON() ([]byte, error) {
	out := make([]json.RawMessage, len(ps))
	for i, p := range ps {
		raw, err := MarshalPart(p)
		if err != nil {
			return nil, err
		}
		out[i] = raw
	}
	return json.Marshal(out)
}

// UnmarshalJSON decodes an array of Part variants.
func (ps *Parts) UnmarshalJSON(data []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return fmt.Errorf("a2a: Parts: %w", err)
	}
	out := make(Parts, 0, len(raws))
	for i, r := range raws {
		p, err := UnmarshalPart(r)
		if err != nil {
			return fmt.Errorf("a2a: Parts[%d]: %w", i, err)
		}
		out = append(out, p)
	}
	*ps = out
	return nil
}

// -----------------------------------------------------------------------------
// Task / TaskStatus / Message / Artifact
// -----------------------------------------------------------------------------

// Task is the core unit of action for A2A.
//
// Proto: message Task ("`Task` is the core unit of action for A2A.
// It has a current status and when results are created for the task
// they are stored in the artifact. If there are multiple turns for a
// task, these are stored in history.").
type Task struct {
	// ID — proto: `string id = 1` (REQUIRED). Unique identifier (e.g. UUID).
	ID string `json:"id"`
	// ContextID — proto: `string context_id = 2`.
	ContextID string `json:"context_id,omitempty"`
	// Status — proto: `TaskStatus status = 3` (REQUIRED).
	Status TaskStatus `json:"status"`
	// Artifacts — proto: `repeated Artifact artifacts = 4`.
	Artifacts []Artifact `json:"artifacts,omitempty"`
	// History — proto: `repeated Message history = 5`.
	History []Message `json:"history,omitempty"`
	// Metadata — proto: `google.protobuf.Struct metadata = 6`.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// TaskStatus is a container for the status of a task.
//
// Proto: message TaskStatus.
type TaskStatus struct {
	// State — proto: `TaskState state = 1` (REQUIRED).
	State TaskState `json:"state"`
	// Message — proto: `Message message = 2`.
	Message *Message `json:"message,omitempty"`
	// Timestamp — proto: `google.protobuf.Timestamp timestamp = 3`. ISO 8601.
	Timestamp time.Time `json:"timestamp,omitempty"`
}

// Message is one unit of communication between client and server.
//
// Proto: message Message ("`Message` is one unit of communication
// between client and server. It can be associated with a context
// and/or a task.").
type Message struct {
	// MessageID — proto: `string message_id = 1` (REQUIRED).
	MessageID string `json:"message_id"`
	// ContextID — proto: `string context_id = 2`.
	ContextID string `json:"context_id,omitempty"`
	// TaskID — proto: `string task_id = 3`.
	TaskID string `json:"task_id,omitempty"`
	// Role — proto: `Role role = 4` (REQUIRED).
	Role Role `json:"role"`
	// Parts — proto: `repeated Part parts = 5` (REQUIRED).
	Parts Parts `json:"parts"`
	// Metadata — proto: `google.protobuf.Struct metadata = 6`.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Extensions — proto: `repeated string extensions = 7`.
	Extensions []string `json:"extensions,omitempty"`
	// ReferenceTaskIDs — proto: `repeated string reference_task_ids = 8`.
	ReferenceTaskIDs []string `json:"reference_task_ids,omitempty"`
}

// Artifact represents task outputs.
//
// Proto: message Artifact ("Artifacts represent task outputs.").
type Artifact struct {
	// ArtifactID — proto: `string artifact_id = 1` (REQUIRED).
	ArtifactID string `json:"artifact_id"`
	// Name — proto: `string name = 2`. Human readable name.
	Name string `json:"name,omitempty"`
	// Description — proto: `string description = 3`.
	Description string `json:"description,omitempty"`
	// Parts — proto: `repeated Part parts = 4` (REQUIRED).
	Parts Parts `json:"parts"`
	// Metadata — proto: `google.protobuf.Struct metadata = 5`.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Extensions — proto: `repeated string extensions = 6`.
	Extensions []string `json:"extensions,omitempty"`
}

// -----------------------------------------------------------------------------
// Streaming events (TaskStatusUpdateEvent / TaskArtifactUpdateEvent)
// -----------------------------------------------------------------------------

// TaskStatusUpdateEvent is emitted by the agent to notify the client
// of a change in a task's status.
//
// Proto: message TaskStatusUpdateEvent.
type TaskStatusUpdateEvent struct {
	// TaskID — proto: `string task_id = 1` (REQUIRED).
	TaskID string `json:"task_id"`
	// ContextID — proto: `string context_id = 2` (REQUIRED).
	ContextID string `json:"context_id"`
	// Status — proto: `TaskStatus status = 3` (REQUIRED).
	Status TaskStatus `json:"status"`
	// Metadata — proto: `google.protobuf.Struct metadata = 4`.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// TaskArtifactUpdateEvent is a task delta where an artifact has been
// generated.
//
// Proto: message TaskArtifactUpdateEvent.
type TaskArtifactUpdateEvent struct {
	// TaskID — proto: `string task_id = 1` (REQUIRED).
	TaskID string `json:"task_id"`
	// ContextID — proto: `string context_id = 2` (REQUIRED).
	ContextID string `json:"context_id"`
	// Artifact — proto: `Artifact artifact = 3` (REQUIRED).
	Artifact Artifact `json:"artifact"`
	// Append — proto: `bool append = 4`.
	Append bool `json:"append,omitempty"`
	// LastChunk — proto: `bool last_chunk = 5`.
	LastChunk bool `json:"last_chunk,omitempty"`
	// Metadata — proto: `google.protobuf.Struct metadata = 6`.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// -----------------------------------------------------------------------------
// Configuration / push notifications
// -----------------------------------------------------------------------------

// SendMessageConfiguration is the configuration of a send-message request.
//
// Proto: message SendMessageConfiguration.
type SendMessageConfiguration struct {
	// AcceptedOutputModes — proto: `repeated string accepted_output_modes = 1`.
	AcceptedOutputModes []string `json:"accepted_output_modes,omitempty"`
	// TaskPushNotificationConfig — proto: `TaskPushNotificationConfig task_push_notification_config = 2`.
	TaskPushNotificationConfig *TaskPushNotificationConfig `json:"task_push_notification_config,omitempty"`
	// HistoryLength — proto: `optional int32 history_length = 3`.
	HistoryLength *int32 `json:"history_length,omitempty"`
	// ReturnImmediately — proto: `bool return_immediately = 4`.
	ReturnImmediately bool `json:"return_immediately,omitempty"`
}

// TaskPushNotificationConfig is a per-task push-notification configuration.
//
// Proto: message TaskPushNotificationConfig.
type TaskPushNotificationConfig struct {
	// Tenant — proto: `string tenant = 1`.
	Tenant string `json:"tenant,omitempty"`
	// ID — proto: `string id = 2`. Unique identifier (e.g. UUID) for the config.
	ID string `json:"id,omitempty"`
	// TaskID — proto: `string task_id = 3`.
	TaskID string `json:"task_id,omitempty"`
	// URL — proto: `string url = 4` (REQUIRED).
	URL string `json:"url"`
	// Token — proto: `string token = 5`.
	Token string `json:"token,omitempty"`
	// Authentication — proto: `AuthenticationInfo authentication = 6`.
	Authentication *AuthenticationInfo `json:"authentication,omitempty"`
}

// AuthenticationInfo defines authentication details for push notifications.
//
// Proto: message AuthenticationInfo ("Defines authentication details,
// used for push notifications.").
type AuthenticationInfo struct {
	// Scheme — proto: `string scheme = 1` (REQUIRED). HTTP auth scheme name.
	Scheme string `json:"scheme"`
	// Credentials — proto: `string credentials = 2`.
	Credentials string `json:"credentials,omitempty"`
}

// -----------------------------------------------------------------------------
// Agent metadata
// -----------------------------------------------------------------------------

// Canonical protocol bindings declared by the A2A spec. Open-form
// string elsewhere — agents MAY declare extension bindings.
const (
	// ProtocolBindingJSONRPC — proto: documented `"JSONRPC"` binding.
	ProtocolBindingJSONRPC = "JSONRPC"
	// ProtocolBindingGRPC — proto: documented `"GRPC"` binding.
	ProtocolBindingGRPC = "GRPC"
	// ProtocolBindingHTTPJSON — proto: documented `"HTTP+JSON"` binding.
	ProtocolBindingHTTPJSON = "HTTP+JSON"
)

// AgentInterface declares a target URL + protocol binding + protocol
// version for interacting with an agent.
//
// Proto: message AgentInterface.
type AgentInterface struct {
	// URL — proto: `string url = 1` (REQUIRED).
	URL string `json:"url"`
	// ProtocolBinding — proto: `string protocol_binding = 2` (REQUIRED).
	// Open-form; canonical values are "JSONRPC", "GRPC", "HTTP+JSON".
	ProtocolBinding string `json:"protocol_binding"`
	// Tenant — proto: `string tenant = 3`.
	Tenant string `json:"tenant,omitempty"`
	// ProtocolVersion — proto: `string protocol_version = 4` (REQUIRED).
	ProtocolVersion string `json:"protocol_version"`
}

// AgentCard is the self-describing manifest for an agent.
//
// Proto: message AgentCard ("A self-describing manifest for an agent.
// It provides essential metadata including the agent's identity,
// capabilities, skills, supported communication methods, and security
// requirements.").
type AgentCard struct {
	// Name — proto: `string name = 1` (REQUIRED).
	Name string `json:"name"`
	// Description — proto: `string description = 2` (REQUIRED).
	Description string `json:"description"`
	// SupportedInterfaces — proto: `repeated AgentInterface supported_interfaces = 3` (REQUIRED).
	SupportedInterfaces []AgentInterface `json:"supported_interfaces"`
	// Provider — proto: `AgentProvider provider = 4`.
	Provider *AgentProvider `json:"provider,omitempty"`
	// Version — proto: `string version = 5` (REQUIRED).
	Version string `json:"version"`
	// DocumentationURL — proto: `optional string documentation_url = 6`.
	DocumentationURL string `json:"documentation_url,omitempty"`
	// Capabilities — proto: `AgentCapabilities capabilities = 7` (REQUIRED).
	Capabilities AgentCapabilities `json:"capabilities"`
	// SecuritySchemes — proto: `map<string, SecurityScheme> security_schemes = 8`.
	// Use the SecuritySchemeMap typed wrapper so the discriminated-union
	// JSON round-trip dispatches automatically.
	SecuritySchemes SecuritySchemeMap `json:"security_schemes,omitempty"`
	// SecurityRequirements — proto: `repeated SecurityRequirement security_requirements = 9`.
	SecurityRequirements []SecurityRequirement `json:"security_requirements,omitempty"`
	// DefaultInputModes — proto: `repeated string default_input_modes = 10` (REQUIRED).
	DefaultInputModes []string `json:"default_input_modes"`
	// DefaultOutputModes — proto: `repeated string default_output_modes = 11` (REQUIRED).
	DefaultOutputModes []string `json:"default_output_modes"`
	// Skills — proto: `repeated AgentSkill skills = 12` (REQUIRED).
	Skills []AgentSkill `json:"skills"`
	// Signatures — proto: `repeated AgentCardSignature signatures = 13`.
	Signatures []AgentCardSignature `json:"signatures,omitempty"`
	// IconURL — proto: `optional string icon_url = 14`.
	IconURL string `json:"icon_url,omitempty"`
}

// AgentProvider represents the service provider of an agent.
//
// Proto: message AgentProvider.
type AgentProvider struct {
	// URL — proto: `string url = 1` (REQUIRED).
	URL string `json:"url"`
	// Organization — proto: `string organization = 2` (REQUIRED).
	Organization string `json:"organization"`
}

// AgentCapabilities defines optional capabilities supported by an agent.
//
// Proto: message AgentCapabilities.
type AgentCapabilities struct {
	// Streaming — proto: `optional bool streaming = 1`.
	Streaming *bool `json:"streaming,omitempty"`
	// PushNotifications — proto: `optional bool push_notifications = 2`.
	PushNotifications *bool `json:"push_notifications,omitempty"`
	// Extensions — proto: `repeated AgentExtension extensions = 3`.
	Extensions []AgentExtension `json:"extensions,omitempty"`
	// ExtendedAgentCard — proto: `optional bool extended_agent_card = 4`.
	ExtendedAgentCard *bool `json:"extended_agent_card,omitempty"`
}

// AgentExtension declares a protocol extension supported by an agent.
//
// Proto: message AgentExtension.
type AgentExtension struct {
	// URI — proto: `string uri = 1`.
	URI string `json:"uri,omitempty"`
	// Description — proto: `string description = 2`.
	Description string `json:"description,omitempty"`
	// Required — proto: `bool required = 3`.
	Required bool `json:"required,omitempty"`
	// Params — proto: `google.protobuf.Struct params = 4`.
	Params map[string]any `json:"params,omitempty"`
}

// AgentSkill represents a distinct capability that an agent can perform.
//
// Proto: message AgentSkill.
type AgentSkill struct {
	// ID — proto: `string id = 1` (REQUIRED).
	ID string `json:"id"`
	// Name — proto: `string name = 2` (REQUIRED).
	Name string `json:"name"`
	// Description — proto: `string description = 3` (REQUIRED).
	Description string `json:"description"`
	// Tags — proto: `repeated string tags = 4` (REQUIRED).
	Tags []string `json:"tags"`
	// Examples — proto: `repeated string examples = 5`.
	Examples []string `json:"examples,omitempty"`
	// InputModes — proto: `repeated string input_modes = 6`.
	InputModes []string `json:"input_modes,omitempty"`
	// OutputModes — proto: `repeated string output_modes = 7`.
	OutputModes []string `json:"output_modes,omitempty"`
	// SecurityRequirements — proto: `repeated SecurityRequirement security_requirements = 8`.
	SecurityRequirements []SecurityRequirement `json:"security_requirements,omitempty"`
}

// AgentCardSignature represents a JWS signature of an AgentCard.
//
// Proto: message AgentCardSignature ("This follows the JSON format of
// an RFC 7515 JSON Web Signature (JWS).").
type AgentCardSignature struct {
	// Protected — proto: `string protected = 1` (REQUIRED). Base64url-encoded JSON object.
	Protected string `json:"protected"`
	// Signature — proto: `string signature = 2` (REQUIRED). Base64url-encoded.
	Signature string `json:"signature"`
	// Header — proto: `google.protobuf.Struct header = 3`.
	Header map[string]any `json:"header,omitempty"`
}

// -----------------------------------------------------------------------------
// Security schemes — discriminated union (proto: SecurityScheme oneof)
// -----------------------------------------------------------------------------

// SecurityScheme is the proto SecurityScheme oneof Go interface. One
// concrete variant per scheme; the JSON wire form is a flat object
// per OpenAPI 3.2 conventions.
//
// Proto: message SecurityScheme.
type SecurityScheme interface {
	// Kind returns the variant discriminator (e.g. "apiKey", "http", "oauth2", "openIdConnect", "mutualTLS").
	Kind() string
	isSecurityScheme()
}

// SecurityScheme kind discriminators. Names mirror the OpenAPI
// Security Scheme Object's `type` field so JSON producers can use the
// same vocabulary.
const (
	// SecurityKindAPIKey — proto: APIKeySecurityScheme.
	SecurityKindAPIKey = "apiKey"
	// SecurityKindHTTPAuth — proto: HTTPAuthSecurityScheme.
	SecurityKindHTTPAuth = "http"
	// SecurityKindOAuth2 — proto: OAuth2SecurityScheme.
	SecurityKindOAuth2 = "oauth2"
	// SecurityKindOpenIDConnect — proto: OpenIdConnectSecurityScheme.
	SecurityKindOpenIDConnect = "openIdConnect"
	// SecurityKindMutualTLS — proto: MutualTlsSecurityScheme.
	SecurityKindMutualTLS = "mutualTLS"
)

// APIKeySecurityScheme defines a security scheme using an API key.
//
// Proto: message APIKeySecurityScheme.
type APIKeySecurityScheme struct {
	// Description — proto: `string description = 1`.
	Description string `json:"description,omitempty"`
	// Location — proto: `string location = 2` (REQUIRED). "query", "header", or "cookie".
	Location string `json:"in"`
	// Name — proto: `string name = 3` (REQUIRED).
	Name string `json:"name"`
}

// Kind returns SecurityKindAPIKey.
func (s *APIKeySecurityScheme) Kind() string      { return SecurityKindAPIKey }
func (s *APIKeySecurityScheme) isSecurityScheme() {}

// HTTPAuthSecurityScheme defines a security scheme using HTTP authentication.
//
// Proto: message HTTPAuthSecurityScheme.
type HTTPAuthSecurityScheme struct {
	// Description — proto: `string description = 1`.
	Description string `json:"description,omitempty"`
	// Scheme — proto: `string scheme = 2` (REQUIRED). e.g. "Bearer".
	Scheme string `json:"scheme"`
	// BearerFormat — proto: `string bearer_format = 3`. e.g. "JWT".
	BearerFormat string `json:"bearerFormat,omitempty"`
}

// Kind returns SecurityKindHTTPAuth.
func (s *HTTPAuthSecurityScheme) Kind() string      { return SecurityKindHTTPAuth }
func (s *HTTPAuthSecurityScheme) isSecurityScheme() {}

// OAuth2SecurityScheme defines a security scheme using OAuth 2.0.
//
// Proto: message OAuth2SecurityScheme.
type OAuth2SecurityScheme struct {
	// Description — proto: `string description = 1`.
	Description string `json:"description,omitempty"`
	// Flows — proto: `OAuthFlows flows = 2` (REQUIRED).
	Flows OAuthFlows `json:"flows"`
	// OAuth2MetadataURL — proto: `string oauth2_metadata_url = 3`. RFC 8414 metadata URL.
	OAuth2MetadataURL string `json:"oauth2MetadataUrl,omitempty"`
}

// Kind returns SecurityKindOAuth2.
func (s *OAuth2SecurityScheme) Kind() string      { return SecurityKindOAuth2 }
func (s *OAuth2SecurityScheme) isSecurityScheme() {}

// MarshalJSON encodes OAuth2SecurityScheme with the OAuthFlows oneof
// represented as the OpenAPI flow-keyed object.
func (s *OAuth2SecurityScheme) MarshalJSON() ([]byte, error) {
	type alias struct {
		Description       string          `json:"description,omitempty"`
		Flows             *OAuthFlowsWire `json:"flows"`
		OAuth2MetadataURL string          `json:"oauth2MetadataUrl,omitempty"`
	}
	w, err := flowsToWire(s.Flows)
	if err != nil {
		return nil, err
	}
	return json.Marshal(alias{Description: s.Description, Flows: w, OAuth2MetadataURL: s.OAuth2MetadataURL})
}

// UnmarshalJSON decodes OAuth2SecurityScheme, dispatching the OAuthFlows
// variant via flowsFromWire.
func (s *OAuth2SecurityScheme) UnmarshalJSON(data []byte) error {
	type alias struct {
		Description       string          `json:"description"`
		Flows             *OAuthFlowsWire `json:"flows"`
		OAuth2MetadataURL string          `json:"oauth2MetadataUrl"`
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("a2a: OAuth2SecurityScheme: %w", err)
	}
	flows, err := flowsFromWire(a.Flows)
	if err != nil {
		return err
	}
	s.Description = a.Description
	s.Flows = flows
	s.OAuth2MetadataURL = a.OAuth2MetadataURL
	return nil
}

// OpenIdConnectSecurityScheme defines a security scheme using OIDC.
//
// Proto: message OpenIdConnectSecurityScheme.
//
//nolint:stylecheck // ST1003: name is transcribed verbatim from the A2A proto message OpenIdConnectSecurityScheme; keeping it in sync with the proto contract wins over Go initialism style.
type OpenIdConnectSecurityScheme struct {
	// Description — proto: `string description = 1`.
	Description string `json:"description,omitempty"`
	// OpenIDConnectURL — proto: `string open_id_connect_url = 2` (REQUIRED).
	OpenIDConnectURL string `json:"openIdConnectUrl"`
}

// Kind returns SecurityKindOpenIDConnect.
func (s *OpenIdConnectSecurityScheme) Kind() string      { return SecurityKindOpenIDConnect }
func (s *OpenIdConnectSecurityScheme) isSecurityScheme() {}

// MutualTlsSecurityScheme defines a security scheme using mTLS.
//
// Proto: message MutualTlsSecurityScheme.
//
//nolint:stylecheck // ST1003: name is transcribed verbatim from the A2A proto message MutualTlsSecurityScheme; keeping it in sync with the proto contract wins over Go initialism style.
type MutualTlsSecurityScheme struct {
	// Description — proto: `string description = 1`.
	Description string `json:"description,omitempty"`
}

// Kind returns SecurityKindMutualTLS.
func (s *MutualTlsSecurityScheme) Kind() string      { return SecurityKindMutualTLS }
func (s *MutualTlsSecurityScheme) isSecurityScheme() {}

// securitySchemeWire is the on-the-wire JSON form used to dispatch
// SecurityScheme unmarshalling. The "type" field is the OpenAPI-style
// discriminator; the rest is the variant-specific shape.
type securitySchemeWire struct {
	Type string `json:"type"`
	// API key fields.
	In   string `json:"in,omitempty"`
	Name string `json:"name,omitempty"`
	// HTTP auth fields.
	Scheme       string `json:"scheme,omitempty"`
	BearerFormat string `json:"bearerFormat,omitempty"`
	// OAuth2 fields.
	Flows             *OAuthFlowsWire `json:"flows,omitempty"`
	OAuth2MetadataURL string          `json:"oauth2MetadataUrl,omitempty"`
	// OIDC fields.
	OpenIDConnectURL string `json:"openIdConnectUrl,omitempty"`
	// Common.
	Description string `json:"description,omitempty"`
}

// MarshalSecurityScheme encodes a SecurityScheme variant with an
// OpenAPI-style `"type":...` discriminator.
func MarshalSecurityScheme(s SecurityScheme) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("a2a: %w: nil SecurityScheme", ErrInvalidOneof)
	}
	switch v := s.(type) {
	case *APIKeySecurityScheme:
		return json.Marshal(securitySchemeWire{Type: SecurityKindAPIKey, Description: v.Description, In: v.Location, Name: v.Name})
	case *HTTPAuthSecurityScheme:
		return json.Marshal(securitySchemeWire{Type: SecurityKindHTTPAuth, Description: v.Description, Scheme: v.Scheme, BearerFormat: v.BearerFormat})
	case *OAuth2SecurityScheme:
		fw, err := flowsToWire(v.Flows)
		if err != nil {
			return nil, err
		}
		return json.Marshal(securitySchemeWire{Type: SecurityKindOAuth2, Description: v.Description, Flows: fw, OAuth2MetadataURL: v.OAuth2MetadataURL})
	case *OpenIdConnectSecurityScheme:
		return json.Marshal(securitySchemeWire{Type: SecurityKindOpenIDConnect, Description: v.Description, OpenIDConnectURL: v.OpenIDConnectURL})
	case *MutualTlsSecurityScheme:
		return json.Marshal(securitySchemeWire{Type: SecurityKindMutualTLS, Description: v.Description})
	default:
		return nil, fmt.Errorf("a2a: %w: unknown SecurityScheme %T", ErrInvalidOneof, s)
	}
}

// UnmarshalSecurityScheme decodes a SecurityScheme variant.
func UnmarshalSecurityScheme(data []byte) (SecurityScheme, error) {
	var w securitySchemeWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("a2a: SecurityScheme: %w", err)
	}
	switch w.Type {
	case SecurityKindAPIKey:
		return &APIKeySecurityScheme{Description: w.Description, Location: w.In, Name: w.Name}, nil
	case SecurityKindHTTPAuth:
		return &HTTPAuthSecurityScheme{Description: w.Description, Scheme: w.Scheme, BearerFormat: w.BearerFormat}, nil
	case SecurityKindOAuth2:
		flows, err := flowsFromWire(w.Flows)
		if err != nil {
			return nil, err
		}
		return &OAuth2SecurityScheme{Description: w.Description, Flows: flows, OAuth2MetadataURL: w.OAuth2MetadataURL}, nil
	case SecurityKindOpenIDConnect:
		return &OpenIdConnectSecurityScheme{Description: w.Description, OpenIDConnectURL: w.OpenIDConnectURL}, nil
	case SecurityKindMutualTLS:
		return &MutualTlsSecurityScheme{Description: w.Description}, nil
	default:
		return nil, fmt.Errorf("a2a: %w: unknown SecurityScheme type %q", ErrInvalidOneof, w.Type)
	}
}

// securitySchemeMapWire is the per-key shape inside an `AgentCard.SecuritySchemes` map.
// The shipped struct uses a custom map type so the JSON envelope auto-dispatches
// via the wire helpers.
//
// AgentCard.SecuritySchemes uses map[string]SecurityScheme (interface).
// JSON unmarshalling needs help: define MarshalJSON / UnmarshalJSON on
// the surrounding AgentCard? Instead, we provide explicit helpers so
// callers can round-trip a map. The simplest path: expose
// MarshalSecuritySchemeMap / UnmarshalSecuritySchemeMap as named.
//
// To avoid AgentCard custom Marshal/Unmarshal, we shadow the field via
// a named map type whose JSON methods do the dispatch.

// SecuritySchemeMap is a typed wrapper over `map[string]SecurityScheme`
// that round-trips through MarshalSecurityScheme / UnmarshalSecurityScheme.
type SecuritySchemeMap map[string]SecurityScheme

// MarshalJSON implements the json.Marshaler contract for the map.
func (m SecuritySchemeMap) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		raw, err := MarshalSecurityScheme(v)
		if err != nil {
			return nil, err
		}
		out[k] = raw
	}
	return json.Marshal(out)
}

// UnmarshalJSON implements the json.Unmarshaler contract for the map.
func (m *SecuritySchemeMap) UnmarshalJSON(data []byte) error {
	var raws map[string]json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return fmt.Errorf("a2a: SecuritySchemeMap: %w", err)
	}
	out := make(SecuritySchemeMap, len(raws))
	for k, r := range raws {
		s, err := UnmarshalSecurityScheme(r)
		if err != nil {
			return fmt.Errorf("a2a: SecuritySchemeMap[%q]: %w", k, err)
		}
		out[k] = s
	}
	*m = out
	return nil
}

// SecurityRequirement defines the security requirements for an agent.
//
// Proto: message SecurityRequirement.
type SecurityRequirement struct {
	// Schemes — proto: `map<string, StringList> schemes = 1`.
	Schemes map[string]StringList `json:"schemes,omitempty"`
}

// StringList is a list of strings.
//
// Proto: message StringList.
type StringList struct {
	// List — proto: `repeated string list = 1`.
	List []string `json:"list,omitempty"`
}

// -----------------------------------------------------------------------------
// OAuth flows — discriminated union (proto: OAuthFlows oneof)
// -----------------------------------------------------------------------------

// OAuthFlows is the proto OAuthFlows oneof Go interface. Each concrete
// flow variant satisfies this; runtime discrimination via `Kind()`.
//
// Proto: message OAuthFlows.
type OAuthFlows interface {
	// Kind returns the variant discriminator.
	Kind() string
	isOAuthFlows()
}

// OAuthFlow kind discriminators. Names mirror the OpenAPI 3.2 Security
// Scheme Object's flow names.
const (
	// OAuthFlowKindAuthorizationCode — proto: AuthorizationCodeOAuthFlow.
	OAuthFlowKindAuthorizationCode = "authorizationCode"
	// OAuthFlowKindClientCredentials — proto: ClientCredentialsOAuthFlow.
	OAuthFlowKindClientCredentials = "clientCredentials"
	// OAuthFlowKindImplicit — proto: ImplicitOAuthFlow (deprecated).
	OAuthFlowKindImplicit = "implicit"
	// OAuthFlowKindPassword — proto: PasswordOAuthFlow (deprecated).
	OAuthFlowKindPassword = "password"
	// OAuthFlowKindDeviceCode — proto: DeviceCodeOAuthFlow.
	OAuthFlowKindDeviceCode = "deviceCode"
)

// AuthorizationCodeOAuthFlow is the Authorization Code flow.
//
// Proto: message AuthorizationCodeOAuthFlow.
type AuthorizationCodeOAuthFlow struct {
	// AuthorizationURL — proto: `string authorization_url = 1` (REQUIRED).
	AuthorizationURL string `json:"authorizationUrl"`
	// TokenURL — proto: `string token_url = 2` (REQUIRED).
	TokenURL string `json:"tokenUrl"`
	// RefreshURL — proto: `string refresh_url = 3`.
	RefreshURL string `json:"refreshUrl,omitempty"`
	// Scopes — proto: `map<string, string> scopes = 4` (REQUIRED).
	Scopes map[string]string `json:"scopes"`
	// PKCERequired — proto: `bool pkce_required = 5`.
	PKCERequired bool `json:"pkceRequired,omitempty"`
}

// Kind returns OAuthFlowKindAuthorizationCode.
func (f *AuthorizationCodeOAuthFlow) Kind() string  { return OAuthFlowKindAuthorizationCode }
func (f *AuthorizationCodeOAuthFlow) isOAuthFlows() {}

// ClientCredentialsOAuthFlow is the Client Credentials flow.
//
// Proto: message ClientCredentialsOAuthFlow.
type ClientCredentialsOAuthFlow struct {
	// TokenURL — proto: `string token_url = 1` (REQUIRED).
	TokenURL string `json:"tokenUrl"`
	// RefreshURL — proto: `string refresh_url = 2`.
	RefreshURL string `json:"refreshUrl,omitempty"`
	// Scopes — proto: `map<string, string> scopes = 3` (REQUIRED).
	Scopes map[string]string `json:"scopes"`
}

// Kind returns OAuthFlowKindClientCredentials.
func (f *ClientCredentialsOAuthFlow) Kind() string  { return OAuthFlowKindClientCredentials }
func (f *ClientCredentialsOAuthFlow) isOAuthFlows() {}

// ImplicitOAuthFlow is the (deprecated) Implicit flow. Included for
// spec parity per the vendored proto.
//
// Proto: message ImplicitOAuthFlow ("Deprecated: Use Authorization Code + PKCE instead.").
//
// Deprecated: use AuthorizationCodeOAuthFlow with PKCE.
type ImplicitOAuthFlow struct {
	// AuthorizationURL — proto: `string authorization_url = 1`.
	AuthorizationURL string `json:"authorizationUrl,omitempty"`
	// RefreshURL — proto: `string refresh_url = 2`.
	RefreshURL string `json:"refreshUrl,omitempty"`
	// Scopes — proto: `map<string, string> scopes = 3`.
	Scopes map[string]string `json:"scopes,omitempty"`
}

// Kind returns OAuthFlowKindImplicit.
func (f *ImplicitOAuthFlow) Kind() string  { return OAuthFlowKindImplicit }
func (f *ImplicitOAuthFlow) isOAuthFlows() {}

// PasswordOAuthFlow is the (deprecated) Resource Owner Password
// Credentials flow. Included for spec parity per the vendored proto.
//
// Proto: message PasswordOAuthFlow ("Deprecated: Use Authorization Code + PKCE or Device Code.").
//
// Deprecated: use AuthorizationCodeOAuthFlow with PKCE or DeviceCodeOAuthFlow.
type PasswordOAuthFlow struct {
	// TokenURL — proto: `string token_url = 1`.
	TokenURL string `json:"tokenUrl,omitempty"`
	// RefreshURL — proto: `string refresh_url = 2`.
	RefreshURL string `json:"refreshUrl,omitempty"`
	// Scopes — proto: `map<string, string> scopes = 3`.
	Scopes map[string]string `json:"scopes,omitempty"`
}

// Kind returns OAuthFlowKindPassword.
func (f *PasswordOAuthFlow) Kind() string  { return OAuthFlowKindPassword }
func (f *PasswordOAuthFlow) isOAuthFlows() {}

// DeviceCodeOAuthFlow is the OAuth 2.0 Device Code flow (RFC 8628).
//
// Proto: message DeviceCodeOAuthFlow.
type DeviceCodeOAuthFlow struct {
	// DeviceAuthorizationURL — proto: `string device_authorization_url = 1` (REQUIRED).
	DeviceAuthorizationURL string `json:"deviceAuthorizationUrl"`
	// TokenURL — proto: `string token_url = 2` (REQUIRED).
	TokenURL string `json:"tokenUrl"`
	// RefreshURL — proto: `string refresh_url = 3`.
	RefreshURL string `json:"refreshUrl,omitempty"`
	// Scopes — proto: `map<string, string> scopes = 4` (REQUIRED).
	Scopes map[string]string `json:"scopes"`
}

// Kind returns OAuthFlowKindDeviceCode.
func (f *DeviceCodeOAuthFlow) Kind() string  { return OAuthFlowKindDeviceCode }
func (f *DeviceCodeOAuthFlow) isOAuthFlows() {}

// OAuthFlowsWire is the JSON envelope used by OAuth2SecurityScheme to
// carry the proto OAuthFlows oneof. Per the OpenAPI 3.2 spec, all
// flows are objects under named keys ("authorizationCode",
// "clientCredentials", "implicit", "password", "deviceCode"); exactly
// one is non-nil for a valid scheme.
type OAuthFlowsWire struct {
	AuthorizationCode *AuthorizationCodeOAuthFlow `json:"authorizationCode,omitempty"`
	ClientCredentials *ClientCredentialsOAuthFlow `json:"clientCredentials,omitempty"`
	// Implicit is deprecated; retained for spec parity.
	Implicit *ImplicitOAuthFlow `json:"implicit,omitempty"`
	// Password is deprecated; retained for spec parity.
	Password   *PasswordOAuthFlow   `json:"password,omitempty"`
	DeviceCode *DeviceCodeOAuthFlow `json:"deviceCode,omitempty"`
}

func flowsToWire(f OAuthFlows) (*OAuthFlowsWire, error) {
	if f == nil {
		return nil, fmt.Errorf("a2a: %w: nil OAuthFlows", ErrInvalidOneof)
	}
	w := &OAuthFlowsWire{}
	switch v := f.(type) {
	case *AuthorizationCodeOAuthFlow:
		w.AuthorizationCode = v
	case *ClientCredentialsOAuthFlow:
		w.ClientCredentials = v
	case *ImplicitOAuthFlow:
		w.Implicit = v
	case *PasswordOAuthFlow:
		w.Password = v
	case *DeviceCodeOAuthFlow:
		w.DeviceCode = v
	default:
		return nil, fmt.Errorf("a2a: %w: unknown OAuthFlows %T", ErrInvalidOneof, f)
	}
	return w, nil
}

func flowsFromWire(w *OAuthFlowsWire) (OAuthFlows, error) {
	if w == nil {
		return nil, fmt.Errorf("a2a: %w: missing flows", ErrInvalidOneof)
	}
	switch {
	case w.AuthorizationCode != nil:
		return w.AuthorizationCode, nil
	case w.ClientCredentials != nil:
		return w.ClientCredentials, nil
	case w.Implicit != nil:
		return w.Implicit, nil
	case w.Password != nil:
		return w.Password, nil
	case w.DeviceCode != nil:
		return w.DeviceCode, nil
	}
	return nil, fmt.Errorf("a2a: %w: empty OAuthFlows", ErrInvalidOneof)
}

// MarshalOAuthFlows encodes an OAuthFlows variant.
func MarshalOAuthFlows(f OAuthFlows) ([]byte, error) {
	w, err := flowsToWire(f)
	if err != nil {
		return nil, err
	}
	return json.Marshal(w)
}

// UnmarshalOAuthFlows decodes an OAuthFlows variant.
func UnmarshalOAuthFlows(data []byte) (OAuthFlows, error) {
	var w OAuthFlowsWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("a2a: OAuthFlows: %w", err)
	}
	return flowsFromWire(&w)
}

// -----------------------------------------------------------------------------
// Request / Response envelopes (referenced by A2AService RPCs)
// -----------------------------------------------------------------------------

// SendMessageRequest represents a request for the SendMessage method.
//
// Proto: message SendMessageRequest.
type SendMessageRequest struct {
	// Tenant — proto: `string tenant = 1`. Provided as path parameter.
	Tenant string `json:"tenant,omitempty"`
	// Message — proto: `Message message = 2` (REQUIRED).
	Message Message `json:"message"`
	// Configuration — proto: `SendMessageConfiguration configuration = 3`.
	Configuration *SendMessageConfiguration `json:"configuration,omitempty"`
	// Metadata — proto: `google.protobuf.Struct metadata = 4`.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// SendMessageResponse represents the response for the SendMessage method.
//
// Proto: message SendMessageResponse (oneof payload { Task task; Message message; }).
//
// Discriminated-union semantics: exactly one of Task or Message is
// non-nil on a valid response. `Kind()` returns "task" or "message".
type SendMessageResponse struct {
	// Task — proto: `Task task = 1` variant.
	Task *Task `json:"task,omitempty"`
	// Message — proto: `Message message = 2` variant.
	Message *Message `json:"message,omitempty"`
}

// SendMessageResponse kind discriminators.
const (
	// SendMessageResponseKindTask — the response carries a Task.
	SendMessageResponseKindTask = "task"
	// SendMessageResponseKindMessage — the response carries a Message.
	SendMessageResponseKindMessage = "message"
)

// Kind returns the variant discriminator. Empty when neither variant
// is set (an invalid state).
func (r SendMessageResponse) Kind() string {
	switch {
	case r.Task != nil:
		return SendMessageResponseKindTask
	case r.Message != nil:
		return SendMessageResponseKindMessage
	}
	return ""
}

// GetTaskRequest represents a request for the GetTask method.
//
// Proto: message GetTaskRequest.
type GetTaskRequest struct {
	// Tenant — proto: `string tenant = 1`.
	Tenant string `json:"tenant,omitempty"`
	// ID — proto: `string id = 2` (REQUIRED). Resource ID of the task.
	ID string `json:"id"`
	// HistoryLength — proto: `optional int32 history_length = 3`.
	HistoryLength *int32 `json:"history_length,omitempty"`
}

// ListTasksRequest is the parameters for listing tasks with optional filters.
//
// Proto: message ListTasksRequest.
type ListTasksRequest struct {
	// Tenant — proto: `string tenant = 1`.
	Tenant string `json:"tenant,omitempty"`
	// ContextID — proto: `string context_id = 2`. Filter by context.
	ContextID string `json:"context_id,omitempty"`
	// Status — proto: `TaskState status = 3`. Filter by status.
	Status TaskState `json:"status,omitempty"`
	// PageSize — proto: `optional int32 page_size = 4`. Max 100; default 50.
	PageSize *int32 `json:"page_size,omitempty"`
	// PageToken — proto: `string page_token = 5`.
	PageToken string `json:"page_token,omitempty"`
	// HistoryLength — proto: `optional int32 history_length = 6`.
	HistoryLength *int32 `json:"history_length,omitempty"`
	// StatusTimestampAfter — proto: `google.protobuf.Timestamp status_timestamp_after = 7`.
	StatusTimestampAfter time.Time `json:"status_timestamp_after,omitempty"`
	// IncludeArtifacts — proto: `optional bool include_artifacts = 8`.
	IncludeArtifacts *bool `json:"include_artifacts,omitempty"`
}

// ListTasksResponse is the result object for ListTasks.
//
// Proto: message ListTasksResponse.
type ListTasksResponse struct {
	// Tasks — proto: `repeated Task tasks = 1` (REQUIRED).
	Tasks []Task `json:"tasks"`
	// NextPageToken — proto: `string next_page_token = 2` (REQUIRED).
	NextPageToken string `json:"next_page_token"`
	// PageSize — proto: `int32 page_size = 3` (REQUIRED).
	PageSize int32 `json:"page_size"`
	// TotalSize — proto: `int32 total_size = 4` (REQUIRED).
	TotalSize int32 `json:"total_size"`
}

// CancelTaskRequest represents a request for the CancelTask method.
//
// Proto: message CancelTaskRequest.
type CancelTaskRequest struct {
	// Tenant — proto: `string tenant = 1`.
	Tenant string `json:"tenant,omitempty"`
	// ID — proto: `string id = 2` (REQUIRED).
	ID string `json:"id"`
	// Metadata — proto: `google.protobuf.Struct metadata = 3`.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// SubscribeToTaskRequest represents a request for the SubscribeToTask method.
//
// Proto: message SubscribeToTaskRequest.
type SubscribeToTaskRequest struct {
	// Tenant — proto: `string tenant = 1`.
	Tenant string `json:"tenant,omitempty"`
	// ID — proto: `string id = 2` (REQUIRED).
	ID string `json:"id"`
}

// GetTaskPushNotificationConfigRequest represents a request for the
// GetTaskPushNotificationConfig method.
//
// Proto: message GetTaskPushNotificationConfigRequest.
type GetTaskPushNotificationConfigRequest struct {
	// Tenant — proto: `string tenant = 1`.
	Tenant string `json:"tenant,omitempty"`
	// TaskID — proto: `string task_id = 2` (REQUIRED).
	TaskID string `json:"task_id"`
	// ID — proto: `string id = 3` (REQUIRED).
	ID string `json:"id"`
}

// ListTaskPushNotificationConfigsRequest represents a request for the
// ListTaskPushNotificationConfigs method.
//
// Proto: message ListTaskPushNotificationConfigsRequest.
type ListTaskPushNotificationConfigsRequest struct {
	// Tenant — proto: `string tenant = 4`.
	Tenant string `json:"tenant,omitempty"`
	// TaskID — proto: `string task_id = 1` (REQUIRED).
	TaskID string `json:"task_id"`
	// PageSize — proto: `int32 page_size = 2`.
	PageSize int32 `json:"page_size,omitempty"`
	// PageToken — proto: `string page_token = 3`.
	PageToken string `json:"page_token,omitempty"`
}

// ListTaskPushNotificationConfigsResponse is the successful response
// for ListTaskPushNotificationConfigs.
//
// Proto: message ListTaskPushNotificationConfigsResponse.
type ListTaskPushNotificationConfigsResponse struct {
	// Configs — proto: `repeated TaskPushNotificationConfig configs = 1`.
	Configs []TaskPushNotificationConfig `json:"configs,omitempty"`
	// NextPageToken — proto: `string next_page_token = 2`.
	NextPageToken string `json:"next_page_token,omitempty"`
}

// DeleteTaskPushNotificationConfigRequest represents a request for the
// DeleteTaskPushNotificationConfig method.
//
// Proto: message DeleteTaskPushNotificationConfigRequest.
type DeleteTaskPushNotificationConfigRequest struct {
	// Tenant — proto: `string tenant = 1`.
	Tenant string `json:"tenant,omitempty"`
	// TaskID — proto: `string task_id = 2` (REQUIRED).
	TaskID string `json:"task_id"`
	// ID — proto: `string id = 3` (REQUIRED).
	ID string `json:"id"`
}

// GetExtendedAgentCardRequest represents a request for the
// GetExtendedAgentCard method.
//
// Proto: message GetExtendedAgentCardRequest.
type GetExtendedAgentCardRequest struct {
	// Tenant — proto: `string tenant = 1`.
	Tenant string `json:"tenant,omitempty"`
}

// -----------------------------------------------------------------------------
// StreamResponse — discriminated union (proto: StreamResponse oneof)
// -----------------------------------------------------------------------------

// StreamResponse is the wrapper object used in streaming operations.
// Holds one of: Task, Message, TaskStatusUpdateEvent, TaskArtifactUpdateEvent.
//
// Proto: message StreamResponse.
//
// Exactly one field is non-nil on a valid response. `Kind()` returns
// the variant discriminator.
type StreamResponse struct {
	// Task — proto: `Task task = 1`.
	Task *Task `json:"task,omitempty"`
	// Message — proto: `Message message = 2`.
	Message *Message `json:"message,omitempty"`
	// StatusUpdate — proto: `TaskStatusUpdateEvent status_update = 3`.
	StatusUpdate *TaskStatusUpdateEvent `json:"status_update,omitempty"`
	// ArtifactUpdate — proto: `TaskArtifactUpdateEvent artifact_update = 4`.
	ArtifactUpdate *TaskArtifactUpdateEvent `json:"artifact_update,omitempty"`
}

// StreamResponse kind discriminators.
const (
	// StreamResponseKindTask — Task variant.
	StreamResponseKindTask = "task"
	// StreamResponseKindMessage — Message variant.
	StreamResponseKindMessage = "message"
	// StreamResponseKindStatusUpdate — TaskStatusUpdateEvent variant.
	StreamResponseKindStatusUpdate = "status_update"
	// StreamResponseKindArtifactUpdate — TaskArtifactUpdateEvent variant.
	StreamResponseKindArtifactUpdate = "artifact_update"
)

// Kind returns the variant discriminator, or empty when no variant set.
func (r StreamResponse) Kind() string {
	switch {
	case r.Task != nil:
		return StreamResponseKindTask
	case r.Message != nil:
		return StreamResponseKindMessage
	case r.StatusUpdate != nil:
		return StreamResponseKindStatusUpdate
	case r.ArtifactUpdate != nil:
		return StreamResponseKindArtifactUpdate
	}
	return ""
}

// Validate reports whether exactly one variant is set. Returns
// ErrInvalidOneof when zero or more than one variant is non-nil.
func (r StreamResponse) Validate() error {
	count := 0
	if r.Task != nil {
		count++
	}
	if r.Message != nil {
		count++
	}
	if r.StatusUpdate != nil {
		count++
	}
	if r.ArtifactUpdate != nil {
		count++
	}
	if count != 1 {
		return fmt.Errorf("a2a: %w: StreamResponse must set exactly one variant, set=%d", ErrInvalidOneof, count)
	}
	return nil
}
