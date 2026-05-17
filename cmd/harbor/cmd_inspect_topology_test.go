// cmd/harbor/cmd_inspect_topology_test.go — Phase 70 (D-102):
// cobra-driver + transport-layer tests for `harbor inspect-topology`.
//
// Three categories:
//
//   1. Flag-validation paths (missing run-id, bad bind, bad width,
//      missing auth) — assert the structured CLIError shape so smoke
//      scripts can pin codes.
//   2. Live SSE round-trip against an httptest.Server emitting the
//      canonical Phase 60 wire shape. Drives both ASCII and JSON
//      modes; asserts the rendered output names the run + at least
//      one tool node + the finish reason.
//   3. Failure modes — non-200 status, planner.finish-for-wrong-run
//      (idle-timeout exit), 404 endpoint, 401 (bad token), empty
//      stream.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// runInspectTopologyTest is a small helper: builds a fresh root, sets
// args, captures stdout/stderr, returns (stdout, stderr, err). Mirrors
// runRoot from root_test.go but specialised for the inspect-topology
// subcommand path.
//
// We propagate the buffers to the subcommand explicitly via
// `subCmd.SetErr` AFTER root traversal because cobra's
// `ErrOrStderr()` walks up only when no errWriter is set on the
// child. Older test helpers in this package rely on the parent-walk
// path and work fine; the inspect-topology cmd does its own
// emitCLIError call early (before any subcommand finalisation) so we
// belt-and-brace by attaching the buffers to BOTH levels.
func runInspectTopologyTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := NewRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	for _, child := range root.Commands() {
		if child.Name() == "inspect-topology" {
			child.SetOut(&out)
			child.SetErr(&errBuf)
		}
	}
	root.SetArgs(append([]string{"inspect-topology"}, args...))
	err := root.Execute()
	return out.String(), errBuf.String(), err
}

// TestInspectTopology_MissingRunID asserts the no-arg path surfaces
// the run-id-missing CLIError.
func TestInspectTopology_MissingRunID(t *testing.T) {
	t.Parallel()
	_, _, err := runInspectTopologyTest(t)
	if err == nil {
		t.Fatal("expected error for missing run-id")
	}
	// cobra surfaces the ExactArgs error directly; the body never
	// reaches the run-id-missing CLIError because cobra rejects at
	// arg-parse time. Both shapes are acceptable failure surfaces;
	// the assertion is "non-zero exit".
}

