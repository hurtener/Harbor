// cmd/harbor/cmd_dev_hot_reload.go — `harbor dev`
// hot-reload watcher. fsnotify-driven graceful-drain restart when a
// watched file changes (Go source, harbor.yaml, project-local
// `.harbor/agents/` drafts).
//
// # Shape
//
// In-process devStack rebuild (NOT binary re-exec). The watcher fires
// on a file event, the supervisor calls `stack.close(ctx)` on the
// current devStack with bounded drain semantics, then `bootDevStack`
// is invoked again with the same opts — the listener re-binds, the
// canonical event bus exposes `dev.hot_reload.triggered` and
// `dev.hot_reload.completed` so wire consumers (Console, integration
// tests, third-party Protocol clients) observe the restart.
//
// Binary re-exec was considered and rejected for V1: it requires an
// out-of-process supervisor (the binary cannot re-exec itself without
// losing the current http.Server's connections), it costs a Go build
// per cycle (~5s on a warm machine; the developer feedback loop is
// the load-bearing UX here), and a developer iterating on a YAML
// config file does NOT need a binary rebuild. The §4.3 "smaller
// approach that still satisfies acceptance" carve-out applies: the
// acceptance criterion is "new code picked up" — at the dev-time
// granularity (operator edits a file, restart picks up the change),
// the in-process devStack rebuild satisfies it for every config /
// scaffold change.
//
// A Go-source (`.go`) change is a different case: the in-process
// devStack rebuild re-reads config from disk and re-wires the stack,
// but it does NOT recompile the binary — so the edited Go is never
// picked up. Driving a rebuild and reporting a successful hot-reload
// in that case is a loud false-success (the inverse of the §13
// no-silent-degradation rule: do not claim success for work that did
// not happen). The watcher therefore CLASSIFIES every event: config /
// YAML / scaffold changes take the in-process rebuild path, while a
// `.go` change takes a WARN-and-guide path that logs the change and
// tells the operator to run `make build` and restart `harbor dev` — it
// does NOT reboot and does NOT emit dev.hot_reload.completed. The
// recompile-and-re-exec policy (auto `go build` + re-exec on a `.go`
// change) is deferred. This is documented in the dev-loop recipe
// (docs/recipes/run-harbor-dev.md).
//
// # Concurrency contract
//
// One supervisor goroutine OWNS the active devStack. It receives
// events from one fsnotify watcher goroutine over a debounced channel
// and serialises stack rebuilds. The supervisor's lifetime is bounded
// by the boot ctx; on ctx-cancel the supervisor closes the active
// stack and returns. No two devStacks are ever live at the same time
// — the supervisor's serial discipline IS the concurrency invariant.
//
// # Identity and admin scope
//
// The `dev.hot_reload.*` bus events are emitted with the dev token's
// canonical identity triple (DevTenant / DevUser / DevSession) so
// admin-scoped subscribers (the integration test stack, the Console
// fleet view) see them without per-tenant filter contortion. A wire
// consumer that subscribes by triple matches the dev triple by
// construction.
//
// # Fail-loud at boot
//
// fsnotify.NewWatcher errors, a watcher.Add(path) error on a watched
// root (permissions, missing dir, EMFILE), or a rebuilt devStack that
// fails to boot — every error path returns up the call chain rather
// than silently degrading. The §13 amendment is binding: a hot-reload
// subsystem that "fails safe" by quietly disabling itself when its
// watched path is missing is exactly the silent-degradation shape the
// amendment closes.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/serve"
)

// Canonical event types the hot-reload supervisor emits on the bus.
// Registered with the canonical events registry via init() so a wire
// consumer subscribing to them is accepted.
const (
	// EventTypeDevHotReloadTriggered fires the moment the supervisor
	// observes a watched file change and decides to restart. Payload
	// is DevHotReloadTriggeredPayload.
	EventTypeDevHotReloadTriggered events.EventType = "dev.hot_reload.triggered"
	// EventTypeDevHotReloadCompleted fires the moment the rebuilt
	// devStack returns from bootDevStack. Payload is
	// DevHotReloadCompletedPayload.
	EventTypeDevHotReloadCompleted events.EventType = "dev.hot_reload.completed"
)

