package finish_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/finish"
)

func TestStub_DefaultReturnsFinishGoal(t *testing.T) {
	p := finish.New()
	dec, err := p.Next(context.Background(), planner.RunContext{})
	if err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("Next returned %T, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Finish.Reason = %q want %q", fin.Reason, planner.FinishGoal)
	}
}

func TestStub_PayloadAndMetadataRoundTrip(t *testing.T) {
	p := finish.New(
		finish.WithPayload("hello world"),
		finish.WithMetadata(map[string]any{"k": "v"}),
	)
	rc := planner.RunContext{
		Quadruple: identity.Quadruple{
			Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
			RunID:    "r-1",
		},
	}
	dec, err := p.Next(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	fin := dec.(planner.Finish)
	if fin.Payload != "hello world" {
		t.Fatalf("Payload = %v want \"hello world\"", fin.Payload)
	}
	if fin.Metadata["k"] != "v" {
		t.Fatalf("Metadata[k] = %v want \"v\"", fin.Metadata["k"])
	}
	if fin.Metadata["run_id"] != "r-1" {
		t.Fatalf("Metadata[run_id] = %v want \"r-1\"", fin.Metadata["run_id"])
	}
}

func TestStub_CtxCancelPropagates(t *testing.T) {
	p := finish.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Next(ctx, planner.RunContext{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Next on cancelled ctx returned %v want context.Canceled", err)
	}
}

// The stub planner does not mutate the configured Metadata template
// between calls — verifies isolation when callers share a configured
// instance. Concrete check: re-call with a different RunID; the
// configured map's "k" key must still be intact.
func TestStub_TemplateNotMutatedAcrossCalls(t *testing.T) {
	tmpl := map[string]any{"k": "v"}
	p := finish.New(finish.WithMetadata(tmpl))

	rc1 := planner.RunContext{Quadruple: identity.Quadruple{
		Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
		RunID:    "run-A",
	}}
	rc2 := planner.RunContext{Quadruple: identity.Quadruple{
		Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
		RunID:    "run-B",
	}}

	out1, _ := p.Next(context.Background(), rc1)
	out2, _ := p.Next(context.Background(), rc2)

	m1 := out1.(planner.Finish).Metadata
	m2 := out2.(planner.Finish).Metadata

	if m1["run_id"] != "run-A" || m2["run_id"] != "run-B" {
		t.Fatalf("per-call RunID lost: m1=%v m2=%v", m1, m2)
	}
	if tmpl["k"] != "v" || tmpl["run_id"] != nil {
		t.Fatalf("template was mutated: %v", tmpl)
	}
}

func TestStub_WakeMode_IsPush(t *testing.T) {
	p := finish.New()
	if got := p.WakeMode(); got != planner.WakePush {
		t.Fatalf("WakeMode = %q want %q", got, planner.WakePush)
	}
	if got := planner.ResolveWakeMode(p); got != planner.WakePush {
		t.Fatalf("ResolveWakeMode = %q want %q", got, planner.WakePush)
	}
}
