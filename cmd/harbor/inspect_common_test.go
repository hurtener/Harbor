// cmd/harbor/inspect_common_test.go — unit tests for the shared
// Phase 69 inspect-* plumbing.

package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestResolveToken_FromEnv_PreferredOverFile — HARBOR_TOKEN beats
// the file, even when the file is present and non-empty.
func TestResolveToken_FromEnv_PreferredOverFile(t *testing.T) {
	auth, err := resolveToken(
		func(string) string { return "jwt-from-env" },
		func() (string, error) { return "/tmp/notused", nil },
		func(string) ([]byte, error) { return []byte("jwt-from-file"), nil },
	)
	if err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	if auth.Token != "jwt-from-env" || auth.Source != "env" {
		t.Errorf("got Token=%q Source=%q; want jwt-from-env / env", auth.Token, auth.Source)
	}
}

// TestResolveToken_FromFile_WhenEnvAbsent — empty env, present file →
// file source.
func TestResolveToken_FromFile_WhenEnvAbsent(t *testing.T) {
	auth, err := resolveToken(
		func(string) string { return "" },
		func() (string, error) { return "/fake/home", nil },
		func(p string) ([]byte, error) {
			if !strings.HasSuffix(p, ".harbor/token") {
				return nil, errors.New("unexpected path: " + p)
			}
			return []byte("jwt-from-file\n"), nil
		},
	)
	if err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	if auth.Token != "jwt-from-file" || auth.Source != "file" {
		t.Errorf("got Token=%q Source=%q; want jwt-from-file / file", auth.Token, auth.Source)
	}
}

// TestResolveToken_FailLoud_NoEnv_NoFile — both sources empty → fail
// loud with auth_required (§13 "fail loudly at boot when a required
// external dependency is missing", applied to the Bearer token).
func TestResolveToken_FailLoud_NoEnv_NoFile(t *testing.T) {
	_, err := resolveToken(
		func(string) string { return "" },
		func() (string, error) { return "/fake/home", nil },
		func(string) ([]byte, error) { return nil, os.ErrNotExist },
	)
	if err == nil {
		t.Fatal("resolveToken returned nil error; want fail-loud CLIError")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("err is %T, want CLIError: %v", err, err)
	}
	if cli.Code != CodeAuthRequired {
		t.Errorf("CLIError.Code = %q, want %q", cli.Code, CodeAuthRequired)
	}
	if !strings.Contains(cli.Message, envHarborToken) {
		t.Errorf("CLIError.Message %q should mention %s", cli.Message, envHarborToken)
	}
}

// TestResolveToken_FailLoud_EmptyFile — file exists but is empty (or
// whitespace-only) → fail loud rather than send a Bearer with an
// empty token (which the Runtime would 401 on anyway, but the CLI's
// error message is more actionable).
func TestResolveToken_FailLoud_EmptyFile(t *testing.T) {
	_, err := resolveToken(
		func(string) string { return "" },
		func() (string, error) { return "/fake/home", nil },
		func(string) ([]byte, error) { return []byte("   \n"), nil },
	)
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("err is %T, want CLIError", err)
	}
	if cli.Code != CodeAuthRequired {
		t.Errorf("CLIError.Code = %q, want %q", cli.Code, CodeAuthRequired)
	}
}

// TestInspectEndpoint_BareHostPort — common case: --bind 127.0.0.1:18080
// composes to http://127.0.0.1:18080/v1/events.
func TestInspectEndpoint_BareHostPort(t *testing.T) {
	got, err := inspectEndpoint("127.0.0.1:18080")
	if err != nil {
		t.Fatalf("inspectEndpoint: %v", err)
	}
	if got != "http://127.0.0.1:18080/v1/events" {
		t.Errorf("got %q, want http://127.0.0.1:18080/v1/events", got)
	}
}

// TestInspectEndpoint_FullURL — operator passing http://host/some/prefix
// gets /v1/events appended cleanly.
func TestInspectEndpoint_FullURL(t *testing.T) {
	got, err := inspectEndpoint("https://harbor.example.com")
	if err != nil {
		t.Fatalf("inspectEndpoint: %v", err)
	}
	if got != "https://harbor.example.com/v1/events" {
		t.Errorf("got %q, want https://harbor.example.com/v1/events", got)
	}
}

// TestInspectEndpoint_Empty_FailsLoud — empty bind = fail-loud
// bind_invalid; no silent default.
func TestInspectEndpoint_Empty_FailsLoud(t *testing.T) {
	_, err := inspectEndpoint("   ")
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("err is %T, want CLIError", err)
	}
	if cli.Code != CodeBindInvalid {
		t.Errorf("CLIError.Code = %q, want %q", cli.Code, CodeBindInvalid)
	}
}

// TestInspectFilter_Validate_RejectsIncompleteIdentity — identity is
// mandatory at the Protocol edge (CLAUDE.md §6 rule 9). The CLI fails
// at its OWN edge so the error message names the missing --flag.
func TestInspectFilter_Validate_RejectsIncompleteIdentity(t *testing.T) {
	cases := []inspectFilter{
		{}, // all empty
		{Tenant: "t"},
		{Tenant: "t", User: "u"},
		{User: "u", Sess: "s"},
	}
	for _, f := range cases {
		err := f.validate()
		if err == nil {
			t.Errorf("validate(%+v) returned nil; want CodeIdentityIncomplete", f)
			continue
		}
		var cli CLIError
		if !errors.As(err, &cli) || cli.Code != CodeIdentityIncomplete {
			t.Errorf("validate(%+v) = %v; want CodeIdentityIncomplete", f, err)
		}
	}
}

