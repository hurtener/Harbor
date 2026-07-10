// cmd/harbor/devcompose.go — the dev-only serve composition.
//
// `harbor dev` and `harbor console` share the same dev-only policy layered
// over the promoted serve band: an ephemeral ES256 signer (never a production
// posture), the mock-LLM snapshot gate, the draft-scaffolding + bootstrap-token
// pre-CORS routes, the token-rotate surface, fixture seeding, and (for
// `harbor console`) the embedded Console static build. This file composes that
// policy onto `serve.Options` through the promoted injection seams. The
// production `harbor serve` path builds its own `serve.Options` with the JWKS
// factory and none of these seams (cmd_serve.go).

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/devdraft"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/server"
)

// devCompositionOptions selects the dev-only policy variants.
type devCompositionOptions struct {
	// allowMock threads the HARBOR_DEV_ALLOW_MOCK escape hatch through the
	// LLM snapshot gate.
	allowMock bool
	// serveConsole additionally mounts the embedded Console static build at
	// `/` (the `harbor console` posture). `harbor dev` leaves it false — the
	// Console build is NEVER embedded into the dev-loop surface.
	serveConsole bool
}

// devComposition holds the dev-only signer and policy the serve seams close
// over. One is built per `harbor dev` / `harbor console` invocation; the same
// signer backs every hot-reload reboot so the printed dev token stays valid.
type devComposition struct {
	signer       *devSigner
	allowMock    bool
	serveConsole bool
}

// newDevComposition mints the ephemeral dev signer and returns the dev-only
// serve composition.
func newDevComposition(o devCompositionOptions) (*devComposition, error) {
	signer, err := newDevSigner()
	if err != nil {
		return nil, fmt.Errorf("dev signer: %w", err)
	}
	return &devComposition{
		signer:       signer,
		allowMock:    o.allowMock,
		serveConsole: o.serveConsole,
	}, nil
}

// serveOptions projects the dev composition onto serve.Options, wiring the
// dev-only policy through the promoted injection seams.
func (c *devComposition) serveOptions(cfgPath string, port int, bindAddr, label string, logger *slog.Logger, stderr io.Writer) serve.Options {
	return serve.Options{
		ConfigPath:      cfgPath,
		Port:            port,
		BindAddr:        bindAddr,
		Logger:          logger,
		Stderr:          stderr,
		SubcommandLabel: label,
		AuthValidatorFactory: func(_ context.Context, _ *config.Config, red audit.Redactor, bus events.EventBus, lg *slog.Logger) (auth.Validator, error) {
			return auth.NewValidator(c.signer.KeySet(),
				auth.WithRedactor(red),
				auth.WithLogger(lg),
				auth.WithEventBus(bus),
			)
		},
		MCPDefaultIdentity: identity.Identity{
			TenantID:  DevTenant,
			UserID:    DevUser,
			SessionID: DevSession,
		},
		DisplayName:      "harbor dev",
		InstanceID:       devInstanceID(),
		BuildVersion:     HarborVersion,
		BuildCommit:      "dev",
		BuildLLMSnapshot: newLLMSnapshotBuilder(c.allowMock),
		BuildAuthSurface: func(red audit.Redactor, bus events.EventBus, lg *slog.Logger) (*auth.RotateSurface, error) {
			// The dev signer is the V1 auth.TokenIssuer; the redactor + bus
			// are wired so every accepted rotation emits a redacted admin
			// event. Production has no in-runtime token issuer, so this
			// surface is dev-only and un-mounted there.
			return auth.NewRotateSurface(c.signer, red,
				auth.WithRotateBus(bus),
				auth.WithRotateLogger(lg),
			)
		},
		ExtraRoutes: c.extraRoutes(),
		PostBoot:    c.postBoot(stderr),
	}
}

