// cmd/harbor/inspect_common.go — shared plumbing for the Phase 69
// inspect-events / inspect-runs subcommands.
//
// Phase 69 (D-101) graduates the two inspect subcommands from §13 stubs.
// They share the same wire shape, the same Bearer-token discovery, the
// same SSE consumption: this file centralises the helpers so neither
// subcommand body re-rolls a fetch loop, a JSON shape, or a token
// resolver.
//
// # Authentication (D-101)
//
// The CLI is a Protocol client of a running Harbor Runtime. The Runtime
// requires `Authorization: Bearer <jwt>` on every Protocol request
// (Phase 61 / D-079 — the auth.Middleware wraps both /v1/control and
// /v1/events). The CLI resolves the token from two sources, in order:
//
//  1. `HARBOR_TOKEN` env var.
//  2. `~/.harbor/token` file (operator convenience — `harbor dev`
//     prints `HARBOR_DEV_TOKEN=...` to stderr; the operator can
//     `mkdir -p ~/.harbor && echo "$HARBOR_DEV_TOKEN" > ~/.harbor/token`).
//
// Missing token at BOTH sources fails loud with `auth_required` — no
// silent fallback, no anonymous probe (CLAUDE.md §13 "fail loudly").
// The token is NEVER logged or echoed (CLAUDE.md §7 rule 2). HS* / none
// algorithms are out of scope: the CLI does not verify the token; the
// Runtime's auth.Middleware does, and that path is locked to ES256 /
// RS256 etc. (CLAUDE.md §7 rule 1).
//
// # Wire shape (D-101 / Phase 60)
//
// The Phase 60 SSE transport (`internal/protocol/transports/stream`)
// serialises each `events.Event` as a JSON `wireEvent`:
//
//	{
//	  "type": "task.spawned",
//	  "sequence": 42,
//	  "occurred_at": "2026-05-17T12:34:56.789Z",
//	  "tenant": "t1",
//	  "user":   "u1",
//	  "session":"s1",
//	  "run":    "r-abc",     (optional)
//	  "payload": { … },      (event-typed; optional)
//	  "extra":   { … }       (optional)
//	}
//
// The CLI re-parses that shape onto `wireEvent` here (NOT a re-import
// of `internal/protocol/transports/stream.wireEvent` — that struct is
// unexported precisely so wire consumers re-derive). The two structs
// are kept in lockstep by golden tests; a wire-shape change WITHOUT a
// CLI update fails the golden.
//
// CLAUDE.md §8 forbidden practice: a Protocol method that maps 1:1 onto
// an internal Go signature is a smell. `wireEvent` is on the Protocol
// wire side, not the internal side — the CLI consuming the public SSE
// stream is the correct posture.

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Stable CLI error codes for the inspect subcommands. Each value is a
// fixed string smoke / golden tests assert against.
const (
	// CodeAuthRequired — neither HARBOR_TOKEN nor ~/.harbor/token was
	// readable. The operator must mint a token (`harbor dev` prints
	// `HARBOR_DEV_TOKEN=...` to stderr; OIDC integrations land in a
	// later release-engineering phase).
	CodeAuthRequired = "auth_required"
	// CodeBindInvalid — the operator-supplied --bind value is not a
	// valid host:port.
	CodeBindInvalid = "bind_invalid"
	// CodeStreamFailed — the SSE GET against /v1/events returned a
	// non-2xx status or the connection dropped mid-stream.
	CodeStreamFailed = "stream_failed"
	// CodeIdentityIncomplete — --tenant / --user / --session were
	// not all supplied. Identity is mandatory at the Protocol edge
	// (CLAUDE.md §6 rule 9).
	CodeIdentityIncomplete = "identity_incomplete"
	// CodeRunNotFound — `inspect-runs <run-id>` could not find the
	// run in the replayed event stream (it never ran in this
	// session, or the replay window did not cover it).
	CodeRunNotFound = "run_not_found"
)

