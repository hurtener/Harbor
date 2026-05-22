// cmd/harbor/cmd_inspect_topology.go — Phase 70 (D-102): the
// `harbor inspect-topology <run-id>` subcommand. Renders the run's
// node graph as deterministic ASCII (golden-pinned). Per D-102 the
// topology is trajectory-synthesised from the Phase 60 SSE event
// stream — `topology.snapshot` events (the canonical source) land
// with Phase 74; until then the synthesiser scrapes
// `tool.invoked` / `tool.completed` / `task.spawned` / `pause.requested`
// / `planner.finish` from the run-filtered SSE stream and assembles
// an indent-based tree.
//
// # Auth posture
//
// Same Bearer-token surface as Phase 64's `harbor dev` mints: an
// `HARBOR_TOKEN` env var OR a file at `~/.harbor/token`. Operators
// running against the local dev stack grep `HARBOR_DEV_TOKEN=...` out
// of the dev server's stderr log and `export HARBOR_TOKEN=$VALUE`.
// Phase 69 ships the matching helper for inspect-events / inspect-runs;
// this file uses an in-package equivalent so the two CLI bodies can
// land in parallel-worktree merges without colliding on a shared
// helper file. When Phase 69's helper merges, the two implementations
// can be consolidated in a follow-up.
//
// # Identity-filter flags
//
// `--tenant`, `--user`, `--session` populate the optional
// `X-Harbor-Tenant` / `-User` / `-Session` headers — the Phase 60
// SSE transport honours these when the auth middleware has not
// already attached an identity to the request ctx. In the common
// case the Bearer-token's claims are the source of truth; the flags
// are present so a fleet operator (`admin` scope) can sub-narrow
// past their token's wide claims.
//
// # Idle timeout
//
// The SSE stream is open-ended. `inspect-topology` is a snapshot
// command — it reads until either the run terminates
// (`planner.finish` arrives) OR the bus is idle for
// `--idle-timeout` (default 1500ms — two SSE keepalive intervals at
// the Phase 60 default of 800ms gives a small buffer). The default
// is generous enough to capture a freshly-started run that's still
// building tools but tight enough to keep the CLI snappy.
//
// # JSON mode
//
// `--json` emits `RenderJSON(topology)` instead of the ASCII tree.
// The JSON shape is the same `Topology` struct serialised — useful
// for piping into `jq` or the future Console topology page (which
// will eventually read `topology.snapshot` frames directly per
// Phase 74; until then the CLI's JSON output is the same shape).

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Stable CLI error codes for `harbor inspect-topology`. New codes
// ADD entries to this block; existing codes are wire contracts pinned
// by the smoke + the cmd_inspect_topology_test.go table.
const (
	// CodeInspectTopologyBindInvalid — `--bind` did not parse as a
	// host:port. Exit 1.
	CodeInspectTopologyBindInvalid = "inspect_topology_bind_invalid"
	// CodeInspectTopologyAuthMissing — no Bearer token resolved from
	// the environment (HARBOR_TOKEN + ~/.harbor/token both empty).
	// Exit 1.
	CodeInspectTopologyAuthMissing = "inspect_topology_auth_missing"
	// CodeInspectTopologyRunIDMissing — no positional `<run-id>` arg.
	// Exit 1.
	CodeInspectTopologyRunIDMissing = "inspect_topology_run_id_missing"
	// CodeInspectTopologyConnectFailed — could not open the SSE stream
	// (transport error, refused connection). Exit 2.
	CodeInspectTopologyConnectFailed = "inspect_topology_connect_failed"
	// CodeInspectTopologyHTTPStatus — non-200 HTTP status from the
	// SSE endpoint. Exit 1 if 4xx (caller bug), 2 if 5xx (server bug).
	CodeInspectTopologyHTTPStatus = "inspect_topology_http_status"
	// CodeInspectTopologyRunNotFound — the run produced no events
	// within the idle timeout AND nothing already replayed. Exit 1.
	CodeInspectTopologyRunNotFound = "inspect_topology_run_not_found"
	// CodeInspectTopologyWidthInvalid — `--width` outside the accepted
	// range [MinRenderWidth, MaxRenderWidth]. Exit 1.
	CodeInspectTopologyWidthInvalid = "inspect_topology_width_invalid"
)