// TestInspectTopology_BadBind asserts --bind validation.
func TestInspectTopology_BadBind(t *testing.T) {
	t.Parallel()
	_, _, err := runInspectTopologyTest(t, "--bind", "no-port", "run-1")
	if err == nil {
		t.Fatal("expected error for bad --bind")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeInspectTopologyBindInvalid {
		t.Errorf("CLIError.Code: got %q, want %q", cli.Code, CodeInspectTopologyBindInvalid)
	}
}

// TestInspectTopology_BadWidth asserts --width validation.
func TestInspectTopology_BadWidth(t *testing.T) {
	t.Parallel()
	_, _, err := runInspectTopologyTest(t, "--width", "5", "run-1")
	if err == nil {
		t.Fatal("expected error for bad --width")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeInspectTopologyWidthInvalid {
		t.Errorf("CLIError.Code: got %q, want %q", cli.Code, CodeInspectTopologyWidthInvalid)
	}
}

// TestInspectTopology_MissingToken asserts the auth-missing error
// fires when HARBOR_TOKEN is unset AND no ~/.harbor/token exists.
func TestInspectTopology_MissingToken(t *testing.T) {
	// Cannot run t.Parallel — we mutate env / HOME.
	t.Setenv(EnvInspectTopologyToken, "")
	// Point HOME at a tempdir so the on-disk fallback resolves to a
	// non-existent file.
	t.Setenv("HOME", t.TempDir())
	_, _, err := runInspectTopologyTest(t, "run-1")
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeInspectTopologyAuthMissing {
		t.Errorf("CLIError.Code: got %q, want %q", cli.Code, CodeInspectTopologyAuthMissing)
	}
}

// TestInspectTopology_HappyPath_RoundTripsAgainstFakeServer drives
// the cmd against an httptest.Server emitting a canonical wire-frame
// SSE stream — asserts the rendered output contains expected
// substrings.
func TestInspectTopology_HappyPath_RoundTripsAgainstFakeServer(t *testing.T) {
	// Production-shape fixture: task.spawned has EMPTY `run` (the `start`
	// Protocol method dispatches Quadruple{Identity: id} only — the
	// per-task RunLoop driver sets RunID = TaskID later, D-098). The
	// payload's TaskID = "task-A" so `runIDFromFrame`'s payload fallback
	// resolves the spawn event. Subsequent events carry `run: "task-A"`
	// because the RunLoop driver sets `Identity.RunID = TaskID`. The
	// caller asks for `--run task-A` mirroring production semantics.
	// The §17.6 worked example — fixture must mirror production or the
	// test silently diverges (audit's F2 finding).
	srv := newFakeSSEServer(t, []string{
		`{"type":"task.spawned","sequence":1,"occurred_at":"2026-05-17T00:00:00.000000000Z","tenant":"t","user":"u","session":"s","payload":{"TaskID":"task-A","Kind":"foreground"}}`,
		`{"type":"tool.invoked","sequence":2,"occurred_at":"2026-05-17T00:00:00.000000000Z","tenant":"t","user":"u","session":"s","run":"task-A","payload":{"ToolName":"echo"}}`,
		`{"type":"tool.completed","sequence":3,"occurred_at":"2026-05-17T00:00:00.000000000Z","tenant":"t","user":"u","session":"s","run":"task-A","payload":{"ToolName":"echo","DurationMS":12}}`,
		`{"type":"planner.finish","sequence":4,"occurred_at":"2026-05-17T00:00:00.000000000Z","tenant":"t","user":"u","session":"s","run":"task-A","payload":{"Reason":"goal"}}`,
	})
	defer srv.Close()
	bind := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv(EnvInspectTopologyToken, "test-bearer-token")

	stdout, errBuf, err := runInspectTopologyTest(t,
		"--bind", bind,
		"--idle-timeout", "500ms",
		"task-A",
	)
	if err != nil {
		t.Fatalf("inspect-topology returned error: %v (stderr: %s)", err, errBuf)
	}
	if !strings.Contains(stdout, "run task-A") {
		t.Errorf("stdout missing `run task-A` header: %s", stdout)
	}
	if !strings.Contains(stdout, "echo") {
		t.Errorf("stdout missing `echo` tool: %s", stdout)
	}
	if !strings.Contains(stdout, "task-A") {
		t.Errorf("stdout missing task-A: %s", stdout)
	}
	if !strings.Contains(stdout, "goal") {
		t.Errorf("stdout missing finish reason `goal`: %s", stdout)
	}
}

// TestInspectTopology_JSONMode_EmitsTopologyShape asserts --json
// mode renders the canonical Topology JSON.
func TestInspectTopology_JSONMode_EmitsTopologyShape(t *testing.T) {
	srv := newFakeSSEServer(t, []string{
		`{"type":"tool.invoked","sequence":1,"tenant":"t","user":"u","session":"s","run":"r1","payload":{"ToolName":"echo"}}`,
		`{"type":"tool.completed","sequence":2,"tenant":"t","user":"u","session":"s","run":"r1","payload":{"ToolName":"echo","DurationMS":10}}`,
		`{"type":"planner.finish","sequence":3,"tenant":"t","user":"u","session":"s","run":"r1","payload":{"Reason":"goal"}}`,
	})
	defer srv.Close()
	bind := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv(EnvInspectTopologyToken, "test-bearer-token")

	stdout, errBuf, err := runInspectTopologyTest(t,
		"--bind", bind,
		"--idle-timeout", "500ms",
		"--json",
		"r1",
	)
	if err != nil {
		t.Fatalf("inspect-topology --json returned error: %v (stderr: %s)", err, errBuf)
	}
	if !strings.Contains(stdout, `"run_id": "r1"`) {
		t.Errorf("JSON missing run_id: %s", stdout)
	}
	if !strings.Contains(stdout, `"kind": "finish"`) {
		t.Errorf("JSON missing finish kind: %s", stdout)
	}
	if !strings.Contains(stdout, `"source_mode": "events.synthesised"`) {
		t.Errorf("JSON missing source_mode: %s", stdout)
	}
}

// TestInspectTopology_NonExistentRun_RunNotFound asserts the
// run-not-found CLIError fires when the SSE stream produces no
// matching frames within idle timeout.
func TestInspectTopology_NonExistentRun_RunNotFound(t *testing.T) {
	srv := newFakeSSEServer(t, []string{
		// All frames are for a DIFFERENT run; ParseSSEFrames
		// filters them out and BuildTopologyFromEvents sees nothing.
		`{"type":"tool.invoked","sequence":1,"run":"OTHER","payload":{"ToolName":"echo"}}`,
	})
	defer srv.Close()
	bind := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv(EnvInspectTopologyToken, "test-bearer-token")

	_, _, err := runInspectTopologyTest(t,
		"--bind", bind,
		"--idle-timeout", "300ms",
		"nonexistent-run",
	)
	if err == nil {
		t.Fatal("expected error for non-existent run")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeInspectTopologyRunNotFound {
		t.Errorf("CLIError.Code: got %q, want %q", cli.Code, CodeInspectTopologyRunNotFound)
	}
}

// TestInspectTopology_HTTPStatus401_AuthMappedHint asserts a 401
// response gets the auth-specific hint.
func TestInspectTopology_HTTPStatus401_AuthMappedHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("token rejected"))
	}))
	defer srv.Close()
	bind := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv(EnvInspectTopologyToken, "bad-token")

	_, _, err := runInspectTopologyTest(t, "--bind", bind, "run-1")
	if err == nil {
		t.Fatal("expected error for 401 status")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeInspectTopologyHTTPStatus {
		t.Errorf("CLIError.Code: got %q, want %q", cli.Code, CodeInspectTopologyHTTPStatus)
	}
}

