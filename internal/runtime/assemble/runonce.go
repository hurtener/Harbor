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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
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
	// outputSchema carries the run-level output schema (raw JSON-Schema
	// bytes) set via WithOutputSchema. Nil means "not set"; a set-but-
	// empty schema is caught as a loud error in RunOnce (never a silent
	// no-op). outputSchemaSet disambiguates "not set" from "set to nil".
	outputSchema    json.RawMessage
	outputSchemaSet bool
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

// WithOutputSchema opts a run into run-level structured output: the
// terminal answer is validated against the supplied JSON Schema and
// delivered as the envelope's additive AnswerPayload key (raw validated
// JSON), with AnswerEnvelope.Answer carrying the same payload's string
// rendering. A nil / empty schema is a LOUD config error at call time
// (surfaced when RunOnce runs), never a silent no-op.
//
// Behaviour when the option is set:
//   - The generation-steering planner (the ReAct driver) constrains its
//     terminal completion via the profile's existing OutputMode strategy
//     and downgrade chain, and engages the retry-with-feedback loop
//     bounded by ModelProfile.MaxRetries on a schema-invalid answer — no
//     new shaping toggle.
//   - The terminal Finish payload is validated against the schema at the
//     RunOnce edge for EVERY planner (React, deterministic, external
//     concretes) — but ONLY on a goal-satisfying Finish. A non-goal
//     terminal (a steering CANCEL surfaced as Finish{Cancelled}, a
//     planner NoPath finish, a deadline/constraints terminal) was never
//     asked to produce a schema-shaped answer, so it returns the
//     envelope exactly as a plain (no-schema) run would — empty
//     AnswerPayload, FinishReason verbatim, never ErrOutputInvalid. A
//     schema-invalid answer on a goal finish after the correction budget
//     is a loud, typed planner.ErrOutputInvalid — NEVER a silent
//     fallback to unvalidated text. ErrOutputInvalid also wraps a
//     provider that rejected every OutputMode/downgrade attempt
//     (llm.ErrDowngradeExhausted), alongside a react retry-loop
//     exhaustion (llm.ErrRetryExhausted) — two producer sites, ONE
//     terminal error shape.
//
// Per-OutputMode interaction (the profile's existing OutputMode strategy
// decides which of these a schema-constrained run rides — no new
// toggle):
//   - `native` — schema and native tool-calling compose cleanly: a
//     tool-call turn carries no content (so the Validator's tool-call-
//     aware pass-through applies), and the content-bearing terminal turn
//     carries the provider-enforced schema. The recommended profile for
//     tool-heavy schema runs.
//   - `prompted` — the JSON-only instruction + FormatJSONObject ride
//     EVERY turn (not just the terminal one), which can bias the model
//     against dispatching tools mid-run in favour of emitting JSON
//     immediately. Prefer `native` when the run is expected to call
//     tools before its terminal answer.
//   - `tools` — the downgrade layer's write half asks the model for a
//     `{"name":"respond_with","arguments":{...}}` envelope; the react
//     terminal-answer path transparently unwraps it (both before
//     Validator checks and before the Finish payload is captured) so the
//     caller's schema-shaped `arguments` reach AnswerPayload, never the
//     envelope wrapper. See internal/llm/output.ParseRespondWith.
//
// Streaming posture (composes with WithStream): tool_dispatched and step
// events stream exactly as today, but ALL token chunks — content AND
// reasoning — are SUPPRESSED for a schema-constrained run: a validate-
// and-retry loop cannot retract tokens already streamed, so the
// validated answer arrives once, in the envelope. This is a documented
// behaviour choice, deliberately more conservative than raw-JSON delta
// streaming. Each corrective retry/downgrade attempt the schema
// constraint engages emits its own `step` event (one per LLM attempt),
// so a schema-constrained run streams MORE step events per planner turn
// than a plain run when a correction fires.
//
// Zero default-path change: without the option, RunOnce behaviour and
// the envelope bytes are byte-identical to before.
func WithOutputSchema(schema json.RawMessage) RunOption {
	return func(c *runOnceConfig) {
		c.outputSchema = schema
		c.outputSchemaSet = true
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
	// WithOutputSchema fails loud on a nil/empty schema at call time — a
	// set-but-empty schema is a config mistake, never a silent no-op.
	if cfg.outputSchemaSet && len(bytes.TrimSpace(cfg.outputSchema)) == 0 {
		return planner.AnswerEnvelope{}, fmt.Errorf("assemble: RunOnce: WithOutputSchema was given a nil/empty schema (a schema is required when the option is set)")
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
		OutputSchema:    cfg.outputSchema,
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
		if base.OutputSchema != nil {
			// Schema-constrained run: suppress token deltas at the sink —
			// the validated answer arrives once, in the envelope, because a
			// validate-and-retry loop cannot retract already-streamed
			// tokens. step + tool_dispatched still stream. See the
			// WithOutputSchema + WithStream godoc.
			inner := sink
			sink = func(ev StreamEvent) {
				if ev.Kind == StreamToken {
					return
				}
				inner(ev)
			}
		}
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
		// One StreamToolDispatched event PER dispatched tool: a
		// CallParallel dispatch carries count == len(Branches), so the
		// stream reports N tool dispatches, not one (see
		// streamDispatchHook).
		spec.OnToolDispatched = streamDispatchHook(spec.OnToolDispatched, cfg.stream)
	}

	fin, err := s.RunLoop.Run(runCtx, spec)
	if err != nil {
		// A schema-constrained run whose generation-steering retry loop
		// exhausted its budget surfaces as ErrRetryExhausted from the LLM
		// edge; a provider that rejects every OutputMode/downgrade attempt
		// surfaces ErrDowngradeExhausted. Map BOTH to the typed,
		// planner-agnostic ErrOutputInvalid so the caller sees ONE of two
		// terminal error shapes regardless of where the budget ran out
		// (react retry loop, the downgrade chain, or the runtime-edge
		// check below) — never a silent fallback to unvalidated text
		// (§13).
		if base.OutputSchema != nil && (errors.Is(err, llm.ErrRetryExhausted) || errors.Is(err, llm.ErrDowngradeExhausted)) {
			return planner.AnswerEnvelope{}, fmt.Errorf("%w: schema-constrained run exhausted the correction budget: %w", planner.ErrOutputInvalid, err)
		}
		return planner.AnswerEnvelope{}, fmt.Errorf("assemble: RunOnce: %w", err)
	}

	// Runtime-edge final validation + envelope construction via the ONE
	// shared builder (runctx.FinishAnswerEnvelope) both per-task drivers
	// also call — the validation is planner-agnostic (no capability
	// ceremony), applies ONLY on a goal-satisfying Finish, and captures
	// the validated bytes at the validation site so AnswerPayload carries
	// the exact bytes (never a lossy round-trip through the
	// ExtractAssistantAnswer collapse). A non-goal terminal
	// (FinishCancelled from a steering CANCEL, FinishNoPath, a
	// deadline/constraints terminal) is returned exactly as a plain
	// (no-schema) run would — empty AnswerPayload, FinishReason verbatim,
	// never ErrOutputInvalid. A schema-invalid goal Finish surfaces a
	// loud, typed planner.ErrOutputInvalid (wrapped by the builder).
	env, err := runctx.FinishAnswerEnvelope(fin, base.Trajectory, base.OutputSchema)
	if err != nil {
		return planner.AnswerEnvelope{}, err
	}

	// Best-effort memory writeback on a goal-satisfying finish (mirrors
	// the drivers): a writeback error does not downgrade the answer.
	if s.Memory != nil && fin.Reason == planner.FinishGoal {
		sessionQ := identity.Quadruple{Identity: id}
		if wErr := s.Memory.AddTurn(runCtx, sessionQ, memory.ConversationTurn{
			UserMessage:       goal,
			AssistantResponse: env.Answer,
			Timestamp:         time.Now(),
		}); wErr != nil {
			logger.Warn("assemble: RunOnce memory writeback failed; answer still returned",
				"run_id", runID, "err", wErr.Error())
		}
	}

	return env, nil
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
