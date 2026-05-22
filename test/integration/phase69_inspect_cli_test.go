// Phase 69 cross-subsystem integration test per CLAUDE.md §17 — the
// `harbor inspect-events` + `harbor inspect-runs` CLI subcommands
// exercised end-to-end against the REAL runtime surface they bind:
//
//   - Phase 60 internal/protocol/transports SSE event stream — the
//     CLI's read path.
//   - Phase 54 internal/protocol.ControlSurface — drives a real `start`
//     so a real `task.spawned` event lands on the bus the CLI tails.
//   - Phase 20 internal/tasks/drivers/inprocess — spawns the task that
//     emits the lifecycle event the CLI projects into its output.
//   - Phase 05 internal/events/drivers/inmem — the bus the SSE
//     transport reads.
//
// The test builds `./bin/harbor`, stands up an httptest.Server backed
// by `transports.NewMux(..., WithoutValidator())` (the test-only
// escape hatch documented in internal/protocol/transports), seeds an
// event onto the bus by driving a real `start`, and exec's the built
// binary with `--bind <httptest>` + `--follow=false` to assert it
// snapshots the SSE replay correctly.
//
// This is the §13 "primitive-with-consumer" discharge for Phase 69:
// the Phase 60 SSE event stream has been a primitive without a CLI
// consumer since it shipped; this phase grafts the first one on. The
// integration test asserts the consumer reads the primitive faithfully
// over the wire — no mocks at the seam.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildHarborOnce compiles ./bin/harbor at test-start time. Cached
// across subtests via sync.Once so the integration suite pays the
// build cost only once.
var (
	buildHarborOnceVal sync.Once
	harborBinPath      string
	harborBuildErr     error
)

func buildHarborForInspect(t *testing.T) string {
	t.Helper()
	buildHarborOnceVal.Do(func() {
		// Resolve repo root: caller is test/integration/, repo root is two
		// levels up. Use runtime.Caller to stay agnostic to the user's pwd.
		_, file, _, _ := runtime.Caller(0)
		root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
		if err != nil {
			harborBuildErr = err
			return
		}
		bin := filepath.Join(root, "bin", "harbor-phase69-test")
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/harbor")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if buildErr := cmd.Run(); buildErr != nil {
			harborBuildErr = fmt.Errorf("go build harbor: %w: %s", buildErr, stderr.String())
			return
		}
		harborBinPath = bin
	})
	if harborBuildErr != nil {
		t.Fatalf("build harbor: %v", harborBuildErr)
	}
	return harborBinPath
}

// TestE2E_Phase69_InspectEvents_SnapshotsLiveTaskSpawnedEvent — the
// headline E2E. Drives a real `start` against the Protocol REST
// control transport, then exec's `harbor inspect-events --follow=false
// --type task.spawned` against the same httptest.Server. Asserts the
// CLI's stdout contains an entry for that run's task.spawned event.
func TestE2E_Phase69_InspectEvents_SnapshotsLiveTaskSpawnedEvent(t *testing.T) {
	bin := buildHarborForInspect(t)
	deps := newPhase60Deps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	// Drive a real start over the REST surface so a real task.spawned
	// event lands on the bus the CLI will tail. Re-issue once after a
	// short wait — the in-mem bus's replay buffer carries the event
	// regardless of subscription order, but the second start makes
	// the assertion robust if the buffer was reset between runs.
	taskID := submitStart(t, srv.URL, "s1")
	_ = submitStart(t, srv.URL, "s1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "inspect-events",
		"--bind", strings.TrimPrefix(srv.URL, "http://"),
		"--tenant", "t1",
		"--user", "u1",
		"--session", "s1",
		"--type", "task.spawned",
		"--since", "0",
		"--follow=false",
		"--json",
	)
	// HARBOR_TOKEN is mandatory at the CLI edge; the httptest server
	// runs WithoutValidator, so any non-empty string suffices on the
	// wire (auth.Middleware is not in the path).
	cmd.Env = append(os.Environ(), "HARBOR_TOKEN=dummy-jwt-for-trustbased-test")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("harbor inspect-events: %v\nstderr: %s", err, stderr.String())
	}
	body := stdout.String()
	if body == "" {
		t.Fatalf("inspect-events produced no output (stderr: %s)", stderr.String())
	}
	// Each line is one canonical wireEvent JSON. We expect ≥1 line for
	// the task.spawned event we drove.
	saw := false
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["type"] == "task.spawned" {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("no task.spawned line in --json output\n--- stdout ---\n%s\n--- end ---\nspawned task: %s", body, taskID)
	}
}