// Flag names. Constants so subcommand bodies + tests reference one
// canonical spelling.
const (
	flagBind     = "bind"
	flagTenant   = "tenant"
	flagUser     = "user"
	flagSession  = "session"
	flagRun      = "run"
	flagType     = "type"
	flagSince    = "since"
	flagFollow   = "follow"
	flagSnapshot = "snapshot" // accept either --follow=false or --snapshot
)

// DefaultBind is the loopback address the CLI defaults to. Matches
// `harbor dev`'s DefaultDevPort so the no-config common case "just
// works": `harbor inspect-events` against a `harbor dev` running in
// the same terminal.
const DefaultBind = "127.0.0.1:18080"

// snapshotIdleTimeout is the floor a snapshot run waits for a quiet
// stream before declaring "no more events" and exiting cleanly. SSE
// has no end-of-stream signal, so a snapshot mode must drain the
// replay buffer and then time out on silence; the keepalive cadence
// (15s default) is the natural upper bound.
const snapshotIdleTimeout = 20 * time.Second

// envHarborToken is the env var the CLI reads for the Bearer token.
// Documented here so smoke scripts + tests reference the same symbol.
const envHarborToken = "HARBOR_TOKEN"

// tokenFileRel is the on-disk fallback path, relative to the
// operator's home directory.
const tokenFileRel = ".harbor/token"

// wireEvent mirrors the Phase 60 SSE `data:` payload. Kept verbatim
// (field names + tags) so a wire-shape change at the SSE transport
// side without a CLI update fails the golden. See file-level godoc.
type wireEvent struct {
	Type       string            `json:"type"`
	Sequence   uint64            `json:"sequence"`
	OccurredAt string            `json:"occurred_at"`
	Tenant     string            `json:"tenant"`
	User       string            `json:"user"`
	Session    string            `json:"session"`
	Run        string            `json:"run,omitempty"`
	Payload    any               `json:"payload,omitempty"`
	Extra      map[string]string `json:"extra,omitempty"`
}

// inspectAuth bundles the resolved Bearer token + the source it came
// from (used in the optional `--quiet=false` informational log).
type inspectAuth struct {
	Token  string
	Source string // "env" or "file"
}

// resolveToken reads HARBOR_TOKEN (preferred) or ~/.harbor/token. A
// missing token at BOTH sources is a fail-loud CLIError per §13:
// "Test stubs as production defaults on operator-facing seams" — the
// CLI never silently anonymous-probes the Protocol surface.
//
// Helpers (getenv, homedir) are injected for testability — tests pass
// in-memory shims to drive both code paths deterministically.
func resolveToken(getenv func(string) string, homedir func() (string, error), readFile func(string) ([]byte, error)) (inspectAuth, error) {
	if tok := strings.TrimSpace(getenv(envHarborToken)); tok != "" {
		return inspectAuth{Token: tok, Source: "env"}, nil
	}
	home, err := homedir()
	if err != nil {
		return inspectAuth{}, CLIError{
			Subcommand: "",
			Code:       CodeAuthRequired,
			Message:    fmt.Sprintf("no %s set and home directory unreadable: %v", envHarborToken, err),
			Hint:       "set HARBOR_TOKEN=<jwt> or write the token to ~/.harbor/token",
		}
	}
	path := filepath.Join(home, tokenFileRel)
	body, readErr := readFile(path)
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return inspectAuth{}, CLIError{
				Code:    CodeAuthRequired,
				Message: fmt.Sprintf("no %s set and %s is absent", envHarborToken, path),
				Hint:    "run `harbor dev` and copy HARBOR_DEV_TOKEN, then `mkdir -p ~/.harbor && pbpaste > ~/.harbor/token` (or set HARBOR_TOKEN)",
			}
		}
		return inspectAuth{}, CLIError{
			Code:    CodeAuthRequired,
			Message: fmt.Sprintf("read %s: %v", path, readErr),
			Hint:    "ensure the token file is readable by the current user",
		}
	}
	tok := strings.TrimSpace(string(body))
	if tok == "" {
		return inspectAuth{}, CLIError{
			Code:    CodeAuthRequired,
			Message: fmt.Sprintf("%s is empty", path),
			Hint:    "rewrite the file with a valid JWT — see `harbor dev` stderr for HARBOR_DEV_TOKEN",
		}
	}
	return inspectAuth{Token: tok, Source: "file"}, nil
}