// Flag names declared as constants so the cmd body, tests, and any
// future docs reference one spelling.
const (
	flagInspectTopologyBind        = "bind"
	flagInspectTopologyTenant      = "tenant"
	flagInspectTopologyUser        = "user"
	flagInspectTopologySession     = "session"
	flagInspectTopologyWidth       = "width"
	flagInspectTopologyIdleTimeout = "idle-timeout"
)

// DefaultInspectTopologyBind matches Phase 64's default dev port so
// the common case (`harbor dev` running locally) needs no flags.
const DefaultInspectTopologyBind = "127.0.0.1:18080"

// DefaultInspectTopologyIdleTimeout is the no-event-arrived window
// after which the CLI assumes the run is quiescent and renders what
// it has. 1500ms is generous against the Phase 60 default keepalive
// of 800ms — one missed keepalive + the next is still under threshold.
const DefaultInspectTopologyIdleTimeout = 1500 * time.Millisecond

// EnvInspectTopologyToken is the env var name the cmd reads the
// Bearer token from. Mirrors the convention `harbor dev` uses when
// it prints `HARBOR_DEV_TOKEN=...` to stderr; operators are expected
// to set HARBOR_TOKEN=$HARBOR_DEV_TOKEN in their shell.
// Alias for `envHarborToken` (inspect_common.go). The two constants
// share the same string; the topology cmd kept its own name so its
// test surface stays self-contained.
const EnvInspectTopologyToken = envHarborToken

// DefaultTokenPath is the on-disk fallback path the cmd consults when
// the env var is unset. `~/.harbor/token` matches the convention CLI
// tools use (kubectl, gh, etc.).
// Alias for `tokenFileRel` (inspect_common.go). Same string; kept
// here so the topology test surface doesn't depend on an unexported
// const in a different file.
const DefaultTokenPath = tokenFileRel

// newInspectTopologyCmd builds the cobra command.
func newInspectTopologyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect-topology <run-id>",
		Short: "render a run's node graph as ASCII",
		Long: `Render the topology of a Harbor run as an ASCII node graph.

The CLI opens an SSE subscription against the connected Runtime
(default 127.0.0.1:18080), filters for events tagged with the supplied
RunID via the X-Harbor-Run header, and reads frames until either a
planner.finish event arrives OR the bus is idle for --idle-timeout.

Authentication: a Bearer token is read from $HARBOR_TOKEN, with
~/.harbor/token as a fallback. For local development run "harbor dev"
in another terminal and export the printed HARBOR_DEV_TOKEN value as
HARBOR_TOKEN.

Output: deterministic indent-based ASCII (golden-pinned) by default,
or --json for a structured Topology shape. Both forms are byte-stable
for a given input event ordering (sorted by Sequence + EventID).

Topology source: trajectory-synthesised from tool.invoked /
tool.completed / task.spawned / pause.requested / planner.finish
events. When Phase 74 ships the canonical topology.snapshot event,
this command will prefer that source automatically.

Examples:
  harbor inspect-topology run-abc-123
  harbor inspect-topology --bind 127.0.0.1:8080 --width 120 run-abc-123
  HARBOR_TOKEN=$(cat /tmp/token) harbor inspect-topology --json run-x`,
		Args: cobra.ExactArgs(1),
		RunE: runInspectTopology,
	}
	cmd.Flags().String(flagInspectTopologyBind, DefaultInspectTopologyBind, "target Runtime host:port")
	cmd.Flags().String(flagInspectTopologyTenant, "", "optional tenant filter (X-Harbor-Tenant)")
	cmd.Flags().String(flagInspectTopologyUser, "", "optional user filter (X-Harbor-User)")
	cmd.Flags().String(flagInspectTopologySession, "", "optional session filter (X-Harbor-Session)")
	cmd.Flags().Int(flagInspectTopologyWidth, DefaultRenderWidth, "max line width for the ASCII renderer")
	cmd.Flags().Duration(flagInspectTopologyIdleTimeout, DefaultInspectTopologyIdleTimeout, "stop reading after this much idle time")
	return cmd
}

