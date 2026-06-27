// runonce.go — the production one-call goal runner.
//
// Assembling a stack is one call (Assemble); running a goal against it
// was not. An embedder had to hand-build a planner.RunContext + a
// steering.RunSpec and drive RunLoop.Run itself — ~15-27 lines of
// per-run ceremony re-deriving projections the runtime already owns.
// RunOnce closes that gap: one blocking call turns a goal + identity
// into the terminal answer envelope, building the RunContext through
// the shared runctx.NewRunContext factory and driving the assembled
// RunLoop.
//
// House style: RunOnce BLOCKS on the calling goroutine and has no Sync
// suffix — it mirrors steering.RunLoop.Run and harbortest.RunOnce, both
// of which block; "async" is the caller's own `go`. The functional
// RunOption type is extensible so a streaming sink can be added without
// changing the signature.

package assemble

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tasks"
)

// Sentinel errors RunOnce returns when the stack is not runnable.
// Callers compare via errors.Is.
var (
	// ErrNotRunnable fires when RunOnce is called on a stack assembled
	// without a planner / run loop (no LLM driver, or SkipSteering /
	// SkipRunLoop). The wrapped detail names the missing component.
	ErrNotRunnable = errors.New("assemble: stack is not runnable")
)

// runOnceConfig holds the per-call knobs RunOption sets. The type is
// extensible: a new per-run knob adds a field here without touching
// RunOnce's signature. The streaming sink (WithStream) is one such
// sibling addition.
type runOnceConfig struct {
	runID            string
	inputArtifactIDs []string
	stream           func(StreamEvent)
}

// RunOption configures a RunOnce invocation. The functional-option
// shape keeps RunOnce's signature stable as new per-run knobs land
// (a streaming sink is a sibling addition, not a signature change).
type RunOption func(*runOnceConfig)

// WithRunID pins the run's RunID instead of synthesising a fresh ULID.
// Useful for correlating a run with externally-minted identifiers.
func WithRunID(runID string) RunOption {
	return func(c *runOnceConfig) { c.runID = runID }
}

// WithInputArtifacts pre-resolves operator-uploaded artifact IDs into
// the run's first-turn multimodal inputs.
func WithInputArtifacts(ids ...string) RunOption {
	return func(c *runOnceConfig) {
		c.inputArtifactIDs = append(c.inputArtifactIDs, ids...)
	}
}

