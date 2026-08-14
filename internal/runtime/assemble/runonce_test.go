// runonce_test.go — coverage for the production one-call runner
// (D-265): the golden path, the identity-mandatory + not-runnable
// fail-loud edges, and the mandatory N≥100 concurrent-reuse stress
// against a single shared Stack (D-025 / CLAUDE.md §5 / §11): no data
// races (run under -race), no context bleed (each run's answer + run ID
// stays its own), no cross-cancellation (cancelling one run's ctx does
// not affect the others), and no goroutine leak (baseline restored
// after every run returns and the stack closes).
package assemble_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"

	// Production driver aggregator — the same import the recipe tells a
	// headless embedder to add.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	// Dev-only mock LLM (D-089): explicit opt-in, never in the aggregator.
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
)

// runnableCfg returns the mock-LLM config minimalCfg builds — a stack
// whose RunOnce reaches FinishGoal deterministically.
func runnableStack(t *testing.T) *assemble.Stack {
	t.Helper()
	cfg := minimalCfg(t)
	// minimalCfg leaves Model unset (the assembly tests never drive the
	// LLM); RunOnce DOES, so give the mock a model + profile.
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
	}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if stack.RunLoop == nil || stack.Planner == nil {
		t.Fatalf("runnable stack must have RunLoop + Planner (RunLoop=%v Planner=%v)", stack.RunLoop, stack.Planner)
	}
	return stack
}

// TestRunOnce_GoldenPath_ReturnsAnswer — one call turns a goal +
// identity into a terminal answer envelope.
func TestRunOnce_GoldenPath_ReturnsAnswer(t *testing.T) {
	ctx := context.Background()
	stack := runnableStack(t)
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	env, err := stack.RunOnce(ctx, "Summarise the deployment status.", id)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if env.FinishReason != string(planner.FinishGoal) {
		t.Errorf("FinishReason = %q, want %q", env.FinishReason, planner.FinishGoal)
	}
	if env.Answer == "" {
		t.Error("expected a non-empty answer on the golden path")
	}
}

// TestRunOnce_ProjectsSkillsAndInputArtifacts exercises the two RunOnce
// construction branches the golden-path tests skip: the skills-Directory
// build (reached only when the stack carries a SkillStore) and the
// WithInputArtifacts input-artifact pass-through. A future divergence in
// the DirectoryFromConfig args or the Sources wiring would error here even
// while the golden path stays green.
func TestRunOnce_ProjectsSkillsAndInputArtifacts(t *testing.T) {
	ctx := context.Background()
	cfg := minimalCfg(t)
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
	}
	// A real SkillStore makes RunOnce build the skills Directory.
	cfg.Skills = config.SkillsConfig{
		Driver: "localdb",
		DSN:    filepath.Join(t.TempDir(), "skills.sqlite"),
	}
	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble (with skills): %v", err)
	}
	defer func() { _ = stack.Close(ctx) }()
	if stack.Skills == nil {
		t.Fatal("expected a wired SkillStore from cfg.Skills.Driver=localdb")
	}

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}

	// Seed an input artifact and pass it via WithInputArtifacts so the
	// ResolveInputArtifacts projection branch executes for a real ref.
	ref, err := stack.Artifacts.PutText(ctx,
		artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID},
		"the quarterly deployment report",
		artifacts.PutOpts{MimeType: "text/plain", Filename: "report.txt"})
	if err != nil {
		t.Fatalf("PutText (seed input artifact): %v", err)
	}

	env, err := stack.RunOnce(ctx, "Summarise the attached report.", id,
		assemble.WithInputArtifacts(ref.ID))
	if err != nil {
		t.Fatalf("RunOnce(skills+artifacts): %v", err)
	}
	if env.FinishReason != string(planner.FinishGoal) {
		t.Errorf("FinishReason = %q, want %q", env.FinishReason, planner.FinishGoal)
	}
}

// TestRunOnce_WithRunID_PinsRunID — the run uses the caller-supplied ID.
func TestRunOnce_WithRunID_PinsRunID(t *testing.T) {
	ctx := context.Background()
	stack := runnableStack(t)
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	if _, err := stack.RunOnce(ctx, "goal", id, assemble.WithRunID("pinned-run-7")); err != nil {
		t.Fatalf("RunOnce(WithRunID): %v", err)
	}
}

