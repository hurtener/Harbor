// projections.go — the serve-band composition of the runtime-owned
// durable projections (HA-64 conversation turns, HA-65 observability
// rollups) and their READ-ONLY adapters.
//
// The projection stores are opened HERE (the single serve-band wiring
// shared by production `serve.Boot` and the devstack kit), over the
// operator's config blocks (`sessions.turns`, `observability.rollups`),
// with inmem / SQLite / Postgres parity. Each projection runs its own
// cancelable Run loop (wake-driven with a bounded lost-wake poll) whose
// closer joins the serve closer chain, and each exposes the narrow
// read seams the Protocol services consume:
//
//   - the turns projector satisfies turns/protocol.Projector (the
//     `sessions.turns.list` / `get` reads),
//   - the materializer's TaskSnapshotReader adapter is the runtime's
//     read-only window onto the ALREADY-REDACTED canonical task records
//     (the answer envelope, the query, the effective agent, and
//     metadata-only input/output attachment refs — never artifact
//     bytes, never a Protocol query fallback),
//   - the rollups worker satisfies observability/protocol's
//     QualitySource (the store satisfies its Querier) for
//     `observability.query`,
//   - both stores satisfy sessions.ProjectionFencer so the session
//     erasure cascade erases AND permanently fences their rows.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/drivers/postgres"
	"github.com/hurtener/Harbor/internal/observability/rollups/drivers/sqlite"
	rollupsmem "github.com/hurtener/Harbor/internal/observability/rollups/memstore"
	"github.com/hurtener/Harbor/internal/observability/rollups/projectorworker"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/inmem"
	turnspg "github.com/hurtener/Harbor/internal/sessions/turns/drivers/postgres"
	turnssqlite "github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/sessions/turns/materializer"
	"github.com/hurtener/Harbor/internal/tasks"
)

// openTurnsProjection opens the HA-64 conversation-turn projection
// store over cfg.Sessions.Turns, builds the Projector + Materializer,
// starts the materializer's wake-driven Run loop (with the bounded
// lost-wake poll so a completed task whose answer record converges
// after its terminal event seals without a new canonical event and
// after restart), and registers a closer that stops the loop and
// closes the store. A nil/empty driver config returns (nil, nil, nil)
// — the projection is unwired and the turn routes stay at 501.
//
// The materializer is wired with the runtime's durable erasure probe
// (the sessions Registry's exported Erased seam) and the
// TaskSnapshotReader adapter over the canonical task records, so an
// erased session is never re-materialized from sequence zero merely
// because an in-memory store restarted.
func OpenTurnsProjection(ctx context.Context, cfg *config.Config, deps TurnsProjectionDeps) (proj *turns.Projector, svc *turnsService, closer func(context.Context) error, err error) {
	t := cfg.Sessions.Turns
	if t.Driver == "" {
		return nil, nil, nil, nil
	}
	store, err := openTurnsStore(t)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sessions.turns: %w", err)
	}
	closers := []func(context.Context) error{store.Close}

	projector, pErr := turns.New(store,
		turns.WithErasureProbe(turnsErasureProbe(deps.Sessions)),
	)
	if pErr != nil {
		closeProjectionClosers(closers)
		return nil, nil, nil, fmt.Errorf("sessions.turns projector: %w", pErr)
	}

	src, sErr := events.OpenProjectionSource(deps.Bus)
	if sErr != nil {
		closeProjectionClosers(closers)
		return nil, nil, nil, fmt.Errorf("sessions.turns projection source: %w", sErr)
	}

	mat, mErr := materializer.New(src, projector,
		materializer.WithErasureProbe(turnsErasureProbe(deps.Sessions)),
		// The bounded lost-wake / pending-snapshot poll: a completed
		// task whose answer record converges after its terminal event
		// (or after a restart) seals without a new canonical event.
		materializer.WithPollInterval(turnsPollInterval),
		materializer.WithTaskSnapshotReader(&taskSnapshotAdapter{
			reg:   deps.Tasks,
			arts:  deps.Artifacts,
			clock: time.Now,
		}),
	)
	if mErr != nil {
		closeProjectionClosers(closers)
		return nil, nil, nil, fmt.Errorf("sessions.turns materializer: %w", mErr)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if rErr := mat.Run(runCtx); rErr != nil && !errors.Is(rErr, context.Canceled) {
			deps.Logger.ErrorContext(runCtx, "turns materializer exited",
				slog.Any("error", rErr))
		}
	}()
	closers = append(closers, func(context.Context) error {
		cancel()
		<-done
		return nil
	})

	svc = &turnsService{projector: projector, store: store}
	return projector, svc, func(cctx context.Context) error {
		var errs []error
		for i := len(closers) - 1; i >= 0; i-- {
			if cErr := closers[i](cctx); cErr != nil {
				errs = append(errs, cErr)
			}
		}
		return errors.Join(errs...)
	}, nil
}

