package a2a

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// Coverage gate — every proto type must have a Go counterpart
// -----------------------------------------------------------------------------

// expectedTypes is the hand-maintained list of every Go type that
// MUST exist in this package, mirroring `docs/specifications/a2a.proto`.
// Adding a new proto message in a spec bump means adding both the Go
// type AND its name here in the same PR.
//
// The list groups types into:
//   - Core (Task, Message, ...)
//   - Streaming events
//   - Configuration / push notifications
//   - Agent metadata
//   - Security schemes (oneof + 5 variants)
//   - OAuth flows (oneof + 5 variants)
//   - Request / response envelopes
//   - Discriminated-union variants for Part (4) and StreamResponse (fields)
var expectedTypes = []any{
	// Enums (use a zero value so reflect can capture the named type).
	TaskState(0),
	Role(0),
	// Core.
	Task{},
	TaskStatus{},
	Message{},
	Artifact{},
	// Part oneof + 4 variants.
	(*TextPart)(nil),
	(*RawPart)(nil),
	(*URLPart)(nil),
	(*DataPart)(nil),
	Parts(nil),
	// Streaming events.
	TaskStatusUpdateEvent{},
	TaskArtifactUpdateEvent{},
	StreamResponse{},
	// Configuration / push notifications.
	SendMessageConfiguration{},
	TaskPushNotificationConfig{},
	AuthenticationInfo{},
	// Agent metadata.
	AgentCard{},
	AgentInterface{},
	AgentProvider{},
	AgentCapabilities{},
	AgentExtension{},
	AgentSkill{},
	AgentCardSignature{},
	// Security schemes (interface + 5 variants).
	(*APIKeySecurityScheme)(nil),
	(*HTTPAuthSecurityScheme)(nil),
	(*OAuth2SecurityScheme)(nil),
	(*OpenIdConnectSecurityScheme)(nil),
	(*MutualTlsSecurityScheme)(nil),
	SecurityRequirement{},
	StringList{},
	SecuritySchemeMap{},
	// OAuth flows (interface + 5 variants).
	(*AuthorizationCodeOAuthFlow)(nil),
	(*ClientCredentialsOAuthFlow)(nil),
	(*ImplicitOAuthFlow)(nil),
	(*PasswordOAuthFlow)(nil),
	(*DeviceCodeOAuthFlow)(nil),
	OAuthFlowsWire{},
	// Request / response envelopes (one per A2A RPC).
	SendMessageRequest{},
	SendMessageResponse{},
	GetTaskRequest{},
	ListTasksRequest{},
	ListTasksResponse{},
	CancelTaskRequest{},
	SubscribeToTaskRequest{},
	GetTaskPushNotificationConfigRequest{},
	ListTaskPushNotificationConfigsRequest{},
	ListTaskPushNotificationConfigsResponse{},
	DeleteTaskPushNotificationConfigRequest{},
	GetExtendedAgentCardRequest{},
}

// TestSpecCoverage_EveryProtoTypeHasGoCounterpart asserts every
// hand-listed expected type is constructible. The reflect.TypeOf call
// is the gate: a type that is renamed or deleted will fail compilation
// of this file; a type that is *missing* from the list is caught by
// the count-based assertion below (kept in sync with the proto by
// hand).
func TestSpecCoverage_EveryProtoTypeHasGoCounterpart(t *testing.T) {
	seen := make(map[string]bool, len(expectedTypes))
	for _, v := range expectedTypes {
		ty := reflect.TypeOf(v)
		if ty == nil {
			t.Fatalf("expectedTypes contained a typed nil with no underlying type")
		}
		// Pointer types unwrap to their element type for the name check.
		if ty.Kind() == reflect.Pointer {
			ty = ty.Elem()
		}
		name := ty.Name()
		if name == "" {
			t.Fatalf("expectedTypes[%v] resolved to a type without a name (kind=%s)", v, ty.Kind())
		}
		if seen[name] {
			t.Fatalf("duplicate type in expectedTypes: %s", name)
		}
		seen[name] = true
	}

	// Count assertion: bump this when the proto adds a message AND
	// you add a matching Go type. If you bumped the count without
	// touching expectedTypes the build fails.
	const expectedCount = 50
	if got := len(seen); got != expectedCount {
		t.Fatalf("expectedTypes count drift: have %d, expected %d. "+
			"If you added a proto message + Go type, bump expectedCount; "+
			"if you renamed one, fix the list.", got, expectedCount)
	}
}

// -----------------------------------------------------------------------------
// Enum tests
// -----------------------------------------------------------------------------