// DevHotReloadTriggeredPayload reports a fired hot-reload. Carries
// the canonical event path that triggered the rebuild and the
// configured retain-in-flight policy. SafePayload by construction —
// every field is internal bookkeeping.
type DevHotReloadTriggeredPayload struct {
	events.SafeSealed
	// Path is the file path the watcher observed change on. May be
	// the harbor.yaml file or a path inside one of the watch roots.
	Path string
	// Op is the fsnotify operation string (e.g. "WRITE", "CREATE").
	Op string
	// Policy is the configured retain-in-flight policy
	// (drain / cancel) applied to this restart.
	Policy string
}

// DevHotReloadCompletedPayload reports a finished restart cycle.
// Carries the duration the cycle took (close + reboot) and the
// success/failure shape. SafePayload by construction.
type DevHotReloadCompletedPayload struct {
	events.SafeSealed
	// Path is the file path the supervisor restarted in response to.
	Path string
	// Op is the fsnotify operation string from the triggering event.
	Op string
	// Policy is the configured retain-in-flight policy applied.
	Policy string
	// DurationMS is the elapsed wall-clock time of the close +
	// reboot cycle in milliseconds.
	DurationMS int64
	// Success reports whether the rebuilt devStack booted cleanly. A
	// failed restart cycle leaves the supervisor with no active
	// devStack — the boot ctx is cancelled and the runDev loop exits
	// with a non-zero CLIError. By design a fail-loud restart
	// is preferable to a stack-less binary lingering on the port.
	Success bool
	// ErrorMessage is the wrapped boot error string when Success is
	// false. Empty when Success is true.
	ErrorMessage string
}

func init() {
	events.RegisterEventType(EventTypeDevHotReloadTriggered)
	events.RegisterEventType(EventTypeDevHotReloadCompleted)
}

// debounceWindow is the minimum interval between rebuilds. A burst
// of fsnotify events (editors that save via rename-and-replace
// generate multiple events) collapses to one restart. 250ms is the
// standard window for IDE-driven saves; smaller windows lose events,
// larger windows feel sluggish to operators.
const debounceWindow = 250 * time.Millisecond

// hotReloadSupervisor owns the active devStack and the fsnotify
// watcher. Run blocks until ctx cancels OR a rebuild fails fatally.
// Construct via newHotReloadSupervisor; the supervisor is NOT
// reusable across boots (each `harbor dev` invocation gets one).
type hotReloadSupervisor struct {
	logger       *slog.Logger
	bootOpts     serve.Options
	cfg          config.DevHotReloadConfig
	watchRoots   []string
	policy       string
	drainTimeout time.Duration

	// onReboot, when non-nil, runs after each successful reboot with the
	// fresh stack — the dev caller re-prints its per-boot surfaces (the mock
	// banner, a re-minted dev token) so every boot announces its posture and
	// a long-lived dev session never outlives the printed token's expiry.
	onReboot func(*serve.Handle)

	mu    sync.Mutex
	stack *serve.Handle
}

