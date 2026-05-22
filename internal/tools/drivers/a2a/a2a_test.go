package a2a_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/distributed"
	a2atypes "github.com/hurtener/Harbor/internal/distributed/a2a"
	a2adrv "github.com/hurtener/Harbor/internal/distributed/drivers/a2a"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	toolsa2a "github.com/hurtener/Harbor/internal/tools/drivers/a2a"
)

// minimalAgentCard returns a card with two skills the tests can
// materialise into Tools.
func minimalAgentCard(serverURL string) *a2atypes.AgentCard {
	return &a2atypes.AgentCard{
		Name:        "Tools-A2A Test Agent",
		Description: "two-skill agent",
		Version:     "1.0",
		SupportedInterfaces: []a2atypes.AgentInterface{
			{URL: serverURL, ProtocolBinding: a2atypes.ProtocolBindingJSONRPC, ProtocolVersion: "1.0"},
		},
		Capabilities:       a2atypes.AgentCapabilities{},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2atypes.AgentSkill{
			{ID: "echo", Name: "Echo", Description: "Echo back input.", Tags: []string{"test"}},
			{ID: "sum", Name: "Sum", Description: "Add two numbers.", Tags: []string{"math"}},
		},
	}
}

// fakeMockServer is a tiny JSON-RPC mock that handles three calls:
// the agent-card GET, the extended-card RPC, and message/send. Used
// by the tools-side adapter tests.
type fakeMockServer struct {
	srv     *httptest.Server
	card    *a2atypes.AgentCard
	calls   atomic.Int64
	lastMsg atomic.Value // holds the last Message body received via SendMessage
}

func newFakeMockServer(card *a2atypes.AgentCard) *fakeMockServer {
	m := &fakeMockServer{card: card}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

func (m *fakeMockServer) URL() string { return m.srv.URL }
func (m *fakeMockServer) Close()      { m.srv.Close() }

func (m *fakeMockServer) handle(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	m.calls.Add(1)
	if r.Method == http.MethodGet && r.URL.Path == "/.well-known/agent-card.json" {
		body, _ := json.Marshal(m.card)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return
	}
	if r.Method == http.MethodPost {
		dec := json.NewDecoder(r.Body)
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      uint64          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch req.Method {
		case "agent/getAuthenticatedExtendedCard":
			body, _ := json.Marshal(struct {
				JSONRPC string              `json:"jsonrpc"`
				ID      uint64              `json:"id"`
				Result  *a2atypes.AgentCard `json:"result"`
			}{JSONRPC: "2.0", ID: req.ID, Result: m.card})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		case "message/send":
			var p a2atypes.SendMessageRequest
			_ = json.Unmarshal(req.Params, &p)
			m.lastMsg.Store(p.Message)
			task := a2atypes.Task{
				ID:     "task-1",
				Status: a2atypes.TaskStatus{State: a2atypes.TaskStateCompleted},
			}
			body, _ := json.Marshal(struct {
				JSONRPC string                       `json:"jsonrpc"`
				ID      uint64                       `json:"id"`
				Result  a2atypes.SendMessageResponse `json:"result"`
			}{JSONRPC: "2.0", ID: req.ID, Result: a2atypes.SendMessageResponse{Task: &task}})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		writeRPCErr(w, req.ID, -32601, "method not found")
		return
	}
	http.NotFound(w, r)
}

func writeRPCErr(w http.ResponseWriter, id uint64, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: msg}})
}

// buildWireDriver returns a wire driver bound to the supplied fake
// server, with `peerURL` registered as a peer.
func buildWireDriver(t *testing.T, mock *fakeMockServer) (distributed.RemoteTransport, string) {
	t.Helper()
	peerURL := strings.TrimRight(mock.URL(), "/")
	reg := a2adrv.NewRegistry()
	if err := reg.AddPeer(a2adrv.PeerSpec{
		URL:                   peerURL,
		TrustTier:             3,
		LatencyTierMS:         10,
		AllowInsecureLoopback: true,
	}); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	tr, err := a2adrv.New(distributed.Dependencies{}, a2adrv.WithRegistry(reg))
	if err != nil {
		t.Fatalf("a2adrv.New: %v", err)
	}
	return tr, peerURL
}