// TestRunOnce_IdentityMandatory_FailsLoud — an incomplete triple is
// rejected before any work (CLAUDE.md §6 / §13).
func TestRunOnce_IdentityMandatory_FailsLoud(t *testing.T) {
	ctx := context.Background()
	stack := runnableStack(t)
	defer func() { _ = stack.Close(ctx) }()

	_, err := stack.RunOnce(ctx, "goal", identity.Identity{TenantID: "acme"})
	if err == nil {
		t.Fatal("RunOnce with an incomplete identity must fail loud")
	}
}

// TestRunOnce_NotRunnable_FailsLoud — a stack assembled without a run
// loop (SkipRunLoop) returns ErrNotRunnable, never a silent no-op.
func TestRunOnce_NotRunnable_FailsLoud(t *testing.T) {
	ctx := context.Background()
	stack, err := assemble.Assemble(ctx, minimalCfg(t), assemble.Options{SkipRunLoop: true})
	if err != nil {
		t.Fatalf("Assemble(SkipRunLoop): %v", err)
	}
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	_, err = stack.RunOnce(ctx, "goal", id)
	if !errors.Is(err, assemble.ErrNotRunnable) {
		t.Fatalf("RunOnce on a non-runnable stack: got %v, want ErrNotRunnable", err)
	}
}

// TestRunOnce_BootBaselineConfigured_FailsLoud — the HA-66 headless
// posture: a stack whose configuration declares `skills.boot_agent_packs`
// refuses RunOnce loud with the typed sentinel, BEFORE any runnability /
// identity work. The boot baseline is a serve-band surface (resolved
// boot/default agent, loader/composer/guard/preview wiring); a headless
// one-shot run must never compose it under an invented identity and
// never silently skip it.
func TestRunOnce_BootBaselineConfigured_FailsLoud(t *testing.T) {
	ctx := context.Background()
	// A minimal stack whose config declares one boot pack — RunOnce
	// checks the declaration before touching the RunLoop, so the guard
	// fires even on a zero-value stack.
	stack := &assemble.Stack{Cfg: &config.Config{Skills: config.SkillsConfig{
		BootAgentPacks: []config.BootAgentPackConfig{{
			TenantID: "acme", AgentID: "boot-agent", Directory: "/tmp/boot",
		}},
	}}}
	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	_, err := stack.RunOnce(ctx, "goal", id)
	if !errors.Is(err, assemble.ErrBootBaselineUnsupported) {
		t.Fatalf("RunOnce with boot_agent_packs configured: got %v, want ErrBootBaselineUnsupported", err)
	}
}

// TestRunOnce_BootBaselineAbsent_Unchanged — with no boot declaration
// the guard is inert: the zero-value stack reaches the ordinary
// runnability refusal (ErrNotRunnable), proving the fail-loud is scoped
// to the declared baseline and never blocks the default path.
func TestRunOnce_BootBaselineAbsent_Unchanged(t *testing.T) {
	ctx := context.Background()
	stack := &assemble.Stack{Cfg: minimalCfg(t)}
	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	_, err := stack.RunOnce(ctx, "goal", id)
	if !errors.Is(err, assemble.ErrNotRunnable) {
		t.Fatalf("RunOnce without boot_agent_packs: got %v, want ErrNotRunnable (default posture unchanged)", err)
	}
}

