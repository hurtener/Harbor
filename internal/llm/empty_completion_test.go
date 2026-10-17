package llm_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"

	"github.com/hurtener/Harbor/internal/llm"
)

type emptyCompletionDriver struct {
	response llm.CompleteResponse
	calls    atomic.Int64
}

func (d *emptyCompletionDriver) Complete(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
	d.calls.Add(1)
	return d.response, nil
}
func (*emptyCompletionDriver) Close(context.Context) error { return nil }

func TestSafety_EmptyCompletionIsTypedAndPreservesAccounting(t *testing.T) {
	for _, tc := range []struct{ name, content, reasoning, reason, wantReason string }{
		{name: "empty", reason: "length", wantReason: "length"},
		{name: "whitespace", content: " \t\n\r", reason: "max_output_tokens", wantReason: "max_output_tokens"},
		{name: "reasoning-only", reasoning: "bounded fixture reasoning", reason: "stop", wantReason: "stop"},
		{name: "missing-reason", wantReason: "unreported"},
		{name: "untrusted-reason", reason: strings.Repeat("private-provider-text", 100), wantReason: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, cleanup := makeDeps(t)
			defer cleanup()
			original := llm.CompleteResponse{Content: tc.content, Reasoning: tc.reasoning, FinishReason: tc.reason, Usage: llm.Usage{ReportPresent: true, TotalTokens: 17}, Cost: llm.Cost{ReportPresent: true, TotalCost: 0.25}}
			driver := &emptyCompletionDriver{response: original}
			name := uniqueDriverName("empty-completion")
			llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
			cfg := makeSnapshot("m", 8192)
			cfg.Driver = name
			profile := cfg.ModelProfiles["m"]
			profile.MaxRetries = 3
			profile.Corrections.UsageBackfillEnabled = true
			cfg.ModelProfiles["m"] = profile
			client, err := llm.Open(t.Context(), cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(t.Context()) }()
			var validations atomic.Int64
			resp, err := client.Complete(withIdentity(t, t.Context()), llm.CompleteRequest{Model: "m", Validator: func(llm.CompleteResponse) error { validations.Add(1); return errors.New("repair must not run") }})
			if !errors.Is(err, llm.ErrEmptyCompletion) {
				t.Fatalf("err=%v, want typed empty completion", err)
			}
			var detail *llm.EmptyCompletionError
			if !errors.As(err, &detail) || detail.FinishReason() != tc.wantReason {
				t.Fatalf("bounded detail=%#v", detail)
			}
			if len(err.Error()) > 160 || strings.Contains(err.Error(), "private-provider-text") {
				t.Fatalf("unsafe error=%s", err)
			}
			if !reflect.DeepEqual(resp, original) {
				t.Fatalf("response/accounting changed: got%+v want%+v", resp, original)
			}
			if driver.calls.Load() != 1 || validations.Load() != 0 {
				t.Fatalf("provider calls=%d, validators=%d", driver.calls.Load(), validations.Load())
			}
		})
	}
}

func TestSafety_EmptyCompletionUnknownAccountingAndConcurrentReuse(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	driver := &emptyCompletionDriver{response: llm.CompleteResponse{FinishReason: "length"}}
	name := uniqueDriverName("empty-concurrent")
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	cfg := makeSnapshot("m", 8192)
	cfg.Driver = name
	profile := cfg.ModelProfiles["m"]
	profile.Corrections.UsageBackfillEnabled = true
	cfg.ModelProfiles["m"] = profile
	client, err := llm.Open(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	var wg sync.WaitGroup
	for range 128 {
		wg.Go(func() {
			resp, err := client.Complete(withIdentity(t, t.Context()), llm.CompleteRequest{Model: "m"})
			if !errors.Is(err, llm.ErrEmptyCompletion) {
				t.Errorf("error=%v", err)
			}
			if !reflect.DeepEqual(resp.Usage, llm.Usage{}) || !reflect.DeepEqual(resp.Cost, llm.Cost{}) {
				t.Errorf("unknown accounting was synthesized: %+v %+v", resp.Usage, resp.Cost)
			}
		})
	}
	wg.Wait()
	if driver.calls.Load() != 128 {
		t.Fatalf("calls=%d, want128", driver.calls.Load())
	}
}

func TestSafety_EmptyCompletionCostObservationPreservesAccounting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage llm.Usage
		cost  llm.Cost
	}{
		{name: "reported", usage: llm.Usage{ReportPresent: true, TotalTokens: 17}, cost: llm.Cost{ReportPresent: true, TotalCost: 0.25}},
		{name: "unknown"},
		// SDK-normalized presence on a zero object is preserved, not promoted
		// into a claim about what the upstream provider actually measured.
		{name: "normalized-zero", usage: llm.Usage{ReportPresent: true}, cost: llm.Cost{ReportPresent: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, cleanup := makeDeps(t)
			defer cleanup()
			sub := subscribeBus(t, deps.Bus, llm.EventTypeCostRecorded)
			original := llm.CompleteResponse{FinishReason: "length", Usage: tc.usage, Cost: tc.cost}
			driver := &emptyCompletionDriver{response: original}
			name := uniqueDriverName("empty-cost-observation")
			llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
			cfg := makeSnapshot("m", 8192)
			cfg.Driver = name
			client, err := llm.Open(t.Context(), cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(t.Context()) }()
			ctx := costEmitCtx(t)
			response, err := client.Complete(ctx, llm.CompleteRequest{Model: "m"})
			if !errors.Is(err, llm.ErrEmptyCompletion) || !reflect.DeepEqual(response, original) {
				t.Fatalf("response/error=%+v %v", response, err)
			}
			if err := events.Flush(ctx, deps.Bus); err != nil {
				t.Fatal(err)
			}
			records := countCostEvents(sub, 100*time.Millisecond)
			if len(records) != 1 {
				t.Fatalf("cost events=%d, want1 for the successful driver attempt", len(records))
			}
			if !reflect.DeepEqual(records[0].Usage, tc.usage) || !reflect.DeepEqual(records[0].Cost, tc.cost) {
				t.Fatalf("observation changed accounting: %+v", records[0])
			}
			if records[0].Identity.RunID != "run-cost-emit" || records[0].Model != "m" || records[0].ContextWindowTokens != 8192 {
				t.Fatalf("observation attribution=%+v", records[0])
			}
			if driver.calls.Load() != 1 {
				t.Fatalf("provider attempts=%d, want1", driver.calls.Load())
			}
		})
	}
}
