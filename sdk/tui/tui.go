// Package tui is the public SDK facade for attaching Harbor's native
// terminal client to a running Runtime. It is a curated connection-only
// facade: it carries NO Runtime, stack, event-bus, or server handle —
// the TUI is a Protocol client, identical in boundary to `harbor tui
// --attach`.
//
// Run turns a base URL + rotating token source + optional session into a
// fully operational terminal conversation and control experience:
//
//	import (
//	    "context"
//
//	    "github.com/hurtener/Harbor/sdk/protocolclient"
//	    "github.com/hurtener/Harbor/sdk/tui"
//	)
//
//	tokens := protocolclient.StaticToken(jwt, principal)
//	err := tui.Run(ctx, tui.Options{
//	    BaseURL: "http://127.0.0.1:8686",
//	    Token:   tokens,
//	    Session: "session-1",
//	})
//
// # Production-only by construction
//
// Run dials normal authenticated REST/SSE with an operator JWT. There is
// no anonymous loopback, no automatic token minting, and no mock
// fallback — the operator supplies the token. Every request and SSE
// reconnect resolves the credential fresh, so a rotated token file or
// replacement credential keeps the same active session alive.
//
// # Co-launch
//
// A generated serving binary's `--tui` flag and `harbor serve --tui`
// both call Run after waiting on the server's explicit readiness
// (sdk/server.Handle.WaitReady). The TUI never receives a Runtime
// handle — it dials the bound listener like any external client.
//
// This package forwards to internal/tui/entry (the shared attach entry
// point that cmd_tui.go also calls); no mechanism lives here beyond the
// single Options adapter.
package tui

import (
	"context"
	"errors"
	"strings"

	"github.com/hurtener/Harbor/internal/tui/entry"
	protocolclient "github.com/hurtener/Harbor/sdk/protocolclient"
)

// Options carries the connection-only inputs Run consumes. No Runtime,
// stack, event-bus, or server handle is present.
type Options struct {
	// BaseURL is the Runtime REST/SSE base URL (e.g.
	// http://127.0.0.1:8686). Required.
	BaseURL string

	// Token is the rotating JWT source. Resolved before every request
	// and SSE reconnect. Required — the operator supplies the credential;
	// there is no anonymous loopback, automatic token minting, or mock
	// fallback.
	Token protocolclient.TokenSource

	// Session is the authorized session to restore or select. May be
	// empty; the facade restores the last durable session for the
	// resolved identity when blank.
	Session string
}

// Run attaches the native terminal client to a running Runtime and
// blocks until the operator quits. It is the curated facade behind
// `harbor serve --tui`, generated serving binaries' `--tui`, and direct
// embedder use.
//
// The flow is identical to `harbor tui --attach`: resolve the token,
// build the Protocol client, probe RuntimeInfo, construct the
// Controller, build the RuntimeModel, and run the Bubble Tea program.
// The facade holds NO Runtime handle — every byte of state and streaming
// data travels through authenticated REST/SSE.
//
// Returns nil on a clean quit. In a co-launch context, the caller
// drains its owned server after Run returns. In an attach context, the
// remote server stays alive.
func Run(ctx context.Context, opts Options) error {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return errors.New("tui: base URL is required")
	}
	if opts.Token == nil {
		return errors.New("tui: token source is required (no anonymous loopback, automatic token minting, or mock fallback)")
	}
	return entry.Run(ctx, entry.Options{
		BaseURL: opts.BaseURL,
		Token:   opts.Token,
		Session: opts.Session,
	})
}
