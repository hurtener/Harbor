// cmd/harbor/cmd_inspect_events.go — `harbor inspect-events` (Phase 69,
// D-101).
//
// Tails or snapshots the Phase 60 SSE event stream against a running
// Harbor Runtime. Identity-scoped filtering by tenant/user/session/run
// plus optional repeatable --type filters land server-side via the
// existing X-Harbor-Event-Type carrier header (no new Protocol method
// — the consumer reuses the surface Phase 60 shipped, which matches
// the §13 primitive-with-consumer rule by exercising existing
// primitives rather than minting unused ones).
//
// Output modes:
//
//   - Human (default): one event per line on stdout, ISO-8601 timestamp
//     + identity triple + run + event type + a short payload sketch.
//   - --json: the wireEvent shape verbatim (one JSON object per line —
//     newline-delimited JSON, the canonical streaming-consumer shape).
//
// The wire shape pinned by both modes is asserted by goldens
// (cmd/harbor/testdata/golden/inspect-events-*.txt). A wire-shape
// change without a CLI update fails the golden.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// newInspectEventsCmd builds the `inspect-events` cobra subcommand.
// All flags inherit through cobra's flag system; tests drive each
// flag in isolation via the public root.
func newInspectEventsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect-events",
		Short: "tail or filter the event bus of a running Runtime",
		Long: `Connect to a running Harbor Runtime's Phase 60 SSE event stream and
emit each frame on stdout. Identity-scoped filtering by tenant /
user / session / run plus optional repeatable --type filters land
server-side. Output is one event per line — human-readable by default,
canonical JSON when --json is passed (the wire shape downstream
consumers like the Console will read).

The Runtime requires a Bearer JWT. The CLI resolves it from
HARBOR_TOKEN (preferred) or ~/.harbor/token. ` + "`harbor dev`" + ` prints the
ephemeral dev token to stderr at boot under the prefix HARBOR_DEV_TOKEN=
— copy that value into HARBOR_TOKEN (or write it to ~/.harbor/token).

Examples:
  HARBOR_TOKEN=$jwt harbor inspect-events \\
    --tenant dev --user dev --session dev --type task.spawned

  harbor inspect-events --tenant dev --user dev --session dev \\
    --type task.completed --follow=false       # snapshot the replay buffer and exit

  harbor inspect-events --tenant dev --user dev --session dev \\
    --since 0 --json                            # replay from the start as NDJSON
`,
		RunE: runInspectEvents,
	}
	cmd.Flags().String(flagBind, DefaultBind, "Runtime bind (host:port or full URL)")
	cmd.Flags().String(flagTenant, "", "tenant id (required)")
	cmd.Flags().String(flagUser, "", "user id (required)")
	cmd.Flags().String(flagSession, "", "session id (required)")
	cmd.Flags().String(flagRun, "", "filter to a single run id (optional)")
	cmd.Flags().StringSlice(flagType, nil, "filter to event type(s) (repeatable; matches X-Harbor-Event-Type)")
	cmd.Flags().String(flagSince, "", "replay cursor (SSE Last-Event-ID — sequence number as decimal)")
	cmd.Flags().BoolP(flagFollow, "f", true, "keep the stream open (use --follow=false for a snapshot)")
	return cmd
}

