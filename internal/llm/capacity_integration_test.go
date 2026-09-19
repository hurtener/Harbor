package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

type capacityRecordingDriver struct{ calls atomic.Int64 }

func (d *capacityRecordingDriver) Complete(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
	d.calls.Add(1)
	return llm.CompleteResponse{Content: "ok"}, nil
}
func (*capacityRecordingDriver) Close(context.Context) error { return nil }

func TestSafety_OutputReservationAndNativeInput(t *testing.T) {
	t.Parallel()
	deps, cleanup := makeDeps(t)
	defer cleanup()
	driver := &capacityRecordingDriver{}
	name := uniqueDriverName("capacity")
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	cfg := makeSnapshot("large", 1000)
	cfg.Driver = name
	cfg.DisableCorrections, cfg.DisableDowngrade, cfg.DisableRetry, cfg.DisableGovernance = true, true, true, true
	defaultOutput := 200
	cfg.ModelProfiles["large"] = llm.ModelProfile{ContextWindowTokens: 1000, DefaultMaxTokens: &defaultOutput}
	cfg.ModelProfiles["smaller"] = llm.ModelProfile{ContextWindowTokens: 500, DefaultMaxTokens: &defaultOutput}
	client, err := llm.Open(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	ctx := withIdentity(t, context.Background())
	// 4 message framing + len/4 + 1 = 749; input capacity is 750.
	text := strings.Repeat("x", (749-5)*4)
	req := llm.CompleteRequest{Model: "large", Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}}
	if _, err = client.Complete(ctx, req); err != nil {
		t.Fatalf("below cap: %v", err)
	}
	before := driver.calls.Load()
	text += "xxxx"
	if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("at exclusive cap: %v", err)
	}
	if driver.calls.Load() != before {
		t.Fatal("oversized request reached driver")
	}
	smallOutput := 100
	req.MaxTokens = &smallOutput
	if _, err = client.Complete(ctx, req); err != nil {
		t.Fatalf("smaller explicit output: %v", err)
	}
	req.Model = "smaller"
	before = driver.calls.Load()
	if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("model switch failed to recheck: %v", err)
	}
	if driver.calls.Load() != before {
		t.Fatal("smaller model received oversized request")
	}
	req.Model = "large"
	text = "inspect"
	req.MaxTokens = nil
	req.Tools = []llm.ToolDeclaration{{Name: "read", Schema: json.RawMessage(`{"description":"` + strings.Repeat("s", 3200) + `"}`)}}
	if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("native declarations not counted: %v", err)
	}
	req.Tools = nil
	callID := "call"
	req.Messages = append(req.Messages, llm.ChatMessage{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCallStructured{{ID: callID, Name: "edit", Args: json.RawMessage(`{"source":"` + strings.Repeat("a", 3200) + `"}`)}}}, llm.ChatMessage{Role: llm.RoleTool, ToolCallID: &callID, Content: llm.Content{Text: &text}})
	if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("historical arguments not counted: %v", err)
	}
}

func TestSafety_OutputReservationConcurrentReuse(t *testing.T) {
	t.Parallel()
	deps, cleanup := makeDeps(t)
	defer cleanup()
	driver := &capacityRecordingDriver{}
	name := uniqueDriverName("capacity-concurrent")
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	cfg := makeSnapshot("m", 1000)
	cfg.Driver = name
	cfg.DisableCorrections, cfg.DisableDowngrade, cfg.DisableRetry, cfg.DisableGovernance = true, true, true, true
	client, err := llm.Open(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	ctx := withIdentity(t, context.Background())
	text := strings.Repeat("x", 1600)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			output := 100
			if i%2 == 1 {
				output = 600
			}
			_, callErr := client.Complete(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &output, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}})
			if i%2 == 0 && callErr != nil {
				t.Errorf("fitting request: %v", callErr)
			}
			if i%2 == 1 && !errors.Is(callErr, llm.ErrContextWindowExceeded) {
				t.Errorf("oversized request: %v", callErr)
			}
		}()
	}
	wg.Wait()
	if got := driver.calls.Load(); got != 64 {
		t.Fatalf("driver calls=%d, want exactly64 admitted requests", got)
	}
}