// turnsPollInterval is the bounded lost-wake / pending-snapshot poll
// cadence of the turns materializer. It is deliberately small enough
// that a completed task whose answer record converges shortly after
// its terminal event seals promptly, and large enough that an idle
// projection does not busy-loop.
const turnsPollInterval = 2 * time.Second

// turnsErasureProbe adapts the sessions Registry's exported Erased
// seam to the turns ErasureProbe contract: the runtime's DURABLE
// erasure authority (pending ledger + terminal tombstone) consulted
// during restart reconciliation. A nil registry (headless read-only
// stack) leaves the probe nil — the honest availability gap the
// projection documents.
func turnsErasureProbe(sessions sessionEraser) turns.ErasureProbe {
	if sessions == nil {
		return nil
	}
	return erasureProbeFunc(func(ctx context.Context, id identity.Identity) (bool, error) {
		return sessions.Erased(ctx, id)
	})
}

// erasureProbeFunc adapts a plain function to the turns.ErasureProbe
// interface (the interface has no func adapter of its own).
type erasureProbeFunc func(ctx context.Context, id identity.Identity) (bool, error)

func (f erasureProbeFunc) Erased(ctx context.Context, id identity.Identity) (bool, error) {
	return f(ctx, id)
}

// sessionEraser is the narrow read-only erasure seam the projection
// probes need. *sessions.Registry satisfies it.
type sessionEraser interface {
	Erased(ctx context.Context, id identity.Identity) (bool, error)
}

// turnsService bundles the constructed turns projection surfaces the
// mux consumes: the projector (the Protocol read seam) and the store
// (the erasure-cascade fencer).
type turnsService struct {
	projector *turns.Projector
	store     turns.Store
}

// Store returns the turns projection store (the erasure-cascade
// fencer). Exported for the devstack kit's mirrored wiring.
func (s *turnsService) Store() turns.Store { return s.store }

// TurnsProjectionDeps bundles the runtime collaborators OpenTurnsProjection
// threads into the materializer + snapshot adapter.
type TurnsProjectionDeps struct {
	Bus       events.EventBus
	Sessions  sessionEraser
	Tasks     tasks.TaskRegistry
	Artifacts artifacts.ArtifactStore
	Logger    *slog.Logger
}

// openTurnsStore dispatches the turns projection store by the
// operator's configured driver name (inmem / sqlite / postgres).
func openTurnsStore(t config.TurnsConfig) (turns.Store, error) {
	switch t.Driver {
	case "", "inmem":
		return inmem.New()
	case "sqlite":
		if t.DSN == "" {
			return nil, errors.New("sqlite driver requires sessions.turns.dsn (validated upstream — sanity check)")
		}
		return turnssqlite.New(turnssqlite.Config{DSN: t.DSN, Retention: t.Retention})
	case "postgres":
		if t.DSN == "" {
			return nil, errors.New("postgres driver requires sessions.turns.dsn (validated upstream — sanity check)")
		}
		return turnspg.New(turnspg.Config{DSN: t.DSN, Retention: t.Retention})
	default:
		return nil, fmt.Errorf("unknown sessions.turns.driver %q (known: inmem, sqlite, postgres)", t.Driver)
	}
}

// taskSnapshotAdapter implements materializer.TaskSnapshotReader over
// the runtime's canonical task registry + artifact store — the
// runtime's ONLY window from the projection onto the already-redacted
// task records.
//
// Contract (see materializer.TaskSnapshotReader):
//
//   - READ-ONLY: Get never mutates the task record.
//   - ALREADY-REDACTED: every string comes from the persisted task
//     record (the runtime redacts Query / Description / Result.Value /
//     Error.Message before persistence); this adapter re-redacts
//     nothing.
//   - BOUNDED: the turns projector validates every bound loudly.
//   - METADATA ONLY: attachment refs resolve through
//     ArtifactStore.GetRef — never artifact bytes, never a Protocol
//     query fallback.
//
// Binding: Get runs under the exact event identity (identity.With),
// the returned Task.ID must byte-match the requested taskID, and the
// snapshot binds that exact Task.ID + Task.Identity.RunID.
type taskSnapshotAdapter struct {
	reg   tasks.TaskRegistry
	arts  artifacts.ArtifactStore
	clock func() time.Time
}

