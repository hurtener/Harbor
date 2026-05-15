package harbortest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/harbortest"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// addArgs / addOut — a trivial deterministic tool used by the
// SimulateFailure tests. Side-effect-free; production code is fine
// to invoke many times.
type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

type addOut struct {
	Sum int `json:"sum"`
}

// registerAdd registers the "add" tool against the given catalog
// with NO retries on the policy so SimulateFailure failures surface
// to the caller directly (we want to count failures, not the policy
// shell's retry behaviour).
func registerAdd(t *testing.T, cat tools.ToolCatalog) {
	t.Helper()
	if err := inproc.RegisterFunc[addArgs, addOut](
		cat,
		"add",
		func(_ context.Context, in addArgs) (addOut, error) {
			return addOut{Sum: in.A + in.B}, nil
		},
		// Empty policy = zero-value (no retries). The policy shell
		// applies defaults only for retryable classes; for the
		// SimulateFailure tests we want to count raw failures.
		tools.WithPolicy(tools.ToolPolicy{}),
	); err != nil {
		t.Fatalf("RegisterFunc add: %v", err)
	}
}

// TestSimulateFailure_FailsThenResumes — inject 3 transient failures
// against "add", invoke 5 times, assert the first 3 fail with the
// kit's simulated-failure sentinel and the next 2 succeed.
func TestSimulateFailure_FailsThenResumes(t *testing.T) {
	inner := tools.NewCatalog()
	registerAdd(t, inner)
	inj := harbortest.NewFaultInjector(inner)
	harbortest.SimulateFailure(inj, "add", tools.ErrClassTransient, 3)

	cat := inj.Catalog()
	d, ok := cat.Resolve("add")
	if !ok {
		t.Fatal("Resolve(add) = !ok")
	}
	args, _ := json.Marshal(addArgs{A: 1, B: 2})

	results := make([]error, 5)
	for i := 0; i < 5; i++ {
		_, err := d.Invoke(context.Background(), args)
		results[i] = err
	}
	for i := 0; i < 3; i++ {
		if !errors.Is(results[i], harbortest.ErrSimulatedFailure) {
			t.Errorf("invocation %d: err = %v, want errors.Is ErrSimulatedFailure", i, results[i])
		}
	}
	for i := 3; i < 5; i++ {
		if results[i] != nil {
			t.Errorf("invocation %d: err = %v, want success after failure budget exhausted", i, results[i])
		}
	}
}

// TestSimulateFailure_PermanentClass_WrapsInvalidArgs — class-typed
// errors classify correctly for the policy shell.
func TestSimulateFailure_PermanentClass_WrapsInvalidArgs(t *testing.T) {
	inner := tools.NewCatalog()
	registerAdd(t, inner)
	inj := harbortest.NewFaultInjector(inner)
	harbortest.SimulateFailure(inj, "add", tools.ErrClassPermanent, 1)

	cat := inj.Catalog()
	d, _ := cat.Resolve("add")
	args, _ := json.Marshal(addArgs{A: 1, B: 2})
	_, err := d.Invoke(context.Background(), args)
	if !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Errorf("permanent simulated failure: err = %v, want errors.Is tools.ErrToolInvalidArgs", err)
	}
}

// TestSimulateFailure_TimeoutClass_WrapsDeadlineExceeded — same
// shape for the timeout class.
func TestSimulateFailure_TimeoutClass_WrapsDeadlineExceeded(t *testing.T) {
	inner := tools.NewCatalog()
	registerAdd(t, inner)
	inj := harbortest.NewFaultInjector(inner)
	harbortest.SimulateFailure(inj, "add", tools.ErrClassTimeout, 1)

	cat := inj.Catalog()
	d, _ := cat.Resolve("add")
	args, _ := json.Marshal(addArgs{A: 1, B: 2})
	_, err := d.Invoke(context.Background(), args)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout simulated failure: err = %v, want errors.Is context.DeadlineExceeded", err)
	}
}

// TestSimulateFailure_PerToolIsolated — failures on toolA do not
// affect toolB.
func TestSimulateFailure_PerToolIsolated(t *testing.T) {
	inner := tools.NewCatalog()
	registerAdd(t, inner)
	// Register a second tool, "sub", that subtracts.
	if err := inproc.RegisterFunc[addArgs, addOut](
		inner,
		"sub",
		func(_ context.Context, in addArgs) (addOut, error) {
			return addOut{Sum: in.A - in.B}, nil
		},
		tools.WithPolicy(tools.ToolPolicy{}),
	); err != nil {
		t.Fatalf("RegisterFunc sub: %v", err)
	}

	inj := harbortest.NewFaultInjector(inner)
	harbortest.SimulateFailure(inj, "add", tools.ErrClassTransient, 1)

	cat := inj.Catalog()
	args, _ := json.Marshal(addArgs{A: 5, B: 3})

	// add fails once.
	addD, _ := cat.Resolve("add")
	if _, err := addD.Invoke(context.Background(), args); !errors.Is(err, harbortest.ErrSimulatedFailure) {
		t.Errorf("add invocation 1: err = %v, want ErrSimulatedFailure", err)
	}
	// sub is unaffected — succeeds.
	subD, _ := cat.Resolve("sub")
	if _, err := subD.Invoke(context.Background(), args); err != nil {
		t.Errorf("sub invocation: err = %v, want success (per-tool isolation)", err)
	}
}

