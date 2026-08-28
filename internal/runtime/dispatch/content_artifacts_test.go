package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactcontent"
)

// dispatchContentResult is a provider-neutral fixture that models a driver
// returning one binary content part. Its JSON form intentionally includes the
// transient Data field, so the test catches any path that projects before the
// generic materializer has removed it.
type dispatchContentResult struct {
	Text string                    `json:"text,omitempty"`
	Data []byte                    `json:"data,omitempty"`
	Ref  *tools.ArtifactContentRef `json:"artifact,omitempty"`
}

func (v dispatchContentResult) ArtifactContentParts() []tools.ArtifactContentPart {
	if v.Ref != nil && len(v.Data) == 0 {
		return nil
	}
	return []tools.ArtifactContentPart{{
		Kind:     "binary",
		MIMEType: "application/octet-stream",
		Filename: "fixture.bin",
		Data:     append([]byte(nil), v.Data...),
	}}
}

func (v dispatchContentResult) WithArtifactContentRefs(refs []tools.ArtifactContentRef) (tools.ArtifactContentResult, error) {
	if len(refs) != 1 {
		return nil, errors.New("want one artifact ref")
	}
	v.Data = nil
	v.Ref = &refs[0]
	return v, nil
}

var _ tools.ArtifactContentResult = dispatchContentResult{}

func TestExecutor_TypedContentMaterializesBeforeRawAndLLM(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "typed"},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: dispatchContentResult{Text: "kept", Data: []byte("secret")}}, nil
		},
	}); err != nil {
		t.Fatalf("register typed: %v", err)
	}
	store := newTestArtifactStore(t)
	exec := NewToolExecutor(cat, store, nil, WithHeavyThreshold(1<<20))
	q := dispatchTestQuad("run-typed")
	raw, llm, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), planner.CallTool{Tool: "typed"})
	if err != nil {
		t.Fatalf("ExecuteDecision: %v", err)
	}
	assertProjectedDispatchContent(t, raw, "raw trajectory")
	assertProjectedDispatchContent(t, llm, "LLM projection")
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "c2VjcmV0") {
		t.Fatalf("raw observation leaked binary bytes: %s", encoded)
	}
	refs, err := store.List(context.Background(), artifacts.ArtifactScope{
		TenantID:  dispatchTestID.TenantID,
		UserID:    dispatchTestID.UserID,
		SessionID: dispatchTestID.SessionID,
	})
	if err != nil || len(refs) != 1 {
		t.Fatalf("session artifact manifest = %d err=%v, want one typed artifact", len(refs), err)
	}
}

func assertProjectedDispatchContent(t *testing.T, value any, label string) {
	t.Helper()
	got, ok := value.(dispatchContentResult)
	if !ok {
		t.Fatalf("%s type = %T, want dispatchContentResult", label, value)
	}
	if got.Text != "kept" || len(got.Data) != 0 || got.Ref == nil {
		t.Fatalf("%s = %+v, want text + ref and no bytes", label, got)
	}
}

func TestExecutor_TypedContentStoreFailureIsLoud(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "typed"},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: dispatchContentResult{Data: []byte("secret")}}, nil
		},
	}); err != nil {
		t.Fatalf("register typed: %v", err)
	}
	exec := NewToolExecutor(cat, nil, nil)
	q := dispatchTestQuad("run-typed-no-store")
	raw, llm, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), planner.CallTool{Tool: "typed"})
	if !errors.Is(err, artifactcontent.ErrStoreUnavailable) {
		t.Fatalf("error = %v, want a loud ArtifactStore failure", err)
	}
	if raw != nil || llm != nil {
		t.Fatalf("store failure returned planner values raw=%#v llm=%#v", raw, llm)
	}
}

func TestExecutor_TypedContentParallelOrdering(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	for _, name := range []string{"first", "second"} {
		name := name
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: dispatchContentResult{Text: name, Data: []byte(name)}}, nil
			},
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	store := newTestArtifactStore(t)
	exec := NewToolExecutor(cat, store, nil)
	q := dispatchTestQuad("run-typed-parallel")
	raw, llm, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "first", CallID: "c0"},
			{Tool: "second", CallID: "c1"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteDecision: %v", err)
	}
	obs, ok := raw.(planner.ParallelObservation)
	if !ok || len(obs.Branches) != 2 {
		t.Fatalf("parallel raw = %#v, want two branches", raw)
	}
	llmObs, ok := llm.(planner.ParallelObservation)
	if !ok || len(llmObs.Branches) != 2 {
		t.Fatalf("parallel LLM = %#v, want two branches", llm)
	}
	for i, want := range []string{"first", "second"} {
		if obs.Branches[i].Index != i || obs.Branches[i].CallID != []string{"c0", "c1"}[i] {
			t.Errorf("branch[%d] identity = %+v, want index/call id order", i, obs.Branches[i])
		}
		got, ok := obs.Branches[i].Value.(dispatchContentResult)
		if !ok || got.Text != want || len(got.Data) != 0 || got.Ref == nil {
			t.Errorf("branch[%d] value = %#v, want projected %q", i, obs.Branches[i].Value, want)
		}
		if llmObs.Branches[i].Index != i || llmObs.Branches[i].CallID != []string{"c0", "c1"}[i] {
			t.Errorf("LLM branch[%d] identity = %+v, want index/call id order", i, llmObs.Branches[i])
		}
		llmValue, ok := llmObs.Branches[i].Value.(dispatchContentResult)
		if !ok || llmValue.Text != want || len(llmValue.Data) != 0 || llmValue.Ref == nil {
			t.Errorf("LLM branch[%d] value = %#v, want projected %q", i, llmObs.Branches[i].Value, want)
		}
	}
}