// newHotReloadSupervisor validates the inputs and returns a fresh
// supervisor wrapped around the supplied initial stack. The
// supervisor takes ownership of the stack — callers MUST NOT call
// stack.close after handing it in; the supervisor's Run loop drains
// it on ctx-cancel or restart.
//
// resolveHotReloadConfig must have been called by the caller to seed
// watchRoots with the union of cfg.CLI.DevHotReload.WatchRoots and the
// loaded config file's directory.
func newHotReloadSupervisor(
	logger *slog.Logger,
	bootOpts serve.Options,
	initialStack *serve.Handle,
	cfg config.DevHotReloadConfig,
	watchRoots []string,
) (*hotReloadSupervisor, error) {
	if logger == nil {
		return nil, fmt.Errorf("hot-reload: logger is nil")
	}
	if initialStack == nil {
		return nil, fmt.Errorf("hot-reload: initial stack is nil")
	}
	if len(watchRoots) == 0 {
		return nil, fmt.Errorf("hot-reload: watch roots empty (validator should have caught this)")
	}
	policy := cfg.Policy
	if policy == "" {
		policy = config.DevHotReloadPolicyDrain
	}
	drainTimeout := cfg.DrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = 5 * time.Second
	}
	return &hotReloadSupervisor{
		logger:       logger,
		bootOpts:     bootOpts,
		cfg:          cfg,
		watchRoots:   watchRoots,
		policy:       policy,
		drainTimeout: drainTimeout,
		stack:        initialStack,
	}, nil
}

// Run installs the fsnotify watcher AND serves the active stack until
// ctx cancels OR a rebuild fails fatally. The supervisor owns both the
// fsnotify watcher and the serve loop: each rebuild stops the current
// serve, drains the stack per policy, reboots, then resumes serving on
// the new stack. The caller drains the supervisor's final stack via
// `CurrentStack()` after Run returns.
//
// A rebuild failure returns the wrapped boot error; the final
// `CurrentStack()` is whatever was last successfully booted (which may
// be nil only if the initial boot was the failing one — but that case
// is impossible here because the supervisor is constructed AFTER the
// initial boot).
func (s *hotReloadSupervisor) Run(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("hot-reload: fsnotify.NewWatcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	// Add every watch root. A missing / unreadable root is fail-loud
	// per §13 — the watcher cannot meaningfully serve a partial set of
	// roots, and an operator-typo'd path SHOULD surface immediately
	// rather than silently doing nothing.
	added := 0
	for _, root := range s.watchRoots {
		clean := filepath.Clean(root)
		info, statErr := os.Stat(clean)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				// A non-existent root is acceptable for the project-local
				// `.harbor/agents/` default (A later phase will create it on
				// first draft-save). Log Info and skip — the watcher serves
				// the roots that DO exist; the missing root is harmless
				// (no file will land there to trigger anything). We do NOT
				// fail-loud here because the loader's defaults() seeds the
				// `.harbor/agents` root for every config; new projects
				// would otherwise be unable to boot `harbor dev` without
				// pre-creating the directory.
				s.logger.Info("hot-reload: watch root does not exist (skipping)",
					slog.String("root", clean))
				continue
			}
			return fmt.Errorf("hot-reload: stat %q: %w", clean, statErr)
		}
		// Watch the directory itself (fsnotify watches files OR
		// directories; for directories it reports events on contained
		// entries). When the root is a file (e.g. the loaded harbor.yaml),
		// watch the file directly.
		_ = info
		if addErr := watcher.Add(clean); addErr != nil {
			return fmt.Errorf("hot-reload: watcher.Add(%q): %w", clean, addErr)
		}
		added++
	}
	if added == 0 {
		// Every configured root was missing. The supervisor still runs
		// — a future file appearing under a watched root would not be
		// observed (fsnotify can't watch a non-existent dir), but the
		// supervisor exits cleanly on ctx-cancel without blocking the
		// boot. Log Warn so the operator sees the no-op shape.
		//
		// First-clone vs §13 rationale (audit N4): the §13 fail-loud
		// principle would argue this should `return error`. We accept
		// the Warn-then-no-op because the default watch root is
		// `.harbor/agents`, which does NOT exist in a freshly-cloned
		// project. Failing-loud here would block `harbor dev` from
		// booting in a fresh checkout — a strictly worse UX than
		// silently running the supervisor as a no-op until the
		// operator creates an agent.
		s.logger.Warn("hot-reload: no watch roots exist; supervisor is a no-op until paths appear")
	}

	s.logger.Info("hot-reload: watcher started",
		slog.Int("roots", added),
		slog.String("policy", s.policy),
		slog.Duration("drain_timeout", s.drainTimeout),
	)

	// Start the initial serve goroutine. Each rebuild stops this
	// goroutine via the per-serve cancel, then re-spawns it against
	// the new stack. The serveErr channel surfaces fatal serve errors
	// (the http.Server failing to bind, an unrecoverable listen
	// error) — a clean shutdown via ctx-cancel writes nil.
	serveCtx, serveCancel := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.stack.Serve(serveCtx)
	}()

	// Debounce timer — collapses a burst of events into one rebuild.
	// The timer is started fresh on each fsnotify event; it fires
	// after debounceWindow of quiet.
	var (
		debounce      *time.Timer
		lastPath      string
		lastOp        string
		lastGoWarn    time.Time
		debounceFired = make(chan struct{}, 1)
	)
	cleanup := func() {
		if debounce != nil {
			debounce.Stop()
		}
		serveCancel()
		// Drain serveErr so the goroutine doesn't leak.
		<-serveErr
	}

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("hot-reload: supervisor stopping (ctx cancelled)")
			cleanup()
			return nil
		case err := <-serveErr:
			// The active stack's serve loop returned. On a clean
			// shutdown (ctx cancelled at the http level) err is nil;
			// on a fatal listen failure it carries the error. Either
			// way the supervisor exits — the http listener IS the
			// runtime's reason for existing.
			if debounce != nil {
				debounce.Stop()
			}
			serveCancel()
			if err != nil {
				return fmt.Errorf("hot-reload: serve exited: %w", err)
			}
			return nil
		case ev, ok := <-watcher.Events:
			if !ok {
				s.logger.Info("hot-reload: watcher channel closed")
				cleanup()
				return nil
			}
			switch classifyEvent(ev) {
			case reloadGoSourceWarn:
				// A `.go` change cannot be hot-reloaded — the in-process
				// rebuild never recompiles the binary. WARN and guide the
				// operator to rebuild manually; do NOT reboot and do NOT
				// emit dev.hot_reload.completed (that would be a loud
				// false-success). Throttle to one WARN per debounce window
				// so an editor's rename-and-replace save burst collapses to
				// a single line.
				if time.Since(lastGoWarn) >= debounceWindow {
					s.warnGoSourceChange(ev.Name, ev.Op.String())
					lastGoWarn = time.Now()
				}
				continue
			case reloadRebuild:
				lastPath = ev.Name
				lastOp = ev.Op.String()
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(debounceWindow, func() {
					select {
					case debounceFired <- struct{}{}:
					default:
					}
				})
			default: // reloadIgnore — Chmod-only, non-content op, db sidecar.
				continue
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				s.logger.Info("hot-reload: watcher errors channel closed")
				cleanup()
				return nil
			}
			s.logger.Warn("hot-reload: fsnotify error",
				slog.String("err", err.Error()))
		case <-debounceFired:
			newServeCtx, newServeCancel, rebuildErr := s.handleRebuildAndRestartServe(
				ctx, serveCancel, serveErr, lastPath, lastOp)
			if rebuildErr != nil {
				return rebuildErr
			}
			serveCtx = newServeCtx
			serveCancel = newServeCancel
			// serveErr is reused — handleRebuildAndRestartServe
			// re-spawns the goroutine against the new stack.
		}
	}
}