// runInspectEvents is the cobra RunE entry. It assembles the filter +
// auth context, opens the SSE stream, and emits one line per event
// per --json. snapshot mode (--follow=false) drains the replay buffer
// (server-side replay from --since cursor; default "" = "no replay,
// live-tail only") and exits when the stream idles for
// snapshotIdleTimeout — there is no end-of-stream signal in SSE.
func runInspectEvents(cmd *cobra.Command, _ []string) error {
	// Every flag below is statically registered on this command, so the
	// GetX lookups cannot fail; the blank-error discards are intentional.
	bind, _ := cmd.Flags().GetString(flagBind) //nolint:errcheck // flag statically registered; lookup cannot fail
	jsonMode := resolveJSONMode(cmd)

	filter := inspectFilter{}
	filter.Tenant, _ = cmd.Flags().GetString(flagTenant)   //nolint:errcheck // flag statically registered; lookup cannot fail
	filter.User, _ = cmd.Flags().GetString(flagUser)       //nolint:errcheck // flag statically registered; lookup cannot fail
	filter.Sess, _ = cmd.Flags().GetString(flagSession)    //nolint:errcheck // flag statically registered; lookup cannot fail
	filter.Run, _ = cmd.Flags().GetString(flagRun)         //nolint:errcheck // flag statically registered; lookup cannot fail
	filter.Types, _ = cmd.Flags().GetStringSlice(flagType) //nolint:errcheck // flag statically registered; lookup cannot fail
	filter.Since, _ = cmd.Flags().GetString(flagSince)     //nolint:errcheck // flag statically registered; lookup cannot fail
	follow, _ := cmd.Flags().GetBool(flagFollow)           //nolint:errcheck // flag statically registered; lookup cannot fail

	if err := filter.validate(); err != nil {
		return emitCLIError(cmd, asCLIErrorOr(err, "inspect-events"))
	}

	endpoint, err := inspectEndpoint(bind)
	if err != nil {
		return emitCLIError(cmd, asCLIErrorOr(err, "inspect-events"))
	}

	auth, err := resolveTokenFromOS()
	if err != nil {
		return emitCLIError(cmd, asCLIErrorOr(err, "inspect-events"))
	}

	return runInspectEventsAgainst(cmd.Context(), cmd.OutOrStdout(), inspectEventsOpts{
		Endpoint:   endpoint,
		Filter:     filter,
		Auth:       auth,
		JSON:       jsonMode,
		Follow:     follow,
		Client:     defaultInspectClient(),
		IdleCutoff: snapshotIdleTimeout,
		Now:        time.Now,
	}, func(cli CLIError) error { return emitCLIError(cmd, cli) })
}

// inspectEventsOpts bundles the inputs runInspectEventsAgainst needs.
// Kept as a struct so tests drive each path against an httptest
// server without re-creating cobra wiring.
type inspectEventsOpts struct {
	Endpoint   string
	Filter     inspectFilter
	Auth       inspectAuth
	JSON       bool
	Follow     bool
	Client     *http.Client
	IdleCutoff time.Duration
	Now        func() time.Time
}

// runInspectEventsAgainst is the testable core: takes a fully-resolved
// options struct, drives the SSE stream, writes each event to out, and
// returns CLIError via emit. Tests pass an httptest.Server's URL via
// opts.Endpoint and a stub client.
func runInspectEventsAgainst(
	ctx context.Context,
	out io.Writer,
	opts inspectEventsOpts,
	emit func(CLIError) error,
) error {
	// Snapshot mode: bound the connection by IdleCutoff so the stream
	// closes after a quiet window. Follow mode runs until ctx cancels.
	streamCtx := ctx
	if !opts.Follow && opts.IdleCutoff > 0 {
		var cancel context.CancelFunc
		streamCtx, cancel = context.WithTimeout(ctx, opts.IdleCutoff)
		defer cancel()
	}

	err := inspectSSE(streamCtx, opts.Client, opts.Endpoint, opts.Filter, opts.Auth, func(frame sseFrame) (bool, error) {
		// Comment frames: surface keepalives + replay-gap markers under
		// --json as a sentinel; suppress under human mode (operators
		// don't need to see them).
		if frame.Comment != "" {
			if opts.JSON {
				// Emit a one-line JSON sentinel for the comment so a
				// scripting consumer can distinguish "the server is
				// alive" from a real event. The shape is documented
				// alongside wireEvent.
				rec := map[string]string{"comment": frame.Comment}
				if buf, mErr := json.Marshal(rec); mErr == nil {
					//nolint:errcheck // best-effort keepalive sentinel to CLI stdout; a write error is handled on the next real event
					_, _ = out.Write(append(buf, '\n'))
				}
			}
			return false, nil
		}
		if frame.Data == "" {
			return false, nil
		}
		var ev wireEvent
		if dErr := json.Unmarshal([]byte(frame.Data), &ev); dErr != nil {
			// A malformed `data:` payload is a Runtime bug, not a CLI
			// bug; surface it as a single error line and continue
			// (don't tear down a long-running tail for one bad frame).
			fmt.Fprintf(out, "# decode error: %v (frame: %s)\n", dErr, abbreviate(frame.Data, 200))
			return false, nil
		}
		if opts.JSON {
			// NDJSON: re-encode through wireEvent so the on-wire and
			// emitted shapes match exactly (we do NOT pass-through the
			// raw bytes — that would prevent the CLI from normalising
			// field order across Runtime versions).
			buf, mErr := json.Marshal(ev)
			if mErr != nil {
				// Re-encoding a value we just decoded should not fail;
				// surface it as an error line and continue rather than
				// silently dropping the event.
				fmt.Fprintf(out, "# encode error: %v\n", mErr)
				return false, nil
			}
			if _, wErr := out.Write(append(buf, '\n')); wErr != nil {
				return true, nil //nolint:nilerr // client hung up: stop the stream cleanly, write error is not a stream failure
			}
			return false, nil
		}
		// Human mode: ISO-8601 ts | tenant/user/session[/run] | type | payload sketch
		line := humanEventLine(ev)
		if _, wErr := fmt.Fprintln(out, line); wErr != nil {
			return true, nil //nolint:nilerr // client hung up: stop the stream cleanly, write error is not a stream failure
		}
		return false, nil
	})
	if err != nil {
		var cli CLIError
		if errors.As(err, &cli) {
			cli.Subcommand = "inspect-events"
			return emit(cli)
		}
		return emit(CLIError{
			Subcommand: "inspect-events",
			Code:       CodeStreamFailed,
			Message:    err.Error(),
		})
	}
	return nil
}