// TestSimulateFailure_StacksFifo — two SimulateFailure calls stack
// FIFO: (transient, 2) then (permanent, 1) yields transient,
// transient, permanent.
func TestSimulateFailure_StacksFifo(t *testing.T) {
	inner := tools.NewCatalog()
	registerAdd(t, inner)
	inj := harbortest.NewFaultInjector(inner)
	harbortest.SimulateFailure(inj, "add", tools.ErrClassTransient, 2)
	harbortest.SimulateFailure(inj, "add", tools.ErrClassPermanent, 1)

	cat := inj.Catalog()
	d, _ := cat.Resolve("add")
	args, _ := json.Marshal(addArgs{A: 1, B: 2})

	got := make([]error, 3)
	for i := 0; i < 3; i++ {
		_, got[i] = d.Invoke(context.Background(), args)
	}
	// First two: transient → ErrSimulatedFailure wrap.
	for i := 0; i < 2; i++ {
		if !errors.Is(got[i], harbortest.ErrSimulatedFailure) {
			t.Errorf("invocation %d: err = %v, want ErrSimulatedFailure (transient)", i, got[i])
		}
	}
	// Third: permanent → ErrToolInvalidArgs wrap.
	if !errors.Is(got[2], tools.ErrToolInvalidArgs) {
		t.Errorf("invocation 2: err = %v, want ErrToolInvalidArgs (permanent)", got[2])
	}
}

// TestSimulateFailure_NoInjection_PassesThrough — when no failure
// is scheduled, the wrapper is transparent.
func TestSimulateFailure_NoInjection_PassesThrough(t *testing.T) {
	inner := tools.NewCatalog()
	registerAdd(t, inner)
	inj := harbortest.NewFaultInjector(inner)

	cat := inj.Catalog()
	d, _ := cat.Resolve("add")
	args, _ := json.Marshal(addArgs{A: 4, B: 7})
	got, err := d.Invoke(context.Background(), args)
	if err != nil {
		t.Fatalf("no-injection call err = %v", err)
	}
	res, ok := got.Value.(addOut)
	if !ok {
		t.Fatalf("result.Value type = %T, want addOut", got.Value)
	}
	if res.Sum != 11 {
		t.Errorf("result = %d, want 11", res.Sum)
	}
}

// TestSimulateFailure_GuardsAgainstZeroAndNil — defensive: n<=0,
// nil injector, empty toolName are silent no-ops.
func TestSimulateFailure_GuardsAgainstZeroAndNil(t *testing.T) {
	inner := tools.NewCatalog()
	registerAdd(t, inner)
	inj := harbortest.NewFaultInjector(inner)

	// All three guards should be silent no-ops.
	harbortest.SimulateFailure(nil, "add", tools.ErrClassTransient, 1)
	harbortest.SimulateFailure(inj, "", tools.ErrClassTransient, 1)
	harbortest.SimulateFailure(inj, "add", tools.ErrClassTransient, 0)
	harbortest.SimulateFailure(inj, "add", tools.ErrClassTransient, -3)

	cat := inj.Catalog()
	d, _ := cat.Resolve("add")
	args, _ := json.Marshal(addArgs{A: 1, B: 1})
	if _, err := d.Invoke(context.Background(), args); err != nil {
		t.Errorf("after defensive no-ops, invocation err = %v, want success", err)
	}
}

// TestNewFaultInjector_NilCatalog_Panics — the kit panics on a nil
// catalog at the test-only boundary; the panic message is
// grep-friendly.
func TestNewFaultInjector_NilCatalog_Panics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewFaultInjector(nil) did not panic")
		}
	}()
	_ = harbortest.NewFaultInjector(nil)
}

// TestFaultInjector_UnknownTool_NotFound — Resolve on an unknown
// tool returns ok=false unchanged.
func TestFaultInjector_UnknownTool_NotFound(t *testing.T) {
	inner := tools.NewCatalog()
	inj := harbortest.NewFaultInjector(inner)
	cat := inj.Catalog()
	if _, ok := cat.Resolve("nonexistent"); ok {
		t.Error("Resolve(nonexistent) = ok, want !ok")
	}
}

// TestFaultInjector_Register_Forwards — Register on the wrapped
// catalog reaches the inner catalog so subsequent Resolves find it.
func TestFaultInjector_Register_Forwards(t *testing.T) {
	inner := tools.NewCatalog()
	inj := harbortest.NewFaultInjector(inner)

	if err := inproc.RegisterFunc[addArgs, addOut](
		inj.Catalog(),
		"add",
		func(_ context.Context, in addArgs) (addOut, error) {
			return addOut{Sum: in.A + in.B}, nil
		},
		tools.WithPolicy(tools.ToolPolicy{}),
	); err != nil {
		t.Fatalf("RegisterFunc via wrapper: %v", err)
	}
	// Confirm Resolve works on both wrapper + inner.
	if _, ok := inner.Resolve("add"); !ok {
		t.Error("inner.Resolve(add) = !ok after Register via wrapper")
	}
	if _, ok := inj.Catalog().Resolve("add"); !ok {
		t.Error("wrapper.Resolve(add) = !ok after Register via wrapper")
	}
}

// TestFaultInjector_List_Forwards — List on the wrapped catalog
// returns the same tools as the inner catalog.
func TestFaultInjector_List_Forwards(t *testing.T) {
	inner := tools.NewCatalog()
	registerAdd(t, inner)
	inj := harbortest.NewFaultInjector(inner)
	got := inj.Catalog().List(toolsCatalogFilter())
	if len(got) != 1 {
		t.Errorf("List = %d tools, want 1", len(got))
	}
}

// toolsCatalogFilter returns an empty filter; the inproc tool's
// AuthScopes are empty so visibility is unconditional.
func toolsCatalogFilter() tools.CatalogFilter {
	return tools.CatalogFilter{}
}