// CurrentStack returns the supervisor's current active stack. Used by
// runDev to drain the stack on ctx-cancel.
func (s *hotReloadSupervisor) CurrentStack() *serve.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stack
}

// handleRebuildAndRestartServe fires one restart cycle: emits
// triggered on the old stack's bus, stops the active serve goroutine,
// drains the old stack per policy, reboots, swaps in the new stack,
// emits completed on the new bus, and starts a fresh serve goroutine
// against the new stack. Returns the NEW serve ctx + cancel so the
// caller's loop can keep observing the serve goroutine. A boot
// failure during reboot returns the wrapped error — the supervisor
// exits.
func (s *hotReloadSupervisor) handleRebuildAndRestartServe(
	ctx context.Context,
	oldServeCancel context.CancelFunc,
	serveErr chan error,
	path, op string,
) (context.Context, context.CancelFunc, error) {
	s.logger.Info("hot-reload: file change observed",
		slog.String("path", path),
		slog.String("op", op),
		slog.String("policy", s.policy),
	)

	s.mu.Lock()
	current := s.stack
	s.mu.Unlock()

	start := time.Now()

	// Emit triggered BEFORE the close so a wire subscriber sees the
	// canonical "we are about to restart" beat before the bus closes.
	s.emitTriggered(ctx, current, path, op)

	// Stop the active serve goroutine: cancelling oldServeCtx ends the
	// http.Server's serve loop via its BaseContext, which causes
	// http.Server.Shutdown to flush in-flight requests within the
	// stack.serve grace window. Then drain serveErr so the goroutine
	// doesn't leak.
	oldServeCancel()
	<-serveErr

	// Now drain the stack itself per policy (this is the runtime-side
	// drain — RunLoops, tool catalogs, etc.).
	drainCtx, drainCancel := s.drainContext(ctx)
	current.Close(drainCtx)
	drainCancel()

	// Rebuild. The bootDevStack opts are unchanged across rebuilds —
	// the supervisor is a "re-read everything from disk and rebuild"
	// mechanism. Operators who changed harbor.yaml see the new values
	// because bootDevStack re-runs config.Load.
	newStack, err := serve.Boot(ctx, s.bootOpts)
	if err != nil {
		s.logger.Error("hot-reload: reboot failed",
			slog.String("err", err.Error()),
			slog.Duration("elapsed", time.Since(start)),
		)
		// We can't emit completed{Success=false} because the old bus is
		// closed and the new bus does not exist. The error propagates
		// up via the supervisor's return; the operator sees the CLIError.
		return nil, nil, fmt.Errorf("hot-reload reboot: %w", err)
	}

	s.mu.Lock()
	s.stack = newStack
	s.mu.Unlock()

	// Re-announce the per-boot dev surfaces (mock banner + a re-minted dev
	// token) on the fresh stack — every boot prints its posture, and a
	// long-lived dev session gets a token whose expiry restarts per reboot.
	if s.onReboot != nil {
		s.onReboot(newStack)
	}

	// Start the fresh serve goroutine. Spawn BEFORE emitting completed
	// so a wire subscriber that re-subscribes immediately upon
	// observing completed has a live server to connect to.
	newServeCtx, newServeCancel := context.WithCancel(ctx)
	go func() {
		serveErr <- newStack.Serve(newServeCtx)
	}()

	// Emit completed on the NEW stack's bus.
	s.emitCompleted(ctx, newStack, path, op, time.Since(start), true, "")

	s.logger.Info("hot-reload: reboot complete",
		slog.Duration("elapsed", time.Since(start)),
	)
	return newServeCtx, newServeCancel, nil
}

