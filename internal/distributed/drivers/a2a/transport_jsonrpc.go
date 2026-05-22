package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/identity"
)

// jsonRPCVersion is the JSON-RPC 2.0 spec version string.
const jsonRPCVersion = "2.0"

// Canonical A2A JSON-RPC method names. The wire driver speaks these
// over POST against the peer's JSON-RPC endpoint (the AgentInterface
// URL discovered from the AgentCard).
const (
	MethodSendMessage                      = "message/send"
	MethodSendStreamingMessage             = "message/stream"
	MethodGetTask                          = "tasks/get"
	MethodListTasks                        = "tasks/list"
	MethodCancelTask                       = "tasks/cancel"
	MethodSubscribeToTask                  = "tasks/subscribe"
	MethodCreateTaskPushNotificationConfig = "tasks/pushNotificationConfig/set"
	MethodGetTaskPushNotificationConfig    = "tasks/pushNotificationConfig/get"
	MethodListTaskPushNotificationConfigs  = "tasks/pushNotificationConfig/list"
	MethodDeleteTaskPushNotificationConfig = "tasks/pushNotificationConfig/delete"
	MethodGetExtendedAgentCard             = "agent/getAuthenticatedExtendedCard"
)

// JSON-RPC application error codes. The spec's standard codes are in
// the [-32700, -32600] range; the A2A application codes use positive
// integers. We treat code 1 as ErrTaskNotFound per the A2A's
// `TaskNotFoundError` convention.
const (
	jsonRPCErrParse          = -32700
	jsonRPCErrInvalidRequest = -32600
	jsonRPCErrMethodNotFound = -32601
	jsonRPCErrInvalidParams  = -32602
	jsonRPCErrInternal       = -32603
	a2aErrTaskNotFound       = 1
)

// jsonRPCRequest is the wire request envelope. ID is monotonically
// increasing per transport instance via atomic counter — JSON-RPC
// requires unique IDs only per request/response correlation, not
// globally, but a monotone counter is the simplest correct choice.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCResponse is the wire response envelope. Either Result or
// Error is populated; never both.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// jsonRPCError is the standard JSON-RPC error envelope.
type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("jsonrpc error code=%d: %s", e.Code, e.Message)
}

// jsonRPCClient is the wire-level JSON-RPC over HTTPS POST client. One
// instance per RemoteTransport; uses a single *http.Client and a
// monotone ID counter.
//
// Concurrent reuse (D-025): the client is safe for N concurrent calls
// against a single instance. `*http.Client` is shared (its public
// surface is documented concurrent-safe), the atomic counter handles
// ID generation, and per-call state lives on the goroutine stack.
type jsonRPCClient struct {
	httpc *http.Client
	idGen atomic.Uint64
}

// newJSONRPCClient constructs the client. A nil http.Client falls
// back to http.DefaultClient.
func newJSONRPCClient(httpc *http.Client) *jsonRPCClient {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	return &jsonRPCClient{httpc: httpc}
}

// Call issues a JSON-RPC request to endpoint with the supplied params.
// Returns the raw Result bytes on success; on a JSON-RPC error
// envelope returns an error wrapping ErrJSONRPCError (and ErrTaskNotFound
// when the error code matches the A2A TaskNotFoundError convention).
//
// Identity propagation: when ctx carries an identity triple, the
// driver does NOT modify the endpoint URL — instead it sets a
// `X-Harbor-Tenant` header. The proto's "tenant" path parameter is
// peer-specific and many peers prefer the header form; the proto
// allows the param to be elided when supplied out-of-band. (Phase 29
// keeps this off the URL to avoid double-encoding edge cases on
// arbitrary paths.) The peer is expected to consume the header per
// its own conventions; the conformance suite asserts the header
// arrives unmodified.
func (c *jsonRPCClient) Call(ctx context.Context, endpoint, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := c.buildRequestBody(method, params)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("a2a: build jsonrpc request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "harbor-a2a/1.0")
	applyIdentityHeader(ctx, req)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: jsonrpc transport: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, jsonRPCMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("a2a: read jsonrpc body: %w", err)
	}

	// Treat any non-2xx with a JSON-RPC envelope as a normal RPC
	// error; the spec allows transports to mirror HTTP status to the
	// envelope. Non-JSON 5xx is a wire error.
	var env jsonRPCResponse
	if jerr := json.Unmarshal(respBody, &env); jerr != nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, fmt.Errorf("a2a: parse jsonrpc response: %w", jerr)
		}
		return nil, fmt.Errorf("a2a: jsonrpc transport HTTP %d: %s", resp.StatusCode, snippet(respBody, 256))
	}
	if env.Error != nil {
		if env.Error.Code == a2aErrTaskNotFound {
			return nil, fmt.Errorf("%w: %w", distributed.ErrTaskNotFound, env.Error)
		}
		return nil, fmt.Errorf("%w: %w", ErrJSONRPCError, env.Error)
	}
	return env.Result, nil
}

// buildRequestBody encodes a JSON-RPC request with a fresh ID.
func (c *jsonRPCClient) buildRequestBody(method string, params any) ([]byte, error) {
	id := c.idGen.Add(1)
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("a2a: marshal params for %q: %w", method, err)
		}
		raw = b
	}
	envelope := jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Method:  method,
		Params:  raw,
	}
	return json.Marshal(envelope)
}

// applyIdentityHeader sets the X-Harbor-Tenant / X-Harbor-User /
// X-Harbor-Session request headers when ctx carries identity. The
// identity headers carry NO secret content (just the IDs); they let a
// receiving Harbor northbound (post-V1) reconstruct the identity
// triple without re-authenticating. Missing identity is the caller-
// side concern (the wire driver does not gate calls on identity; the
// runtime above does — see Phase 22's contract).
func applyIdentityHeader(ctx context.Context, req *http.Request) {
	id, ok := identity.From(ctx)
	if !ok {
		return
	}
	if id.TenantID != "" {
		req.Header.Set("X-Harbor-Tenant", id.TenantID)
	}
	if id.UserID != "" {
		req.Header.Set("X-Harbor-User", id.UserID)
	}
	if id.SessionID != "" {
		req.Header.Set("X-Harbor-Session", id.SessionID)
	}
}

// jsonRPCMaxResponseBytes bounds the per-response body so a hostile
// peer can't OOM the runtime. 8 MiB is plenty for non-streaming
// A2A responses (large outputs route through artifacts; the protocol
// envelope itself is metadata-shaped).
const jsonRPCMaxResponseBytes int64 = 8 << 20