var _ materializer.TaskSnapshotReader = (*taskSnapshotAdapter)(nil)

// Task implements materializer.TaskSnapshotReader.
func (a *taskSnapshotAdapter) Task(ctx context.Context, id identity.Identity, taskID string) (materializer.TaskSnapshot, error) {
	if a.reg == nil {
		return materializer.TaskSnapshot{}, nil
	}
	taskCtx, err := identity.With(ctx, id)
	if err != nil {
		return materializer.TaskSnapshot{}, fmt.Errorf("turns task snapshot: identity ctx: %w", err)
	}
	t, err := a.reg.Get(taskCtx, tasks.TaskID(taskID))
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return materializer.TaskSnapshot{}, fmt.Errorf("%w: task %q under %s/%s/%s", materializer.ErrTaskSnapshotNotFound, taskID, id.TenantID, id.UserID, id.SessionID)
		}
		return materializer.TaskSnapshot{}, err
	}
	// The binding: the returned Task.ID MUST byte-match the requested
	// task id — a corrupt registry record must never cross-route
	// content across tasks.
	if string(t.ID) != taskID {
		return materializer.TaskSnapshot{}, fmt.Errorf("%w: requested %s, record reports %s", materializer.ErrSnapshotTaskIDMismatch, taskID, t.ID)
	}

	snap := materializer.TaskSnapshot{
		TaskID:    string(t.ID),
		RunID:     t.Identity.RunID,
		QueryAt:   unixNanoTime(t.CreatedAt),
		AgentID:   t.AgentID,
		AgentName: t.AgentID,
	}
	if t.Query != "" {
		snap.QueryPresent = true
		snap.Query = t.Query
	}
	if strings.TrimSpace(t.AgentID) != "" {
		snap.AgentPresent = true
		snap.AgentBindingSource = turns.AgentBindingExplicit
	} else {
		snap.AgentBindingSource = turns.AgentBindingUnknown
	}

	// Input attachment METADATA only — resolved through
	// ArtifactStore.GetRef (never bytes, never a Protocol fallback).
	if len(t.InputArtifactIDs) > 0 {
		snap.InputsPresent = true
		snap.Inputs = make([]turns.Attachment, 0, len(t.InputArtifactIDs))
		for _, artifactID := range t.InputArtifactIDs {
			att := turns.Attachment{
				ID:           artifactID,
				Disposition:  t.InputArtifactDispositions[artifactID],
				Availability: turns.CompletenessUnavailable,
			}
			if a.arts != nil {
				scope := artifacts.ArtifactScope{
					TenantID:  id.TenantID,
					UserID:    id.UserID,
					SessionID: id.SessionID,
				}
				if ref, found, rErr := a.arts.GetRef(ctx, scope, artifactID); rErr == nil && found && ref != nil {
					att.Filename = ref.Filename
					att.MimeType = ref.MimeType
					att.SizeBytes = ref.SizeBytes
					att.SHA256 = ref.SHA256
					att.Availability = turns.CompletenessComplete
				} else if rErr != nil {
					// A metadata-read failure leaves the ref honestly
					// unavailable — it never fails the projection
					// (attachment metadata is best-effort).
					att.Availability = turns.CompletenessUnavailable
				}
			}
			snap.Inputs = append(snap.Inputs, att)
		}
	}

	// The closed persisted answer envelope: decode ONLY the Harbor
	// answer envelope (planner.AnswerEnvelope) out of TaskResult.Value.
	// A malformed Value is NOT fabricated into a claim — the answer
	// component stays honestly unavailable.
	if t.Result != nil && len(t.Result.Value) > 0 {
		var env planner.AnswerEnvelope
		if json.Unmarshal(t.Result.Value, &env) == nil {
			snap.AnswerPresent = true
			snap.Answer = turns.Answer{
				State:    turns.AnswerStateInline,
				Inline:   env.Answer,
				Complete: turns.CompletenessComplete,
			}
		}
	}

	// The terminal failure classification, mapped exactly: the task's
	// Error.Code / Error.Message are already redacted; the FAILED seal
	// derives the closed error class from the code.
	if t.Error != nil {
		snap.FailurePresent = true
		snap.ErrorCode = t.Error.Code
		snap.ErrorMessage = t.Error.Message
	}

	// Output attachment metadata: the task record declares no output
	// artifact ids, so the honest gap (OutputsPresent=false) stands.
	return snap, nil
}

