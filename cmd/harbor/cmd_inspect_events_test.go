// cmd/harbor/cmd_inspect_events_test.go — Phase 69 inspect-events
// unit + golden tests. The CLI command body is driven against an
// httptest SSE server (no real Runtime needed); goldens lock both
// human-mode and --json output shapes.

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// updateGoldens aliases the existing -update flag defined in
// root_test.go. New goldens reuse the same invocation (`go test -update
// ./cmd/harbor/...`) — one flag for the whole package.
func updateRequested() bool { return update != nil && *update }

// sseServer is a tiny test helper: stands up an httptest.Server that
// serves a canned SSE script (the raw bytes operators would see on
// the wire). The script is written verbatim; the test owns when
// to .Close() the server (which terminates any in-flight stream).
func sseServer(t *testing.T, script string, assertReq func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if assertReq != nil {
			assertReq(r)
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(script))
		flusher.Flush()
		// Hold the connection open until the client disconnects so the
		// snapshot-timeout path is observable.
		<-r.Context().Done()
	}))
}

// canonicalScript is the fixture used by every golden test: three
// events with stable timestamps + payloads so the golden bytes are
// deterministic. The shape MATCHES the Phase 60 SSE wire format
// verbatim (event/id/data + blank line).
const canonicalScript = `event: task.spawned
id: 1
data: {"type":"task.spawned","sequence":1,"occurred_at":"2026-05-17T12:00:00.000Z","tenant":"t1","user":"u1","session":"s1","run":"r-1","payload":{"TaskID":"r-1","Kind":"foreground","Priority":0}}

event: task.completed
id: 2
data: {"type":"task.completed","sequence":2,"occurred_at":"2026-05-17T12:00:01.000Z","tenant":"t1","user":"u1","session":"s1","run":"r-1","payload":{"TaskID":"r-1"}}

event: task.spawned
id: 3
data: {"type":"task.spawned","sequence":3,"occurred_at":"2026-05-17T12:00:02.000Z","tenant":"t1","user":"u1","session":"s1","run":"r-2","payload":{"TaskID":"r-2","Kind":"background"}}

`

// TestInspectEvents_Human_Golden — snapshot mode (--follow=false) +
// human output renders one line per event, matches
// testdata/golden/inspect-events-human.txt.
func TestInspectEvents_Human_Golden(t *testing.T) {
	t.Parallel()
	srv := sseServer(t, canonicalScript, func(r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-jwt" {
			t.Errorf("server: missing Bearer")
		}
		if r.Header.Get("X-Harbor-Tenant") != "t1" {
			t.Errorf("server: X-Harbor-Tenant missing")
		}
	})
	defer srv.Close()

	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := runInspectEventsAgainst(ctx, &out, inspectEventsOpts{
		Endpoint:   srv.URL,
		Filter:     inspectFilter{Tenant: "t1", User: "u1", Sess: "s1"},
		Auth:       inspectAuth{Token: "test-jwt"},
		JSON:       false,
		Follow:     false,
		Client:     srv.Client(),
		IdleCutoff: 500 * time.Millisecond, // fast snapshot for tests
		Now:        func() time.Time { return time.Date(2026, 5, 17, 12, 0, 5, 0, time.UTC) },
	}, func(cli CLIError) error { return cli })
	if err != nil {
		t.Fatalf("runInspectEventsAgainst: %v", err)
	}
	assertGolden(t, "inspect-events-human.txt", out.String())
}

// TestInspectEvents_JSON_Golden — same fixture, --json output is
// newline-delimited canonical wireEvent JSON.
func TestInspectEvents_JSON_Golden(t *testing.T) {
	t.Parallel()
	srv := sseServer(t, canonicalScript, nil)
	defer srv.Close()

	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := runInspectEventsAgainst(ctx, &out, inspectEventsOpts{
		Endpoint:   srv.URL,
		Filter:     inspectFilter{Tenant: "t1", User: "u1", Sess: "s1"},
		Auth:       inspectAuth{Token: "test-jwt"},
		JSON:       true,
		Follow:     false,
		Client:     srv.Client(),
		IdleCutoff: 500 * time.Millisecond,
		Now:        func() time.Time { return time.Date(2026, 5, 17, 12, 0, 5, 0, time.UTC) },
	}, func(cli CLIError) error { return cli })
	if err != nil {
		t.Fatalf("runInspectEventsAgainst: %v", err)
	}
	assertGolden(t, "inspect-events-json.txt", out.String())
}

// TestInspectEvents_TypeFilter_PropagatesAsHeader — the --type flag
// translates to X-Harbor-Event-Type carrier header(s).
func TestInspectEvents_TypeFilter_PropagatesAsHeader(t *testing.T) {
	t.Parallel()
	sawTypes := make(chan []string, 1)
	srv := sseServer(t, "", func(r *http.Request) {
		sawTypes <- r.Header.Values("X-Harbor-Event-Type")
	})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = runInspectEventsAgainst(ctx, &bytes.Buffer{}, inspectEventsOpts{
		Endpoint: srv.URL,
		Filter: inspectFilter{
			Tenant: "t1", User: "u1", Sess: "s1",
			Types: []string{"task.completed", "planner.decision"},
		},
		Auth:       inspectAuth{Token: "j"},
		JSON:       false,
		Follow:     false,
		Client:     srv.Client(),
		IdleCutoff: 200 * time.Millisecond,
	}, func(cli CLIError) error { return cli })

	select {
	case got := <-sawTypes:
		if len(got) != 2 || got[0] != "task.completed" || got[1] != "planner.decision" {
			t.Errorf("X-Harbor-Event-Type = %v; want [task.completed planner.decision]", got)
		}
	case <-time.After(2 * time.Second):
		t.Error("server never received the request")
	}
}

// TestInspectEvents_BadStreamStatus_FailsLoud — the server returns
// 403; the CLI surfaces it as stream_failed with the body included.
func TestInspectEvents_BadStreamStatus_FailsLoud(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "scope_mismatch", http.StatusForbidden)
	}))
	defer srv.Close()

	var captured CLIError
	emit := func(cli CLIError) error {
		captured = cli
		return cli
	}
	err := runInspectEventsAgainst(context.Background(), &bytes.Buffer{}, inspectEventsOpts{
		Endpoint:   srv.URL,
		Filter:     inspectFilter{Tenant: "t", User: "u", Sess: "s"},
		Auth:       inspectAuth{Token: "j"},
		Follow:     false,
		Client:     srv.Client(),
		IdleCutoff: 500 * time.Millisecond,
	}, emit)
	if err == nil {
		t.Fatal("expected error")
	}
	if captured.Code != CodeStreamFailed {
		t.Errorf("captured.Code = %q, want %q", captured.Code, CodeStreamFailed)
	}
}

// assertGolden compares got to testdata/golden/<name>. When -update is
// passed (`go test ./cmd/harbor/... -args -update`), failing
// comparisons rewrite the golden to got.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if updateRequested() {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // golden testdata
			t.Fatalf("write golden %s: %v", path, err)
		}
		t.Logf("updated golden %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to seed)", path, err)
	}
	if got != string(want) {
		t.Errorf("golden mismatch (%s).\n--- want ---\n%s\n--- got ---\n%s\n--- end ---", path, want, got)
	}
}

// init helper — make sure the testdata/golden dir exists so -update
// runs do not fail on a fresh checkout.
func init() {
	_ = os.MkdirAll(filepath.Join("testdata", "golden"), 0o755)
	// Suppress fmt unused-import in case the test file shrinks during
	// future edits (defensive).
	_ = fmt.Sprintf
}
