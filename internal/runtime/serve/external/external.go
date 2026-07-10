// Package external is the production-only entry point behind the public
// sdk/server facade: it turns a validated configuration into a running
// Harbor Protocol server that an external Go binary can host, with its
// compiled in-process tools wrapped at the pre-policy catalog seam.
//
// # Production posture by construction
//
// Open ALWAYS builds the JWT validator from the operator's identity
// config (a JWK Set fetched from identity.jwks_url or loaded from
// identity.jwks_file) and re-runs the full-binary Validate on the
// configuration, so a hand-built config cannot bypass validation and a
// config missing its JWKS source fails loud, naming the field. There is
// no dev-signer, no mock-LLM escape hatch, and none of the caller-side
// injection seams the serve band exposes for the dev subcommand — a
// server opened here is exactly the production surface harbor serve
// mounts, parameterized only by the compiled-tool registrar.
//
// The local-development loop is the three-command harbor token flow:
// keygen a keypair, point identity.jwks_file at the emitted JWK Set,
// and mint a short-lived JWT — the same loop a self-hosted harbor serve
// operator uses.
package external

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/tools"
)

// ErrConfigRequired is returned by Open when neither a config value nor
// a config path is supplied. Identity and configuration are mandatory:
// the serving facade never stands up a server from nothing.
var ErrConfigRequired = errors.New("server: a config or a config path is required")

// Handle is the running Protocol server a successful Open returns. It
// wraps the serve band's lifecycle so Close reports an error return for
// symmetry with Serve.
type Handle struct {
	h *serve.Handle
}

// Serve binds the listener and runs the Protocol server until ctx
// cancels, then drains within the configured shutdown grace period.
func (h *Handle) Serve(ctx context.Context) error { return h.h.Serve(ctx) }

// Close drains every subsystem in reverse dependency order. Idempotent:
// a second Close is a no-op. The returned error is always nil today
// (per-subsystem close failures are logged during the drain); the
// signature carries an error return so a future teardown that can fail
// loud does not break callers.
func (h *Handle) Close(ctx context.Context) error {
	h.h.Close(ctx)
	return nil
}

// BindAddr reports the address the listener is (or will be) bound to.
// After Serve binds an ephemeral port it reflects the OS-assigned
// address. Safe to call while Serve runs.
func (h *Handle) BindAddr() string { return h.h.BindAddr() }

// Open composes the production Protocol server from cfg (or, when cfg is
// nil, from the configuration loaded at configPath) and returns a Handle
// whose Serve binds the listener. registerCatalog, when non-nil, is the
// compiled-tool registrar wired onto the assembly's pre-policy catalog
// seam so the served agent's tools receive their declared approval /
// OAuth / policy wrapping.
//
// The configuration is validated (loud, naming the offending field) and
// the JWT validator is built from the operator's identity config before
// any subsystem opens: a missing JWKS source, an unreachable provider,
// or a missing LLM provider fails Open non-zero rather than starting a
// server that rejects every request.
func Open(ctx context.Context, cfg *config.Config, configPath string, registerCatalog func(catalog tools.ToolCatalog) error) (*Handle, error) {
	if cfg == nil {
		if configPath == "" {
			return nil, ErrConfigRequired
		}
		loaded, err := config.Load(ctx, configPath)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		cfg = loaded
	}

	boot, err := serve.Boot(ctx, serve.Options{
		Config:          cfg,
		SubcommandLabel: "server",
		// Production honors the operator-configured server.bind_addr,
		// which may name a non-loopback interface.
		PreferConfigBindAddr: true,
		// The production auth path: build the JWKS-backed validator from
		// the operator's identity config. No dev-only seam is composed.
		AuthValidatorFactory: prodJWKSFactory(),
		RegisterCatalog:      registerCatalog,
		DisplayName:          cfg.Telemetry.ServiceName,
		InstanceID:           serverInstanceID(),
		Stderr:               os.Stderr,
		// BuildLLMSnapshot nil → the default production projection; a
		// missing real provider fails loud at driver construction.
	})
	if err != nil {
		return nil, err
	}
	return &Handle{h: boot}, nil
}

// prodJWKSFactory returns the production auth-validator factory: it
// projects the operator's identity config onto a JWKS-backed Validator
// (URL or file source), wiring the assembled redactor / bus / logger.
// The initial JWKS fetch runs synchronously, so a bad source fails the
// boot loud.
func prodJWKSFactory() serve.AuthValidatorFactory {
	return func(ctx context.Context, cfg *config.Config, red audit.Redactor, bus events.EventBus, logger *slog.Logger) (auth.Validator, error) {
		return auth.NewJWKSValidator(ctx, cfg.Identity, auth.ValidatorDeps{
			Redactor: red,
			Logger:   logger,
			Bus:      bus,
		})
	}
}

// serverInstanceID mints a stable-per-process instance identifier for
// the served Runtime. A Console attached to multiple Runtimes keys each
// attachment by it.
func serverInstanceID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return "harbor-server-" + h
	}
	return "harbor-server"
}
