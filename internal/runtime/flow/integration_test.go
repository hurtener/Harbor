package flow_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// TestIntegration_FlowAndInProcTool_CoexistInCatalog confirms a
// flow tool and an in-process tool live side-by-side in the same
// catalog.
func TestIntegration_FlowAndInProcTool_CoexistInCatalog(t *testing.T) {
	type EchoArgs struct {
		Text string `json:"text"`
	}
	type EchoOut struct {
		Echo string `json:"echo"`
	}
	cat := tools.NewCatalog()
	err := inproc.RegisterFunc[EchoArgs, EchoOut](cat, "echo",
		func(ctx context.Context, in EchoArgs) (EchoOut, error) {
			return EchoOut{Echo: in.Text}, nil
		},
	)
	if err != nil {
		t.Fatalf("register echo: %v", err)
	}

	def := flow.Definition{
		Name:        "passthrough_flow",
		Description: "3-node passthrough",
		Entry:       "a",
		Exit:        "c",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough, To: []flow.NodeID{"b"}},
			"b": {Func: passthrough, To: []flow.NodeID{"c"}},
			"c": {Func: passthrough},
		},
	}
	type FlowIn struct {
		Q string `json:"q"`
	}
	type FlowOut struct {
		Answer string `json:"answer"`
	}
	def, err = flow.WithSchemasFrom[FlowIn, FlowOut](def)
	if err != nil {
		t.Fatalf("WithSchemasFrom: %v", err)
	}
	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err = eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = eng.Stop(ctx)
	})
	_, err = flow.RegisterAsTool(cat, def, eng)
	if err != nil {
		t.Fatalf("RegisterAsTool: %v", err)
	}

	filter := tools.CatalogFilter{
		TenantID: "t", UserID: "u", SessionID: "s",
	}
	list := cat.List(filter)
	if len(list) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(list))
	}

	for _, tool := range list {
		switch tool.Name {
		case "echo":
			if tool.Transport != tools.TransportInProcess {
				t.Errorf("echo: expected TransportInProcess, got %q", tool.Transport)
			}
		case "passthrough_flow":
			if tool.Transport != tools.TransportFlow {
				t.Errorf("passthrough_flow: expected TransportFlow, got %q", tool.Transport)
			}
		}
	}

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(context.Background(), id)

	echoDesc, _ := cat.Resolve("echo")
	echoArgs, _ := json.Marshal(EchoArgs{Text: "hello"})
	echoRes, err := echoDesc.Invoke(ctx, echoArgs)
	if err != nil {
		t.Fatalf("echo Invoke: %v", err)
	}
	if echoRes.Value == nil {
		t.Fatalf("echo: nil result")
	}

	flowDesc, _ := cat.Resolve("passthrough_flow")
	flowArgs := json.RawMessage(`{"q":"hi"}`)
	flowRes, err := flowDesc.Invoke(ctx, flowArgs)
	if err != nil {
		t.Fatalf("flow Invoke: %v", err)
	}
	if flowRes.Value == nil {
		t.Fatalf("flow: nil result")
	}
}

func TestIntegration_FlowBudget_ParentCtxDeadline_FiresViaCtx(t *testing.T) {
	slow := func(ctx context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		select {
		case <-ctx.Done():
			return messages.Envelope{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return in, nil
		}
	}
	def := flow.Definition{
		Name:  "slow_flow_ctx",
		Entry: "a",
		Exit:  "a",
		Budget: flow.Budget{
			Deadline: 1 * time.Second,
		},
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: slow},
		},
	}
	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err = eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = eng.Stop(ctx)
	})

	cat := tools.NewCatalog()
	_, err = flow.RegisterAsTool(cat, def, eng)
	if err != nil {
		t.Fatalf("RegisterAsTool: %v", err)
	}
	d, _ := cat.Resolve("slow_flow_ctx")

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(context.Background(), id)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = d.Invoke(ctx, []byte(`{}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected ctx-deadline error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Logf("invoke error: %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("expected ctx deadline to fire ~30ms, elapsed=%v", elapsed)
	}
}