// extraRoutes mounts the dev-only pre-CORS routes: the draft-scaffolding
// handler, the loopback bootstrap-token endpoint, and (for `harbor console`)
// the embedded Console static build.
func (c *devComposition) extraRoutes() serve.ExtraRoutesFunc {
	return func(_ context.Context, m serve.RouteMount) ([]func(context.Context) error, error) {
		closers := make([]func(context.Context) error, 0, 1)

		// Draft-save scaffolding under `.harbor/drafts/` (operator working
		// dir, identity-scoped). Auth-wrapped so every draft endpoint reads
		// identity from ctx via the same path the Protocol transports do.
		draftStore, err := devdraft.NewStore(devdraft.Options{
			Root:   filepath.Join(".", ".harbor", "drafts"),
			Bus:    m.Bus,
			Logger: m.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("devdraft: %w", err)
		}
		closers = append(closers, draftStore.Close)
		draftHandler, err := devdraft.NewHandler(draftStore, m.Logger)
		if err != nil {
			return nil, fmt.Errorf("devdraft: handler: %w", err)
		}
		draftMW := auth.Middleware(m.Validator, auth.MWLogger(m.Logger))
		m.Router.Handle(devdraft.RoutePrefix+"/", draftMW(draftHandler))

		// Loopback bootstrap-token endpoint — mints a fresh dev token +
		// returns the connection envelope the Console needs for a one-click
		// "Attach to local Runtime" button. Mounted WITHOUT auth middleware;
		// the loopback gate is the security boundary.
		bootstrapHandler := server.NewBootstrapHandler(
			c.signer,
			identity.Identity{TenantID: DevTenant, UserID: DevUser, SessionID: DevSession},
			[]string{string(auth.ScopeAdmin), string(auth.ScopeConsoleFleet)},
			"http://"+m.BindAddr,
			m.Logger,
		)
		m.Router.Handle("POST /v1/dev/bootstrap.json", bootstrapHandler)

		// Console static build at `/` (the `harbor console` posture). The
		// Protocol surface (`/v1/*`, `/healthz`) takes precedence via Go's
		// longest-prefix match.
		if c.serveConsole {
			consoleHandler, err := newConsoleAssetHandler(m.Logger)
			if err != nil {
				return nil, fmt.Errorf("console assets: %w", err)
			}
			m.Router.Handle("/", consoleHandler)
		}

		return closers, nil
	}
}

// postBoot seeds the dev-only runtime-entity fixture set when
// HARBOR_DEV_SEED_FIXTURES=1. A production runtime boots empty.
func (c *devComposition) postBoot(stderr io.Writer) serve.PostBootFunc {
	return func(ctx context.Context, h serve.PostBootHandles) error {
		if os.Getenv(EnvDevSeedFixtures) != "1" {
			return nil
		}
		_, _ = fmt.Fprintln(stderr, DevSeedBanner)
		return seedDevFixtures(ctx, devSeedDeps{
			sessions:  h.Sessions,
			agents:    h.Agents,
			tasks:     h.Tasks,
			artifacts: h.Artifacts,
			memory:    h.Memory,
			tools:     h.Tools,
			flows:     h.Flows,
			bus:       h.Bus,
			logger:    h.Logger,
		})
	}
}

// printDevToken mints a default-identity dev token and prints the
// HARBOR_DEV_TOKEN contract line to stderr. Called once per `harbor dev` /
// `harbor console` run before serving (the token is stable across reboots).
func (c *devComposition) printDevToken(logger *slog.Logger, stderr io.Writer) error {
	token, err := c.signer.SignDevToken(time.Now(), DevTenant, DevUser, DevSession, []string{"admin", "console:fleet"})
	if err != nil {
		return err
	}
	logger.Info("harbor dev: dev token minted",
		slog.String("kid", DevKID),
		slog.String("tenant", DevTenant),
		slog.String("user", DevUser),
		slog.String("session", DevSession),
		slog.Duration("ttl", DevTokenTTL),
	)
	_, _ = fmt.Fprintf(stderr, "HARBOR_DEV_TOKEN=%s\n", token)
	return nil
}

// newLLMSnapshotBuilder returns the LLM snapshot builder shared by every
// cmd/harbor serving subcommand. It runs the fail-loud provider gate
// (`validateLLMProvider`) then projects the config onto the snapshot,
// overriding the driver to the mock when the dev escape hatch fired.
func newLLMSnapshotBuilder(allowMock bool) serve.LLMSnapshotBuilder {
	return func(cfg *config.Config) (*llm.ConfigSnapshot, error) {
		if err := validateLLMProvider(cfg, allowMock); err != nil {
			return nil, err
		}
		snap := llm.SnapshotFromConfig(cfg.LLM, cfg.Artifacts)
		if allowMock {
			// The operator's intent ("give me the mock") is explicit via the
			// env var; bypass the bifrost knobs.
			snap.Driver = "mock"
			snap.APIKey = ""
		}
		return &snap, nil
	}
}