// runInspectTopology is the cobra RunE entry. Resolves flags, fetches
// the SSE stream, parses frames, synthesises the topology, renders.
func runInspectTopology(cmd *cobra.Command, args []string) error {
	if len(args) == 0 || args[0] == "" {
		return emitCLIError(cmd, CLIError{
			Subcommand: "inspect-topology",
			Message:    "missing required positional <run-id>",
			Code:       CodeInspectTopologyRunIDMissing,
			Hint:       "usage: harbor inspect-topology <run-id>",
		})
	}
	runID := args[0]

	// Every flag below is statically registered on this command, so the
	// GetX lookups cannot fail; the blank-error discards are intentional.
	bind, _ := cmd.Flags().GetString(flagInspectTopologyBind)          //nolint:errcheck // flag statically registered; lookup cannot fail
	tenant, _ := cmd.Flags().GetString(flagInspectTopologyTenant)      //nolint:errcheck // flag statically registered; lookup cannot fail
	user, _ := cmd.Flags().GetString(flagInspectTopologyUser)          //nolint:errcheck // flag statically registered; lookup cannot fail
	session, _ := cmd.Flags().GetString(flagInspectTopologySession)    //nolint:errcheck // flag statically registered; lookup cannot fail
	width, _ := cmd.Flags().GetInt(flagInspectTopologyWidth)           //nolint:errcheck // flag statically registered; lookup cannot fail
	idle, _ := cmd.Flags().GetDuration(flagInspectTopologyIdleTimeout) //nolint:errcheck // flag statically registered; lookup cannot fail

	if err := validateInspectBind(bind); err != nil {
		return emitCLIError(cmd, CLIError{
			Subcommand: "inspect-topology",
			Message:    err.Error(),
			Code:       CodeInspectTopologyBindInvalid,
			Hint:       "use --bind host:port (e.g. --bind 127.0.0.1:18080)",
		})
	}
	if width != 0 && (width < MinRenderWidth || width > MaxRenderWidth) {
		return emitCLIError(cmd, CLIError{
			Subcommand: "inspect-topology",
			Message:    fmt.Sprintf("--width %d outside accepted range [%d, %d]", width, MinRenderWidth, MaxRenderWidth),
			Code:       CodeInspectTopologyWidthInvalid,
			Hint:       fmt.Sprintf("pass --width between %d and %d (default %d)", MinRenderWidth, MaxRenderWidth, DefaultRenderWidth),
		})
	}

	token, tokenErr := resolveInspectToken()
	if tokenErr != nil {
		return emitCLIError(cmd, CLIError{
			Subcommand: "inspect-topology",
			Message:    tokenErr.Error(),
			Code:       CodeInspectTopologyAuthMissing,
			Hint:       "set HARBOR_TOKEN or place a Bearer token at ~/.harbor/token (export HARBOR_TOKEN=\"$HARBOR_DEV_TOKEN\" after 'harbor dev')",
		})
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), idle+10*time.Second)
	defer cancel()

	body, fetchErr := fetchSSEUntilIdle(ctx, sseFetchOpts{
		Bind:        bind,
		Token:       token,
		Tenant:      tenant,
		User:        user,
		Session:     session,
		RunID:       runID,
		IdleTimeout: idle,
	})
	if fetchErr != nil {
		return emitCLIError(cmd, fetchErrorToCLIError(fetchErr))
	}

	frames, _, parseErr := ParseSSEFrames(body, runID)
	if parseErr != nil {
		// A malformed SSE frame is a server bug, not a caller bug.
		return emitCLIError(cmd, CLIError{
			Subcommand: "inspect-topology",
			Message:    fmt.Sprintf("parse SSE frames: %v", parseErr),
			Code:       CodeInspectTopologyConnectFailed,
			Hint:       "this usually indicates a Runtime version mismatch; check the server log",
		})
	}

	if len(frames) == 0 {
		return emitCLIError(cmd, CLIError{
			Subcommand: "inspect-topology",
			Message:    fmt.Sprintf("no events for run %q within %s", runID, idle),
			Code:       CodeInspectTopologyRunNotFound,
			Hint:       "verify the run ID, ensure the run has started, or increase --idle-timeout",
		})
	}

	topology := BuildTopologyFromEvents(runID, frames)
	if topology.Tenant == "" && tenant != "" {
		topology.Tenant = tenant
	}
	if topology.User == "" && user != "" {
		topology.User = user
	}
	if topology.Session == "" && session != "" {
		topology.Session = session
	}

	if resolveJSONMode(cmd) {
		out, err := RenderJSON(topology)
		if err != nil {
			return emitCLIError(cmd, CLIError{
				Subcommand: "inspect-topology",
				Message:    fmt.Sprintf("render JSON: %v", err),
				Code:       CodeInspectTopologyConnectFailed,
				Hint:       "internal renderer error; please report",
			})
		}
		if _, wErr := cmd.OutOrStdout().Write(out); wErr != nil {
			return emitCLIError(cmd, CLIError{
				Subcommand: "inspect-topology",
				Message:    fmt.Sprintf("write JSON output: %v", wErr),
				Code:       CodeInspectTopologyConnectFailed,
				Hint:       "stdout write failed; check the output pipe",
			})
		}
		return nil
	}

	rendered, err := Render(topology, width)
	if err != nil {
		return emitCLIError(cmd, CLIError{
			Subcommand: "inspect-topology",
			Message:    fmt.Sprintf("render ASCII: %v", err),
			Code:       CodeInspectTopologyConnectFailed,
			Hint:       "internal renderer error; please report",
		})
	}
	if _, wErr := cmd.OutOrStdout().Write(rendered); wErr != nil {
		return emitCLIError(cmd, CLIError{
			Subcommand: "inspect-topology",
			Message:    fmt.Sprintf("write ASCII output: %v", wErr),
			Code:       CodeInspectTopologyConnectFailed,
			Hint:       "stdout write failed; check the output pipe",
		})
	}
	return nil
}

