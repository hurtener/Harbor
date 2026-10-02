package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
)

// The official MCP server/client transports and native dispatcher are real;
// only the server's deterministic binary-producing tool is synthetic.
func TestNativeMCPBinaryOutputWitness_FirstConsumer(t *testing.T) {
	bus := newTestBus(t)
	server := newMockServer()
	data := []byte{0, 255, 13, 10, 128, 42}
	mcpsdk.AddTool(server.server, &mcpsdk.Tool{Name: "emit_binary", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest, any) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{URI: "memory://native-output", MIMEType: "application/octet-stream", Blob: data}}}}, nil, nil
	})
	arts := newContentArtifactStore(t)
	provider, err := New(Config{Name: "native-witness", URL: "http://example.invalid", TransportMode: TransportAuto, Bus: bus, DefaultIdentity: defaultIdentity(), ArtifactStore: arts, DefaultPolicy: tools.ToolPolicy{Validate: tools.ValidateNone}})
	if err != nil {
		t.Fatal(err)
	}
	cleanup := pairProvider(t, server, provider)
	defer cleanup()
	defer func() { _ = provider.Close(context.Background()) }()
	id := defaultIdentity()
	ctx, _ := identity.With(context.Background(), id)
	ctx, _ = identity.WithRun(ctx, id, "shared-original-writer")
	q := identity.Quadruple{Identity: id, RunID: "shared-original-writer"}
	descs, err := provider.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cat := tools.NewCatalog()
	name := ""
	for _, desc := range descs {
		if err = cat.Register(desc); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(desc.Tool.Name, "emit_binary") {
			name = desc.Tool.Name
		}
	}
	if name == "" {
		t.Fatal("missing native binary tool")
	}
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(ctx) }()
	reg, err := tasks.OpenDriver("inprocess", tasks.Dependencies{Store: st, Bus: bus, Redactor: auditpatterns.New(), Cfg: config.TasksConfig{Driver: "inprocess"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close(ctx) }()
	executor := dispatch.NewToolExecutor(cat, arts, reg)
	var firstID string
	for i := range 2 {
		if i == 1 {
			q.RunID = "different-current-run"
			ctx, _ = identity.WithRun(ctx, id, q.RunID)
		}
		h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
		if err != nil {
			t.Fatal(err)
		}
		bound := tasks.WithOutputTask(ctx, h.ID)
		if err = reg.MarkRunning(bound, h.ID); err != nil {
			t.Fatal(err)
		}
		rc := planner.RunContext{Quadruple: q, Trajectory: &planner.Trajectory{}, Catalog: tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID})}
		raw, _, err := executor.ExecuteDecision(bound, rc, planner.CallTool{Tool: name, Args: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, data) || bytes.Contains(encoded, []byte(base64.StdEncoding.EncodeToString(data))) {
			t.Fatal("raw binary leaked into planner value")
		}
		if err = reg.MarkComplete(bound, h.ID, tasks.TaskResult{}); err != nil {
			t.Fatal(err)
		}
		task, err := reg.Get(bound, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.OutputManifest == nil || !task.OutputManifest.Sealed || len(task.OutputManifest.Artifacts) != 1 {
			t.Fatalf("missing current-task output witness: %+v", task.OutputManifest)
		}
		ref := task.OutputManifest.Artifacts[0]
		if i == 0 {
			firstID = ref.ID
		} else if firstID != ref.ID {
			t.Fatal("identical bytes did not reuse artifact")
		}
		scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
		metadata, found, err := arts.GetRef(bound, scope, ref.ID)
		if err != nil || !found || metadata.Scope.TaskID != "shared-original-writer" {
			t.Fatalf("first-writer stamp changed: %+v %v", metadata, err)
		}
		got, found, err := arts.Get(bound, scope, ref.ID)
		if err != nil || !found || !bytes.Equal(data, got) {
			t.Fatalf("exact native bytes unavailable: %v", err)
		}
	}
}
