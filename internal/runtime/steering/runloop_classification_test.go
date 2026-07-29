// internal/runtime/steering/runloop_classification_test.go — the run
// loop's rendering of a CLASSIFIED executor error onto the step's
// observation.
//
// External test package (`steering_test`) on purpose: the errors under
// test are produced by `internal/runtime/dispatch`, which imports
// `steering`, so an in-package test importing dispatch would be an
// import cycle. Everything on the seam is therefore the real thing — the
// production executor, the shipped artifact-reference consumer tool, a
// real in-memory artifact store, a real pause coordinator and the real
// run loop.

package steering_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/examples/tools/artifactstats"
	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tools"
)

// classifyPlanner emits one decision and then finishes, so the run loop
// records exactly one step whose observation the test can read.
type classifyPlanner struct {
	decision planner.Decision
	emitted  bool
}

func (p *classifyPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	if !p.emitted {
		p.emitted = true
		return p.decision, nil
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

// classifyStack wires the production executor over real drivers.
func classifyStack(t *testing.T, withStore bool) steering.ToolExecutor {
	t.Helper()
	cat := tools.NewCatalog()
	if err := artifactstats.Register(cat); err != nil {
		t.Fatalf("register %s: %v", artifactstats.ToolName, err)
	}
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "explodes"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, errors.New("upstream 503")
		},
	}); err != nil {
		t.Fatalf("register explodes: %v", err)
	}
	if !withStore {
		return dispatch.NewToolExecutor(cat, nil, nil)
	}
	store, err := artifactsinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return dispatch.NewToolExecutor(cat, store, nil)
}

// runOneStep drives the real run loop over a single decision and returns
// the recorded step.
func runOneStep(t *testing.T, exec steering.ToolExecutor, decision planner.Decision) planner.Step {
	t.Helper()
	rl, err := steering.NewRunLoop(steering.NewRegistry(), pauseresume.New())
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}
	q := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "tenant-classify",
			UserID:    "user-classify",
			SessionID: "session-classify",
		},
		RunID: "run-classify",
	}
	traj := &trajectory.Trajectory{}
	if _, err := rl.Run(context.Background(), steering.RunSpec{
		Planner: &classifyPlanner{decision: decision},
		Base: planner.RunContext{
			Quadruple:  q,
			Goal:       "classify",
			Trajectory: traj,
		},
		MaxSteps:     4,
		ToolExecutor: exec,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(traj.Steps) != 1 {
		t.Fatalf("trajectory carries %d steps, want 1", len(traj.Steps))
	}
	return traj.Steps[0]
}

// artifactDecision is the CallTool a planner emits when it names an
// artifact id for a tool that consumes one by reference.
func artifactDecision(refID string) planner.CallTool {
	return planner.CallTool{
		CallID: "call_1",
		Tool:   artifactstats.ToolName,
		Args:   json.RawMessage(fmt.Sprintf(`{"artifact":%q}`, refID)),
	}
}

// observationClass reads the class key off an error observation. The
// slot is asserted INDEPENDENTLY on Observation and LLMObservation
// rather than assumed to alias: the run loop assigns one payload to
// both today, and a future split must not silently drop one.
func observationClass(t *testing.T, label string, obs any) string {
	t.Helper()
	m, ok := obs.(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want the error-observation map", label, obs)
	}
	if m["error"] == nil || m["error"] == "" {
		t.Fatalf("%s carries no error: %#v", label, m)
	}
	if m[planner.ObservationClassKey] == nil {
		return ""
	}
	s, ok := m[planner.ObservationClassKey].(string)
	if !ok {
		t.Fatalf("%s carries a non-string class %#v", label, m[planner.ObservationClassKey])
	}
	return s
}

// TestRunLoop_UnresolvableArtifactRef_ClassLandsOnBothObservationSlots
// is the phase's end-to-end classification gate: the class survives the
// whole %w chain from the seated resolver to the step the planner reads
// on its next turn, and it lands on the CANONICAL observation as well as
// the LLM-facing one — the canonical view is what a post-incident reader
// reconstructs a failed run from.
func TestRunLoop_UnresolvableArtifactRef_ClassLandsOnBothObservationSlots(t *testing.T) {
	t.Parallel()
	step := runOneStep(t, classifyStack(t, true), artifactDecision("id_the_model_invented"))

	want := string(planner.ObservationClassArtifactRefNotFound)
	if got := observationClass(t, "Step.Observation", step.Observation); got != want {
		t.Errorf("Step.Observation class = %q, want %q", got, want)
	}
	if got := observationClass(t, "Step.LLMObservation", step.LLMObservation); got != want {
		t.Errorf("Step.LLMObservation class = %q, want %q", got, want)
	}
}

// TestRunLoop_NoArtifactStoreWired_CarriesTheResolverClass — the
// operator-misconfiguration class, so a planner does not burn its step
// budget on a failure it cannot repair.
func TestRunLoop_NoArtifactStoreWired_CarriesTheResolverClass(t *testing.T) {
	t.Parallel()
	step := runOneStep(t, classifyStack(t, false), artifactDecision("anything"))

	want := string(planner.ObservationClassArtifactResolverUnavailable)
	if got := observationClass(t, "Step.Observation", step.Observation); got != want {
		t.Errorf("Step.Observation class = %q, want %q", got, want)
	}
	if got := observationClass(t, "Step.LLMObservation", step.LLMObservation); got != want {
		t.Errorf("Step.LLMObservation class = %q, want %q", got, want)
	}
}

// TestRunLoop_ToolsOwnError_ObservationIsUnchanged is the no-change pin
// on the runtime's hottest path: a tool's own failure produces the
// SINGLE-KEY payload it always produced.
func TestRunLoop_ToolsOwnError_ObservationIsUnchanged(t *testing.T) {
	t.Parallel()
	step := runOneStep(t, classifyStack(t, true), planner.CallTool{
		CallID: "call_1", Tool: "explodes", Args: json.RawMessage(`{}`),
	})

	m, ok := step.Observation.(map[string]any)
	if !ok {
		t.Fatalf("Step.Observation = %#v, want the error-observation map", step.Observation)
	}
	if len(m) != 1 {
		t.Fatalf("an unclassified error observation carries %d keys (%#v), want exactly the one `error` key", len(m), m)
	}
	if want := `tool "explodes" invoke: upstream 503`; m["error"] != want {
		t.Errorf("error = %v, want the pre-phase message %q", m["error"], want)
	}
	if _, present := m[planner.ObservationClassKey]; present {
		t.Error("a tool's own error acquired the class key")
	}
}

// TestRunLoop_ClassifiedObservation_SurvivesTrajectoryPersistence closes
// the persistence half: the observation is JSON-encoded into the
// trajectory across a checkpoint and read back, so a class that only
// existed in memory would be lost on resume. The prompt-rendering half
// lives with the renderer, in internal/planner/react.
func TestRunLoop_ClassifiedObservation_SurvivesTrajectoryPersistence(t *testing.T) {
	t.Parallel()
	step := runOneStep(t, classifyStack(t, true), artifactDecision("id_the_model_invented"))

	encoded, err := json.Marshal(step.LLMObservation)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `"` + planner.ObservationClassKey + `":"` + string(planner.ObservationClassArtifactRefNotFound) + `"`
	if !strings.Contains(string(encoded), want) {
		t.Fatalf("the persisted observation does not carry %s:\n%s", want, encoded)
	}

	var round map[string]any
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if round[planner.ObservationClassKey] != string(planner.ObservationClassArtifactRefNotFound) {
		t.Fatalf("the class did not survive the round trip: %#v", round)
	}
}
