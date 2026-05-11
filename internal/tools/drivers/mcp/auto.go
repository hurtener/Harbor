package mcp

import (
	"context"
	"errors"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPTransportMode selects the wire transport for one MCP attachment.
// Mirrors brief 03 §4 ("`MCPTransportMode = Auto | SSE |
// StreamableHTTP`"). Stdio is the implicit fourth mode: selected
// when `Auto` sees a `Command` but no `URL`.
type MCPTransportMode string

const (
	// TransportAuto inspects Config and picks: streamable-HTTP first
	// if URL is set; on connect failure fall back to SSE; if Command
	// is set with no URL, stdio.
	TransportAuto MCPTransportMode = "auto"
	// TransportSSE selects the SDK's SSEClientTransport. URL must
	// be set.
	TransportSSE MCPTransportMode = "sse"
	// TransportStreamableHTTP selects the SDK's
	// StreamableClientTransport. URL must be set.
	TransportStreamableHTTP MCPTransportMode = "streamable_http"
	// TransportStdio selects the SDK's CommandTransport. Command
	// (argv form) must be set.
	TransportStdio MCPTransportMode = "stdio"
)

// validModes is the operator-facing allowlist used by Config
// validation and the config-package validator.
var validModes = map[MCPTransportMode]struct{}{
	TransportAuto:           {},
	TransportSSE:            {},
	TransportStreamableHTTP: {},
	TransportStdio:          {},
}

// isValidMode reports whether m is in validModes.
func isValidMode(m MCPTransportMode) bool {
	_, ok := validModes[m]
	return ok
}

// IsValidTransportMode is the exported helper used by `internal/config`
// to validate the raw string from YAML.
func IsValidTransportMode(s string) bool {
	return isValidMode(MCPTransportMode(s))
}

// transportFactory builds an MCP SDK Transport for cfg. The
// factory pattern hides which concrete transport is selected — the
// driver's Connect path is one call to factory(cfg).Connect().
//
// Returned errors are wrapped against ErrInvalidConfig (selection-
// time errors from a malformed Config) or ErrTransportFailed
// (connection-time failures).
type transportFactory func(ctx context.Context, cfg Config) (mcpsdk.Transport, MCPTransportMode, error)

// selectTransport resolves Config into a single concrete Transport.
// Auto-mode tries streamable-HTTP first, then SSE, then stdio per
// Config.Command. Explicit modes select directly.
//
// The selector is stateless; concurrent calls produce independent
// Transport values.
func selectTransport(ctx context.Context, cfg Config) (mcpsdk.Transport, MCPTransportMode, error) {
	switch cfg.TransportMode {
	case TransportSSE:
		if cfg.URL == "" {
			return nil, "", fmt.Errorf("%w: sse transport requires URL", ErrInvalidConfig)
		}
		return newSSETransport(cfg), TransportSSE, nil
	case TransportStreamableHTTP:
		if cfg.URL == "" {
			return nil, "", fmt.Errorf("%w: streamable_http transport requires URL", ErrInvalidConfig)
		}
		return newStreamableTransport(cfg), TransportStreamableHTTP, nil
	case TransportStdio:
		if len(cfg.Command) == 0 {
			return nil, "", fmt.Errorf("%w: stdio transport requires Command (argv form)", ErrInvalidConfig)
		}
		return newStdioTransport(cfg)
	case "", TransportAuto:
		return autoSelect(ctx, cfg)
	default:
		return nil, "", fmt.Errorf("%w: unknown transport mode %q", ErrInvalidConfig, cfg.TransportMode)
	}
}

// autoSelect implements the documented preference order:
//
//  1. URL set → streamable-HTTP first; on Connect+Initialize failure,
//     fall back to SSE.
//  2. URL unset + Command set → stdio.
//  3. Neither set → ErrInvalidConfig.
//
// At selection time we return a single concrete Transport (no
// fallback wrapping). The Provider.Connect layer is responsible for
// the fallback dance — it can observe `client.Connect` failure
// (which includes Initialize) and retry with the SSE transport.
// Putting fallback at the Transport.Connect level would only catch
// transport-Connect failures, missing the Initialize-time errors
// that "endpoint mistakenly answered to streamable but isn't really
// streamable" produces.
//
// Returns the chosen Transport, the chosen mode, and an error.
func autoSelect(ctx context.Context, cfg Config) (mcpsdk.Transport, MCPTransportMode, error) {
	if cfg.URL != "" {
		// In auto-mode with a URL, the streamable-HTTP transport is
		// the preferred first try; Provider.Connect handles fallback
		// after observing the higher-level failure.
		return newStreamableTransport(cfg), TransportStreamableHTTP, nil
	}
	if len(cfg.Command) > 0 {
		return newStdioTransport(cfg)
	}
	return nil, "", fmt.Errorf("%w: auto mode requires URL or Command", ErrInvalidConfig)
}

// classifyConnectError reports whether err is a recoverable
// transport-level failure (worth trying the next candidate). Used
// by Provider.Connect's auto-fallback path.
func classifyConnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}