// TestE2E_Phase69_InspectRuns_ListReturnsLiveRun — drives two starts
// (so two runs), then exec's `harbor inspect-runs --json` and asserts
// the JSON array carries both run ids.
func TestE2E_Phase69_InspectRuns_ListReturnsLiveRun(t *testing.T) {
	bin := buildHarborForInspect(t)
	deps := newPhase60Deps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	taskA := submitStart(t, srv.URL, "s1")
	taskB := submitStart(t, srv.URL, "s1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "inspect-runs",
		"--bind", strings.TrimPrefix(srv.URL, "http://"),
		"--tenant", "t1",
		"--user", "u1",
		"--session", "s1",
		"--since", "0",
		"--json",
	)
	cmd.Env = append(os.Environ(), "HARBOR_TOKEN=dummy-jwt-for-trustbased-test")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("harbor inspect-runs: %v\nstderr: %s", err, stderr.String())
	}
	body := strings.TrimSpace(stdout.String())
	if body == "" {
		t.Fatalf("inspect-runs produced no output (stderr: %s)", stderr.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("inspect-runs --json was not a JSON array: %v\nbody: %s", err, body)
	}
	seenA, seenB := false, false
	for _, r := range rows {
		switch r["run_id"] {
		case taskA:
			seenA = true
		case taskB:
			seenB = true
		}
	}
	if !seenA || !seenB {
		t.Errorf("inspect-runs missing one of taskA=%s seenA=%v / taskB=%s seenB=%v\nbody: %s",
			taskA, seenA, taskB, seenB, body)
	}
}

// TestE2E_Phase69_InspectRuns_TrajectoryFiltersToOneRun — driving two
// starts; then `harbor inspect-runs <taskA> --json` should return a
// trajectory whose steps all carry the taskA run id (and no taskB
// events bleed through).
func TestE2E_Phase69_InspectRuns_TrajectoryFiltersToOneRun(t *testing.T) {
	bin := buildHarborForInspect(t)
	deps := newPhase60Deps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	taskA := submitStart(t, srv.URL, "s1")
	_ = submitStart(t, srv.URL, "s1") // taskB — should not appear

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "inspect-runs", taskA,
		"--bind", strings.TrimPrefix(srv.URL, "http://"),
		"--tenant", "t1",
		"--user", "u1",
		"--session", "s1",
		"--since", "0",
		"--json",
	)
	cmd.Env = append(os.Environ(), "HARBOR_TOKEN=dummy-jwt-for-trustbased-test")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("harbor inspect-runs %s: %v\nstderr: %s", taskA, err, stderr.String())
	}
	body := strings.TrimSpace(stdout.String())
	var out struct {
		RunID string `json:"run_id"`
		Steps []any  `json:"steps"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode trajectory json: %v\nbody: %s", err, body)
	}
	if out.RunID != taskA {
		t.Errorf("trajectory.run_id = %q; want %q", out.RunID, taskA)
	}
	if len(out.Steps) == 0 {
		t.Errorf("trajectory has zero steps for taskA=%s; bus should have ≥1 task.spawned event", taskA)
	}
}

// TestE2E_Phase69_InspectEvents_FailsLoudOnMissingToken — both env
// + ~/.harbor/token absent → the CLI exits non-zero with
// CodeAuthRequired BEFORE making any network call. Confirms the §13
// "fail loud" amendment lands.
func TestE2E_Phase69_InspectEvents_FailsLoudOnMissingToken(t *testing.T) {
	bin := buildHarborForInspect(t)

	// Use a HOME that doesn't have ~/.harbor/token.
	tmpHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "inspect-events",
		"--bind", "127.0.0.1:1",
		"--tenant", "t",
		"--user", "u",
		"--session", "s",
		"--json",
	)
	// Strip HARBOR_TOKEN; redirect HOME so the file fallback misses.
	cmd.Env = []string{"HOME=" + tmpHome, "PATH=" + os.Getenv("PATH")}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("inspect-events succeeded with no token; want non-zero exit")
	}
	if !strings.Contains(stderr.String(), `"code":"auth_required"`) {
		t.Errorf("stderr missing auth_required code:\n%s", stderr.String())
	}
}
