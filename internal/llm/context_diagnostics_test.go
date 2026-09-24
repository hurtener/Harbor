package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

func TestContextDiagnostics_EstimateConservation(t *testing.T) {
	t.Parallel()
	text, id := "abcdefgh", "call"
	cases := []struct {
		name string
		req  llm.CompleteRequest
		want llm.RequestTokenSections
	}{
		{"empty", llm.CompleteRequest{}, llm.RequestTokenSections{}},
		{"text", llm.CompleteRequest{Messages: []llm.ChatMessage{{Content: llm.Content{Text: &text}}}}, llm.RequestTokenSections{Text: 3, Framing: 4}},
		{"call", llm.CompleteRequest{Messages: []llm.ChatMessage{{ToolCalls: []llm.ToolCallStructured{{ID: id, Name: "read", Args: json.RawMessage(`{}`)}}}}}, llm.RequestTokenSections{Calls: 5, Framing: 8}},
		{"result", llm.CompleteRequest{Messages: []llm.ChatMessage{{Content: llm.Content{Text: &text}, ToolCallID: &id}}}, llm.RequestTokenSections{Text: 3, Calls: 2, Framing: 4}},
		{"declaration", llm.CompleteRequest{Tools: []llm.ToolDeclaration{{Name: "read", Description: "desc", Schema: json.RawMessage(`{}`)}}}, llm.RequestTokenSections{Tools: 5, Framing: 4}},
		{"schema", llm.CompleteRequest{ResponseFormat: &llm.ResponseFormat{JSONSchema: json.RawMessage(`{}`)}}, llm.RequestTokenSections{Schema: 1}},
		{"other", llm.CompleteRequest{Stops: []string{"STOP", "END"}, Extra: map[string]any{"x": 1}, ToolChoice: "required"}, llm.RequestTokenSections{Framing: 3, Other: 5}},
		{"media", llm.CompleteRequest{Messages: []llm.ChatMessage{{Content: llm.Content{Parts: []llm.ContentPart{{Type: llm.PartText, Text: text}, {Type: llm.PartImage}, {Type: llm.PartAudio}, {Type: llm.PartFile}}}}}}, llm.RequestTokenSections{Text: 3, Media: 768, Framing: 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, algorithm := range []string{"", "chars_div_4", "unknown"} {
				profile := llm.ModelProfile{TokenEstimator: algorithm}
				got := llm.EstimateRequestTokenSections(tc.req, profile)
				if got != tc.want || got.Total() != llm.EstimateRequestTokens(tc.req, profile) {
					t.Fatalf("got %+v want %+v", got, tc.want)
				}
			}
		})
	}
}

func nextContextDiagnostic(t *testing.T, sub events.Subscription) llm.ContextPreparedPayload {
	t.Helper()
	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(llm.ContextPreparedPayload)
		if !ok || p.Identity != ev.Identity || p.OccurredAt != ev.OccurredAt {
			t.Fatalf("invalid diagnostic envelope: %#v", ev)
		}
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no request diagnostic received")
		return llm.ContextPreparedPayload{}
	}
}

