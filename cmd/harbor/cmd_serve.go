// cmd/harbor/cmd_serve.go — the `harbor serve` production subcommand
// (RFC §5.4 wire transport, RFC §5.5 authentication).
//
// `harbor serve` boots the headless Harbor Runtime + Protocol server
// behind a PRODUCTION JWT verifier: the operator's own IdP signing keys,
// fetched as a JWK Set (RFC 7517) from `identity.jwks_url` (or loaded
// from `identity.jwks_file`) and verified against the asymmetric
// algorithm allowlist (RS*/ES*, never HS*/`none`). It is the production
// sibling of `harbor dev`:
//
//   - `harbor dev` mints an ephemeral ES256 dev token and prints it at
//     boot — a first-clone convenience, never a production posture.
//   - `harbor serve` mints nothing: every `/v1/*` request must carry a
//     JWT the operator's IdP signed, which the JWKS verifier checks at
//     the Protocol edge. No bootstrap-token endpoint, no dev-token
//     mint, no draft scaffolding, no Console embedding (the Console is
//     served only by `harbor console`, a binding deployment rule).
//
// # Fail loud at boot
//
// The full-binary config profile (config.Validate) requires the
// identity block — asymmetric `jwt_algorithms`, `issuer`, `audience`,
// and one of `jwks_url` / `jwks_file`. A config missing the JWKS source
// fails at load with a message naming the field; the JWKS source is then
// fetched+parsed synchronously, so an unreachable URL or a keyset with
// no usable asymmetric signing key also fails the boot non-zero rather
// than starting a server that rejects every request.
//
// # No mock LLM
//
// `harbor serve` demands a real LLM provider. It does NOT honour the
// `HARBOR_DEV_ALLOW_MOCK` escape hatch; a config without a real provider
// fails loud at boot (the same gate `harbor dev` applies when the escape
// hatch is absent).

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	sdkprotocolclient "github.com/hurtener/Harbor/sdk/protocolclient"
	tui "github.com/hurtener/Harbor/sdk/tui"
)

// Flag names for the `serve` subcommand. Declared as constants so the
// cmd body, tests, and the help golden reference one spelling.
const (
	flagServeConfig = "config"
	flagServePort   = "port"
	flagServeBind   = "bind"
	flagServeTUI    = "tui"
)

// DefaultServePort is the loopback port `harbor serve` falls back to
// only when the config carries no `server.bind_addr` (which the
// validator requires, so this is effectively a backstop) and no
// `--bind` / `HARBOR_BIND` override is given. Distinct from the dev /
// console ports so an operator can run them side by side.
const DefaultServePort = 8686

// newServeCmd builds the `serve` cobra subcommand. Flags:
//
//	--config <path>     default `harbor.yaml`
//	--port <int>        default 8686 (backstop; server.bind_addr wins)
//	--bind <host:port>  overrides server.bind_addr; `host:0` = ephemeral
//
// The flag surface mirrors `harbor dev` / `harbor console` so operator
// muscle memory carries over; the behavioural difference is the
// production auth posture and the absence of every dev-only surface.
func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "serve the headless Runtime + Protocol surface behind a production JWKS verifier",
		Long: `Boot the headless Harbor Runtime and open the Protocol transports
behind a PRODUCTION JWT verifier.

Unlike ` + "`harbor dev`" + `, ` + "`harbor serve`" + ` mints no token: every
request must carry a JWT signed by your identity provider. Harbor verifies
it against the provider's published JWK Set (RFC 7517), configured via
` + "`identity.jwks_url`" + ` (or ` + "`identity.jwks_file`" + `) in harbor.yaml,
using the asymmetric algorithm allowlist (RS*/ES*, never HS*/none).

The bind address comes from ` + "`server.bind_addr`" + ` in harbor.yaml
(override with --bind). A missing JWKS source, an unreachable provider,
or a missing LLM provider fails the boot non-zero with a named-field
error — Harbor never silently degrades to the dev signer or a mock.

The Console is served only by ` + "`harbor console`" + `, never here.

Pass --tui to co-launch the native terminal client after the server is
ready. The TUI attaches through authenticated REST/SSE exactly like
` + "`harbor tui --attach`" + ` — it receives no Runtime handle. The operator
supplies the token (HARBOR_TOKEN or ~/.harbor/token); there is no
anonymous loopback, automatic token minting, or mock fallback. Quitting
the TUI drains the co-launched server. Runtime logs go to a sink, not
the terminal, so Bubble Tea frames are never overwritten.

Examples:
  harbor serve --config /etc/harbor/harbor.yaml
  harbor serve --bind 0.0.0.0:8080
  harbor serve --bind 127.0.0.1:0
  harbor serve --bind 127.0.0.1:0 --tui`,
		Args: cobra.NoArgs,
		RunE: runServe,
	}
	cmd.Flags().String(flagServeConfig, DefaultDevConfig, "path to harbor.yaml")
	cmd.Flags().Int(flagServePort, DefaultServePort, "backstop port used only if the config carries no server.bind_addr (config loading defaults it, so --bind is the practical override)")
	cmd.Flags().String(flagServeBind, "", "host:port to bind (overrides server.bind_addr; host:0 = ephemeral)")
	cmd.Flags().Bool(flagServeTUI, false, "co-launch the native terminal client after readiness (attaches through authenticated REST/SSE; quit drains the server)")
	return cmd
}