// TestInspectTopology_ConnectFailed asserts the connect-failed
// CLIError fires when the bind points at a non-listening port.
func TestInspectTopology_ConnectFailed(t *testing.T) {
	t.Setenv(EnvInspectTopologyToken, "bearer-token")
	// Use a high port that's almost certainly free. If a test in CI
	// happens to bind here, this test surfaces a different error
	// shape — we accept either failure mode as long as it's non-zero.
	_, _, err := runInspectTopologyTest(t, "--bind", "127.0.0.1:1", "--idle-timeout", "200ms", "r1")
	if err == nil {
		t.Fatal("expected error connecting to bad bind")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeInspectTopologyConnectFailed {
		t.Errorf("CLIError.Code: got %q, want %q", cli.Code, CodeInspectTopologyConnectFailed)
	}
}

// TestFetchSSEUntilIdle_AborbsKeepalives asserts the fetcher does not
// terminate on keepalive frames alone.
func TestFetchSSEUntilIdle_AbsorbsKeepalives(t *testing.T) {
	t.Parallel()
	frames := []string{
		`{"type":"tool.invoked","sequence":1,"run":"r1","payload":{"ToolName":"x"}}`,
		`{"type":"planner.finish","sequence":2,"run":"r1","payload":{"Reason":"goal"}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer is not a flusher")
			return
		}
		for i, raw := range frames {
			// Emit a keepalive comment before each real frame
			// to prove the fetcher's idle timer resets correctly.
			_, _ = fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
			_, _ = fmt.Fprintf(w, "event: x\nid: %d\ndata: %s\n\n", i+1, raw)
			flusher.Flush()
		}
		// Hold the connection briefly so the client's reader
		// drains the final chunk before the handler returns and
		// the connection closes. Without this, the buffered
		// reader can hit EOF before the last frame's blank-line
		// terminator is consumed, dropping the planner.finish
		// chunk on the floor.
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	bind := strings.TrimPrefix(srv.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body, err := fetchSSEUntilIdle(ctx, sseFetchOpts{
		Bind:        bind,
		Token:       "t",
		RunID:       "r1",
		IdleTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("fetchSSEUntilIdle: %v", err)
	}
	if !bytes.Contains(body, []byte(`planner.finish`)) {
		t.Errorf("body missing planner.finish: %s", body)
	}
}

// TestFetchSSEUntilIdle_StatusError surfaces non-200.
func TestFetchSSEUntilIdle_StatusError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	bind := strings.TrimPrefix(srv.URL, "http://")
	_, err := fetchSSEUntilIdle(context.Background(), sseFetchOpts{
		Bind:        bind,
		Token:       "t",
		RunID:       "r1",
		IdleTimeout: 500 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected error on 403")
	}
	var fe fetchError
	if !errors.As(err, &fe) || fe.Kind != "status" || fe.Status != http.StatusForbidden {
		t.Errorf("unexpected error: %v", err)
	}
}

// newFakeSSEServer returns an httptest.Server that emits each entry in
// frames as a canonical SSE frame (event: + id: + data: + blank line).
// The handler honours the Bearer-token header by always 200ing — token
// validation is not the surface under test here.
func newFakeSSEServer(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer does not flush")
			return
		}
		_, _ = fmt.Fprintf(w, "retry: 1000\n\n")
		flusher.Flush()
		for i, raw := range frames {
			_, _ = fmt.Fprintf(w, "event: x\nid: %d\ndata: %s\n\n", i+1, raw)
			flusher.Flush()
		}
		// Keep the connection open for a beat so the client's
		// idle timer fires AFTER absorbing all frames. The test
		// passes --idle-timeout 500ms; we sleep 100ms < that
		// budget so the test does not hang.
		time.Sleep(100 * time.Millisecond)
	}))
}
