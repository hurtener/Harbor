package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

type preparationClientFunc func(context.Context, CompleteRequest) (CompleteResponse, error)

func (f preparationClientFunc) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	return f(ctx, req)
}
func (preparationClientFunc) Close(context.Context) error { return nil }

func preparationIdentity(t *testing.T, run string) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(t.Context(), identity.Identity{TenantID: "t", UserID: "u", SessionID: run}, run)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestContextPreparation_AssembledInputAndResolvedCapacity(t *testing.T) {
	t.Parallel()
	output := 200
	profile := ModelProfile{ContextWindowTokens: 1000, DefaultMaxTokens: &output}
	cfg := ConfigSnapshot{Model: "m", ContextWindowReserve: .05, ModelProfiles: map[string]ModelProfile{"m": profile}}
	text, compacted := "inspect", "compacted"
	for _, section := range []string{"instructions", "tool schema", "tool description", "call arguments", "output schema"} {
		t.Run(section, func(t *testing.T) {
			req := CompleteRequest{Messages: []ChatMessage{{Role: RoleUser, Content: Content{Text: &text}}}}
			large := strings.Repeat("x", 4000)
			switch section {
			case "instructions":
				req.Messages = append([]ChatMessage{{Role: RoleSystem, Content: Content{Text: &large}}}, req.Messages...)
			case "tool schema":
				req.Tools = []ToolDeclaration{{Name: "read", Schema: json.RawMessage(`{"description":"` + large + `"}`)}}
			case "tool description":
				req.Tools = []ToolDeclaration{{Name: "read", Description: large}}
			case "call arguments":
				id := "call-1"
				req.Messages = append(req.Messages, ChatMessage{Role: RoleAssistant, ToolCalls: []ToolCallStructured{{ID: id, Name: "read", Args: json.RawMessage(`{"source":"` + large + `"}`)}}}, ChatMessage{Role: RoleTool, ToolCallID: &id, Content: Content{Text: &text}})
			case "output schema":
				req.ResponseFormat = &ResponseFormat{Kind: FormatJSONSchema, JSONSchema: json.RawMessage(`{"description":"` + large + `"}`)}
			}
			before := EstimateRequestTokens(req, profile)
			compactions, rebuilds, sends := 0, 0, 0
			req.RebuildMessages = func() ([]ChatMessage, error) {
				rebuilds++
				return []ChatMessage{{Role: RoleUser, Content: Content{Text: &compacted}}}, nil
			}
			ctx := WithContextPreparation(preparationIdentity(t, section), ContextPreparation{InputTarget: 900, Compact: func(_ context.Context, got, target int) (bool, error) {
				compactions++
				if got != before || target != 749 {
					t.Fatalf("estimate/target=%d/%d, want %d/749", got, target, before)
				}
				return true, nil
			}})
			client := &contextPreparationClient{cfg: cfg, inner: preparationClientFunc(func(_ context.Context, got CompleteRequest) (CompleteResponse, error) {
				sends++
				if got.Model != cfg.Model {
					t.Fatal("resolved decision and maintenance would use different governance model keys")
				}
				if got.RebuildMessages != nil || len(got.Messages) != 1 || *got.Messages[0].Content.Text != compacted {
					t.Fatal("messages were not rebuilt once or callback escaped to downstream")
				}
				if len(got.Tools) != len(req.Tools) || got.ResponseFormat != req.ResponseFormat || got.MaxTokens != req.MaxTokens {
					t.Fatal("preparation rewrote authority or declarations")
				}
				return CompleteResponse{}, nil
			})}
			if _, err := client.Complete(ctx, req); err != nil {
				t.Fatal(err)
			}
			if compactions != 1 || rebuilds != 1 || sends != 1 {
				t.Fatalf("counts=%d/%d/%d", compactions, rebuilds, sends)
			}
		})
	}
}

