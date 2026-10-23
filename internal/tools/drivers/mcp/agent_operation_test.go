package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/appoperation"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Only the broker boundary is forced; all MCP encoding, policy and dispatch
// use the production driver and the actual SDK's in-memory transport.
type agentOperationOAuth struct {
	stubOAuthProvider
	ordinary atomic.Int64
	bound    atomic.Int64
	ack      atomic.Bool
	generic  atomic.Bool
	mu       sync.Mutex
	refs     map[string]bool
	t        *testing.T
}

func (p *agentOperationOAuth) Token(ctx context.Context, source tools.ToolSourceID) (auth.Token, error) {
	b, ok := appoperation.From(ctx)
	if !ok {
		p.ordinary.Add(1)
		if p.generic.Load() {
			return auth.Token{AccessToken: "ordinary-token"}, nil
		}
		return auth.Token{}, &auth.GovernedAppMintRequiredError{}
	}
	p.bound.Add(1)
	if b.ServerID != string(source) || b.AgentID != "agent" {
		p.t.Error("wrong authenticated source/agent")
	}
	if b.Kind == "agent_tool" && (b.Tool != "source_save" || (string(b.Arguments) != `{"n":9007199254740993}` && string(b.Arguments) != `{"n":1e400}`)) {
		p.t.Errorf("wrong input binding: %+v", b)
	}
	p.mu.Lock()
	if p.refs[b.Reference] {
		p.t.Error("reference reused across calls")
	}
	p.refs[b.Reference] = true
	p.mu.Unlock()
	digest, err := b.Digest()
	if err != nil {
		return auth.Token{}, err
	}
	if !p.ack.Load() {
		digest = ""
	}
	return auth.Token{AccessToken: "operation-token", AppOperationSHA256: digest}, nil
}

func TestAgentOperation_InMemoryCallsAndConcurrentReuse(t *testing.T) {
	broker := &agentOperationOAuth{t: t, refs: map[string]bool{}}
	broker.ack.Store(true)
	p, err := New(Config{Name: "source", URL: "http://example.invalid", DefaultIdentity: defaultIdentity(), Bus: newTestBus(t), OAuthProvider: broker})
	if err != nil {
		t.Fatal(err)
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "agent-fixture", Version: "v0"}, nil)
	var calls atomic.Int64
	var fail atomic.Bool
	tool := &mcpsdk.Tool{Name: "save", InputSchema: map[string]any{"type": "object"}}
	server.AddTool(tool, func(_ context.Context, r *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		if !broker.generic.Load() && !strings.Contains(string(r.Params.Arguments), "9007199254740993") && string(r.Params.Arguments) != `{"n":1e400}` {
			t.Error("integer rounded on wire")
		}
		if fail.Load() {
			return nil, errors.New("lost result after effect")
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "saved"}}}, nil
	})
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := p.client.Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.session = cs
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Wait(); _ = p.Close(context.Background()) })
	desc, err := p.buildToolDescriptor(tool)
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"n":9007199254740993}`)
	ctx := tools.WithEffectiveAgentConfig(egressRunCtx(t, defaultIdentity(), "agent-run"), "agent")
	if _, err := desc.Invoke(ctx, args); err == nil || broker.bound.Load() != 0 {
		t.Fatal("untrusted caller promoted")
	}
	trusted := appoperation.WithAgentInvocation(ctx, "agent")
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if _, err := desc.Invoke(trusted, args); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 100 || broker.bound.Load() != 100 {
		t.Fatalf("fresh bindings missing: calls=%d bound=%d", calls.Load(), broker.bound.Load())
	}
	broker.ack.Store(false)
	if _, err := desc.Invoke(trusted, args); !errors.Is(err, appoperation.ErrInvalid) {
		t.Fatalf("bad acknowledgement accepted: %v", err)
	}
	if calls.Load() != 100 || broker.bound.Load() != 101 {
		t.Fatal("bad acknowledgement reached wire or retried")
	}
	broker.ack.Store(true)
	fail.Store(true)
	if _, err := desc.Invoke(trusted, args); err == nil {
		t.Fatal("unknown effect succeeded")
	}
	if calls.Load() != 101 || broker.bound.Load() != 102 {
		t.Fatal("unknown effect retried")
	}
	fail.Store(false)
	broker.generic.Store(true)
	for range 2 {
		if _, err := desc.Invoke(trusted, args); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 103 || broker.bound.Load() != 102 {
		t.Fatal("nongoverned call rebound")
	}
	mismatched := tools.WithEffectiveAgentConfig(trusted, "other")
	broker.generic.Store(false)
	if _, err := desc.Invoke(mismatched, args); err == nil || broker.bound.Load() != 102 {
		t.Fatal("changed effective agent promoted")
	}
	// Valid JSON exponents outside float64 range must survive governed binding.
	overflow := json.RawMessage(`{"n":1e400}`)
	if _, err := desc.Invoke(trusted, overflow); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 104 || broker.bound.Load() != 103 {
		t.Fatal("exact exponent did not reach governed wire")
	}
	broker.generic.Store(true)
	if _, err := desc.Invoke(trusted, overflow); !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("nongoverned overflow behavior changed: %v", err)
	}
	if calls.Load() != 104 || broker.bound.Load() != 103 {
		t.Fatal("nongoverned overflow reached wire")
	}
	ordinary := broker.ordinary.Load()
	for _, raw := range []string{`{"n":1e400,"n":1}`, `{"n":1e400} {}`, `{"n":1e400,`} {
		if _, err := desc.Invoke(trusted, json.RawMessage(raw)); !errors.Is(err, tools.ErrToolInvalidArgs) {
			t.Fatalf("malformed input changed behavior: %v", err)
		}
	}
	if broker.ordinary.Load() != ordinary || calls.Load() != 104 {
		t.Fatal("malformed input reached broker or wire")
	}
}

func TestAgentOperation_EgressPreservesExactNumbers(t *testing.T) {
	p, _ := newEgressProvider(t, Config{Bus: newRecordingBus(t)})
	mapping := egressMapping(t, map[string][]string{"ingest": {"doc"}})
	ctx := egressCtx(t, map[string][]byte{"artifact-id": []byte("payload")}, nil)
	plan, err := p.prepareEgress(ctx, "ingest", json.RawMessage(`{"doc":"artifact-id","n":9007199254740993}`), mapping, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan.args)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"doc":"cGF5bG9hZA==","n":9007199254740993}` {
		t.Fatalf("egress changed outgoing input: %s", raw)
	}
	bound, err := appoperation.WithAgentCall(ctx, "agent", string(p.source), "source_ingest", raw)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := appoperation.From(bound)
	if b.CheckCall(string(p.source), "source_ingest", plan.args) != nil {
		t.Fatal("post-egress input differs from binding")
	}
}