// unixNanoTime converts a unix-nanosecond stamp to time.Time (zero
// for a zero stamp).
func unixNanoTime(ts int64) time.Time {
	if ts == 0 {
		return time.Time{}
	}
	return time.Unix(0, ts).UTC()
}

// openRollupsProjection opens the HA-65 observability rollup store
// over cfg.Observability.Rollups, builds the projector worker, starts
// its wake-driven Run loop, and registers a closer. A nil/empty
// driver config returns (nil, nil, nil) — the rollup surface stays at
// 501.
func OpenRollupsProjection(ctx context.Context, cfg *config.Config, deps RollupsProjectionDeps) (store rollups.Store, worker *rollupWorker, closer func(context.Context) error, err error) {
	r := cfg.Observability.Rollups
	if r.Driver == "" {
		return nil, nil, nil, nil
	}
	store, err = openRollupsStore(r)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("observability.rollups: %w", err)
	}
	closers := []func(context.Context) error{store.Close}

	src, sErr := events.OpenProjectionSource(deps.Bus)
	if sErr != nil {
		closeProjectionClosers(closers)
		return nil, nil, nil, fmt.Errorf("observability.rollups projection source: %w", sErr)
	}

	wk, wErr := projectorworker.New(src, store)
	if wErr != nil {
		closeProjectionClosers(closers)
		return nil, nil, nil, fmt.Errorf("observability.rollups projector worker: %w", wErr)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if rErr := wk.Run(runCtx); rErr != nil && !errors.Is(rErr, context.Canceled) {
			deps.Logger.ErrorContext(runCtx, "rollups projector exited",
				slog.Any("error", rErr))
		}
	}()
	closers = append(closers, func(context.Context) error {
		cancel()
		<-done
		return nil
	})

	return store, &rollupWorker{wk: wk}, func(cctx context.Context) error {
		var errs []error
		for i := len(closers) - 1; i >= 0; i-- {
			if cErr := closers[i](cctx); cErr != nil {
				errs = append(errs, cErr)
			}
		}
		return errors.Join(errs...)
	}, nil
}

// openRollupsStore dispatches the rollup store by the operator's
// configured driver name (inmem / sqlite / postgres).
func openRollupsStore(r config.RollupsConfig) (rollups.Store, error) {
	switch r.Driver {
	case "", "inmem":
		return rollupsmem.New(), nil
	case "sqlite":
		if r.DSN == "" {
			return nil, errors.New("sqlite driver requires observability.rollups.dsn (validated upstream — sanity check)")
		}
		return sqlite.New(r.DSN)
	case "postgres":
		if r.DSN == "" {
			return nil, errors.New("postgres driver requires observability.rollups.dsn (validated upstream — sanity check)")
		}
		return postgres.New(postgres.Config{DSN: r.DSN})
	default:
		return nil, fmt.Errorf("unknown observability.rollups.driver %q (known: inmem, sqlite, postgres)", r.Driver)
	}
}

// rollupWorker wraps the rollup projector worker's Quality surface,
// adapting its embedded rollups.Quality to the observability protocol's
// QualitySource seam.
type rollupWorker struct {
	wk *projectorworker.Worker
}

func (w *rollupWorker) Quality(ctx context.Context) (rollups.Quality, error) {
	q, err := w.wk.Quality(ctx)
	return q.Quality, err
}

// RollupsProjectionDeps bundles the runtime collaborators
// OpenRollupsProjection threads into the worker.
type RollupsProjectionDeps struct {
	Bus    events.EventBus
	Logger *slog.Logger
}

// closeProjectionClosers drains a projection's partially-opened closer
// chain on a construction failure.
func closeProjectionClosers(closers []func(context.Context) error) {
	ctx := context.Background()
	for i := len(closers) - 1; i >= 0; i-- {
		//nolint:errcheck // best-effort drain of a failed projection construction
		_ = closers[i](ctx)
	}
}