func TestContextPreparation_FailureAndBypass(t *testing.T) {
	t.Parallel()
	boom := errors.New("scripted failure")
	for _, scenario := range []string{"under target", "disabled", "maintenance", "no eligible prefix", "compactor failure", "rebuild failure", "cancelled", "missing identity", "invalid pairing", "unknown model"} {
		t.Run(scenario, func(t *testing.T) {
			text := strings.Repeat("x", 4000)
			req := CompleteRequest{Model: "m", Messages: []ChatMessage{{Role: RoleUser, Content: Content{Text: &text}}}}
			compactions, rebuilds, sends := 0, 0, 0
			req.RebuildMessages = func() ([]ChatMessage, error) {
				rebuilds++
				if scenario == "rebuild failure" {
					return nil, boom
				}
				return req.Messages, nil
			}
			prep := ContextPreparation{InputTarget: 500, Compact: func(context.Context, int, int) (bool, error) {
				compactions++
				if scenario == "compactor failure" {
					return false, boom
				}
				return scenario != "no eligible prefix", nil
			}}
			ctx := preparationIdentity(t, scenario)
			switch scenario {
			case "under target":
				text = "small"
			case "disabled":
				prep.Compact = nil
			case "maintenance":
				req.RebuildMessages = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "missing identity":
				ctx = t.Context()
			case "invalid pairing":
				req.Messages = append(req.Messages, ChatMessage{Role: RoleAssistant, ToolCalls: []ToolCallStructured{{ID: "orphan", Name: "read"}}})
			case "unknown model":
				req.Model = "unknown"
			}
			ctx = WithContextPreparation(ctx, prep)
			client := &contextPreparationClient{cfg: ConfigSnapshot{ModelProfiles: map[string]ModelProfile{"m": {ContextWindowTokens: 10000}}, ContextWindowReserve: .05}, inner: preparationClientFunc(func(context.Context, CompleteRequest) (CompleteResponse, error) {
				sends++
				return CompleteResponse{}, nil
			})}
			_, err := client.Complete(ctx, req)
			switch scenario {
			case "compactor failure", "rebuild failure", "cancelled", "missing identity", "invalid pairing":
				if err == nil || sends != 0 {
					t.Fatal("failed preparation reached model")
				}
				if (scenario == "compactor failure" || scenario == "rebuild failure") && !errors.Is(err, boom) {
					t.Fatal("failure identity lost")
				}
			default:
				if err != nil || sends != 1 {
					t.Fatalf("ordinary path changed: %v", err)
				}
			}
			if scenario == "compactor failure" || scenario == "no eligible prefix" || scenario == "rebuild failure" {
				if compactions != 1 {
					t.Fatal("preparation repeated")
				}
			} else if compactions != 0 || rebuilds != 0 {
				t.Fatal("ineligible request started maintenance")
			}
			if scenario == "no eligible prefix" && rebuilds != 0 {
				t.Fatal("unchanged checkpoint rebuilt")
			}
		})
	}
}

func TestContextPreparation_ZeroTargetTracksModelAndPreservesOutput(t *testing.T) {
	t.Parallel()
	output := 128000
	cfg := ConfigSnapshot{
		Model: "large", ContextWindowReserve: .05,
		ModelProfiles: map[string]ModelProfile{
			"large":   {ContextWindowTokens: 200000},
			"smaller": {ContextWindowTokens: 160000},
		},
	}
	// Capacity follows each request's model, never a cached global or the
	// historical sample's 12k input target.
	for _, model := range []string{"large", "smaller"} {
		t.Run(model, func(t *testing.T) {
			before := strings.Repeat("historical evidence ", 20000)
			after := "bounded checkpoint and fresh tool result"
			calls, sends := 0, 0
			req := CompleteRequest{
				Model: model, MaxTokens: &output,
				Messages: []ChatMessage{{Role: RoleUser, Content: Content{Text: &before}}},
				RebuildMessages: func() ([]ChatMessage, error) {
					return []ChatMessage{{Role: RoleUser, Content: Content{Text: &after}}}, nil
				},
			}
			capacity, _, err := requestInputLimit(req, cfg.ModelProfiles[model], cfg.ContextWindowReserve)
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithContextPreparation(preparationIdentity(t, model), ContextPreparation{
				Compact: func(ctx context.Context, input, target int) (bool, error) {
					calls++
					if target != capacity-1 || target <= 12000 || input <= target {
						t.Fatalf("automatic target/input = %d/%d, capacity=%d", target, input, capacity)
					}
					if err := CheckContextCandidate(ctx); err != nil {
						return false, err
					}
					return true, nil
				},
			})
			client := &contextPreparationClient{cfg: cfg, inner: preparationClientFunc(func(_ context.Context, got CompleteRequest) (CompleteResponse, error) {
				sends++
				if got.Model != model || got.MaxTokens != req.MaxTokens || *got.MaxTokens != 128000 || *got.Messages[0].Content.Text != after {
					t.Fatal("input compaction changed model/output allowance or failed to install checkpoint")
				}
				return CompleteResponse{}, nil
			})}
			if _, err := client.Complete(ctx, req); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || sends != 1 {
				t.Fatalf("compactions/sends=%d/%d", calls, sends)
			}
		})
	}
}

func TestContextPreparation_ConcurrentRunIsolation(t *testing.T) {
	t.Parallel()
	var sends atomic.Int64
	client := &contextPreparationClient{cfg: ConfigSnapshot{ModelProfiles: map[string]ModelProfile{"m": {ContextWindowTokens: 10000}}, ContextWindowReserve: .05}, inner: preparationClientFunc(func(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
		id := identityQuad(ctx).Identity
		if req.RebuildMessages != nil || len(req.Messages) != 1 || *req.Messages[0].Content.Text != id.SessionID {
			t.Error("cross-run context or callback escape")
		}
		sends.Add(1)
		return CompleteResponse{}, nil
	})}
	var wg sync.WaitGroup
	for i := range 128 {
		name := fmt.Sprintf("session-%d", i)
		ctx := preparationIdentity(t, name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			text := strings.Repeat(name, 400)
			ctx = WithContextPreparation(ctx, ContextPreparation{InputTarget: 100, Compact: func(context.Context, int, int) (bool, error) { return true, nil }})
			_, err := client.Complete(ctx, CompleteRequest{Model: "m", Messages: []ChatMessage{{Role: RoleUser, Content: Content{Text: &text}}}, RebuildMessages: func() ([]ChatMessage, error) {
				return []ChatMessage{{Role: RoleUser, Content: Content{Text: &name}}}, nil
			}})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if sends.Load() != 128 {
		t.Fatalf("sends=%d", sends.Load())
	}
}
