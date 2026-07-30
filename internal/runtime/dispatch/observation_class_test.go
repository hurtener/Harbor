// internal/runtime/dispatch/observation_class_test.go — the
// machine-readable classification a failed artifact-reference resolution
// carries into the step's observation.
//
// The wrap chain IS the thing under test, so every seam is real
// (CLAUDE.md §17.3): the shipped `examples/tools/artifactstats` consumer
// declaring a real artifact-reference parameter, the real in-process
// driver that binds it, the real in-memory artifact store, and the
// production executor. A fake resolver returning the sentinel directly
// would prove nothing about the five hops between the resolver and the
// run loop.

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/examples/tools/artifactstats"
	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

// registerArtifactConsumer registers the shipped example tool that
// declares an artifact-reference parameter.
func registerArtifactConsumer(t *testing.T, cat tools.ToolCatalog) {
	t.Helper()
	if err := artifactstats.Register(cat); err != nil {
		t.Fatalf("register %s: %v", artifactstats.ToolName, err)
	}
}

func artifactCall(refID string) planner.CallTool {
	return planner.CallTool{
		CallID: "call_class_1",
		Tool:   artifactstats.ToolName,
		Args:   json.RawMessage(fmt.Sprintf(`{"artifact":%q}`, refID)),
	}
}

// TestExecutor_CallTool_UnresolvableRef_CarriesTheClass is the sentinel's
// FIRST consumer. `ErrArtifactRefNotFound` has been declared and produced
// since the reference-parameter arm shipped, with no errors.Is on it
// anywhere in the tree — a classification nothing classified on.
func TestExecutor_CallTool_UnresolvableRef_CarriesTheClass(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	registerArtifactConsumer(t, cat)
	exec := NewToolExecutor(cat, newTestArtifactStore(t), nil)

	q := dispatchTestQuad("r-class-notfound")
	_, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), planner.RunContext{Quadruple: q},
		artifactCall("id_the_model_invented"))
	if err == nil {
		t.Fatal("an unresolvable reference dispatched successfully")
	}
	if got := planner.ObservationClassOf(err); got != planner.ObservationClassArtifactRefNotFound {
		t.Fatalf("ObservationClassOf = %q, want %q (err = %v)",
			got, planner.ObservationClassArtifactRefNotFound, err)
	}
	// The chain still reaches the sentinel and the message is unchanged.
	if !errors.Is(err, ErrArtifactRefNotFound) {
		t.Errorf("classifying broke the wrap chain: %v", err)
	}
	if !strings.Contains(err.Error(), "id_the_model_invented") {
		t.Errorf("err = %v, want it to name the reference id", err)
	}
}

// TestExecutor_CallTool_NoStoreWired_CarriesTheResolverClass — the class
// exists so a planner does NOT spend its step budget retrying an
// operator misconfiguration it cannot repair.
func TestExecutor_CallTool_NoStoreWired_CarriesTheResolverClass(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	registerArtifactConsumer(t, cat)
	exec := NewToolExecutor(cat, nil, nil)

	q := dispatchTestQuad("r-class-nostore")
	_, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), planner.RunContext{Quadruple: q},
		artifactCall("anything"))
	if err == nil {
		t.Fatal("a reference resolved with no artifact store wired")
	}
	if got := planner.ObservationClassOf(err); got != planner.ObservationClassArtifactResolverUnavailable {
		t.Fatalf("ObservationClassOf = %q, want %q (err = %v)",
			got, planner.ObservationClassArtifactResolverUnavailable, err)
	}
	if !errors.Is(err, ErrArtifactStoreUnavailable) {
		t.Errorf("classifying broke the wrap chain: %v", err)
	}
}