// RunOnce drives goal through the assembled planner/run-loop under the
// mandatory identity triple and returns the terminal answer envelope.
// It BLOCKS until the run reaches a terminal Finish (or errors). One
// call replaces the hand-built RunContext + RunSpec + RunLoop.Run +
// envelope-extraction ceremony an embedder previously wrote per run.
//
// The RunContext is built through runctx.NewRunContext, so the run sees
// the SAME memory / skills / artifact / streaming projections the dev
// run-loop drivers compose. Identity is mandatory and fails loud: an
// incomplete triple returns a wrapped identity error before any work.
//
// On a goal-satisfying Finish the session's memory (when wired) records
// the turn — best-effort, mirroring the drivers: a writeback error is
// not fatal to the returned answer. A non-goal Finish is returned with
// its FinishReason verbatim (the caller maps it via
// planner.TaskErrorCodeForFinish); a RunLoop error is wrapped and
// returned.
//
// Streaming: pass WithStream(sink) to observe token deltas, planner-step
// boundaries, and tool dispatches as they occur. RunOnce still blocks
// and returns the same envelope; the sink fires synchronously on the run
// goroutine, so every StreamEvent arrives before RunOnce returns.
//
// Concurrent reuse: RunOnce is safe to call from N concurrent
// goroutines against a single shared Stack — every invocation builds
// its own per-run RunContext (counters, trajectory, projections) and
// reads run-specific data only from its arguments, never from the
// Stack. The compiled artifacts (RunLoop, Planner, Executor, Catalog,
// stores) are immutable after Assemble and internally synchronised.
func (s *Stack) RunOnce(
	ctx context.Context,
	goal string,
	id identity.Identity,
	opts ...RunOption,
) (planner.AnswerEnvelope, error) {
	if s.RunLoop == nil {
		return planner.AnswerEnvelope{}, fmt.Errorf("%w: no RunLoop (assembled without an LLM/planner, or with SkipSteering/SkipRunLoop)", ErrNotRunnable)
	}
	if s.Planner == nil {
		return planner.AnswerEnvelope{}, fmt.Errorf("%w: no Planner", ErrNotRunnable)
	}
	if err := identity.Validate(id); err != nil {
		return planner.AnswerEnvelope{}, fmt.Errorf("assemble: RunOnce identity: %w", err)
	}

	var cfg runOnceConfig
	for _, o := range opts {
		o(&cfg)
	}
	runID := cfg.runID
	if runID == "" {
		runID = newRunOnceID()
	}
	q := identity.Quadruple{Identity: id, RunID: runID}

	// Attach the identity triple + run to ctx so the best-effort memory
	// writeback below (an identity-scoped store call) is satisfied. The
	// RunLoop re-derives the same identity ctx from spec.Base.Quadruple
	// internally, so passing this ctx through is idempotent.
	runCtx, err := identity.WithRun(ctx, id, runID)
	if err != nil {
		return planner.AnswerEnvelope{}, fmt.Errorf("assemble: RunOnce identity ctx: %w", err)
	}

	logger := s.boundLogger()

	// Build the skills Directory from the stack's SkillStore — mirroring
	// the production cmd/harbor + devstack drivers (same DirectoryFromConfig
	// inputs) so the skills surface RunOnce projects matches theirs.
	var skillsDir *skills.Directory
	if s.Skills != nil {
		sd, dErr := skills.NewDirectory(s.Skills, skills.Deps{Bus: s.Bus},
			skills.DirectoryFromConfig(s.Cfg.Skills, s.Cfg.Planner.SkillsContextMaxResolved()))
		if dErr != nil {
			return planner.AnswerEnvelope{}, fmt.Errorf("assemble: RunOnce skills directory: %w", dErr)
		}
		skillsDir = sd
	}

	base, err := runctx.NewRunContext(runCtx, runctx.Sources{
		Memory:          s.Memory,
		MemoryRecall:    memory.RecallFromConfig(s.Cfg.Memory),
		SkillsDirectory: skillsDir,
		Catalog:         s.Catalog,
		Artifacts:       s.Artifacts,
		Bus:             s.Bus,
		Logger:          logger,
		GrantedScopes:   s.Cfg.Tools.GrantedScopes,
		Budget:          planner.Budget{TokenBudget: s.Cfg.Planner.TokenBudget},
	}, q, goal, runctx.WithInputArtifacts(cfg.inputArtifactIDs...))
	if err != nil {
		return planner.AnswerEnvelope{}, fmt.Errorf("assemble: RunOnce: %w", err)
	}

	// Streaming sink wiring. WithStream rides the SAME blocking RunOnce
	// call by tapping the two callbacks the run loop fires SYNCHRONOUSLY
	// on this goroutine: planner.RunContext.OnChunk (token deltas + step
	// boundaries) and steering.RunSpec.OnToolDispatched (tool dispatches).
	// Both are additive — any bus-backed publisher NewRunContext already
	// wired stays in place; the sink composes on top. Because the taps
	// fire inline, every StreamEvent reaches the sink before RunOnce
	// returns the envelope, and each call captures its own sink so
	// concurrent runs never cross chunks (the concurrent-reuse contract).
	if cfg.stream != nil {
		sink := cfg.stream
		prevOnChunk := base.OnChunk
		base.OnChunk = func(delta string, done bool, kind planner.ChunkKind) {
			if prevOnChunk != nil {
				prevOnChunk(delta, done, kind)
			}
			sink(streamChunkEvent(delta, done, kind))
		}
	}

	spec := steering.RunSpec{
		Planner:      s.Planner,
		Base:         base,
		TaskID:       tasks.TaskID(runID),
		ToolExecutor: s.Executor,
		MaxSteps:     s.Cfg.Planner.MaxSteps,
		Compression:  s.Compression,
	}
	if cfg.stream != nil {
		sink := cfg.stream
		prevDispatch := spec.OnToolDispatched
		spec.OnToolDispatched = func(hookCtx context.Context) error {
			if prevDispatch != nil {
				if err := prevDispatch(hookCtx); err != nil {
					return err
				}
			}
			sink(StreamEvent{Kind: StreamToolDispatched})
			return nil
		}
	}

	fin, err := s.RunLoop.Run(runCtx, spec)
	if err != nil {
		return planner.AnswerEnvelope{}, fmt.Errorf("assemble: RunOnce: %w", err)
	}

	answer := runctx.ExtractAssistantAnswer(fin)

	// Best-effort memory writeback on a goal-satisfying finish (mirrors
	// the drivers): a writeback error does not downgrade the answer.
	if s.Memory != nil && fin.Reason == planner.FinishGoal {
		sessionQ := identity.Quadruple{Identity: id}
		if wErr := s.Memory.AddTurn(runCtx, sessionQ, memory.ConversationTurn{
			UserMessage:       goal,
			AssistantResponse: answer,
			Timestamp:         time.Now(),
		}); wErr != nil {
			logger.Warn("assemble: RunOnce memory writeback failed; answer still returned",
				"run_id", runID, "err", wErr.Error())
		}
	}

	steps := 0
	if base.Trajectory != nil {
		steps = len(base.Trajectory.Steps)
	}
	return planner.AnswerEnvelope{
		Answer:        answer,
		FinishReason:  string(fin.Reason),
		ToolCallsSeen: steps,
	}, nil
}

// boundLogger returns the stack's canonical structured logger, falling
// back to slog.Default() before Telemetry is populated (a partial stack
// returned alongside an Assemble error).
func (s *Stack) boundLogger() *slog.Logger {
	if s.Telemetry != nil {
		return s.Telemetry.Slog()
	}
	return slog.Default()
}

// newRunOnceID synthesises a fresh lexicographically-sortable run ID.
// ulid.MustNew with crypto-strong entropy is concurrency-safe and does
// no shared-state mutation, so concurrent RunOnce calls never collide.
func newRunOnceID() string {
	return "run-" + ulid.MustNew(ulid.Now(), rand.Reader).String()
}