// runServe is the cobra RunE entry for `harbor serve`. It boots the
// headless Runtime stack behind the JWKS-backed production validator,
// serves until SIGINT / SIGTERM, then drains. Every failure path
// returns a CLIError so the structured-error surface routes through the
// root.
//
// When --tui is set, runServe boots the server, waits for readiness
// (WaitReady), resolves the operator token, and attaches the TUI through
// sdk/tui. Runtime stderr is redirected to a captured sink so Bubble Tea
// owns stdout+stderr; on server failure the terminal is restored before
// the captured log is printed. Quitting the TUI drains the owned server.
func runServe(cmd *cobra.Command, _ []string) error {
	// Every flag below is statically registered on this command, so the
	// GetX lookups cannot fail; the blank-error discards are intentional.
	cfgPath, _ := cmd.Flags().GetString(flagServeConfig) //nolint:errcheck // flag statically registered; lookup cannot fail
	port, _ := cmd.Flags().GetInt(flagServePort)         //nolint:errcheck // flag statically registered; lookup cannot fail
	bindFlag, _ := cmd.Flags().GetString(flagServeBind)  //nolint:errcheck // flag statically registered; lookup cannot fail
	withTUI, _ := cmd.Flags().GetBool(flagServeTUI)      //nolint:errcheck // flag statically registered; lookup cannot fail

	// `--bind` (or HARBOR_BIND) overrides the config bind address.
	bindAddrOverride := bindFlag
	if bindAddrOverride == "" {
		bindAddrOverride = os.Getenv("HARBOR_BIND")
	}
	if bindAddrOverride != "" {
		if p, ok := parsePortFromBind(bindAddrOverride); ok {
			port = p
		}
	}

	// Co-launch log separation (AC6): when --tui is set, Runtime stderr
	// (slog logs, HARBOR_DEV_BOUND) goes to a captured buffer, NOT the
	// terminal. Bubble Tea owns stdout+stderr. On server failure the
	// terminal is restored, then the captured stderr is printed so the
	// operator sees what happened.
	var logSink = cmd.ErrOrStderr()
	var capturedLog *bytes.Buffer
	if withTUI {
		capturedLog = &bytes.Buffer{}
		logSink = capturedLog
	}

	// Production: JSON structured logging on the sink (CLAUDE.md §5).
	logger := slog.New(slog.NewJSONHandler(logSink, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	stack, err := serve.Boot(ctx, serve.Options{
		ConfigPath:      cfgPath,
		Port:            port,
		BindAddr:        bindAddrOverride,
		Logger:          logger,
		Stderr:          logSink,
		SubcommandLabel: "serve",
		// Production honors the operator-configured `server.bind_addr`
		// (which may be non-loopback). Dev/console never set this opt-in —
		// they stay loopback-only regardless of the yaml.
		PreferConfigBindAddr: true,
		// The PRODUCTION auth path: build the JWKS-backed validator from the
		// operator's identity config. No dev-only seam is composed, so no
		// dev surface (bootstrap, drafts, rotate, Console) is mounted.
		AuthValidatorFactory: serve.NewJWKSAuthValidatorFactory(),
		MCPDefaultIdentity: identity.Identity{
			TenantID:  DevTenant,
			UserID:    DevUser,
			SessionID: DevSession,
		},
		DisplayName:  "harbor serve",
		InstanceID:   serve.InstanceID("harbor-serve"),
		BuildVersion: HarborVersion,
		BuildCommit:  "dev",
		// production demands a real LLM provider (allowMock=false): the gate
		// fails loud on a missing provider.
		BuildLLMSnapshot: newLLMSnapshotBuilder(false),
	})
	if err != nil {
		if withTUI && capturedLog != nil {
			restoreTerminalAndPrintLog(capturedLog)
		}
		return emitCLIError(cmd, bootErrorToCLIError("serve", err))
	}
	defer stack.Close(context.Background())

	if !withTUI {
		// Headless path: the original behavior, unchanged.
		if err := stack.Serve(ctx); err != nil {
			return emitCLIError(cmd, CLIError{
				Subcommand: "serve",
				Message:    fmt.Sprintf("serve stopped: %v", err),
				Code:       CodeBootInternal,
				Hint:       "check the server log lines above for the originating subsystem",
			})
		}
		return nil
	}

	// Co-launch path (--tui): serve in a goroutine, wait for readiness,
	// attach the TUI, and on TUI exit drain the owned server.
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- stack.Serve(ctx)
	}()

	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	boundAddr, err := stack.WaitReady(readyCtx)
	readyCancel()
	if err != nil {
		// Server failed to bind. Restore the terminal, print the captured
		// log, then surface the error.
		stop() // cancel the serve goroutine
		<-serveErrCh
		restoreTerminalAndPrintLog(capturedLog)
		return emitCLIError(cmd, CLIError{
			Subcommand: "serve",
			Message:    fmt.Sprintf("server readiness failed: %v", err),
			Code:       CodeBindInvalid,
			Hint:       "check the bind address, port availability, and the captured server log above",
		})
	}

	// Resolve the operator token for the TUI. No anonymous loopback, no
	// automatic minting, no mock fallback (AC2 non-goal).
	auth, err := resolveTokenFromOS()
	if err != nil {
		stop()
		<-serveErrCh
		restoreTerminalAndPrintLog(capturedLog)
		return emitCLIError(cmd, tuiCLIError(err))
	}

	baseURL := "http://" + boundAddr
	tokens := sdkprotocolclient.TokenSourceFunc(func(_ context.Context, _ sdkprotocolclient.IdentityScope) (string, error) {
		return auth.Token, nil
	})

	// Attach the TUI. On exit, drain the owned server (AC5: co-launch
	// quit drains its owned server).
	tuiErr := tui.Run(ctx, tui.Options{
		BaseURL: baseURL,
		Token:   tokens,
	})

	// Cancel the serve context to drain the owned server.
	stop()
	serveErr := <-serveErrCh

	// Restore the terminal before printing any captured log.
	restoreTerminalAndPrintLog(capturedLog)

	if tuiErr != nil {
		return emitCLIError(cmd, tuiCLIError(tuiErr))
	}
	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		return emitCLIError(cmd, CLIError{
			Subcommand: "serve",
			Message:    fmt.Sprintf("co-launched server stopped with error: %v", serveErr),
			Code:       CodeBootInternal,
			Hint:       "check the captured server log above for the originating subsystem",
		})
	}
	return nil
}

// restoreTerminalAndPrintLog restores the terminal to normal mode (exiting
// the Bubble Tea alternate screen if active) and then prints the captured
// Runtime log so the operator can see what happened. This is the AC6
// contract: on server failure, restore the terminal, then surface the log.
func restoreTerminalAndPrintLog(captured *bytes.Buffer) {
	if captured == nil {
		return
	}
	// Bubble Tea restores the terminal on program exit; this is a
	// best-effort safety net for the path where the program never started
	// or crashed before cleanup. Writing raw ANSI to stdout is safe even
	// if the terminal is already restored.
	os.Stdout.WriteString("\x1b[?25h\x1b[0m")
	if captured.Len() > 0 {
		fmt.Fprintln(os.Stderr, "--- Runtime log (captured during co-launch) ---")
		os.Stderr.Write(captured.Bytes())
		fmt.Fprintln(os.Stderr, "--- end Runtime log ---")
	}
}