// validateInspectBind checks bind for a host:port shape. Same rule the
// dev cmd applies to HARBOR_BIND; the function is local so cmd/harbor
// does not depend on a shared helper file (the parallel-worktree merge
// constraint named in the prompt).
// W3 partial — `resolveInspectToken` and the constants collapsed to
// the Phase 69 canonical helpers (inspect_common.go). The bind
// validator is intentionally NOT collapsed: `inspectEndpoint` (Phase
// 69) is a URL composer that accepts any non-empty `bind` and prefixes
// `http://...`; it does NOT enforce host:port shape. The topology cmd
// uses a strict host:port shape check so a malformed bind surfaces
// CodeInspectTopologyBindInvalid at the CLI edge BEFORE any network
// call. The two helpers serve different purposes — the duplication
// the audit flagged was over-broad on this one.
func validateInspectBind(bind string) error {
	if bind == "" {
		return fmt.Errorf("--bind is empty")
	}
	i := strings.LastIndex(bind, ":")
	if i < 0 || i == len(bind)-1 {
		return fmt.Errorf("--bind %q is not host:port", bind)
	}
	port := bind[i+1:]
	for _, c := range port {
		if c < '0' || c > '9' {
			return fmt.Errorf("--bind %q has non-numeric port", bind)
		}
	}
	return nil
}

// resolveInspectToken reads the Bearer token from the standard
// locations (env var → ~/.harbor/token). Delegates to the Phase 69
// canonical helper. Returns an error when both sources are empty /
// unreadable so the caller can surface CodeAuthMissing.
func resolveInspectToken() (string, error) {
	auth, err := resolveTokenFromOS()
	if err != nil {
		var ce CLIError
		if errors.As(err, &ce) {
			// Preserve the rich Phase 69 message + hint.
			return "", errors.New(ce.Message)
		}
		return "", err
	}
	return auth.Token, nil
}

// sseFetchOpts bundles the inputs to fetchSSEUntilIdle. Kept as a
// struct so the test surface can drive the fetcher in isolation
// (the cmd_inspect_topology_test.go suite spins httptest.Server
// instances and exercises the body-parse + idle-timeout paths).
type sseFetchOpts struct {
	Bind        string
	Token       string
	Tenant      string
	User        string
	Session     string
	RunID       string
	IdleTimeout time.Duration
}