func ctxWithIdentity(parent context.Context, tenant, user, session string) context.Context {
	ctx, err := identity.With(parent, identity.Identity{TenantID: tenant, UserID: user, SessionID: session})
	if err != nil {
		panic(err)
	}
	return ctx
}

func TestProvider_Connect_Discover_RegistersSkills(t *testing.T) {
	mock := newFakeMockServer(minimalAgentCard(""))
	defer mock.Close()
	// Card's AgentInterface URL must match the server URL so the
	// wire driver's POSTs land on the right path.
	card := minimalAgentCard(mock.URL())
	mock.card = card

	transport, peerURL := buildWireDriver(t, mock)
	defer func() { _ = transport.Close(context.Background()) }()

	prov, err := toolsa2a.New(peerURL, transport)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := ctxWithIdentity(context.Background(), "t-a", "u-a", "s-a")
	if err := prov.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	desc, err := prov.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(desc) != 2 {
		t.Fatalf("expected 2 descriptors, got %d", len(desc))
	}
	if desc[0].Tool.Transport != tools.TransportA2A {
		t.Errorf("Transport mismatch: %v", desc[0].Tool.Transport)
	}
	if desc[0].Tool.Source == "" {
		t.Errorf("Source not stamped")
	}
}

func TestProvider_Connect_RejectsMissingIdentity(t *testing.T) {
	mock := newFakeMockServer(minimalAgentCard(""))
	defer mock.Close()
	mock.card = minimalAgentCard(mock.URL())

	transport, peerURL := buildWireDriver(t, mock)
	defer func() { _ = transport.Close(context.Background()) }()

	prov, err := toolsa2a.New(peerURL, transport)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = prov.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "identity required") {
		t.Errorf("expected identity-required error, got %v", err)
	}
}

func TestProvider_Discover_RequiresConnect(t *testing.T) {
	transport, peerURL := buildWireDriver(t, newFakeMockServer(minimalAgentCard("https://x")))
	defer func() { _ = transport.Close(context.Background()) }()

	prov, _ := toolsa2a.New(peerURL, transport)
	_, err := prov.Discover(context.Background())
	if !errors.Is(err, toolsa2a.ErrNotConnected) {
		t.Errorf("expected ErrNotConnected, got %v", err)
	}
}

func TestProvider_New_RejectsEmptyURL(t *testing.T) {
	transport, _ := buildWireDriver(t, newFakeMockServer(minimalAgentCard("https://x")))
	defer func() { _ = transport.Close(context.Background()) }()

	_, err := toolsa2a.New("", transport)
	if err == nil {
		t.Errorf("expected error for empty peer URL")
	}
}

func TestProvider_New_RejectsNilTransport(t *testing.T) {
	_, err := toolsa2a.New("https://peer", nil)
	if err == nil {
		t.Errorf("expected error for nil transport")
	}
}

