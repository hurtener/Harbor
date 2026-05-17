package planner

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// D-103 — planner driver registry tests. Mirror D-095's
// `internal/tools/auth/registry_test.go` structurally; the registries
// share the same shape (§4.4 seam) so the test plan is shared too.

func TestRegister_EmptyNameRejected(t *testing.T) {
	t.Parallel()
	err := Register("", okPlannerFactory)
	if !errors.Is(err, ErrDriverEmptyName) {
		t.Fatalf("Register(empty) err = %v, want ErrDriverEmptyName", err)
	}
}

func TestRegister_NilFactoryRejected(t *testing.T) {
	t.Parallel()
	err := Register("some-planner-name", nil)
	if !errors.Is(err, ErrDriverNilFactory) {
		t.Fatalf("Register(nil) err = %v, want ErrDriverNilFactory", err)
	}
}

func TestRegister_DuplicateRejected(t *testing.T) {
	t.Parallel()
	name := "test-planner-dup-driver"
	defer unregisterForTest(name)

	if err := Register(name, okPlannerFactory); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := Register(name, okPlannerFactory)
	if !errors.Is(err, ErrDriverDuplicate) {
		t.Fatalf("second Register err = %v, want ErrDriverDuplicate", err)
	}
}

func TestRegistry_ResolveUnknownDriverFailsLoud(t *testing.T) {
	t.Parallel()
	_, err := Resolve(context.Background(),
		PlannerConfig{Driver: "no-such-planner-driver-xyz"},
		FactoryDeps{})
	if !errors.Is(err, ErrDriverUnknown) {
		t.Fatalf("Resolve err = %v, want ErrDriverUnknown", err)
	}
	// The error message must list registered drivers so the operator
	// sees the typo (D-095 precedent, §13 fail-loud rationale).
	if !strings.Contains(err.Error(), "registered:") {
		t.Fatalf("Resolve err = %q, want it to list registered drivers", err.Error())
	}
}

func TestRegistry_ResolveEmptyDriverFailsLoud(t *testing.T) {
	t.Parallel()
	_, err := Resolve(context.Background(),
		PlannerConfig{Driver: ""},
		FactoryDeps{})
	if !errors.Is(err, ErrDriverUnknown) {
		t.Fatalf("Resolve(empty) err = %v, want ErrDriverUnknown", err)
	}
}

func TestRegistry_ResolveHonoursCtxCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Resolve(ctx,
		PlannerConfig{Driver: "irrelevant"},
		FactoryDeps{})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve(cancelled) err = %v, want context.Canceled", err)
	}
}

func TestMustRegister_PanicsOnError(t *testing.T) {
	t.Parallel()
	name := "test-planner-must-panic"
	defer unregisterForTest(name)

	MustRegister(name, okPlannerFactory)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustRegister did not panic on duplicate")
		}
	}()
	MustRegister(name, okPlannerFactory)
}

func TestRegisteredDrivers_Sorted(t *testing.T) {
	t.Parallel()
	names := []string{"zzz-planner-test-driver-1", "aaa-planner-test-driver-2"}
	defer func() {
		for _, n := range names {
			unregisterForTest(n)
		}
	}()
	for _, n := range names {
		MustRegister(n, okPlannerFactory)
	}
	listed := RegisteredDrivers()
	var foundIdxA, foundIdxZ = -1, -1
	for i, n := range listed {
		if n == "aaa-planner-test-driver-2" {
			foundIdxA = i
		}
		if n == "zzz-planner-test-driver-1" {
			foundIdxZ = i
		}
	}
	if foundIdxA == -1 || foundIdxZ == -1 {
		t.Fatalf("test drivers not found in %v", listed)
	}
	if foundIdxA >= foundIdxZ {
		t.Fatalf("RegisteredDrivers not sorted: aaa idx=%d, zzz idx=%d", foundIdxA, foundIdxZ)
	}
}

// TestRegistry_ResolveDispatchesToFactory pins the happy path: a
// registered factory returns the planner instance unmodified.
func TestRegistry_ResolveDispatchesToFactory(t *testing.T) {
	t.Parallel()
	name := "test-planner-dispatch"
	defer unregisterForTest(name)

	sentinel := registryTestPlanner{tag: "sentinel"}
	MustRegister(name, func(_ PlannerConfig, _ FactoryDeps) (Planner, error) {
		return sentinel, nil
	})

	got, err := Resolve(context.Background(),
		PlannerConfig{Driver: name},
		FactoryDeps{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	rp, ok := got.(registryTestPlanner)
	if !ok {
		t.Fatalf("Resolve returned %T, want registryTestPlanner", got)
	}
	if rp.tag != "sentinel" {
		t.Fatalf("Resolve returned tag=%q, want %q", rp.tag, "sentinel")
	}
}

// okPlannerFactory is a no-op Factory for registry-bookkeeping tests.
// The returned planner does NOT satisfy real planner semantics — these
// tests only verify the registry's lookup behaviour.
func okPlannerFactory(_ PlannerConfig, _ FactoryDeps) (Planner, error) {
	return registryTestPlanner{}, nil
}

// registryTestPlanner is a no-op Planner that satisfies the interface
// so the registry's bookkeeping tests compile. Test-only per
// CLAUDE.md §13 — never reachable from production code.
type registryTestPlanner struct{ tag string }

func (registryTestPlanner) Next(_ context.Context, _ RunContext) (Decision, error) {
	return Finish{Reason: FinishGoal}, nil
}