// fetchSSEUntilIdle opens a GET /v1/events SSE stream against the
// target Runtime and reads until either:
//   - the connection produced a `planner.finish` event whose Run
//     matches RunID (run terminated cleanly), OR
//   - no new event arrived for opts.IdleTimeout (run quiescent), OR
//   - the parent ctx cancels (operator interrupted or timeout
//     exceeded).
//
// Returns the raw SSE body bytes (suitable for ParseSSEFrames) plus
// the HTTP status code so the caller can distinguish transport
// failure from "we read some bytes successfully but the run was
// empty."
//
// Wraps a typed error shape:
//   - fetchError{Kind: "connect", Err: ...} for transport failure
//   - fetchError{Kind: "status", Status: N} for non-200 responses
//   - nil error on a clean read OR an idle-timeout (both are "we got
//     what we could").
func fetchSSEUntilIdle(ctx context.Context, opts sseFetchOpts) ([]byte, error) {
	u := url.URL{Scheme: "http", Host: opts.Bind, Path: "/v1/events"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fetchError{Kind: "connect", Err: err}
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Authorization", "Bearer "+opts.Token)
	// Last-Event-ID: 0 requests replay from the start of the run.
	// The Phase 60 stream handler maps this to a Cursor{Sequence: 0}
	// which the bus driver replays everything strictly greater than
	// — i.e. the whole run.
	req.Header.Set("Last-Event-ID", "0")
	// Run filter is INTENTIONALLY NOT set via X-Harbor-Run. The Phase 60
	// SSE handler filters server-side by `Event.Identity.RunID`, which
	// is EMPTY on the load-bearing `task.spawned` event (the `start`
	// Protocol method dispatches `Quadruple{Identity: id}` — RunID is
	// populated later by the per-task RunLoop driver from the TaskID).
	// Setting the server-side filter would drop the spawn event from
	// the stream, leaving the topology synthesiser with no Task node
	// to render. The CLI filters client-side via `runIDFromFrame` in
	// `ParseSSEFrames` (TaskID-payload fallback) — same projection
	// Phase 69's inspect-runs uses (D-101). Tracked: extend the Phase
	// 60 stream to fall back to payload TaskID for task.spawned in a
	// future PR; until then the CLI carries the projection.
	if opts.Tenant != "" {
		req.Header.Set("X-Harbor-Tenant", opts.Tenant)
	}
	if opts.User != "" {
		req.Header.Set("X-Harbor-User", opts.User)
	}
	if opts.Session != "" {
		req.Header.Set("X-Harbor-Session", opts.Session)
	}

	client := &http.Client{
		// Do NOT set Timeout — SSE is long-lived. Cancellation is
		// driven by ctx (which the caller's idle loop owns).
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fetchError{Kind: "connect", Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Drain a bounded prefix of the body so the error message
		// can quote the server-side reason.
		prefix := make([]byte, 256)
		//nolint:errcheck // a short read (EOF/ErrUnexpectedEOF) is expected and fine — n bounds the slice below
		n, _ := io.ReadFull(resp.Body, prefix)
		return nil, fetchError{
			Kind:   "status",
			Status: resp.StatusCode,
			Body:   string(prefix[:n]),
		}
	}

	// Read the SSE body in idle-aware chunks.
	return readSSEUntilIdle(ctx, resp.Body, opts.IdleTimeout, opts.RunID)
}

// readSSEUntilIdle reads chunks of body until either:
//   - a planner.finish event arrives whose data line names runFilter,
//   - no chunk arrived for idleTimeout,
//   - ctx cancels.
//
// Returns the accumulated bytes; the caller's ParseSSEFrames does the
// real parsing. The "is this the terminal event" check here is a
// substring match against the chunk — defensive (we read full SSE
// frames including terminating blank line, so the substring is
// reliable for the canonical encoder shape).
func readSSEUntilIdle(ctx context.Context, body io.Reader, idleTimeout time.Duration, runFilter string) ([]byte, error) {
	br := bufio.NewReaderSize(body, 8192)
	var accumulated bytes.Buffer
	// chunkCh carries each accumulated SSE event (between blank-line
	// separators). The reader goroutine pushes; the select loop pops
	// with an idle timer.
	chunkCh := make(chan []byte, 16)
	errCh := make(chan error, 1)
	readerCtx, cancelReader := context.WithCancel(ctx)
	defer cancelReader()
	go func() {
		var frame bytes.Buffer
		for {
			if readerCtx.Err() != nil {
				errCh <- readerCtx.Err()
				return
			}
			line, err := br.ReadString('\n')
			if len(line) > 0 {
				frame.WriteString(line)
				// SSE frames terminate on blank line.
				if line == "\n" || line == "\r\n" {
					out := make([]byte, frame.Len())
					copy(out, frame.Bytes())
					frame.Reset()
					select {
					case chunkCh <- out:
					case <-readerCtx.Done():
						errCh <- readerCtx.Err()
						return
					}
				}
			}
			if err != nil {
				if frame.Len() > 0 {
					out := make([]byte, frame.Len())
					copy(out, frame.Bytes())
					select {
					case chunkCh <- out:
					case <-readerCtx.Done():
					}
				}
				errCh <- err
				return
			}
		}
	}()

	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	finishSearch := []byte(`"type":"planner.finish"`)
	runSearch := []byte(`"run":"` + runFilter + `"`)

	for {
		select {
		case <-ctx.Done():
			return accumulated.Bytes(), nil
		case chunk := <-chunkCh:
			accumulated.Write(chunk)
			// Reset the idle timer on every chunk (keepalive or
			// real frame). The keepalive frames are filtered out
			// by ParseSSEFrames; here we treat any traffic as
			// "stream is alive".
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idleTimeout)
			// Terminal-detection: planner.finish for our run.
			if bytes.Contains(chunk, finishSearch) && (runFilter == "" || bytes.Contains(chunk, runSearch)) {
				return accumulated.Bytes(), nil
			}
		case <-timer.C:
			return accumulated.Bytes(), nil
		case err := <-errCh:
			// EOF / closed connection — return what we have.
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return accumulated.Bytes(), nil
			}
			return accumulated.Bytes(), nil
		}
	}
}