// drainContext builds the ctx the active stack's close runs under per
// policy. The cancel must be invoked by the caller after close
// returns; the supervisor owns the drainCtx lifecycle.
func (s *hotReloadSupervisor) drainContext(parent context.Context) (context.Context, context.CancelFunc) {
	switch s.policy {
	case config.DevHotReloadPolicyCancel:
		// Cancel: no drain — the close runs against an already-cancelled
		// ctx so in-flight RunLoops observe ctx.Err() immediately. The
		// per-task driver's Close drains the WaitGroup as a courtesy,
		// but every Run call's ctx is the supervisor's, which is now
		// cancelled.
		drainCtx, cancel := context.WithCancel(parent)
		cancel()
		return drainCtx, func() {}
	default:
		// Drain: bounded by drainTimeout. The close runs until either
		// every RunLoop drains OR the timeout fires. The per-task
		// driver's Close blocks on its WaitGroup; the bounded ctx is
		// the http.Server's ShutdownGracePeriod analogue for the
		// runtime-side drain.
		return context.WithTimeout(parent, s.drainTimeout)
	}
}

// emitTriggered publishes EventTypeDevHotReloadTriggered on the
// supplied stack's bus. A nil bus or an unwired stack is a no-op (we
// log Debug and continue — observability is best-effort, not
// correctness).
func (s *hotReloadSupervisor) emitTriggered(ctx context.Context, stack *serve.Handle, path, op string) {
	if stack == nil || stack.Bus == nil {
		return
	}
	id := identity.Identity{
		TenantID:  DevTenant,
		UserID:    DevUser,
		SessionID: DevSession,
	}
	if err := stack.Bus.Publish(ctx, events.Event{
		Type:     EventTypeDevHotReloadTriggered,
		Identity: identity.Quadruple{Identity: id},
		Payload: DevHotReloadTriggeredPayload{
			Path:   path,
			Op:     op,
			Policy: s.policy,
		},
	}); err != nil {
		// Observability is best-effort, not correctness — log and continue.
		s.logger.Debug("hot-reload: failed to publish triggered event",
			slog.String("error", err.Error()))
	}
}