// resolveTokenFromOS is the production wrapper around resolveToken
// that wires os.Getenv / os.UserHomeDir / os.ReadFile. The injected
// shape exists so tests do not touch the real env / fs.
func resolveTokenFromOS() (inspectAuth, error) {
	return resolveToken(os.Getenv, os.UserHomeDir, os.ReadFile)
}

// inspectEndpoint composes the canonical /v1/events URL against a
// bind string. Supports both bare host:port and full URLs (http:// or
// https://). A malformed bind returns a fail-loud CLIError so the
// operator sees what they typed wrong.
func inspectEndpoint(bind string) (string, error) {
	bind = strings.TrimSpace(bind)
	if bind == "" {
		return "", CLIError{
			Code:    CodeBindInvalid,
			Message: "--bind is empty",
			Hint:    "pass --bind 127.0.0.1:18080 (or the host:port the Runtime listens on)",
		}
	}
	// Already a full URL?
	if strings.HasPrefix(bind, "http://") || strings.HasPrefix(bind, "https://") {
		u, err := url.Parse(bind)
		if err != nil {
			return "", CLIError{
				Code:    CodeBindInvalid,
				Message: fmt.Sprintf("--bind %q is not a valid URL: %v", bind, err),
			}
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/v1/events"
		return u.String(), nil
	}
	return "http://" + bind + "/v1/events", nil
}

// inspectFilter bundles the identity + type filters the SSE handler
// turns into a triple-scoped subscription. Identity is mandatory;
// types and run are optional.
type inspectFilter struct {
	Tenant string
	User   string
	Sess   string
	Run    string
	Types  []string
	Since  string // SSE Last-Event-ID — RFC3339 ts OR ULID-shaped event id
}

// validate fails loud when the identity triple is incomplete. The
// Runtime would 401 anyway; we fail at the CLI edge so the error
// message names the missing flag rather than relying on the server's
// generic "identity scope incomplete".
func (f inspectFilter) validate() error {
	if strings.TrimSpace(f.Tenant) == "" || strings.TrimSpace(f.User) == "" || strings.TrimSpace(f.Sess) == "" {
		return CLIError{
			Code:    CodeIdentityIncomplete,
			Message: "--tenant, --user, --session are all required",
			Hint:    "pass --tenant=T --user=U --session=S; the Runtime rejects requests with an incomplete identity scope (CLAUDE.md §6)",
		}
	}
	return nil
}

// applyHeaders writes the Phase 60 X-Harbor-* identity carrier headers
// and the Bearer token to req. Phase 61's auth.Middleware prefers the
// ctx-attached verified identity (from the JWT claims) over the
// carrier headers, but we send both so a Runtime booted with
// `WithoutValidator()` (test-only escape hatch — see
// internal/protocol/transports.WithoutValidator) still routes the
// request.
func (f inspectFilter) applyHeaders(req *http.Request, auth inspectAuth) {
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("X-Harbor-Tenant", f.Tenant)
	req.Header.Set("X-Harbor-User", f.User)
	req.Header.Set("X-Harbor-Session", f.Sess)
	if f.Run != "" {
		req.Header.Set("X-Harbor-Run", f.Run)
	}
	for _, t := range f.Types {
		if strings.TrimSpace(t) == "" {
			continue
		}
		req.Header.Add("X-Harbor-Event-Type", t)
	}
	if f.Since != "" {
		req.Header.Set("Last-Event-ID", f.Since)
	}
	req.Header.Set("Accept", "text/event-stream")
}

// sseFrame is one decoded SSE block: the `event:` type line + the
// concatenated `data:` lines + the `id:` cursor. A `:` comment block
// (keepalive, replay-gap announcement) decodes with Comment != "".
type sseFrame struct {
	Event   string
	ID      string
	Data    string
	Comment string
}

// readSSE pulls one SSE block off r. Returns io.EOF when the stream
// ends. A nil frame + nil error means a blank input line (frame
// boundary with no fields); the caller continues.
//
// SSE grammar (the parts we care about):
//
//	field: value\n         — one field line
//	field:value\n          — leading-space-after-colon is optional
//	\n                     — blank line: end-of-frame
//	:comment-text\n        — comment line (whole line ignored EXCEPT
//	                         when whole-frame is a single comment;
//	                         we surface it so the caller sees keepalives /
//	                         the replay-gap explicit comment)
func readSSE(r *bufio.Reader) (*sseFrame, error) {
	var frame sseFrame
	var dataLines []string
	hasContent := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && hasContent {
				// Tail-without-trailing-newline: surface what we
				// gathered, then EOF on the next call.
				if len(dataLines) > 0 {
					frame.Data = strings.Join(dataLines, "\n")
				}
				return &frame, nil
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !hasContent {
				// Lone blank line — skip; SSE allows leading blanks.
				continue
			}
			if len(dataLines) > 0 {
				frame.Data = strings.Join(dataLines, "\n")
			}
			return &frame, nil
		}
		hasContent = true
		if strings.HasPrefix(line, ":") {
			// Comment line. Surface only when no other fields were
			// observed in this frame — keepalives + replay-gap markers
			// always arrive as standalone comment frames.
			frame.Comment = strings.TrimSpace(line[1:])
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			// "field" with no value is permitted by the SSE grammar;
			// treat as `field: ""`.
			continue
		}
		field := line[:idx]
		value := line[idx+1:]
		// Per spec, ONE leading space after the colon is stripped.
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			frame.Event = value
		case "id":
			frame.ID = value
		case "data":
			dataLines = append(dataLines, value)
		case "retry":
			// We ignore retry directives in the CLI — the operator
			// can re-invoke if the stream drops.
		}
	}
}