// fetchError is the typed transport error fetchSSEUntilIdle returns
// for failure paths. Kind selects the CLIError shape.
type fetchError struct {
	Kind   string // "connect" | "status"
	Err    error
	Status int
	Body   string
}

func (e fetchError) Error() string {
	switch e.Kind {
	case "status":
		return fmt.Sprintf("SSE endpoint returned HTTP %d: %s", e.Status, e.Body)
	case "connect":
		if e.Err != nil {
			return fmt.Sprintf("connect: %v", e.Err)
		}
		return "connect: unknown error"
	}
	return "fetch: unknown error"
}

// fetchErrorToCLIError maps a fetchError onto the structured CLIError
// surface. 4xx → exit 1 (caller bug — bad token / unknown run / scope
// mismatch); 5xx → exit 2 (server bug — surface to operator).
func fetchErrorToCLIError(err error) CLIError {
	var fe fetchError
	if errors.As(err, &fe) {
		switch fe.Kind {
		case "status":
			hint := "verify the Bearer token claims and that the Runtime is healthy at the given --bind"
			switch fe.Status {
			case http.StatusUnauthorized:
				hint = "the Bearer token was rejected — check HARBOR_TOKEN claims and expiry"
			case http.StatusForbidden:
				hint = "the token's scope did not authorise this subscription — admin scope is required for cross-tenant reads"
			case http.StatusNotFound:
				hint = "the /v1/events endpoint was not found — is the Runtime running Phase 60 or later?"
			}
			return CLIError{
				Subcommand: "inspect-topology",
				Message:    fe.Error(),
				Code:       CodeInspectTopologyHTTPStatus,
				Hint:       hint,
			}
		case "connect":
			return CLIError{
				Subcommand: "inspect-topology",
				Message:    fe.Error(),
				Code:       CodeInspectTopologyConnectFailed,
				Hint:       "verify the Runtime is bound on the given host:port (try `curl " + DefaultInspectTopologyBind + "/healthz`)",
			}
		}
	}
	return CLIError{
		Subcommand: "inspect-topology",
		Message:    fmt.Sprintf("fetch: %v", err),
		Code:       CodeInspectTopologyConnectFailed,
		Hint:       "transport-layer failure; check the network and the Runtime status",
	}
}