// TestObservationClassOf_ClassifiesTheThreeSentinelsAndNothingElse pins
// that the class set is closed by DECISION. The carrier's other
// sentinels are argument-shape or programming errors for which neither a
// model nor an operator has a repair, so they stay unclassified.
func TestObservationClassOf_ClassifiesTheThreeSentinelsAndNothingElse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want planner.ObservationClass
	}{
		{"nil", nil, ""},
		{"ErrArtifactRefNotFound", fmt.Errorf("x: %w", ErrArtifactRefNotFound), planner.ObservationClassArtifactRefNotFound},
		{"ErrArtifactStoreUnavailable", fmt.Errorf("x: %w", ErrArtifactStoreUnavailable), planner.ObservationClassArtifactResolverUnavailable},
		{"artifactref.ErrNoResolver", fmt.Errorf("x: %w", artifactref.ErrNoResolver), planner.ObservationClassArtifactResolverUnavailable},
		{"artifactref.ErrEmptyID stays unclassified", fmt.Errorf("x: %w", artifactref.ErrEmptyID), ""},
		{"artifactref.ErrUnresolved stays unclassified", fmt.Errorf("x: %w", artifactref.ErrUnresolved), ""},
		{"artifactref.ErrNotAddressable stays unclassified", fmt.Errorf("x: %w", artifactref.ErrNotAddressable), ""},
		{"a tool's own error stays unclassified", errors.New("upstream 503"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := observationClassOf(tc.err); got != tc.want {
				t.Errorf("observationClassOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassify_LeavesAnUnclassifiedErrorIdentical is the no-change pin.
// A widened payload on every tool error would be an unannounced prompt
// change on the runtime's hottest path, so classify must return the
// SAME error value when there is nothing to classify.
func TestClassify_LeavesAnUnclassifiedErrorIdentical(t *testing.T) {
	t.Parallel()
	in := fmt.Errorf("tool %q invoke: %w", "flaky", errors.New("upstream 503"))
	out := classify(in)
	if out != in { //nolint:errorlint // identity comparison is the assertion: the same error VALUE must come back
		t.Fatalf("classify returned a different error value for an unclassified failure: %#v", out)
	}
	if planner.ObservationClassOf(out) != "" {
		t.Errorf("an unclassified error acquired a class: %q", planner.ObservationClassOf(out))
	}
}

// TestExecutor_CallTool_ToolsOwnError_ObservationIsUnchanged is the same
// pin one layer out: a tool whose own body fails produces exactly the
// error a pre-class build produced.
func TestExecutor_CallTool_ToolsOwnError_ObservationIsUnchanged(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "explodes"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, errors.New("upstream 503")
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), nil)

	q := dispatchTestQuad("r-class-toolerr")
	_, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), planner.RunContext{Quadruple: q},
		planner.CallTool{Tool: "explodes", Args: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("a failing tool dispatched successfully")
	}
	if got := planner.ObservationClassOf(err); got != "" {
		t.Fatalf("a tool's own error acquired the class %q", got)
	}
	if want := `tool "explodes" invoke: upstream 503`; err.Error() != want {
		t.Errorf("err = %q, want the pre-phase message %q", err.Error(), want)
	}
}

// TestExecutor_CallParallel_BranchCarriesTheClass — the single-call path
// and the parallel path must agree. `branchObservations` is shared with
// the Batch decision's tool half, so the Batch shape inherits this.
func TestExecutor_CallParallel_BranchCarriesTheClass(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	registerArtifactConsumer(t, cat)
	registerEcho(t, cat, "fine")
	store := newTestArtifactStore(t)
	exec := NewToolExecutor(cat, store, nil)

	q := dispatchTestQuad("r-class-parallel")
	ctx := dispatchTestCtx(t, q)

	// One branch resolves, one does not — so the class is proven to land
	// on the failing branch and NOT on its healthy sibling.
	ref, err := store.PutBytes(ctx, artifacts.ArtifactScope{
		TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID,
	}, []byte("real content"), artifacts.PutOpts{MimeType: "text/plain"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	good := artifactCall(ref.ID)
	good.CallID = "call_good"
	bad := artifactCall("id_the_model_invented")
	bad.CallID = "call_bad"

	raw, llm, execErr := exec.ExecuteDecision(ctx, planner.RunContext{Quadruple: q},
		planner.CallParallel{Branches: []planner.CallTool{good, bad}})
	if execErr != nil {
		t.Fatalf("non-atomic parallel dispatch aborted the whole call: %v", execErr)
	}

	for label, obs := range map[string]any{"raw": raw, "llm": llm} {
		agg, ok := obs.(planner.ParallelObservation)
		if !ok {
			t.Fatalf("%s observation = %#v, want a ParallelObservation", label, obs)
		}
		if len(agg.Branches) != 2 {
			t.Fatalf("%s observation carries %d branches, want 2", label, len(agg.Branches))
		}
		if got := agg.Branches[0].ErrorClass; got != "" {
			t.Errorf("%s: the SUCCEEDING branch acquired the class %q", label, got)
		}
		if agg.Branches[1].Error == "" {
			t.Fatalf("%s: the unresolvable branch did not fail", label)
		}
		if got := agg.Branches[1].ErrorClass; got != planner.ObservationClassArtifactRefNotFound {
			t.Errorf("%s: branch ErrorClass = %q, want %q (error was %q)",
				label, got, planner.ObservationClassArtifactRefNotFound, agg.Branches[1].Error)
		}
	}
}

// TestExecutor_CallParallel_ToolsOwnBranchError_IsUnclassified is the
// parallel path's no-change pin.
func TestExecutor_CallParallel_ToolsOwnBranchError_IsUnclassified(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "explodes"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, errors.New("upstream 503")
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), nil)

	q := dispatchTestQuad("r-class-parallel-toolerr")
	raw, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), planner.RunContext{Quadruple: q},
		planner.CallParallel{Branches: []planner.CallTool{
			{CallID: "c0", Tool: "explodes", Args: json.RawMessage(`{}`)},
		}})
	if err != nil {
		t.Fatalf("parallel dispatch aborted: %v", err)
	}
	agg, ok := raw.(planner.ParallelObservation)
	if !ok || len(agg.Branches) != 1 {
		t.Fatalf("raw observation = %#v, want one branch", raw)
	}
	if agg.Branches[0].Error == "" {
		t.Fatal("the failing branch reported no error")
	}
	if got := agg.Branches[0].ErrorClass; got != "" {
		t.Errorf("a tool's own branch error acquired the class %q", got)
	}
	// And the persisted JSON is byte-identical to the pre-phase shape.
	encoded, mErr := json.Marshal(agg.Branches[0])
	if mErr != nil {
		t.Fatalf("Marshal: %v", mErr)
	}
	if strings.Contains(string(encoded), "error_class") {
		t.Errorf("an unclassified branch emitted the class key: %s", encoded)
	}
}