// inspectSSE opens an SSE connection against /v1/events and yields
// one parsed sseFrame at a time via the visitor closure. The closure
// returns (stop bool, err error): stop=true ends the loop cleanly
// (used by snapshot mode to terminate on keepalive after the replay
// drained); err != nil aborts.
//
// The function is cancellable via ctx — closing the parent ctx
// terminates the HTTP request, which closes the body reader, which
// surfaces an error on the next read.
func inspectSSE(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	filter inspectFilter,
	auth inspectAuth,
	visit func(sseFrame) (bool, error),
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return CLIError{Code: CodeStreamFailed, Message: fmt.Sprintf("build request: %v", err)}
	}
	filter.applyHeaders(req, auth)
	resp, err := client.Do(req)
	if err != nil {
		return CLIError{
			Code:    CodeStreamFailed,
			Message: fmt.Sprintf("connect %s: %v", endpoint, err),
			Hint:    "is `harbor dev` running on the --bind address?",
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		//nolint:errcheck // best-effort bounded read of an error body; a read failure just yields an empty quote
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		hint := ""
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			hint = "the Bearer token is missing or invalid; re-mint via `harbor dev` and refresh HARBOR_TOKEN / ~/.harbor/token"
		case http.StatusForbidden:
			hint = "the token's scope does not authorise the request — admin fan-in needs the admin / console:fleet scope"
		}
		return CLIError{
			Code:    CodeStreamFailed,
			Message: fmt.Sprintf("/v1/events returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))),
			Hint:    hint,
		}
	}
	rd := bufio.NewReader(resp.Body)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		frame, err := readSSE(rd)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return CLIError{Code: CodeStreamFailed, Message: fmt.Sprintf("read stream: %v", err)}
		}
		if frame == nil {
			continue
		}
		stop, vErr := visit(*frame)
		if vErr != nil {
			return vErr
		}
		if stop {
			return nil
		}
	}
}