// humanEventLine renders one event in the human-mode shape pinned by
// goldens. Single-line, ISO-8601 timestamp, identity tuple, event
// type, optional payload sketch.
//
// The payload sketch is a stable map-key alphabetic listing — the
// payload IS event-typed (TaskSpawnedPayload, TaskCompletedPayload,
// PauseResumedPayload, etc.), so a deterministic deep render is not
// possible without per-type knowledge. We render top-level keys and
// scalar values; nested objects collapse to `{…}`. Heavy payloads
// truncate at 240 bytes total to keep one event = one terminal line.
func humanEventLine(ev wireEvent) string {
	id := ev.Tenant + "/" + ev.User + "/" + ev.Session
	if ev.Run != "" {
		id += "/" + ev.Run
	}
	sketch := payloadSketch(ev.Payload)
	if sketch != "" {
		return fmt.Sprintf("%s %s %s %s", ev.OccurredAt, id, ev.Type, sketch)
	}
	return fmt.Sprintf("%s %s %s", ev.OccurredAt, id, ev.Type)
}

// payloadSketch renders a one-line readable approximation of the
// payload. The shape is:
//
//	{key1=value1 key2=value2 ...}
//
// for a map; for a non-map payload we render `<type=...>`. Output is
// truncated at 240 chars to keep one event = one terminal line.
func payloadSketch(p any) string {
	if p == nil {
		return ""
	}
	m, ok := p.(map[string]any)
	if !ok {
		// Some payloads decode as primitives (e.g. counters); render
		// type + best-effort string.
		return fmt.Sprintf("<%T=%v>", p, abbreviate(fmt.Sprintf("%v", p), 80))
	}
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(renderLeaf(m[k]))
	}
	b.WriteByte('}')
	return abbreviate(b.String(), 240)
}

// renderLeaf turns one payload leaf into a single readable token.
// Nested objects collapse to `{…}`; arrays to `[N]`; scalars render
// through fmt.Sprintf("%v"). String values WITH spaces wrap in quotes.
func renderLeaf(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		if strings.ContainsAny(t, " \t") {
			return fmt.Sprintf("%q", t)
		}
		return t
	case map[string]any:
		return "{…}"
	case []any:
		return fmt.Sprintf("[%d]", len(t))
	default:
		return fmt.Sprintf("%v", v)
	}
}

// abbreviate truncates s to maxLen characters, appending "…" when
// truncated. Stable shape so goldens lock the output.
func abbreviate(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return "…"
	}
	return s[:maxLen-1] + "…"
}

// asCLIErrorOr promotes err to a CLIError (preserving the original
// fields when possible) and sets Subcommand. Used at the boundary
// between the testable core (which returns plain errors) and the
// cobra glue (which needs structured CLIErrors).
func asCLIErrorOr(err error, sub string) CLIError {
	var cli CLIError
	if errors.As(err, &cli) {
		if cli.Subcommand == "" {
			cli.Subcommand = sub
		}
		return cli
	}
	return CLIError{Subcommand: sub, Message: err.Error(), Code: CodeStreamFailed}
}

// defaultInspectClient builds the http.Client the inspect subcommands
// use. The CLI has NO read timeout — SSE streams are long-lived — but
// it bounds the dial / response-header phase so a wrong --bind fails
// fast rather than hanging indefinitely.
func defaultInspectClient() *http.Client {
	return &http.Client{
		Timeout: 0, // long-lived stream
		Transport: &http.Transport{
			ResponseHeaderTimeout: 5 * time.Second,
			IdleConnTimeout:       60 * time.Second,
		},
	}
}