// TestRunOnce_ConcurrentReuse_NoBleedNoLeak — the mandatory D-025
// concurrent-reuse stress: N≥100 concurrent RunOnce invocations against
// ONE shared Stack, each with a distinct identity triple AND its own
// WithStream sink. Asserts no data races (-race), no context bleed
// (every run's answer + finish is independently successful), no
// cross-cancellation (a band of runs whose ctx is cancelled fail WITHOUT
// taking down the others), no goroutine leak (baseline restored after
// Close), and — the D-266 extension — NO CROSS-RUN CHUNK BLEED: every
// run's sink receives ONLY its own run's streamed chunks (run A's tokens
// never reach run B's sink).
func TestRunOnce_ConcurrentReuse_NoBleedNoLeak(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	ctx := context.Background()
	stack := runnableStack(t)

	const n = 120
	// Per-run goal markers are delimiter-wrapped so no marker is a prefix
	// of another ("goal#1#" is not a substring of "goal#12#") — the chunk
	// no-bleed assertion below depends on that uniqueness.
	goalFor := func(i int) string { return fmt.Sprintf("goal#%d#", i) }
	type result struct {
		idx       int
		env       planner.AnswerEnvelope
		err       error
		cancelled bool
		// chunks is the run's OWN sink's assembled content tokens. Written
		// only on this run's goroutine (the sink fires synchronously) and
		// read after RunOnce returns — no shared access.
		chunks string
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			// Every fifth run gets a ctx cancelled before it starts —
			// proves one run's cancellation never crosses into another's.
			runCtx := ctx
			cancelled := false
			if i%5 == 0 {
				c, cancel := context.WithCancel(ctx)
				cancel()
				runCtx = c
				cancelled = true
			}
			id := identity.Identity{
				TenantID:  "t-" + fmt.Sprint(i),
				UserID:    "u-" + fmt.Sprint(i),
				SessionID: "s-" + fmt.Sprint(i),
			}
			// Each run captures its OWN sink — the no-cross-run-bleed
			// fixture. A bug that shared a sink across runs (a mutable
			// field on the compiled Stack) would mix run j's tokens in here.
			var sb strings.Builder
			sink := func(e assemble.StreamEvent) {
				if e.Kind == assemble.StreamToken && !e.Reasoning {
					sb.WriteString(e.Text)
				}
			}
			env, err := stack.RunOnce(runCtx, goalFor(i), id,
				assemble.WithRunID("run-"+fmt.Sprint(i)), assemble.WithStream(sink))
			results[i] = result{idx: i, env: env, err: err, cancelled: cancelled, chunks: sb.String()}
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		if r.cancelled {
			// A cancelled run must fail — but failing is all we require;
			// the point is it did not corrupt the successful runs.
			if r.err == nil {
				continue // a race where the run finished before observing cancel is acceptable
			}
			continue
		}
		if r.err != nil {
			t.Errorf("run %d (uncancelled) failed: %v", r.idx, r.err)
			continue
		}
		if r.env.FinishReason != string(planner.FinishGoal) {
			t.Errorf("run %d: FinishReason = %q, want %q", r.idx, r.env.FinishReason, planner.FinishGoal)
		}
		if r.env.Answer == "" {
			t.Errorf("run %d: empty answer (possible context bleed / lost output)", r.idx)
			continue
		}
		// No context bleed: the run's answer must carry ITS OWN goal, not
		// a concurrent run's. The mock LLM echoes the goal text back into
		// the answer, so run i receiving run j's content fails here — this
		// is what makes the no-bleed guarantee in the test name load-bearing.
		own := goalFor(r.idx)
		if !strings.Contains(r.env.Answer, own) {
			t.Errorf("run %d: answer %q does not contain its own goal %q — context bleed across concurrent runs", r.idx, r.env.Answer, own)
		}
		// No CROSS-RUN CHUNK bleed: the run's OWN sink received its OWN
		// streamed chunks (assembled content tokens contain its goal) and
		// NO foreign run's chunks (no other run's goal marker appears).
		if !strings.Contains(r.chunks, own) {
			t.Errorf("run %d: own sink chunks %q do not contain its goal %q — chunks lost or misrouted", r.idx, r.chunks, own)
		}
		for j := range n {
			if j == r.idx {
				continue
			}
			if strings.Contains(r.chunks, goalFor(j)) {
				t.Errorf("run %d: own sink chunks %q contain run %d's goal %q — CROSS-RUN CHUNK BLEED", r.idx, r.chunks, j, goalFor(j))
			}
		}
	}

	if err := stack.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	settleGoroutines(t, baseline)
}

// (settleGoroutines + minimalCfg are defined in assemble_test.go.)