func TestContextDiagnostics_LeafBoundsAndMaintenanceIsolation(t *testing.T) {
	t.Parallel()
	deps, cleanup := makeDeps(t)
	defer cleanup()
	sub, err := deps.Bus.Subscribe(t.Context(), events.Filter{Admin: true, Types: []events.EventType{llm.EventTypeContextPrepared}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	driver := &capacityRecordingDriver{}
	name := uniqueDriverName("diagnostics")
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	cfg := makeSnapshot("PRIVATE-MODEL", 1000)
	cfg.Driver = name
	cfg.DisableCorrections, cfg.DisableDowngrade, cfg.DisableRetry, cfg.DisableGovernance = true, true, true, true
	client, err := llm.Open(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}, RunID: "run"}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatal(err)
	}
	history := &llm.ContextHistory{CheckpointVersion: 1, CheckpointGeneration: 2, ReplayStart: 3, ReplayEnd: 5, UnseenFrom: 4, UnseenKnown: true}
	ctx = llm.WithContextPreparation(ctx, llm.ContextPreparation{InputTarget: 500, History: func() *llm.ContextHistory { return history }})
	ctx = llm.WithAttemptStep(ctx, 7)
	ctx = llm.WithAttemptScope(ctx, &llm.AttemptScope{CallID: "PRIVATE-CALL", AttemptNonce: "PRIVATE-NONCE", Attempt: 2, Retry: 1, Downgrade: 3, FallbackHop: 1})
	text := "PRIVATE-PROMPT"
	req := llm.CompleteRequest{Model: "PRIVATE-MODEL", Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}, Extra: map[string]any{"private_extra": "PRIVATE-EXTRA"}}
	for _, kind := range []string{"unknown-output", "reserved", "capacity", "maintenance"} {
		callCtx := ctx
		output := 100
		req.MaxTokens = &output
		switch kind {
		case "unknown-output":
			req.MaxTokens = nil
		case "capacity":
			output = 1000
		case "maintenance":
			callCtx, err = llm.CompactionAttemptContext(ctx, 2)
			if err != nil {
				t.Fatal(err)
			}
		}
		_, err = client.Complete(callCtx, req)
		if kind == "capacity" {
			if !errors.Is(err, llm.ErrContextWindowExceeded) {
				t.Fatalf("capacity failure: %v", err)
			}
		} else if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		p := nextContextDiagnostic(t, sub)
		if p.EstimatedTokens != llm.EstimateRequestTokens(req, cfg.ModelProfiles[req.Model]) || p.Sections.Total() != p.EstimatedTokens || p.Identity != q || p.PlannerStep != 7 {
			t.Fatalf("incorrect estimate/scope: %+v", p)
		}
		if p.OutputLimitKnown != (kind != "unknown-output") || p.CapacityExceeded != (kind == "capacity") {
			t.Fatalf("unknown and zero conflated: %+v", p)
		}
		wantLimit, wantOutput := 850, 100
		switch kind {
		case "unknown-output":
			wantLimit, wantOutput = 950, 0
		case "capacity":
			wantLimit, wantOutput = 0, 1000
		}
		if p.InputLimitExclusive != wantLimit || p.OutputReserved != wantOutput || p.ContextWindowTokens != 1000 {
			t.Fatalf("wrong effective capacity: %+v", p)
		}
		if kind == "maintenance" {
			if p.MaintenanceOrdinal != 2 || p.History != nil || p.WorkingInputTarget != 0 {
				t.Fatalf("maintenance inherited history: %+v", p)
			}
		} else {
			if p.History == history || p.History == nil || *p.History != *history || p.WorkingInputTarget != 500 || !p.AttemptKnown || p.Attempt != 2 || p.Retry != 1 || p.Downgrade != 3 || p.FallbackHop != 1 {
				t.Fatalf("lost/aliased checkpoint/attempt: %+v", p)
			}
			p.History.ReplayEnd = 999
			if history.ReplayEnd != 5 {
				t.Fatal("subscriber mutated runtime snapshot")
			}
		}
		encoded, err := json.Marshal(p)
		if err != nil || len(encoded) > 1600 || strings.Contains(string(encoded), "PRIVATE-") || strings.Contains(string(encoded), "private_extra") {
			t.Fatalf("content-bearing diagnostic: %s (%v)", encoded, err)
		}
	}
	if driver.calls.Load() != 3 {
		t.Fatalf("capacity rejection reached provider: %d calls", driver.calls.Load())
	}
}

func TestContextDiagnostics_ConcurrentRunIsolation(t *testing.T) {
	t.Parallel()
	deps, cleanup := makeDeps(t)
	defer cleanup()
	sub, err := deps.Bus.Subscribe(t.Context(), events.Filter{Admin: true, Types: []events.EventType{llm.EventTypeContextPrepared}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	driver := &capacityRecordingDriver{}
	name := uniqueDriverName("diagnostics-concurrent")
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	cfg := makeSnapshot("m", 10000)
	cfg.Driver = name
	cfg.DisableCorrections, cfg.DisableDowngrade, cfg.DisableRetry, cfg.DisableGovernance = true, true, true, true
	client, err := llm.Open(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Go(func() {
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: fmt.Sprint(i)}
			ctx, err := identity.WithRun(t.Context(), id, "run")
			if err != nil {
				t.Error(err)
				return
			}
			ctx = llm.WithContextPreparation(ctx, llm.ContextPreparation{History: func() *llm.ContextHistory { return &llm.ContextHistory{ReplayEnd: i} }})
			text := strings.Repeat("x", i*4)
			if _, err := client.Complete(ctx, llm.CompleteRequest{Model: "m", Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for range 128 {
		p := nextContextDiagnostic(t, sub)
		if p.History == nil || fmt.Sprint(p.History.ReplayEnd) != p.Identity.SessionID || seen[p.Identity.SessionID] || p.EstimatedTokens != p.History.ReplayEnd+5 {
			t.Fatalf("scope bleed/duplicate: %+v", p)
		}
		seen[p.Identity.SessionID] = true
	}
}
