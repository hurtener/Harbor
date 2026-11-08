package llm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/telemetry"
)

type timingDriver struct {
	complete func(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error)
}

func (d timingDriver) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	return d.complete(ctx, req)
}
func (timingDriver) Close(context.Context) error { return nil }

type leafTimingDriver struct{ timingDriver }

func (leafTimingDriver) OwnsProviderRequestTiming() {}

func timingClient(t *testing.T, driver llm.Driver, logs *bytes.Buffer, configure ...func(*llm.ConfigSnapshot)) llm.LLMClient {
	t.Helper()
	deps, cleanup := makeDeps(t)
	t.Cleanup(cleanup)
	deps.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	name := uniqueDriverName("timing")
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	cfg := makeSnapshot("m", 100000)
	cfg.Driver, cfg.Provider = name, "openai"
	cfg.DisableCorrections, cfg.DisableDowngrade, cfg.DisableRetry, cfg.DisableGovernance = true, true, true, true
	for _, apply := range configure {
		apply(&cfg)
	}
	client, err := llm.Open(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client
}

func timingRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "llm.call.completed" {
			records = append(records, record)
		}
	}
	return records
}

func TestCallTiming_FieldsOutcomesAndPrivacy(t *testing.T) {
	const secret = "private prompt output token https://example.invalid/?credential=private"
	cases := []struct {
		name                string
		stream, delta, tool bool
		usage               llm.Usage
		callErr             error
		wantOutcome         string
		wantTokens          bool
	}{
		{name: "unary reported", usage: llm.Usage{ReportPresent: true, PromptTokens: 12, CompletionTokens: 3}, wantOutcome: "ok", wantTokens: true},
		{name: "streamed reasoning", stream: true, delta: true, wantOutcome: "ok"},
		{name: "streamed tool", stream: true, tool: true, wantOutcome: "ok"},
		{name: "terminal only", stream: true, wantOutcome: "ok"},
		{name: "unreported counts", usage: llm.Usage{PromptTokens: 12, CompletionTokens: 3}, wantOutcome: "ok"},
		{name: "estimated counts", usage: llm.Usage{ReportPresent: true, Estimated: true, PromptTokens: 12, CompletionTokens: 3}, wantOutcome: "ok"},
		{name: "reported zero counts", usage: llm.Usage{ReportPresent: true}, wantOutcome: "ok", wantTokens: true},
		{name: "reported zero output", usage: llm.Usage{ReportPresent: true, PromptTokens: 12}, wantOutcome: "ok", wantTokens: true},
		{name: "error", callErr: errors.New(secret), wantOutcome: "error"},
		{name: "canceled", callErr: fmt.Errorf("%s: %w", secret, context.Canceled), wantOutcome: "canceled"},
		{name: "timeout", callErr: fmt.Errorf("%s: %w", secret, context.DeadlineExceeded), wantOutcome: "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			client := timingClient(t, timingDriver{complete: func(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
				if tc.stream && req.OnContent != nil {
					req.OnContent("", false)
					req.OnContent("", true)
				}
				if tc.delta {
					req.OnReasoning(secret, false)
				}
				if tc.tool {
					llm.ObserveStreamDelta(ctx)
				}
				return llm.CompleteResponse{Content: secret, Reasoning: secret, Usage: tc.usage}, tc.callErr
			}}, &logs)
			ctx := llm.WithCallRole(withIdentity(t, t.Context()), llm.CallRoleTitle)
			prompt := secret
			req := llm.CompleteRequest{Model: "m", ReasoningEffort: llm.ReasoningHigh, Stream: tc.stream, OnContent: func(string, bool) {}, OnReasoning: func(string, bool) {}, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &prompt}}}, Extra: map[string]any{"credential": secret}}
			_, _ = client.Complete(ctx, req)
			records := timingRecords(t, &logs)
			if len(records) != 1 {
				t.Fatalf("records=%v", records)
			}
			got := records[0]
			for key, want := range map[string]any{"step_kind": "model", "role": "title", "provider": "openai", "model": "m", "reasoning_effort": "high", "outcome": tc.wantOutcome, "tenant_id": "T", "session_id": "S"} {
				if got[key] != want {
					t.Errorf("%s=%v want %v", key, got[key], want)
				}
			}
			_, hasTTFT := got["ttft_ms"]
			if hasTTFT != (tc.stream && (tc.delta || tc.tool)) {
				t.Errorf("ttft presence=%v", hasTTFT)
			}
			for _, key := range []string{"input_tokens", "output_tokens"} {
				_, exists := got[key]
				if exists != tc.wantTokens {
					t.Errorf("%s presence=%v", key, exists)
				}
			}
			if tc.wantTokens && (got["input_tokens"] != float64(tc.usage.PromptTokens) || got["output_tokens"] != float64(tc.usage.CompletionTokens)) {
				t.Errorf("reported usage changed: %v", got)
			}
			if total, ok := got["total_ms"].(float64); !ok || total < 0 {
				t.Errorf("total_ms=%v", got["total_ms"])
			}
			if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "credential") {
				t.Fatalf("secret leaked: %s", logs.String())
			}
		})
	}
}

