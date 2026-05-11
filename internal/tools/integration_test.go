package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// TestIntegration_Catalog_InProc_EndToEnd wires the catalog + the
// in-process driver + identity ctx propagation end-to-end. Real
// drivers everywhere; no mocks at the seam (AGENTS.md §17.3).
func TestIntegration_Catalog_InProc_EndToEnd(t *testing.T) {
	type GreetArgs struct {
		Name string `json:"name"`
	}
	type GreetOut struct {
		Message string `json:"message"`
		Tenant  string `json:"tenant"`
	}

	cat := tools.NewCatalog()
	err := inproc.RegisterFunc[GreetArgs, GreetOut](cat, "greet",
		func(ctx context.Context, in GreetArgs) (GreetOut, error) {
			id, ok := identity.From(ctx)
			if !ok {
				return GreetOut{}, fmt.Errorf("no identity")
			}
			return GreetOut{
				Message: "Hello, " + in.Name,
				Tenant:  id.TenantID,
			}, nil
		},
		tools.WithDescription("Greets a user, including the tenant claim."),
		tools.WithSideEffect(tools.SideEffectPure),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	filter := tools.CatalogFilter{
		TenantID: "acme", UserID: "alice", SessionID: "s-1",
	}
	list := cat.List(filter)
	if len(list) != 1 {
		t.Fatalf("expected 1 tool visible, got %d", len(list))
	}
	if list[0].Name != "greet" {
		t.Errorf("expected greet, got %q", list[0].Name)
	}

	d, _ := cat.Resolve("greet")
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "acme", UserID: "alice", SessionID: "s-1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	args, _ := json.Marshal(GreetArgs{Name: "World"})
	res, err := d.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	out, ok := res.Value.(GreetOut)
	if !ok {
		t.Fatalf("expected GreetOut, got %T", res.Value)
	}
	if out.Message != "Hello, World" {
		t.Errorf("message: got %q", out.Message)
	}
	if out.Tenant != "acme" {
		t.Errorf("tenant: got %q (expected acme — identity propagation)", out.Tenant)
	}
}

// TestIntegration_PolicyRetry_OnTransientFailure exercises the
// policy shell + the in-process driver + a flaky tool + the catalog.
func TestIntegration_PolicyRetry_OnTransientFailure(t *testing.T) {
	type Args struct {
		N int `json:"n"`
	}
	type Out struct {
		Attempt int64 `json:"attempt"`
	}

	var counter atomic.Int64
	cat := tools.NewCatalog()
	err := inproc.RegisterFunc[Args, Out](cat, "flaky_intg",
		func(ctx context.Context, in Args) (Out, error) {
			n := counter.Add(1)
			if n <= 2 {
				return Out{}, fmt.Errorf("transient: attempt %d", n)
			}
			return Out{Attempt: n}, nil
		},
		tools.WithPolicy(tools.ToolPolicy{
			MaxRetries:  5,
			BackoffBase: 1 * time.Millisecond,
			BackoffMult: 2,
			BackoffMax:  10 * time.Millisecond,
			TimeoutMS:   1000,
			RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
			Validate:    tools.ValidateBoth,
		}),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	d, _ := cat.Resolve("flaky_intg")
	ctx, _ := identity.With(context.Background(), identity.Identity{
		TenantID: "t", UserID: "u", SessionID: "s",
	})
	args, _ := json.Marshal(Args{N: 1})
	res, err := d.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	out := res.Value.(Out)
	if out.Attempt != 3 {
		t.Errorf("expected 3 attempts, got %d", out.Attempt)
	}
}

// TestIntegration_InvalidArgs_DoesNotRetry exercises invalid args
// fail with ErrToolInvalidArgs without entering the retry loop.
func TestIntegration_InvalidArgs_DoesNotRetry(t *testing.T) {
	type Args struct {
		Required string `json:"required"`
	}
	type Out struct{}

	var counter atomic.Int64
	cat := tools.NewCatalog()
	err := inproc.RegisterFunc[Args, Out](cat, "strict",
		func(ctx context.Context, in Args) (Out, error) {
			counter.Add(1)
			return Out{}, nil
		},
		tools.WithPolicy(tools.ToolPolicy{
			MaxRetries:  3,
			BackoffBase: 1 * time.Millisecond,
			BackoffMax:  10 * time.Millisecond,
			BackoffMult: 2,
			TimeoutMS:   1000,
			RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
			Validate:    tools.ValidateBoth,
		}),
	)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	d, _ := cat.Resolve("strict")
	ctx, _ := identity.With(context.Background(), identity.Identity{
		TenantID: "t", UserID: "u", SessionID: "s",
	})
	_, err = d.Invoke(ctx, []byte(`{}`))
	if err == nil {
		t.Fatalf("expected ErrToolInvalidArgs, got nil")
	}
	if !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("expected ErrToolInvalidArgs, got: %v", err)
	}
	if got := counter.Load(); got != 0 {
		t.Errorf("expected fn not invoked (invalid args), got %d invocations", got)
	}
}