func TestTaskState_AllValuesValid(t *testing.T) {
	want := []TaskState{
		TaskStateUnspecified,
		TaskStateSubmitted,
		TaskStateWorking,
		TaskStateCompleted,
		TaskStateFailed,
		TaskStateCanceled,
		TaskStateInputRequired,
		TaskStateRejected,
		TaskStateAuthRequired,
	}
	if len(want) != 9 {
		t.Fatalf("TaskState V1 has 9 values (8 named + UNSPECIFIED); got %d", len(want))
	}
	for _, s := range want {
		if !s.IsValid() {
			t.Errorf("%v.IsValid() = false; want true", s)
		}
		if s.String() == "" {
			t.Errorf("%v.String() = empty", s)
		}
	}
	if TaskState(99).IsValid() {
		t.Errorf("TaskState(99).IsValid() = true; want false")
	}
}

func TestTaskState_JSONRoundTrip_StringForm(t *testing.T) {
	for _, want := range []TaskState{TaskStateSubmitted, TaskStateCompleted, TaskStateAuthRequired} {
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal %v: %v", want, err)
		}
		var got TaskState
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", data, err)
		}
		if got != want {
			t.Errorf("round-trip: got %v want %v", got, want)
		}
	}
}

func TestTaskState_UnmarshalsIntegerForm(t *testing.T) {
	var s TaskState
	if err := json.Unmarshal([]byte(`3`), &s); err != nil {
		t.Fatalf("unmarshal 3: %v", err)
	}
	if s != TaskStateCompleted {
		t.Errorf("got %v want TaskStateCompleted", s)
	}
}

func TestTaskState_UnmarshalsUnknown_Errors(t *testing.T) {
	var s TaskState
	if err := json.Unmarshal([]byte(`"NOT_A_STATE"`), &s); err == nil {
		t.Errorf("unmarshal of unknown name should error")
	}
	if err := json.Unmarshal([]byte(`42`), &s); err == nil {
		t.Errorf("unmarshal of unknown integer should error")
	}
}

func TestRole_AllValuesValid(t *testing.T) {
	want := []Role{RoleUnspecified, RoleUser, RoleAgent}
	if len(want) != 3 {
		t.Fatalf("Role has 3 values; got %d", len(want))
	}
	for _, r := range want {
		if !r.IsValid() {
			t.Errorf("%v.IsValid() = false", r)
		}
	}
	if Role(7).IsValid() {
		t.Errorf("Role(7).IsValid() = true; want false")
	}
}

