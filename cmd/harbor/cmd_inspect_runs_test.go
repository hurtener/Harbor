// cmd/harbor/cmd_inspect_runs_test.go — Phase 69 inspect-runs unit +
// golden tests. Same shape as cmd_inspect_events_test.go: an httptest
// SSE server feeds canned events; goldens lock both output modes.

package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// canonicalRunsScript is a small fixture with two runs:
//
//   - run "r-1": spawn → complete
//   - run "r-2": spawn → fail
//
// Stable timestamps + sequences so goldens lock cleanly.
const canonicalRunsScript = `event: task.spawned
id: 1
data: {"type":"task.spawned","sequence":1,"occurred_at":"2026-05-17T12:00:00.000Z","tenant":"t1","user":"u1","session":"s1","run":"r-1","payload":{"TaskID":"r-1","Kind":"foreground"}}

event: task.completed
id: 2
data: {"type":"task.completed","sequence":2,"occurred_at":"2026-05-17T12:00:01.000Z","tenant":"t1","user":"u1","session":"s1","run":"r-1","payload":{"TaskID":"r-1"}}

event: task.spawned
id: 3
data: {"type":"task.spawned","sequence":3,"occurred_at":"2026-05-17T12:00:02.000Z","tenant":"t1","user":"u1","session":"s1","run":"r-2","payload":{"TaskID":"r-2","Kind":"foreground"}}

event: task.failed
id: 4
data: {"type":"task.failed","sequence":4,"occurred_at":"2026-05-17T12:00:03.000Z","tenant":"t1","user":"u1","session":"s1","run":"r-2","payload":{"TaskID":"r-2","ErrorCode":"tool_error"}}

`

// TestInspectRuns_List_Human_Golden — `harbor inspect-runs` with no
// arg renders a table with one row per run.
func TestInspectRuns_List_Human_Golden(t *testing.T) {
	t.Parallel()
	srv := sseServer(t, canonicalRunsScript, nil)
	defer srv.Close()

	var out bytes.Buffer
	err := runInspectRunsList(context.Background(), &out, inspectRunsOpts{
		Endpoint:   srv.URL,
		Filter:     inspectFilter{Tenant: "t1", User: "u1", Sess: "s1", Since: "0"},
		Auth:       inspectAuth{Token: "j"},
		JSON:       false,
		Client:     srv.Client(),
		IdleCutoff: 500 * time.Millisecond,
	}, func(cli CLIError) error { return cli })
	if err != nil {
		t.Fatalf("runInspectRunsList: %v", err)
	}
	assertGolden(t, "inspect-runs-list-human.txt", out.String())
}

// TestInspectRuns_List_JSON_Golden — same fixture, --json mode emits
// a single-line JSON array.
func TestInspectRuns_List_JSON_Golden(t *testing.T) {
	t.Parallel()
	srv := sseServer(t, canonicalRunsScript, nil)
	defer srv.Close()

	var out bytes.Buffer
	err := runInspectRunsList(context.Background(), &out, inspectRunsOpts{
		Endpoint:   srv.URL,
		Filter:     inspectFilter{Tenant: "t1", User: "u1", Sess: "s1", Since: "0"},
		Auth:       inspectAuth{Token: "j"},
		JSON:       true,
		Client:     srv.Client(),
		IdleCutoff: 500 * time.Millisecond,
	}, func(cli CLIError) error { return cli })
	if err != nil {
		t.Fatalf("runInspectRunsList: %v", err)
	}
	assertGolden(t, "inspect-runs-list-json.txt", out.String())
}

// TestInspectRuns_Trajectory_Human_Golden — `harbor inspect-runs r-1`
// renders one row per event filtered to that run.
func TestInspectRuns_Trajectory_Human_Golden(t *testing.T) {
	t.Parallel()
	srv := sseServer(t, canonicalRunsScript, nil)
	defer srv.Close()

	var out bytes.Buffer
	err := runInspectRunsTrajectory(context.Background(), &out, inspectRunsOpts{
		Endpoint:   srv.URL,
		Filter:     inspectFilter{Tenant: "t1", User: "u1", Sess: "s1", Run: "r-1", Since: "0"},
		Auth:       inspectAuth{Token: "j"},
		JSON:       false,
		Client:     srv.Client(),
		IdleCutoff: 500 * time.Millisecond,
		TargetRun:  "r-1",
	}, func(cli CLIError) error { return cli })
	if err != nil {
		t.Fatalf("runInspectRunsTrajectory: %v", err)
	}
	assertGolden(t, "inspect-runs-trajectory-human.txt", out.String())
}

