package a2a_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"

	a2atypes "github.com/hurtener/Harbor/internal/distributed/a2a"
	"github.com/hurtener/Harbor/internal/distributed/drivers/loopback"
	"github.com/hurtener/Harbor/internal/identity"
)

// mockA2AServer is an in-process httptest.Server that speaks A2A's
// JSON-RPC binding + SSE streaming against a function-pointer-driven
// Agent (the same Agent abstraction the loopback driver simulates).
// Phase 29's wire-driver conformance test binds the same `stubAgent`
// shape used by `internal/distributed/conformancetest` so the suite
// runs verbatim against the wire driver.
type mockA2AServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	agents map[string]loopback.Agent // keyed by absolute URL prefix
	card   *a2atypes.AgentCard
	// requestCount tracks how many requests the server has served;
	// the AgentCard test uses this to assert cache coalescing.
	requestCount atomic.Int64
}

// newMockA2AServer constructs the server and returns it ready to
// serve. The caller is expected to call SetAgentCard + BindAgent
// before issuing wire calls. Server is HTTP (not HTTPS) — the wire
// driver accepts http://127.0.0.1 because the host is loopback.
func newMockA2AServer() *mockA2AServer {
	m := &mockA2AServer{
		agents: map[string]loopback.Agent{},
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

// URL returns the base URL the server listens on.
func (m *mockA2AServer) URL() string { return m.srv.URL }

// Close stops the server.
func (m *mockA2AServer) Close() { m.srv.Close() }

// RequestCount returns the total number of HTTP requests served.
func (m *mockA2AServer) RequestCount() int64 { return m.requestCount.Load() }

// SetAgentCard installs the AgentCard the server returns on
// `GET /.well-known/agent-card.json`. The Card's first
// SupportedInterface MUST declare ProtocolBinding=JSONRPC and a URL
// pointing back to the mock — otherwise the wire driver won't speak
// to it.
func (m *mockA2AServer) SetAgentCard(card *a2atypes.AgentCard) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.card = card
}

// BindAgent installs an Agent stub for the supplied URL prefix. The
// server matches each incoming request's URL against the keyset —
// the longest matching prefix wins. Empty key matches all paths
// (default agent).
func (m *mockA2AServer) BindAgent(urlPrefix string, agent loopback.Agent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agents[urlPrefix] = agent
}

func (m *mockA2AServer) resolveAgent(_ *http.Request) loopback.Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	// V1 mock: return any registered agent. Tests register exactly
	// one Agent for the conformance suite.
	for _, a := range m.agents {
		return a
	}
	return nil
}