// emitCompleted publishes EventTypeDevHotReloadCompleted on the
// supplied stack's bus.
func (s *hotReloadSupervisor) emitCompleted(
	ctx context.Context,
	stack *serve.Handle,
	path, op string,
	elapsed time.Duration,
	success bool,
	errMsg string,
) {
	if stack == nil || stack.Bus == nil {
		return
	}
	id := identity.Identity{
		TenantID:  DevTenant,
		UserID:    DevUser,
		SessionID: DevSession,
	}
	if err := stack.Bus.Publish(ctx, events.Event{
		Type:     EventTypeDevHotReloadCompleted,
		Identity: identity.Quadruple{Identity: id},
		Payload: DevHotReloadCompletedPayload{
			Path:         path,
			Op:           op,
			Policy:       s.policy,
			DurationMS:   elapsed.Milliseconds(),
			Success:      success,
			ErrorMessage: errMsg,
		},
	}); err != nil {
		// Observability is best-effort, not correctness — log and continue.
		s.logger.Debug("hot-reload: failed to publish completed event",
			slog.String("error", err.Error()))
	}
}

// reloadClass categorises a watched-file event into the action the
// supervisor takes for it.
type reloadClass int

const (
	// reloadIgnore — the event warrants no action: a Chmod-only event, a
	// non-content op, or a database engine sidecar rewrite.
	reloadIgnore reloadClass = iota
	// reloadRebuild — a config / YAML / scaffold change. The supervisor
	// performs an in-process devStack rebuild (re-read config, drain,
	// reboot, re-bind). This picks up the change because nothing needed
	// recompiling.
	reloadRebuild
	// reloadGoSourceWarn — a Go-source (`.go`) change. An in-process
	// rebuild would NOT recompile the binary, so the edit would never be
	// picked up and a "completed" event would be a false success. The
	// supervisor logs a WARN that guides a manual `make build` + restart
	// and does not reboot.
	reloadGoSourceWarn
)

// classifyEvent maps an fsnotify event to the supervisor's action.
//
// We act on Write, Create, Rename, Remove — the common editor-save
// shapes. Chmod-only events (a `touch` or permission change) are
// ignored — they don't reflect content changes.
//
// Hidden files (those starting with `.`) are NOT ignored: an operator
// might be editing `.harbor/agents/foo.yaml` and that path's leading
// `.` is structural. Editor-temp-file conventions vary across editors
// (vim's `.swp`, emacs's `#`, JetBrains's `___jb_tmp___`); filtering
// by name pattern would create vendor drift. We accept the small cost
// of an occasional spurious restart on swap files — the debounce
// window collapses bursts anyway.
//
// Database engine sidecar files are ignored: SQLite's WAL/SHM/journal
// companions get rewritten on every commit, which under the default
// `sqlite` state + skills drivers means a reboot loop the moment the
// planner writes anything. The skip list is fixed; an operator with an
// exotic db backend can submit a PR.
//
// Go-source (`.go`) changes route to reloadGoSourceWarn, not
// reloadRebuild: the in-process rebuild cannot recompile the binary, so
// rebooting on a `.go` edit re-reads config without picking up the code
// change while reporting a successful hot-reload — a loud false-success.
// The honest action is a WARN that guides a manual rebuild.
func classifyEvent(ev fsnotify.Event) reloadClass {
	if ev.Op == fsnotify.Chmod {
		return reloadIgnore
	}
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
		return reloadIgnore
	}
	if isDBSidecar(ev.Name) {
		return reloadIgnore
	}
	if isGoSource(ev.Name) {
		return reloadGoSourceWarn
	}
	return reloadRebuild
}