// TestInspectRuns_Trajectory_JSON_Golden — same fixture in --json
// mode emits a single-line {run_id, steps[]} object.
func TestInspectRuns_Trajectory_JSON_Golden(t *testing.T) {
	t.Parallel()
	srv := sseServer(t, canonicalRunsScript, nil)
	defer srv.Close()

	var out bytes.Buffer
	err := runInspectRunsTrajectory(context.Background(), &out, inspectRunsOpts{
		Endpoint:   srv.URL,
		Filter:     inspectFilter{Tenant: "t1", User: "u1", Sess: "s1", Run: "r-1", Since: "0"},
		Auth:       inspectAuth{Token: "j"},
		JSON:       true,
		Client:     srv.Client(),
		IdleCutoff: 500 * time.Millisecond,
		TargetRun:  "r-1",
	}, func(cli CLIError) error { return cli })
	if err != nil {
		t.Fatalf("runInspectRunsTrajectory: %v", err)
	}
	assertGolden(t, "inspect-runs-trajectory-json.txt", out.String())
}

// TestInspectRuns_Trajectory_RunNotFound — `harbor inspect-runs r-zzz`
// surfaces CodeRunNotFound when no event with that run_id appears.
func TestInspectRuns_Trajectory_RunNotFound(t *testing.T) {
	t.Parallel()
	srv := sseServer(t, canonicalRunsScript, nil)
	defer srv.Close()

	var captured CLIError
	emit := func(cli CLIError) error {
		captured = cli
		return cli
	}
	err := runInspectRunsTrajectory(context.Background(), &bytes.Buffer{}, inspectRunsOpts{
		Endpoint:   srv.URL,
		Filter:     inspectFilter{Tenant: "t1", User: "u1", Sess: "s1", Run: "r-zzz"},
		Auth:       inspectAuth{Token: "j"},
		Client:     srv.Client(),
		IdleCutoff: 500 * time.Millisecond,
		TargetRun:  "r-zzz",
	}, emit)
	if err == nil {
		t.Fatal("expected error")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("err is %T, want CLIError", err)
	}
	if cli.Code != CodeRunNotFound {
		t.Errorf("Code = %q, want %q", cli.Code, CodeRunNotFound)
	}
	if captured.Subcommand != "inspect-runs" {
		t.Errorf("Subcommand = %q", captured.Subcommand)
	}
}

// TestInspectRuns_List_EmptyStream — empty stream → "no runs visible"
// (one-line human output; empty JSON array under --json).
func TestInspectRuns_List_EmptyStream(t *testing.T) {
	t.Parallel()
	srv := sseServer(t, "", nil)
	defer srv.Close()

	var out bytes.Buffer
	if err := runInspectRunsList(context.Background(), &out, inspectRunsOpts{
		Endpoint:   srv.URL,
		Filter:     inspectFilter{Tenant: "t", User: "u", Sess: "s"},
		Auth:       inspectAuth{Token: "j"},
		Client:     srv.Client(),
		IdleCutoff: 200 * time.Millisecond,
	}, func(cli CLIError) error { return cli }); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := out.String(); got != "no runs visible in the replay window\n" {
		t.Errorf("got %q", got)
	}
}

// TestInspectRuns_FlagSurface_ParityWithInspectEvents — both cobra
// commands expose the same identity/bind/since flags by name. A
// rename in one without the other silently breaks scripting consumers
// that splice both into the same shell pipeline.
func TestInspectRuns_FlagSurface_ParityWithInspectEvents(t *testing.T) {
	t.Parallel()
	ec := newInspectEventsCmd()
	rc := newInspectRunsCmd()
	for _, name := range []string{flagBind, flagTenant, flagUser, flagSession, flagSince} {
		if ec.Flags().Lookup(name) == nil {
			t.Errorf("inspect-events missing flag --%s", name)
		}
		if rc.Flags().Lookup(name) == nil {
			t.Errorf("inspect-runs missing flag --%s", name)
		}
	}
}