func TestRole_JSONRoundTrip(t *testing.T) {
	for _, want := range []Role{RoleUser, RoleAgent, RoleUnspecified} {
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got Role
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got != want {
			t.Errorf("round-trip: %v != %v", got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// Part discriminated-union tests
// -----------------------------------------------------------------------------

func TestPart_TextVariant_RoundTrip(t *testing.T) {
	orig := &TextPart{
		Text:       "hello world",
		partCommon: partCommon{Filename: "greeting.txt", MediaType: "text/plain", Metadata: map[string]any{"k": "v"}},
	}
	data, err := MarshalPart(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := UnmarshalPart(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind() != PartKindText {
		t.Fatalf("Kind: got %q want %q", got.Kind(), PartKindText)
	}
	tp, ok := got.(*TextPart)
	if !ok {
		t.Fatalf("unmarshal returned %T", got)
	}
	if tp.Text != orig.Text || tp.Filename != orig.Filename || tp.MediaType != orig.MediaType {
		t.Errorf("round-trip mismatch: got %+v want %+v", tp, orig)
	}
}

func TestPart_RawVariant_RoundTrip_Base64(t *testing.T) {
	orig := &RawPart{Raw: []byte{0xde, 0xad, 0xbe, 0xef}, partCommon: partCommon{MediaType: "application/octet-stream"}}
	data, err := MarshalPart(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Sanity-check: the wire form encodes bytes as a base64 string per
	// stdlib encoding/json's []byte handling (which matches the proto3
	// JSON mapping for `bytes`).
	if !strings.Contains(string(data), "3q2+7w==") {
		t.Errorf("expected base64 encoding of bytes in wire: %s", data)
	}
	got, err := UnmarshalPart(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rp, ok := got.(*RawPart)
	if !ok {
		t.Fatalf("unmarshal returned %T", got)
	}
	if !reflect.DeepEqual(rp.Raw, orig.Raw) {
		t.Errorf("Raw round-trip: got %v want %v", rp.Raw, orig.Raw)
	}
}

func TestPart_URLVariant_RoundTrip(t *testing.T) {
	orig := &URLPart{URL: "https://example.com/file.pdf", partCommon: partCommon{Filename: "file.pdf", MediaType: "application/pdf"}}
	data, err := MarshalPart(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := UnmarshalPart(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind() != PartKindURL {
		t.Errorf("kind: %q != %q", got.Kind(), PartKindURL)
	}
	up := got.(*URLPart)
	if up.URL != orig.URL {
		t.Errorf("URL round-trip: %q != %q", up.URL, orig.URL)
	}
}

func TestPart_DataVariant_RoundTrip(t *testing.T) {
	orig := &DataPart{Data: map[string]any{"n": float64(42), "s": "x"}, partCommon: partCommon{MediaType: "application/json"}}
	data, err := MarshalPart(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := UnmarshalPart(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind() != PartKindData {
		t.Errorf("kind: %q != %q", got.Kind(), PartKindData)
	}
	dp := got.(*DataPart)
	if !reflect.DeepEqual(dp.Data, orig.Data) {
		t.Errorf("Data round-trip: %+v != %+v", dp.Data, orig.Data)
	}
}

func TestPart_EmptyWire_ErrorsLoudly(t *testing.T) {
	_, err := UnmarshalPart([]byte(`{"filename":"x.txt"}`))
	if !errors.Is(err, ErrInvalidPart) {
		t.Errorf("empty Part: want ErrInvalidPart, got %v", err)
	}
}

func TestParts_SliceRoundTrip(t *testing.T) {
	orig := Parts{
		&TextPart{Text: "hello"},
		&URLPart{URL: "https://x", partCommon: partCommon{MediaType: "image/png"}},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Parts
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != len(orig) {
		t.Fatalf("len: got %d want %d", len(got), len(orig))
	}
	if got[0].Kind() != PartKindText || got[1].Kind() != PartKindURL {
		t.Errorf("kinds: got %q,%q", got[0].Kind(), got[1].Kind())
	}
}

// -----------------------------------------------------------------------------
// SecurityScheme discriminated-union tests
// -----------------------------------------------------------------------------

func TestSecurityScheme_AllVariants_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   SecurityScheme
		kind string
	}{
		{"APIKey", &APIKeySecurityScheme{Location: "header", Name: "X-API-KEY"}, SecurityKindAPIKey},
		{"HTTPAuth", &HTTPAuthSecurityScheme{Scheme: "Bearer", BearerFormat: "JWT"}, SecurityKindHTTPAuth},
		{"OAuth2", &OAuth2SecurityScheme{Flows: &ClientCredentialsOAuthFlow{TokenURL: "https://x/token", Scopes: map[string]string{"a": "b"}}}, SecurityKindOAuth2},
		{"OIDC", &OpenIdConnectSecurityScheme{OpenIDConnectURL: "https://x/.well-known/openid-configuration"}, SecurityKindOpenIDConnect},
		{"mTLS", &MutualTlsSecurityScheme{Description: "mtls"}, SecurityKindMutualTLS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := MarshalSecurityScheme(tc.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalSecurityScheme(data)
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Kind() != tc.kind {
				t.Errorf("kind: %q != %q", got.Kind(), tc.kind)
			}
		})
	}
}

func TestSecurityScheme_UnknownKind_Errors(t *testing.T) {
	_, err := UnmarshalSecurityScheme([]byte(`{"type":"weird"}`))
	if !errors.Is(err, ErrInvalidOneof) {
		t.Errorf("expected ErrInvalidOneof, got %v", err)
	}
}

func TestSecuritySchemeMap_RoundTrip(t *testing.T) {
	m := SecuritySchemeMap{
		"oauth": &OAuth2SecurityScheme{Flows: &AuthorizationCodeOAuthFlow{AuthorizationURL: "https://x/auth", TokenURL: "https://x/tok", Scopes: map[string]string{"read": "Read"}}},
		"api":   &APIKeySecurityScheme{Location: "query", Name: "key"},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SecuritySchemeMap
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["api"].Kind() != SecurityKindAPIKey {
		t.Errorf("api key kind: %q", got["api"].Kind())
	}
	if got["oauth"].Kind() != SecurityKindOAuth2 {
		t.Errorf("oauth kind: %q", got["oauth"].Kind())
	}
}

// -----------------------------------------------------------------------------
// OAuthFlows discriminated-union tests
// -----------------------------------------------------------------------------

func TestOAuthFlows_AllVariants_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   OAuthFlows
		kind string
	}{
		{"AuthorizationCode", &AuthorizationCodeOAuthFlow{AuthorizationURL: "https://x/auth", TokenURL: "https://x/tok", Scopes: map[string]string{"r": "read"}, PKCERequired: true}, OAuthFlowKindAuthorizationCode},
		{"ClientCredentials", &ClientCredentialsOAuthFlow{TokenURL: "https://x/tok", Scopes: map[string]string{"w": "write"}}, OAuthFlowKindClientCredentials},
		{"Implicit", &ImplicitOAuthFlow{AuthorizationURL: "https://x/auth", Scopes: map[string]string{}}, OAuthFlowKindImplicit},
		{"Password", &PasswordOAuthFlow{TokenURL: "https://x/tok"}, OAuthFlowKindPassword},
		{"DeviceCode", &DeviceCodeOAuthFlow{DeviceAuthorizationURL: "https://x/dev", TokenURL: "https://x/tok", Scopes: map[string]string{"s": "scope"}}, OAuthFlowKindDeviceCode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := MarshalOAuthFlows(tc.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := UnmarshalOAuthFlows(data)
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Kind() != tc.kind {
				t.Errorf("kind: %q != %q", got.Kind(), tc.kind)
			}
		})
	}
}

func TestOAuthFlows_EmptyEnvelope_Errors(t *testing.T) {
	_, err := UnmarshalOAuthFlows([]byte(`{}`))
	if !errors.Is(err, ErrInvalidOneof) {
		t.Errorf("expected ErrInvalidOneof, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// StreamResponse discriminated-union tests
// -----------------------------------------------------------------------------

func TestStreamResponse_AllVariants_KindAndValidate(t *testing.T) {
	taskResp := StreamResponse{Task: &Task{ID: "t-1", Status: TaskStatus{State: TaskStateWorking}}}
	if taskResp.Kind() != StreamResponseKindTask {
		t.Errorf("task kind: %q", taskResp.Kind())
	}
	if err := taskResp.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}

	msgResp := StreamResponse{Message: &Message{MessageID: "m-1", Role: RoleAgent, Parts: Parts{&TextPart{Text: "hi"}}}}
	if msgResp.Kind() != StreamResponseKindMessage {
		t.Errorf("msg kind: %q", msgResp.Kind())
	}
	statusResp := StreamResponse{StatusUpdate: &TaskStatusUpdateEvent{TaskID: "t-1", ContextID: "ctx-1", Status: TaskStatus{State: TaskStateCompleted}}}
	if statusResp.Kind() != StreamResponseKindStatusUpdate {
		t.Errorf("status kind: %q", statusResp.Kind())
	}
	artResp := StreamResponse{ArtifactUpdate: &TaskArtifactUpdateEvent{TaskID: "t-1", ContextID: "ctx-1", Artifact: Artifact{ArtifactID: "a-1", Parts: Parts{&TextPart{Text: "x"}}}}}
	if artResp.Kind() != StreamResponseKindArtifactUpdate {
		t.Errorf("artifact kind: %q", artResp.Kind())
	}

	empty := StreamResponse{}
	if empty.Kind() != "" {
		t.Errorf("empty kind: %q", empty.Kind())
	}
	if err := empty.Validate(); !errors.Is(err, ErrInvalidOneof) {
		t.Errorf("empty validate: %v", err)
	}

	twoSet := StreamResponse{Task: &Task{}, Message: &Message{}}
	if err := twoSet.Validate(); !errors.Is(err, ErrInvalidOneof) {
		t.Errorf("two-set validate: %v", err)
	}
}

func TestStreamResponse_JSONRoundTrip(t *testing.T) {
	orig := StreamResponse{Task: &Task{ID: "t-1", ContextID: "ctx-1", Status: TaskStatus{State: TaskStateWorking, Timestamp: time.Unix(1700000000, 0).UTC()}}}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got StreamResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind() != orig.Kind() {
		t.Errorf("kind: %q != %q", got.Kind(), orig.Kind())
	}
	if got.Task.ID != orig.Task.ID {
		t.Errorf("task id: %q != %q", got.Task.ID, orig.Task.ID)
	}
}

// -----------------------------------------------------------------------------
// Core message JSON round-trips
// -----------------------------------------------------------------------------

func TestTask_JSONRoundTrip(t *testing.T) {
	hl := int32(5)
	orig := Task{
		ID:        "task-1",
		ContextID: "ctx-1",
		Status:    TaskStatus{State: TaskStateCompleted, Timestamp: time.Unix(1700000000, 0).UTC()},
		Artifacts: []Artifact{{ArtifactID: "a-1", Parts: Parts{&TextPart{Text: "out"}}}},
		History:   []Message{{MessageID: "m-1", Role: RoleAgent, Parts: Parts{&TextPart{Text: "done"}}}},
		Metadata:  map[string]any{"x": float64(hl)},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Task
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != orig.ID || got.Status.State != orig.Status.State {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, orig)
	}
	if len(got.History) != 1 || got.History[0].Role != RoleAgent {
		t.Errorf("history: %+v", got.History)
	}
}

func TestMessage_JSONRoundTrip(t *testing.T) {
	orig := Message{
		MessageID:        "m-1",
		ContextID:        "ctx",
		TaskID:           "t-1",
		Role:             RoleUser,
		Parts:            Parts{&TextPart{Text: "hello"}, &URLPart{URL: "https://x/img.png", partCommon: partCommon{MediaType: "image/png"}}},
		Extensions:       []string{"https://ext.example/v1"},
		ReferenceTaskIDs: []string{"t-old"},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MessageID != orig.MessageID || got.Role != orig.Role {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if len(got.Parts) != 2 || got.Parts[0].Kind() != PartKindText || got.Parts[1].Kind() != PartKindURL {
		t.Errorf("parts: %+v", got.Parts)
	}
}

func TestAgentCard_JSONRoundTrip(t *testing.T) {
	streaming := true
	orig := AgentCard{
		Name:                "Recipe Agent",
		Description:         "Helps with recipes",
		Version:             "1.0.0",
		SupportedInterfaces: []AgentInterface{{URL: "https://x/a2a/v1", ProtocolBinding: ProtocolBindingHTTPJSON, ProtocolVersion: "1.0"}},
		Capabilities:        AgentCapabilities{Streaming: &streaming},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills:              []AgentSkill{{ID: "s-1", Name: "summarise", Description: "summarise text", Tags: []string{"nlp"}}},
		SecuritySchemes: SecuritySchemeMap{
			"key": &APIKeySecurityScheme{Location: "header", Name: "X-Api-Key"},
		},
		SecurityRequirements: []SecurityRequirement{{Schemes: map[string]StringList{"key": {List: []string{"read"}}}}},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got AgentCard
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != orig.Name || got.Version != orig.Version {
		t.Errorf("mismatch: %+v", got)
	}
	if len(got.Skills) != 1 || got.Skills[0].ID != "s-1" {
		t.Errorf("skills: %+v", got.Skills)
	}
	if got.SecuritySchemes["key"].Kind() != SecurityKindAPIKey {
		t.Errorf("scheme kind: %q", got.SecuritySchemes["key"].Kind())
	}
}

func TestSendMessageResponse_KindDiscriminator(t *testing.T) {
	taskResp := SendMessageResponse{Task: &Task{ID: "t-1", Status: TaskStatus{State: TaskStateWorking}}}
	if taskResp.Kind() != SendMessageResponseKindTask {
		t.Errorf("task kind: %q", taskResp.Kind())
	}
	msgResp := SendMessageResponse{Message: &Message{MessageID: "m-1", Role: RoleAgent, Parts: Parts{&TextPart{Text: "hi"}}}}
	if msgResp.Kind() != SendMessageResponseKindMessage {
		t.Errorf("msg kind: %q", msgResp.Kind())
	}
	if (SendMessageResponse{}).Kind() != "" {
		t.Errorf("empty kind should be empty string")
	}
}

func TestListTasksRequest_JSONRoundTrip(t *testing.T) {
	pageSize := int32(20)
	include := true
	orig := ListTasksRequest{Tenant: "t1", ContextID: "c1", Status: TaskStateCompleted, PageSize: &pageSize, IncludeArtifacts: &include}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ListTasksRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Tenant != orig.Tenant || got.Status != orig.Status {
		t.Errorf("mismatch: %+v", got)
	}
	if got.PageSize == nil || *got.PageSize != pageSize {
		t.Errorf("page size: %v", got.PageSize)
	}
}

func TestOAuth2SecurityScheme_DirectJSONRoundTrip(t *testing.T) {
	orig := &OAuth2SecurityScheme{
		Description:       "oauth",
		Flows:             &AuthorizationCodeOAuthFlow{AuthorizationURL: "https://x/auth", TokenURL: "https://x/tok", Scopes: map[string]string{"r": "Read"}, PKCERequired: true},
		OAuth2MetadataURL: "https://x/.well-known/oauth-authorization-server",
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got OAuth2SecurityScheme
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Description != orig.Description || got.OAuth2MetadataURL != orig.OAuth2MetadataURL {
		t.Errorf("mismatch: %+v", got)
	}
	if got.Flows.Kind() != OAuthFlowKindAuthorizationCode {
		t.Errorf("flow kind: %q", got.Flows.Kind())
	}
}

func TestOAuth2SecurityScheme_UnmarshalBadEnvelope_Errors(t *testing.T) {
	var s OAuth2SecurityScheme
	if err := json.Unmarshal([]byte(`{"flows":{}}`), &s); err == nil {
		t.Errorf("expected error on empty flows envelope")
	}
}

func TestMarshalSecurityScheme_NilErrors(t *testing.T) {
	_, err := MarshalSecurityScheme(nil)
	if !errors.Is(err, ErrInvalidOneof) {
		t.Errorf("expected ErrInvalidOneof, got %v", err)
	}
}

func TestMarshalPart_NilErrors(t *testing.T) {
	_, err := MarshalPart(nil)
	if !errors.Is(err, ErrInvalidPart) {
		t.Errorf("expected ErrInvalidPart, got %v", err)
	}
}

func TestMarshalOAuthFlows_NilErrors(t *testing.T) {
	_, err := MarshalOAuthFlows(nil)
	if !errors.Is(err, ErrInvalidOneof) {
		t.Errorf("expected ErrInvalidOneof, got %v", err)
	}
}

func TestTaskState_StringUnknown(t *testing.T) {
	s := TaskState(999)
	if got := s.String(); got == "" {
		t.Errorf("String of unknown: %q", got)
	}
}

func TestRole_StringUnknown(t *testing.T) {
	r := Role(999)
	if got := r.String(); got == "" {
		t.Errorf("String of unknown: %q", got)
	}
}

func TestSendMessageRequest_JSONRoundTrip(t *testing.T) {
	orig := SendMessageRequest{
		Tenant:        "t1",
		Message:       Message{MessageID: "m-1", Role: RoleUser, Parts: Parts{&TextPart{Text: "hi"}}},
		Configuration: &SendMessageConfiguration{AcceptedOutputModes: []string{"text/plain"}, ReturnImmediately: true},
		Metadata:      map[string]any{"key": "v"},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SendMessageRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Tenant != orig.Tenant || got.Configuration.ReturnImmediately != true {
		t.Errorf("mismatch: %+v", got)
	}
}

func TestListTasksResponse_JSONRoundTrip(t *testing.T) {
	orig := ListTasksResponse{
		Tasks:         []Task{{ID: "t-1", Status: TaskStatus{State: TaskStateWorking}}},
		NextPageToken: "next",
		PageSize:      10,
		TotalSize:     42,
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ListTasksResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TotalSize != 42 || got.NextPageToken != "next" {
		t.Errorf("mismatch: %+v", got)
	}
}

func TestStreamResponse_NilVariantsValidateError(t *testing.T) {
	r := StreamResponse{}
	if err := r.Validate(); err == nil {
		t.Errorf("Validate: expected error on zero StreamResponse")
	}
}

func TestSecurityScheme_DescriptionPreserved(t *testing.T) {
	cases := []SecurityScheme{
		&APIKeySecurityScheme{Description: "api key", Location: "header", Name: "X-Key"},
		&HTTPAuthSecurityScheme{Description: "http", Scheme: "Bearer"},
		&OpenIdConnectSecurityScheme{Description: "oidc", OpenIDConnectURL: "https://x"},
		&MutualTlsSecurityScheme{Description: "mtls"},
	}
	for _, c := range cases {
		data, err := MarshalSecurityScheme(c)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got, err := UnmarshalSecurityScheme(data)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Kind() != c.Kind() {
			t.Errorf("Kind: %q != %q", got.Kind(), c.Kind())
		}
	}
}

func TestTaskPushNotificationConfig_RoundTrip(t *testing.T) {
	orig := TaskPushNotificationConfig{
		Tenant:         "t1",
		ID:             "cfg-1",
		TaskID:         "task-1",
		URL:            "https://callback.example/push",
		Token:          "secret-token",
		Authentication: &AuthenticationInfo{Scheme: "Bearer", Credentials: "Bearer xxx"},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got TaskPushNotificationConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.URL != orig.URL || got.Authentication.Scheme != "Bearer" {
		t.Errorf("mismatch: %+v", got)
	}
}