func TestCallTiming_SafetyTimeout(t *testing.T) {
	var logs bytes.Buffer
	client := timingClient(t, timingDriver{complete: func(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
		<-ctx.Done()
		return llm.CompleteResponse{}, ctx.Err()
	}}, &logs, func(cfg *llm.ConfigSnapshot) { cfg.Timeout = time.Millisecond })
	_, err := client.Complete(withIdentity(t, context.Background()), llm.CompleteRequest{Model: "m"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	records := timingRecords(t, &logs)
	if len(records) != 1 || records[0]["outcome"] != "timeout" {
		t.Fatalf("records = %v", records)
	}
}

func TestCallTiming_PanicRecordsAndPropagates(t *testing.T) {
	for _, callback := range []bool{false, true} {
		t.Run(fmt.Sprint(callback), func(t *testing.T) {
			var logs bytes.Buffer
			original := &struct{ message string }{"private panic payload"}
			client := timingClient(t, timingDriver{complete: func(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
				if callback {
					req.OnContent("private output", false)
				}
				panic(original)
			}}, &logs)
			func() {
				defer func() {
					if got := recover(); got != original {
						t.Fatalf("panic = %v, want original", got)
					}
				}()
				_, _ = client.Complete(withIdentity(t, t.Context()), llm.CompleteRequest{Model: "m", Stream: callback, OnContent: func(string, bool) { panic(original) }})
			}()
			records := timingRecords(t, &logs)
			if len(records) != 1 || records[0]["outcome"] != "error" {
				t.Fatalf("records = %v", records)
			}
			if strings.Contains(logs.String(), "private") {
				t.Fatalf("panic payload leaked: %s", logs.String())
			}
		})
	}
}

func TestCallTiming_PreModelWaitsForPlannerDispatch(t *testing.T) {
	var logs bytes.Buffer
	client := timingClient(t, timingDriver{complete: func(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: "ok"}, nil
	}}, &logs)
	ctx := telemetry.BeginTurnTiming(withIdentity(t, t.Context()), slog.New(slog.NewJSONHandler(&logs, nil)), time.Now())
	for _, role := range []llm.CallRole{llm.CallRoleTitle, llm.CallRoleCompaction} {
		if _, err := client.Complete(llm.WithCallRole(ctx, role), llm.CompleteRequest{Model: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(logs.String(), "turn.pre_model") {
		t.Fatal("auxiliary call consumed pre-model timing")
	}
	for range 2 {
		if _, err := client.Complete(llm.WithCallRole(ctx, llm.CallRolePlanner), llm.CompleteRequest{Model: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(logs.String(), `"msg":"turn.pre_model"`); got != 1 {
		t.Fatalf("pre-model records = %d: %s", got, logs.String())
	}
}

func TestCallTiming_LeafAdmissionAndEmptyResponse(t *testing.T) {
	for _, dispatch := range []bool{false, true} {
		t.Run(fmt.Sprint(dispatch), func(t *testing.T) {
			var logs bytes.Buffer
			client := timingClient(t, leafTimingDriver{timingDriver{complete: func(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
				if dispatch {
					llm.ObserveProviderRequest(ctx)
					return llm.CompleteResponse{}, nil
				}
				return llm.CompleteResponse{}, errors.New("local credential unavailable")
			}}}, &logs)
			_, err := client.Complete(withIdentity(t, t.Context()), llm.CompleteRequest{Model: "m"})
			if err == nil {
				t.Fatal("wanted error")
			}
			records := timingRecords(t, &logs)
			if !dispatch && len(records) != 0 {
				t.Fatal("local admission emitted model timing")
			}
			if dispatch && (len(records) != 1 || records[0]["outcome"] != "error") {
				t.Fatalf("records=%v", records)
			}
		})
	}
}

func TestCallTiming_ConcurrentReuseIsolatedCancellation(t *testing.T) {
	const count = 128
	var logs bytes.Buffer
	client := timingClient(t, timingDriver{complete: func(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
		if req.OnContent != nil {
			req.OnContent("delta", false)
		}
		q, _ := identity.QuadrupleFrom(ctx)
		if q.RunID == "cancel" {
			return llm.CompleteResponse{}, context.Canceled
		}
		return llm.CompleteResponse{Content: "ok"}, nil
	}}, &logs)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run := fmt.Sprintf("run-%d", i)
			if i == 0 {
				run = "cancel"
			}
			ctx, err := identity.WithRun(t.Context(), identity.Identity{TenantID: "tenant", UserID: "user", SessionID: fmt.Sprintf("session-%d", i)}, run)
			if err != nil {
				t.Error(err)
				return
			}
			role := llm.CallRolePlanner
			if i%2 == 0 {
				role = llm.CallRoleTitle
			}
			_, err = client.Complete(llm.WithCallRole(ctx, role), llm.CompleteRequest{Model: "m", Stream: true, OnContent: func(string, bool) {}})
			if i == 0 && !errors.Is(err, context.Canceled) || i > 0 && err != nil {
				t.Errorf("run %d error=%v", i, err)
			}
		}()
	}
	wg.Wait()
	records := timingRecords(t, &logs)
	if len(records) != count {
		t.Fatalf("records=%d", len(records))
	}
	seen := map[string]bool{}
	for _, record := range records {
		sid := record["session_id"].(string)
		if seen[sid] {
			t.Fatalf("duplicate %s", sid)
		}
		seen[sid] = true
		var i int
		if _, err := fmt.Sscanf(sid, "session-%d", &i); err != nil {
			t.Fatal(err)
		}
		role := "planner"
		if i%2 == 0 {
			role = "title"
		}
		if record["role"] != role {
			t.Errorf("session %d role=%v", i, record["role"])
		}
		outcome := "ok"
		if i == 0 {
			outcome = "canceled"
		}
		if record["outcome"] != outcome {
			t.Errorf("session %d outcome=%v", i, record["outcome"])
		}
	}
}

// llm.call.completed carries the provider cache breakdown only when the
// provider returned a prompt-details object; otherwise both fields are absent
// (unknown is never logged as a measured zero).
func TestCallTiming_CacheTokenFields(t *testing.T) {
	cases := []struct {
		name      string
		usage     llm.Usage
		wantCache bool
	}{
		{name: "no prompt details", usage: llm.Usage{ReportPresent: true, PromptTokens: 54000, CompletionTokens: 9}},
		{name: "cache hit", usage: llm.Usage{ReportPresent: true, PromptDetailsPresent: true, PromptTokens: 54000, CacheReadTokens: 53760, CacheWriteTokens: 0}, wantCache: true},
		{name: "cache write", usage: llm.Usage{ReportPresent: true, PromptDetailsPresent: true, PromptTokens: 54000, CacheWriteTokens: 54000}, wantCache: true},
		{name: "estimated usage", usage: llm.Usage{ReportPresent: true, Estimated: true, PromptDetailsPresent: true, CacheReadTokens: 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			client := timingClient(t, timingDriver{complete: func(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
				return llm.CompleteResponse{Content: "ok", Usage: tc.usage}, nil
			}}, &logs)
			text := "hola"
			_, _ = client.Complete(withIdentity(t, t.Context()), llm.CompleteRequest{Model: "m", Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}})
			records := timingRecords(t, &logs)
			if len(records) != 1 {
				t.Fatalf("records=%v", records)
			}
			got := records[0]
			for key, want := range map[string]int{"cached_input_tokens": tc.usage.CacheReadTokens, "cache_write_tokens": tc.usage.CacheWriteTokens} {
				value, present := got[key]
				if present != tc.wantCache || (present && value != float64(want)) {
					t.Errorf("%s=%v present=%v, want present=%v value=%d", key, value, present, tc.wantCache, want)
				}
			}
		})
	}
}
