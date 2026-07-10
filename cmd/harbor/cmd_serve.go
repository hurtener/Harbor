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
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/serve"
)

// Flag names for the `serve` subcommand. Declared as constants so the
// cmd body, tests, and the help golden reference one spelling.
const (
	flagServeConfig = "config"
	flagServePort   = "port"
	flagServeBind   = "bind"
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

Examples:
  harbor serve --config /etc/harbor/harbor.yaml
  harbor serve --bind 0.0.0.0:8080
  harbor serve --bind 127.0.0.1:0`,
		Args: cobra.NoArgs,
		RunE: runServe,
	}
	cmd.Flags().String(flagServeConfig, DefaultDevConfig, "path to harbor.yaml")
	cmd.Flags().Int(flagServePort, DefaultServePort, "backstop port used only if the config carries no server.bind_addr (config loading defaults it, so --bind is the practical override)")
	cmd.Flags().String(flagServeBind, "", "host:port to bind (overrides server.bind_addr; host:0 = ephemeral)")
	return cmd
}

// runServe is the cobra RunE entry for `harbor serve`. It boots the
// headless Runtime stack behind the JWKS-backed production validator,
// serves until SIGINT / SIGTERM, then drains. Every failure path
// returns a CLIError so the structured-error surface routes through the
// root.
func runServe(cmd *cobra.Command, _ []string) error {
	// Every flag below is statically registered on this command, so the
	// GetX lookups cannot fail; the blank-error discards are intentional.
	cfgPath, _ := cmd.Flags().GetString(flagServeConfig) //nolint:errcheck // flag statically registered; lookup cannot fail
	port, _ := cmd.Flags().GetInt(flagServePort)         //nolint:errcheck // flag statically registered; lookup cannot fail
	bindFlag, _ := cmd.Flags().GetString(flagServeBind)  //nolint:errcheck // flag statically registered; lookup cannot fail

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

	// Production: JSON structured logging on stderr (CLAUDE.md §5).
	logger := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	stack, err := serve.Boot(ctx, serve.Options{
		ConfigPath:      cfgPath,
		Port:            port,
		BindAddr:        bindAddrOverride,
		Logger:          logger,
		Stderr:          cmd.ErrOrStderr(),
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
		return emitCLIError(cmd, bootErrorToCLIError("serve", err))
	}
	defer stack.Close(context.Background())

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