// isGoSource reports whether the path is a Go source file. A `.go`
// change cannot be hot-reloaded by the in-process devStack rebuild
// (which never recompiles the binary), so it routes to the
// WARN-and-guide path rather than a reboot.
func isGoSource(path string) bool {
	return strings.HasSuffix(filepath.Base(path), ".go")
}

// shouldTrigger reports whether an fsnotify event warrants an
// in-process devStack rebuild. Only config / YAML / scaffold changes
// (reloadRebuild) qualify; `.go` changes and ignorable events do not.
// Retained as the rebuild-gate predicate the supervisor's tests pin.
func shouldTrigger(ev fsnotify.Event) bool {
	return classifyEvent(ev) == reloadRebuild
}

// warnGoSourceChange logs the honest no-recompile guidance for a Go
// source change. It does NOT reboot the devStack and does NOT emit
// dev.hot_reload.completed — an in-process rebuild cannot recompile the
// binary, so the edit would not be picked up. Reporting success would
// be a false success; the operator must rebuild + restart manually.
func (s *hotReloadSupervisor) warnGoSourceChange(path, op string) {
	s.logger.Warn(
		"hot-reload: Go source change detected — harbor dev does not recompile Go; "+
			"run `make build` and restart `harbor dev` to pick it up "+
			"(config / YAML changes hot-reload in-process; .go changes do not)",
		slog.String("path", path),
		slog.String("op", op),
	)
}

// dbSidecarSuffixes lists filename suffixes for engine artifacts the
// hot-reload watcher skips — these files get rewritten on every commit
// by the running binary, so triggering a reload on them creates a
// feedback loop that reboots the binary indefinitely. The suffixes
// cover SQLite (WAL/SHM/journal) and the rollback-journal forms other
// lightweight engines use.
//
// The MAIN `.sqlite` / `.db` files are skipped for the same reason:
// SQLite rewrites the main file on every commit too (transient metadata
// changes fire fsnotify Write events even when the operator did not
// touch the file). Skipping only the sidecars would leave a slower
// reboot loop on every persisted state change, so a SQLite-managed file
// never triggers hot-reload regardless of which file in the bundle is
// being rewritten.
var dbSidecarSuffixes = []string{
	".sqlite-wal",
	".sqlite-shm",
	".sqlite-journal",
	".db-wal",
	".db-shm",
	".db-journal",
	"-journal",
	".sqlite",
	".db",
}

func isDBSidecar(path string) bool {
	base := filepath.Base(path)
	for _, suf := range dbSidecarSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return false
}

// resolveHotReloadWatchRoots unions the operator-declared watch roots
// with the loaded config file's directory so a config edit also
// triggers a reload. The returned slice contains DEDUPLICATED clean
// paths.
func resolveHotReloadWatchRoots(cfg config.DevHotReloadConfig, cfgPath string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		clean := filepath.Clean(p)
		if _, dup := seen[clean]; dup {
			return
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	for _, root := range cfg.WatchRoots {
		add(root)
	}
	if cfgPath != "" {
		// Watch the config file's parent dir so a YAML edit triggers
		// a reload. Watching the file directly would miss
		// editor-rename-and-replace saves (the new inode would be
		// unwatched). Watching the parent dir catches both shapes.
		add(filepath.Dir(cfgPath))
	}
	return out
}
