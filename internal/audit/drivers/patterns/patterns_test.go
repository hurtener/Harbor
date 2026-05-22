package patterns_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
)

func TestDriver_Names_DeterministicOrder(t *testing.T) {
	d := patterns.New()
	got := d.Names()
	want := []string{
		"api_key",
		"password",
		"secret",
		"token",
		"cookie",
		"authorization",
		"bearer",
		"bearer_in_value",
		"multimodal",
	}
	if len(got) != len(want) {
		t.Fatalf("Names() returned %d rules, want %d: %v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDriver_NewWithRules_CustomList(t *testing.T) {
	rule := &countingRule{name: "counter"}
	d := patterns.NewWithRules([]audit.Rule{rule})
	in := map[string]any{"foo": "bar"}
	if _, err := d.Redact(context.Background(), in); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if rule.calls.Load() != 1 {
		t.Errorf("rule was applied %d times, want 1", rule.calls.Load())
	}
	if d.Names()[0] != "counter" {
		t.Errorf("Names()[0] = %q, want counter", d.Names()[0])
	}
}

// TestDriver_ConcurrentReuse_ReuseContract is the D-025 enforcement
// test for the production driver. Runs 256 goroutines each calling
// Redact on independently-shaped payloads against a single shared
// Driver instance under -race. Asserts: no data races, no context
// bleed (each goroutine recovers its own redacted payload), no
// goroutine leaks (baseline-restored).
func TestDriver_ConcurrentReuse_ReuseContract(t *testing.T) {
	d := patterns.New()
	const goroutines = 256
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	var mismatches atomic.Int64

	wg.Add(goroutines)
	for i := range goroutines {

		go func() {
			defer wg.Done()
			id := fmt.Sprintf("req-%d", i)
			in := map[string]any{
				"request_id": id,
				"api_key":    fmt.Sprintf("secret-for-%d", i),
				"path":       fmt.Sprintf("/runs/%d", i),
			}
			out, err := d.Redact(context.Background(), in)
			if err != nil {
				mismatches.Add(1)
				return
			}
			m, ok := out.(map[string]any)
			if !ok {
				mismatches.Add(1)
				return
			}
			if m["request_id"] != id {
				mismatches.Add(1)
			}
			if m["api_key"] != audit.Placeholder {
				mismatches.Add(1)
			}
			// Original payload must NOT have been mutated.
			if in["api_key"] != fmt.Sprintf("secret-for-%d", i) {
				mismatches.Add(1)
			}
		}()
	}
	wg.Wait()

	if n := mismatches.Load(); n != 0 {
		t.Fatalf("%d/%d goroutines observed cross-talk or mutation", n, goroutines)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}

// TestDriver_FailingRule_StopsPipeline asserts that when a rule
// returns an error, the driver returns (nil, wrapped error) and
// downstream rules are NOT invoked.
func TestDriver_FailingRule_StopsPipeline(t *testing.T) {
	failing := &failingRule{name: "boom"}
	tail := &countingRule{name: "should-not-run"}
	d := patterns.NewWithRules([]audit.Rule{failing, tail})
	out, err := d.Redact(context.Background(), map[string]any{"k": "v"})
	if err == nil {
		t.Fatal("Redact returned nil error for failing rule")
	}
	if out != nil {
		t.Errorf("Redact returned non-nil payload on error: %v", out)
	}
	if !errors.Is(err, audit.ErrRedactionFailed) {
		t.Errorf("err=%v, want errors.Is ErrRedactionFailed", err)
	}
	if tail.calls.Load() != 0 {
		t.Errorf("tail rule ran after failing rule: %d calls", tail.calls.Load())
	}
}

type countingRule struct {
	name  string
	calls atomic.Int64
}

func (r *countingRule) Name() string { return r.name }
func (r *countingRule) Apply(_ context.Context, payload any) (any, error) {
	r.calls.Add(1)
	return payload, nil
}

type failingRule struct{ name string }

func (r *failingRule) Name() string { return r.name }
func (r *failingRule) Apply(_ context.Context, _ any) (any, error) {
	return "leaked-partial", errors.New("rule went bang")
}