// handle is the HTTP entry point. Routes:
//
//	GET /.well-known/agent-card.json → serves the configured AgentCard
//	POST /rpc                        → JSON-RPC dispatch
func (m *mockA2AServer) handle(w http.ResponseWriter, r *http.Request) {
	m.requestCount.Add(1)
	defer r.Body.Close()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/.well-known/agent-card.json":
		m.handleAgentCard(w, r)
	case r.Method == http.MethodPost:
		m.handleRPC(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *mockA2AServer) handleAgentCard(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	card := m.card
	m.mu.Unlock()
	if card == nil {
		http.Error(w, "no card configured", http.StatusNotFound)
		return
	}
	body, err := json.Marshal(card)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// jsonRPC envelopes mirror the wire driver's shape so the server can
// parse incoming requests + write responses without importing the
// driver package (which would create a test import cycle).
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      uint64      `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (m *mockA2AServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeRPCError(w, 0, -32700, "parse error: "+err.Error())
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, 0, -32700, "parse error: "+err.Error())
		return
	}

	agent := m.resolveAgent(r)
	if agent == nil {
		writeRPCError(w, req.ID, -32603, "no agent bound on mock server")
		return
	}

	switch req.Method {
	case "message/send":
		m.handleSendMessage(w, r, req, agent)
	case "message/stream":
		m.handleSendStreamingMessage(w, r, req, agent)
	case "tasks/get":
		m.handleGetTask(w, req, agent)
	case "tasks/list":
		m.handleListTasks(w, req, agent)
	case "tasks/cancel":
		m.handleCancelTask(w, req, agent)
	case "tasks/subscribe":
		m.handleSubscribeToTask(w, r, req, agent)
	case "tasks/pushNotificationConfig/set":
		m.handleCreatePushConfig(w, req, agent)
	case "tasks/pushNotificationConfig/get":
		m.handleGetPushConfig(w, req, agent)
	case "tasks/pushNotificationConfig/list":
		m.handleListPushConfigs(w, req, agent)
	case "tasks/pushNotificationConfig/delete":
		m.handleDeletePushConfig(w, req, agent)
	case "agent/getAuthenticatedExtendedCard":
		m.handleGetExtendedCard(w, req, agent)
	default:
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// reconstructIdentityCtx rebuilds an identity context from the wire
// driver's X-Harbor-* headers so the Agent stubs see the same
// identity the conformance suite asserts on (a "the transport
// propagated the triple" assertion).
func reconstructIdentityCtx(r *http.Request) context.Context {
	ctx := context.Background()
	tenant := r.Header.Get("X-Harbor-Tenant")
	user := r.Header.Get("X-Harbor-User")
	session := r.Header.Get("X-Harbor-Session")
	if tenant == "" || user == "" || session == "" {
		// Return ctx with no identity — the conformance suite asserts
		// this surfaces an agent-side error.
		return ctx
	}
	out, err := identity.With(ctx, identity.Identity{TenantID: tenant, UserID: user, SessionID: session})
	if err != nil {
		return ctx
	}
	return out
}

func (m *mockA2AServer) handleSendMessage(w http.ResponseWriter, r *http.Request, req rpcRequest, agent loopback.Agent) {
	var p a2atypes.SendMessageRequest
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	cfg := a2atypes.SendMessageConfiguration{}
	if p.Configuration != nil {
		cfg = *p.Configuration
	}
	task, err := agent.SendMessage(reconstructIdentityCtx(r), p.Message, cfg)
	if err != nil {
		code := -32603
		writeRPCError(w, req.ID, code, err.Error())
		return
	}
	writeRPCResult(w, req.ID, a2atypes.SendMessageResponse{Task: &task})
}

func (m *mockA2AServer) handleSendStreamingMessage(w http.ResponseWriter, r *http.Request, req rpcRequest, agent loopback.Agent) {
	var p a2atypes.SendMessageRequest
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	cfg := a2atypes.SendMessageConfiguration{}
	if p.Configuration != nil {
		cfg = *p.Configuration
	}
	ch, err := agent.SendStreamingMessage(reconstructIdentityCtx(r), p.Message, cfg)
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	streamSSE(w, r.Context(), ch)
}

func (m *mockA2AServer) handleGetTask(w http.ResponseWriter, req rpcRequest, agent loopback.Agent) {
	var p a2atypes.GetTaskRequest
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	task, err := agent.GetTask(context.Background(), p.ID, "")
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "ErrTaskNotFound") {
			writeRPCError(w, req.ID, 1, err.Error())
			return
		}
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	writeRPCResult(w, req.ID, task)
}

func (m *mockA2AServer) handleListTasks(w http.ResponseWriter, req rpcRequest, agent loopback.Agent) {
	var p a2atypes.ListTasksRequest
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	filter := loopback.ListTasksFilter{
		Tenant:    p.Tenant,
		ContextID: p.ContextID,
		Status:    p.Status,
		PageToken: p.PageToken,
	}
	if p.PageSize != nil {
		filter.PageSize = *p.PageSize
	}
	if p.HistoryLength != nil {
		filter.HistoryLength = *p.HistoryLength
	}
	if !p.StatusTimestampAfter.IsZero() {
		filter.StatusTimestampAfter = p.StatusTimestampAfter.UnixNano()
	}
	if p.IncludeArtifacts != nil {
		filter.IncludeArtifacts = *p.IncludeArtifacts
	}
	tasks, err := agent.ListTasks(context.Background(), filter)
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	writeRPCResult(w, req.ID, a2atypes.ListTasksResponse{Tasks: tasks})
}

func (m *mockA2AServer) handleCancelTask(w http.ResponseWriter, req rpcRequest, agent loopback.Agent) {
	var p a2atypes.CancelTaskRequest
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	task, err := agent.CancelTask(context.Background(), p.ID, "")
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	writeRPCResult(w, req.ID, task)
}

func (m *mockA2AServer) handleSubscribeToTask(w http.ResponseWriter, r *http.Request, req rpcRequest, agent loopback.Agent) {
	var p a2atypes.SubscribeToTaskRequest
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	ch, err := agent.SubscribeToTask(reconstructIdentityCtx(r), p.ID, "")
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	streamSSE(w, r.Context(), ch)
}

func (m *mockA2AServer) handleCreatePushConfig(w http.ResponseWriter, req rpcRequest, agent loopback.Agent) {
	var p a2atypes.TaskPushNotificationConfig
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	out, err := agent.CreateTaskPushNotificationConfig(context.Background(), p)
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	writeRPCResult(w, req.ID, out)
}

func (m *mockA2AServer) handleGetPushConfig(w http.ResponseWriter, req rpcRequest, agent loopback.Agent) {
	var p a2atypes.GetTaskPushNotificationConfigRequest
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	out, err := agent.GetTaskPushNotificationConfig(context.Background(), p.TaskID, p.ID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeRPCError(w, req.ID, 1, err.Error())
			return
		}
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	writeRPCResult(w, req.ID, out)
}

func (m *mockA2AServer) handleListPushConfigs(w http.ResponseWriter, req rpcRequest, agent loopback.Agent) {
	var p a2atypes.ListTaskPushNotificationConfigsRequest
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	configs, err := agent.ListTaskPushNotificationConfigs(context.Background(), p.TaskID)
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	writeRPCResult(w, req.ID, a2atypes.ListTaskPushNotificationConfigsResponse{Configs: configs})
}

func (m *mockA2AServer) handleDeletePushConfig(w http.ResponseWriter, req rpcRequest, agent loopback.Agent) {
	var p a2atypes.DeleteTaskPushNotificationConfigRequest
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	if err := agent.DeleteTaskPushNotificationConfig(context.Background(), p.TaskID, p.ID); err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	writeRPCResult(w, req.ID, struct{}{})
}

func (m *mockA2AServer) handleGetExtendedCard(w http.ResponseWriter, req rpcRequest, agent loopback.Agent) {
	card, err := agent.GetExtendedAgentCard(context.Background())
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	writeRPCResult(w, req.ID, card)
}

// streamSSE flushes a2atypes.StreamResponse events from ch to w as
// SSE `data:` frames. Closes when ch closes OR ctx cancels.
func streamSSE(w http.ResponseWriter, ctx context.Context, ch <-chan a2atypes.StreamResponse) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case <-ctx.Done():
			return
		case resp, ok := <-ch:
			if !ok {
				return
			}
			body, err := json.Marshal(resp)
			if err != nil {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
			flusher.Flush()
		}
	}
}

func writeRPCResult(w http.ResponseWriter, id uint64, result interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, id uint64, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	})
}