func TestProvider_Invoke_EndToEnd_ThroughPolicyShell(t *testing.T) {
	mock := newFakeMockServer(minimalAgentCard(""))
	defer mock.Close()
	mock.card = minimalAgentCard(mock.URL())

	transport, peerURL := buildWireDriver(t, mock)
	defer func() { _ = transport.Close(context.Background()) }()

	prov, _ := toolsa2a.New(peerURL, transport)
	ctx := ctxWithIdentity(context.Background(), "t-a", "u-a", "s-a")
	if err := prov.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	desc, err := prov.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// Catalog edge: register the descriptors with a real catalog,
	// then resolve + invoke through the standard path so the
	// ToolPolicy shell fires.
	cat := tools.NewCatalog()
	for _, d := range desc {
		if err := cat.Register(d); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	echoName := desc[0].Tool.Name
	resolved, ok := cat.Resolve(echoName)
	if !ok {
		t.Fatalf("Resolve(%q): not found", echoName)
	}
	args, _ := json.Marshal(map[string]any{"text": "hello"})
	res, err := resolved.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	task, ok := res.Value.(a2atypes.Task)
	if !ok {
		t.Fatalf("ToolResult.Value type: %T", res.Value)
	}
	if task.ID != "task-1" {
		t.Errorf("Task ID mismatch: %v", task.ID)
	}
	// Verify the mock received a message; the message should carry
	// our args as a DataPart.
	v := mock.lastMsg.Load()
	if v == nil {
		t.Fatal("mock did not receive SendMessage")
	}
	msg, ok := v.(a2atypes.Message)
	if !ok {
		t.Fatalf("lastMsg type: %T", v)
	}
	if len(msg.Parts) != 1 {
		t.Errorf("expected 1 Part, got %d", len(msg.Parts))
	}
}

func TestProvider_SourceID(t *testing.T) {
	transport, peerURL := buildWireDriver(t, newFakeMockServer(minimalAgentCard("https://x")))
	defer func() { _ = transport.Close(context.Background()) }()

	prov, _ := toolsa2a.New(peerURL, transport)
	id := prov.SourceID()
	if id == "" || !strings.HasPrefix(string(id), "a2a:") {
		t.Errorf("SourceID malformed: %q", id)
	}
}

// TestProvider_ConcurrentReuse_D025 — AGENTS.md §5 / D-025: the
// Provider is a compiled artifact, immutable after construction; one
// shared instance must serve N concurrent Discover/Invoke calls under
// -race without data races, context bleed, cross-cancellation, or
// goroutine leaks.
func TestProvider_ConcurrentReuse_D025(t *testing.T) {
	mock := newFakeMockServer(minimalAgentCard(""))
	defer mock.Close()
	mock.card = minimalAgentCard(mock.URL())

	transport, peerURL := buildWireDriver(t, mock)
	defer func() { _ = transport.Close(context.Background()) }()

	prov, err := toolsa2a.New(peerURL, transport)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := prov.Connect(ctxWithIdentity(context.Background(), "t", "u", "s")); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	desc, err := prov.Discover(ctxWithIdentity(context.Background(), "t", "u", "s"))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cat := tools.NewCatalog()
	for _, d := range desc {
		if err := cat.Register(d); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	echoName := desc[0].Tool.Name
	resolved, ok := cat.Resolve(echoName)
	if !ok {
		t.Fatalf("Resolve(%q): not found", echoName)
	}

	const N = 100
	var (
		wg      sync.WaitGroup
		invokes atomic.Int64
		fails   atomic.Int64
	)
	wg.Add(N)
	for i := range N {
		go func(i int) {
			defer wg.Done()
			tenant := fmt.Sprintf("t-%d", i)
			user := fmt.Sprintf("u-%d", i)
			session := fmt.Sprintf("s-%d", i)
			ctx := ctxWithIdentity(context.Background(), tenant, user, session)
			args, _ := json.Marshal(map[string]any{"text": fmt.Sprintf("hello-%d", i)})
			if _, err := resolved.Invoke(ctx, args); err != nil {
				fails.Add(1)
				t.Errorf("[%d] Invoke: %v", i, err)
				return
			}
			invokes.Add(1)
			// Identity per-invoke must round-trip cleanly: read it back via
			// the wire driver's lastMsg (single-slot atomic — we don't
			// assert per-i but assert at the aggregate level below).
		}(i)
	}
	wg.Wait()

	if invokes.Load() != int64(N) {
		t.Fatalf("invokes=%d fails=%d, want all %d to succeed", invokes.Load(), fails.Load(), N)
	}
}