// TestInspectFilter_ApplyHeaders_WritesBearerAndIdentity — the headers
// the SSE handler reads are present after applyHeaders runs.
func TestInspectFilter_ApplyHeaders_WritesBearerAndIdentity(t *testing.T) {
	f := inspectFilter{
		Tenant: "t1", User: "u1", Sess: "s1", Run: "r1",
		Types: []string{"task.spawned", "task.completed"},
		Since: "42",
	}
	auth := inspectAuth{Token: "test-jwt"}
	req, _ := http.NewRequest("GET", "http://localhost/v1/events", nil)
	f.applyHeaders(req, auth)
	if got := req.Header.Get("Authorization"); got != "Bearer test-jwt" {
		t.Errorf("Authorization = %q", got)
	}
	if req.Header.Get("X-Harbor-Tenant") != "t1" {
		t.Errorf("X-Harbor-Tenant missing")
	}
	if req.Header.Get("X-Harbor-User") != "u1" {
		t.Errorf("X-Harbor-User missing")
	}
	if req.Header.Get("X-Harbor-Session") != "s1" {
		t.Errorf("X-Harbor-Session missing")
	}
	if req.Header.Get("X-Harbor-Run") != "r1" {
		t.Errorf("X-Harbor-Run missing")
	}
	if req.Header.Get("Last-Event-ID") != "42" {
		t.Errorf("Last-Event-ID missing")
	}
	types := req.Header.Values("X-Harbor-Event-Type")
	if len(types) != 2 || types[0] != "task.spawned" || types[1] != "task.completed" {
		t.Errorf("X-Harbor-Event-Type = %v; want [task.spawned task.completed]", types)
	}
}

// TestReadSSE_BasicFrame — one event-type/id/data/blank-line frame
// decodes cleanly.
func TestReadSSE_BasicFrame(t *testing.T) {
	body := "event: task.spawned\nid: 1\ndata: {\"hello\":\"world\"}\n\n"
	r := bufio.NewReader(strings.NewReader(body))
	frame, err := readSSE(r)
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if frame.Event != "task.spawned" {
		t.Errorf("Event = %q", frame.Event)
	}
	if frame.ID != "1" {
		t.Errorf("ID = %q", frame.ID)
	}
	if frame.Data != `{"hello":"world"}` {
		t.Errorf("Data = %q", frame.Data)
	}
}

// TestReadSSE_CommentFrame — `:keepalive` arrives as a comment-only
// frame; we surface it so the caller can suppress / count separately.
func TestReadSSE_CommentFrame(t *testing.T) {
	body := ": keepalive\n\n"
	r := bufio.NewReader(strings.NewReader(body))
	frame, err := readSSE(r)
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if frame.Comment != "keepalive" {
		t.Errorf("Comment = %q", frame.Comment)
	}
	if frame.Event != "" || frame.Data != "" {
		t.Errorf("non-comment fields populated unexpectedly")
	}
}

// TestReadSSE_EOFReturnsError — clean end-of-stream surfaces io.EOF.
func TestReadSSE_EOFReturnsError(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(""))
	_, err := readSSE(r)
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v; want io.EOF", err)
	}
}

// TestInspectSSE_ContextCancellationStopsLoop — closing ctx propagates
// to the http.Client and terminates the read loop cleanly.
func TestInspectSSE_ContextCancellationStopsLoop(t *testing.T) {
	// Stand up an SSE server that emits frames forever until the
	// client disconnects. The test cancels its ctx after one frame.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Errorf("server: Authorization = %q", got)
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// emit one event then block until the client disconnects
		_, _ = w.Write([]byte("event: task.spawned\nid: 1\ndata: {\"t\":\"ok\"}\n\n"))
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan struct{}, 1)
	err := inspectSSE(ctx, srv.Client(), srv.URL,
		inspectFilter{Tenant: "t", User: "u", Sess: "s"},
		inspectAuth{Token: "t"},
		func(frame sseFrame) (bool, error) {
			if frame.Data != "" {
				got <- struct{}{}
				cancel()
			}
			return false, nil
		},
	)
	if err != nil {
		// A cancelled stream may surface as context.Canceled; that is
		// not a CLI failure (inspectSSE absorbs it).
		t.Errorf("inspectSSE returned err = %v; want nil (cancellation absorbed)", err)
	}
	select {
	case <-got:
	default:
		t.Error("visitor never saw a frame")
	}
}

// TestInspectSSE_NonOKStatusFailsLoud — a 401 from the server surfaces
// as CLIError{Code:stream_failed} naming the status.
func TestInspectSSE_NonOKStatusFailsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no token", http.StatusUnauthorized)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := inspectSSE(ctx, srv.Client(), srv.URL,
		inspectFilter{Tenant: "t", User: "u", Sess: "s"},
		inspectAuth{Token: "bad"},
		func(sseFrame) (bool, error) { return false, nil },
	)
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("err is %T, want CLIError", err)
	}
	if cli.Code != CodeStreamFailed {
		t.Errorf("Code = %q, want %q", cli.Code, CodeStreamFailed)
	}
	if !strings.Contains(cli.Message, "401") {
		t.Errorf("message %q should mention 401", cli.Message)
	}
}
