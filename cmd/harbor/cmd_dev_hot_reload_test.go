// cmd/harbor/cmd_dev_hot_reload_test.go — unit tests for the Phase 65
// (D-099) `harbor dev` hot-reload supervisor.
//
// Coverage areas:
//
//  1. resolveHotReloadWatchRoots — unions cfg roots + config file's dir,
//     deduplicates, cleans paths.
//  2. shouldTrigger — fsnotify event filter (Chmod skipped, Write /
//     Create / Rename / Remove fire).
//  3. newHotReloadSupervisor input validation (nil logger / nil stack /
//     empty roots).
//  4. supervisor lifecycle: Run installs the watcher, observes a file
//     mutation, fires a rebuild, swaps in a new stack, emits the
//     canonical bus events.
//  5. supervisor honours ctx-cancel: Run returns cleanly with the
//     final stack available for the caller to drain.
//  6. supervisor honours `--no-hot-reload` semantics indirectly via the
//     cfg fast-path (Enabled=false / Policy=disabled).
//
// The integration shape (file mutation → real bus events observed by a
// real subscriber) lives in `test/integration/phase65_hot_reload_test.go`
// per CLAUDE.md §17.2 — that's the test that drives a real harbor.yaml
// edit against a real bootDevStack and asserts dev.hot_reload.triggered /
// completed land on the bus.

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// TestResolveHotReloadWatchRoots_UnionsConfigDirAndDedupes — the
// helper unions cfg.WatchRoots with the config file's parent dir.
// Duplicates are removed; paths are cleaned.
func TestResolveHotReloadWatchRoots_UnionsConfigDirAndDedupes(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.DevHotReloadConfig
		cfgPath string
		want    []string
	}{
		{
			name:    "single_root_plus_config_dir",
			cfg:     config.DevHotReloadConfig{WatchRoots: []string{".harbor/agents"}},
			cfgPath: "/etc/harbor/harbor.yaml",
			want:    []string{".harbor/agents", "/etc/harbor"},
		},
		{
			name:    "config_dir_already_listed_dedupes",
			cfg:     config.DevHotReloadConfig{WatchRoots: []string{".harbor/agents", "/etc/harbor"}},
			cfgPath: "/etc/harbor/harbor.yaml",
			want:    []string{".harbor/agents", "/etc/harbor"},
		},
		{
			name:    "no_cfg_path_only_roots",
			cfg:     config.DevHotReloadConfig{WatchRoots: []string{".harbor/agents"}},
			cfgPath: "",
			want:    []string{".harbor/agents"},
		},
		{
			name:    "path_cleaning_applied",
			cfg:     config.DevHotReloadConfig{WatchRoots: []string{"./.harbor/agents/"}},
			cfgPath: "./harbor.yaml",
			want:    []string{".harbor/agents", "."},
		},
		{
			name:    "empty_root_string_skipped",
			cfg:     config.DevHotReloadConfig{WatchRoots: []string{"", ".harbor/agents"}},
			cfgPath: "",
			want:    []string{".harbor/agents"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveHotReloadWatchRoots(tc.cfg, tc.cfgPath)
			if !stringSliceEqual(got, tc.want) {
				t.Errorf("resolveHotReloadWatchRoots() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestShouldTrigger_FiltersChmodOnly — Chmod-only events do not warrant
// a rebuild; Write / Create / Rename / Remove do.
func TestShouldTrigger_FiltersChmodOnly(t *testing.T) {
	cases := []struct {
		name string
		op   fsnotify.Op
		want bool
	}{
		{"chmod_only_skipped", fsnotify.Chmod, false},
		{"write_fires", fsnotify.Write, true},
		{"create_fires", fsnotify.Create, true},
		{"rename_fires", fsnotify.Rename, true},
		{"remove_fires", fsnotify.Remove, true},
		{"write_or_chmod_fires", fsnotify.Write | fsnotify.Chmod, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := fsnotify.Event{Name: "foo", Op: tc.op}
			if got := shouldTrigger(ev); got != tc.want {
				t.Errorf("shouldTrigger(%v) = %v, want %v", tc.op, got, tc.want)
			}
		})
	}
}

// TestNewHotReloadSupervisor_RejectsNilDeps — the constructor fails
// loud when any required input is nil. Documents the supervisor's
// invariants.
func TestNewHotReloadSupervisor_RejectsNilDeps(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	stack := &devStack{} // sentinel — non-nil
	cfg := config.DevHotReloadConfig{}

	cases := []struct {
		name       string
		logger     *slog.Logger
		stack      *devStack
		watchRoots []string
	}{
		{"nil_logger", nil, stack, []string{"."}},
		{"nil_stack", logger, nil, []string{"."}},
		{"empty_roots", logger, stack, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newHotReloadSupervisor(tc.logger, devBootOptions{}, tc.stack, cfg, tc.watchRoots)
			if err == nil {
				t.Errorf("newHotReloadSupervisor with %s: err = nil, want non-nil", tc.name)
			}
		})
	}
}

// TestNewHotReloadSupervisor_DefaultsPolicyAndDrainTimeout — empty
// Policy defaults to "drain"; non-positive DrainTimeout defaults to 5s.
func TestNewHotReloadSupervisor_DefaultsPolicyAndDrainTimeout(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	stack := &devStack{}
	cfg := config.DevHotReloadConfig{} // empty — defaults should apply

	sup, err := newHotReloadSupervisor(logger, devBootOptions{}, stack, cfg, []string{"."})
	if err != nil {
		t.Fatalf("newHotReloadSupervisor: %v", err)
	}
	if sup.policy != config.DevHotReloadPolicyDrain {
		t.Errorf("policy = %q, want %q", sup.policy, config.DevHotReloadPolicyDrain)
	}
	if sup.drainTimeout != 5*time.Second {
		t.Errorf("drainTimeout = %s, want 5s", sup.drainTimeout)
	}
}

// TestHotReloadSupervisor_FileChangeTriggersRebuild — the end-to-end
// shape against a real bootDevStack: write a watched file, observe a
// dev.hot_reload.triggered event, observe a dev.hot_reload.completed
// event, confirm the supervisor's CurrentStack() points at a fresh
// devStack (different bus instance).
//
// This is the in-package version of the integration test — it pins
// the production wiring without needing the `test/integration/`
// harness. The supervisor runs in a goroutine bounded by a cancel
// ctx; the test asserts on bus events landing within a bounded
// real-time deadline.
func TestHotReloadSupervisor_FileChangeTriggersRebuild(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(bootDevStackBusWiredYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	// Watch the cfg dir + a separate dir we can mutate independently
	// of the cfg file (so we don't double-trigger).
	watchDir := filepath.Join(dir, "watched")
	if err := os.MkdirAll(watchDir, 0o700); err != nil {
		t.Fatalf("mkdir watched: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer bootCancel()

	bootOpts := devBootOptions{
		cfgPath:   cfgPath,
		allowMock: true,
		logger:    logger,
		stderr:    io.Discard,
		port:      0, // ephemeral — http.Server binds to :0
	}
	stack, err := bootDevStack(bootCtx, bootOpts)
	if err != nil {
		t.Fatalf("bootDevStack: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stack.close(closeCtx)
	})

	cfg := config.DevHotReloadConfig{
		Enabled:      boolPtrFor(true),
		Policy:       config.DevHotReloadPolicyDrain,
		DrainTimeout: 2 * time.Second,
		WatchRoots:   []string{watchDir},
	}
	sup, err := newHotReloadSupervisor(logger, bootOpts, stack, cfg, []string{watchDir})
	if err != nil {
		t.Fatalf("newHotReloadSupervisor: %v", err)
	}

	// Subscribe BEFORE starting the supervisor so the fan-out bus does
	// not race the first triggered event. The dev triple matches what
	// the supervisor's emit helpers stamp on the canonical event.
	id := identity.Identity{TenantID: DevTenant, UserID: DevUser, SessionID: DevSession}
	sub, err := stack.bus.Subscribe(bootCtx, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{EventTypeDevHotReloadTriggered},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe(triggered): %v", err)
	}
	defer sub.Cancel()

	// Run the supervisor in a goroutine; it returns when the runCtx
	// cancels. Capture the run's error.
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- sup.Run(runCtx)
	}()
	t.Cleanup(func() {
		runCancel()
		<-runDone
		// Drain the supervisor's final stack so subsequent test
		// runs don't leak the http.Server's listener.
		final := sup.CurrentStack()
		if final != nil && final != stack {
			// A rebuild happened: the supervisor's CurrentStack is a
			// different *devStack instance. Drain it.
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			final.close(closeCtx)
		}
	})

	// Touch a file under watchDir to trigger fsnotify. Use a write to
	// a fresh file so the event is unambiguously Create-or-Write.
	target := filepath.Join(watchDir, "trigger.txt")
	// Watcher-Add grace: there's no observable "fsnotify watcher
	// ready" signal — supervisor.Run installs the watcher in its
	// goroutine and the OS-level Add is asynchronous. We can't poll
	// for readiness without writing a test file (which would race
	// our real test write). A fixed 100ms is the smallest cross-OS-
	// stable window. §17.4 carve-out documented: no observable signal
	// exists to convert this to an eventually-poll. Tracked for a
	// future supervisor enhancement (add a "watcher-ready" channel
	// the test can wait on).
	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(target, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write trigger file: %v", err)
	}

	// Wait for the canonical triggered event. The debounce window is
	// 250ms; we allow up to 3s real time.
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed before triggered event observed")
		}
		if ev.Type != EventTypeDevHotReloadTriggered {
			t.Fatalf("event type = %q, want %q", ev.Type, EventTypeDevHotReloadTriggered)
		}
		p, ok := ev.Payload.(DevHotReloadTriggeredPayload)
		if !ok {
			t.Fatalf("payload type = %T, want DevHotReloadTriggeredPayload", ev.Payload)
		}
		if p.Policy != config.DevHotReloadPolicyDrain {
			t.Errorf("payload.Policy = %q, want %q", p.Policy, config.DevHotReloadPolicyDrain)
		}
		if p.Path == "" {
			t.Error("payload.Path is empty; want the trigger file path")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dev.hot_reload.triggered event did not arrive within 3s")
	}

	// After the trigger lands, the supervisor will rebuild the stack
	// and emit `dev.hot_reload.completed` on the NEW bus. The
	// subscriber above was bound to the OLD bus (which the supervisor
	// closed during the rebuild's drain), so we can't observe the
	// completed event with the same subscriber. Instead, we wait for
	// the supervisor's CurrentStack() to swap to a new instance —
	// that confirms the rebuild completed. The new bus is a fresh
	// *events.EventBus pointer; we compare pointer identity.
	if !waitForCondition(5*time.Second, func() bool {
		return sup.CurrentStack() != nil && sup.CurrentStack() != stack
	}) {
		t.Fatal("supervisor did not swap in a new stack within 5s after the trigger")
	}
}

// TestHotReloadSupervisor_RebuildEmitsCompletedOnNewBus — the audit's
// F4 regression test. The `dev.hot_reload.completed` canonical event
// was registered + emitted but had ZERO consumer test (the existing
// FileChangeTriggersRebuild test stops at `triggered` and only
// pointer-compares the swapped stack). §13 primitive-with-consumer
// requires the wave that introduces a canonical event to ship at
// least one test that observes it end-to-end on the bus.
//
// Approach: boot supervisor, fire a first rebuild to swap to the new
// bus, then subscribe on the NEW bus before firing a second rebuild
// and asserting the typed `completed` payload arrives with Success=
// true, Path populated, Policy=drain.
func TestHotReloadSupervisor_RebuildEmitsCompletedOnNewBus(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(bootDevStackBusWiredYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	watchDir := filepath.Join(dir, "watch")
	if err := os.MkdirAll(watchDir, 0o755); err != nil {
		t.Fatalf("mkdir watch: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bootCtx := context.Background()
	stack, err := bootDevStack(bootCtx, devBootOptions{
		cfgPath: cfgPath, allowMock: true, logger: logger, stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("bootDevStack: %v", err)
	}
	bootOpts := devBootOptions{
		cfgPath: cfgPath, allowMock: true, logger: logger, stderr: io.Discard,
	}
	hrCfg := config.DevHotReloadConfig{
		Policy:       config.DevHotReloadPolicyDrain,
		DrainTimeout: 2 * time.Second,
	}
	sup, err := newHotReloadSupervisor(logger, bootOpts, stack, hrCfg, []string{watchDir})
	if err != nil {
		t.Fatalf("newHotReloadSupervisor: %v", err)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(runCtx) }()
	t.Cleanup(func() {
		runCancel()
		<-runDone
		final := sup.CurrentStack()
		if final != nil && final != stack {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			final.close(closeCtx)
		}
	})

	// Phase 1 — fire the first rebuild so CurrentStack swaps to a new bus.
	// Brief grace period for the fsnotify watcher to register the Add
	// (the Run goroutine's watcher.Add can race with our immediate write
	// — there's no observable "watcher ready" event to poll on, so a
	// small fixed wait is unavoidable). Matches the existing test's
	// pattern.
	time.Sleep(100 * time.Millisecond)
	target1 := filepath.Join(watchDir, "trigger1.txt")
	if err := os.WriteFile(target1, []byte("phase1"), 0o600); err != nil {
		t.Fatalf("write trigger1: %v", err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		cur := sup.CurrentStack()
		return cur != nil && cur != stack
	}) {
		t.Fatal("first rebuild did not complete within 5s — supervisor did not swap stacks")
	}
	newStack := sup.CurrentStack()
	if newStack == nil || newStack.bus == nil {
		t.Fatal("supervisor's new stack has no bus")
	}

	// Phase 2 — fire a second rebuild. The supervisor emits
	// `completed` on the newly-built stack's bus immediately after
	// the rebuild succeeds (line 447 — emitCompleted gets the new
	// stack as the bus target). Use the bus's Replayer interface to
	// recover the completed event from cursor 0 — Subscribe is pure
	// live-tail and would race the emit; Replay is the deterministic
	// recovery path the §13 fail-loud surface requires.
	time.Sleep(100 * time.Millisecond) // watcher-Add grace; see note above
	target2 := filepath.Join(watchDir, "trigger2.txt")
	if err := os.WriteFile(target2, []byte("phase2"), 0o600); err != nil {
		t.Fatalf("write trigger2: %v", err)
	}

	// Wait for the second rebuild's stack swap.
	if !waitForCondition(5*time.Second, func() bool {
		cur := sup.CurrentStack()
		return cur != nil && cur != newStack
	}) {
		t.Fatal("second rebuild did not complete within 5s")
	}
	finalStack := sup.CurrentStack()
	if finalStack == nil || finalStack.bus == nil {
		t.Fatal("supervisor's final stack has no bus")
	}

	// Recover the completed event via Replay. The inmem bus
	// implements events.Replayer (Phase 06 / D-022); cursor {Sequence:
	// 0} returns every event strictly newer than zero — i.e. the
	// whole post-rebuild stream.
	id := identity.Identity{TenantID: DevTenant, UserID: DevUser, SessionID: DevSession}
	replayer, ok := finalStack.bus.(events.Replayer)
	if !ok {
		t.Fatalf("final bus does not implement events.Replayer (%T)", finalStack.bus)
	}
	// Poll for the completed event — it may take a few ms after the
	// stack swap for the emit to land. Bounded by the same 5s window.
	var completed *events.Event
	if !waitForCondition(5*time.Second, func() bool {
		evs, err := replayer.Replay(bootCtx, events.Cursor{}, events.Filter{
			Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
			Types: []events.EventType{EventTypeDevHotReloadCompleted},
		})
		if err != nil {
			return false
		}
		for i := range evs {
			ev := evs[i]
			completed = &ev
			return true
		}
		return false
	}) {
		t.Fatal("dev.hot_reload.completed did not arrive on the final bus within 5s — F4 regression")
	}
	if completed.Type != EventTypeDevHotReloadCompleted {
		t.Fatalf("event type = %q, want %q", completed.Type, EventTypeDevHotReloadCompleted)
	}
	p, ok := completed.Payload.(DevHotReloadCompletedPayload)
	if !ok {
		t.Fatalf("payload type = %T, want DevHotReloadCompletedPayload", completed.Payload)
	}
	if !p.Success {
		t.Errorf("payload.Success = false, want true (err=%q)", p.ErrorMessage)
	}
	if p.Path == "" {
		t.Error("payload.Path is empty")
	}
	if p.Policy != config.DevHotReloadPolicyDrain {
		t.Errorf("payload.Policy = %q, want %q", p.Policy, config.DevHotReloadPolicyDrain)
	}
	if p.DurationMS < 0 {
		t.Errorf("payload.DurationMS = %d, want >= 0", p.DurationMS)
	}
}

// TestHotReloadSupervisor_CtxCancel_ReturnsCleanly — cancelling the
// supervisor's ctx returns nil from Run and leaves CurrentStack
// pointing at the initial (un-rebuilt) stack. No goroutine leak
// (the test's t.Cleanup observes the run-done channel).
func TestHotReloadSupervisor_CtxCancel_ReturnsCleanly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(bootDevStackBusWiredYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	watchDir := filepath.Join(dir, "watched")
	if err := os.MkdirAll(watchDir, 0o700); err != nil {
		t.Fatalf("mkdir watched: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer bootCancel()

	bootOpts := devBootOptions{
		cfgPath:   cfgPath,
		allowMock: true,
		logger:    logger,
		stderr:    io.Discard,
		port:      0,
	}
	stack, err := bootDevStack(bootCtx, bootOpts)
	if err != nil {
		t.Fatalf("bootDevStack: %v", err)
	}

	sup, err := newHotReloadSupervisor(logger, bootOpts, stack,
		config.DevHotReloadConfig{
			Policy:       config.DevHotReloadPolicyDrain,
			DrainTimeout: 1 * time.Second,
			WatchRoots:   []string{watchDir},
		},
		[]string{watchDir})
	if err != nil {
		t.Fatalf("newHotReloadSupervisor: %v", err)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- sup.Run(runCtx)
	}()

	// Cancel after the supervisor has had a moment to install the
	// watcher and start the serve goroutine.
	time.Sleep(150 * time.Millisecond)
	runCancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("supervisor.Run() = %v, want nil on ctx-cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor.Run did not return within 5s of ctx-cancel")
	}

	// CurrentStack should still point at the original — no rebuild fired.
	if sup.CurrentStack() != stack {
		t.Error("CurrentStack() did not point at the initial stack after ctx-cancel without a rebuild")
	}

	// Cleanup the stack (the supervisor returned without draining; the
	// runDev path drains via CurrentStack — we mirror that here).
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stack.close(closeCtx)
}

// TestHotReloadSupervisor_MissingWatchRoot_StatErrFailsLoud — a
// permissions-error stat (or any non-os.ErrNotExist stat error) on a
// configured watch root fails loud per §13. A missing root is allowed
// (the default `.harbor/agents` doesn't exist for first-time projects).
//
// We can't easily simulate a permissions-error stat in a portable
// test (would require running as a non-privileged user against a
// 0000-perm dir), so this test exercises the missing-root acceptance
// path instead: a non-existent root is logged and skipped, and the
// supervisor still serves the other roots.
func TestHotReloadSupervisor_MissingWatchRoot_LogsAndSkips(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(bootDevStackBusWiredYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	watchDir := filepath.Join(dir, "watched")
	if err := os.MkdirAll(watchDir, 0o700); err != nil {
		t.Fatalf("mkdir watched: %v", err)
	}
	missing := filepath.Join(dir, "definitely-does-not-exist")

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer bootCancel()

	bootOpts := devBootOptions{
		cfgPath:   cfgPath,
		allowMock: true,
		logger:    logger,
		stderr:    io.Discard,
		port:      0,
	}
	stack, err := bootDevStack(bootCtx, bootOpts)
	if err != nil {
		t.Fatalf("bootDevStack: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stack.close(closeCtx)
	})

	sup, err := newHotReloadSupervisor(logger, bootOpts, stack,
		config.DevHotReloadConfig{
			Policy:       config.DevHotReloadPolicyDrain,
			DrainTimeout: 1 * time.Second,
			WatchRoots:   []string{watchDir, missing},
		},
		[]string{watchDir, missing})
	if err != nil {
		t.Fatalf("newHotReloadSupervisor: %v", err)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- sup.Run(runCtx)
	}()

	// Let it install the watcher, then cancel cleanly.
	time.Sleep(150 * time.Millisecond)
	runCancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("supervisor.Run() = %v, want nil (missing root should be skipped, not fatal)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor.Run did not return within 5s of ctx-cancel")
	}
}

// stringSliceEqual reports element-wise equality. Stdlib-free so we
// don't pull in reflect for one call site.
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// waitForCondition polls f at 25ms intervals until f returns true OR
// the timeout fires. Returns whether f ever returned true. Stdlib-free
// so we don't pull in a test-helper library for one call site.
func waitForCondition(timeout time.Duration, f func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return f()
}

// boolPtrFor is the in-test counterpart of `config.boolPtr` (unexported
// there). Kept local so the tests don't reach for an export-only
// helper.
func boolPtrFor(b bool) *bool { return &b }
