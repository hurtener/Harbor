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
// config missing its JWKS source fails loud, naming the field, before
// any subsystem opens. The JWKS source itself is then fetched and
// parsed synchronously while the boot composes (an unreachable URL or a
// keyless set fails Open non-zero; subsystems opened up to that point
// are drained before Open returns — the caller never sees a partial
// server). There is no dev-signer, no mock-LLM escape hatch, and none
// of the caller-side injection seams the serve band exposes for the dev
// subcommand — a server opened here is exactly the production surface
// harbor serve mounts, parameterized only by the compiled-tool
// registrar.
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
	"io"
	"os"
	"runtime/debug"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/tools"
)

// ErrConfigRequired is returned by Open when neither a config value nor
// a config path is supplied. Identity and configuration are mandatory:
// the serving facade never stands up a server from nothing.
var ErrConfigRequired = errors.New("server: a config or a config path is required")

// ErrFrameworkIdentityIncomplete is returned when an external host supplies
// only one half of its explicit Harbor framework identity. runtime.info emits
// framework provenance as an immutable pair, so callers must provide both.
var ErrFrameworkIdentityIncomplete = errors.New("server: framework identity requires both version and commit")

// FrameworkIdentity is the Harbor framework release compiled into an external
// host. It is an explicit product identity, not metadata for the host program
// itself. The all-empty value omits framework provenance; a non-empty value
// must provide Version and Commit together.
type FrameworkIdentity struct {
	Version string
	Commit  string
}

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

// WaitReady blocks until the listener binds (returning the actual
// OS-assigned address) or until the bind fails / ctx cancels. It is the
// race-safe one-shot readiness contract a co-launched client waits on
// before dialing. See serve.Handle.WaitReady for the full contract.
func (h *Handle) WaitReady(ctx context.Context) (string, error) { return h.h.WaitReady(ctx) }

// Open composes the production Protocol server from cfg (or, when cfg is
// nil, from the configuration loaded at configPath) and returns a Handle
// whose Serve binds the listener. registerCatalog, when non-nil, is the
// compiled-tool registrar wired onto the assembly's pre-policy catalog
// seam so the served agent's tools receive their declared approval /
// OAuth / policy wrapping.
//
// The configuration is validated (loud, naming the offending field)
// before any subsystem opens; the JWKS source is then fetched + parsed
// synchronously during the boot composition, so a missing JWKS source, an
// unreachable provider, or a missing LLM provider fails Open non-zero —
// never a server that starts and rejects every request. On any boot
// failure the already-opened subsystems are drained before Open returns.
//
// Open writes Runtime lifecycle banners (HARBOR_DEV_BOUND, CORS-wildcard
// warning, pprof banner) to os.Stderr. Callers that need to redirect
// this output (e.g. a co-launch binary that captures stderr so Bubble Tea
// frames are never overwritten) should call OpenWithStderr instead.
func Open(ctx context.Context, cfg *config.Config, configPath string, registerCatalog func(catalog tools.ToolCatalog) error) (*Handle, error) {
	return OpenWithStderr(ctx, cfg, configPath, os.Stderr, registerCatalog, FrameworkIdentity{})
}

// OpenWithStderr is Open with an explicit stderr sink. A nil stderr
// defaults to os.Stderr. The stderr writer receives the HARBOR_DEV_BOUND
// line, the CORS-wildcard warning, and the pprof debug banner — the
// same surfaces `harbor serve`'s co-launch path redirects to a captured
// buffer so the terminal stays clean for Bubble Tea. The slog logger
// the serve band builds also writes to this sink when the caller does
// not inject its own logger.
func OpenWithStderr(ctx context.Context, cfg *config.Config, configPath string, stderr io.Writer, registerCatalog func(catalog tools.ToolCatalog) error, framework FrameworkIdentity) (*Handle, error) {
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
	if stderr == nil {
		stderr = os.Stderr
	}

	resolvedFramework, err := resolveFrameworkIdentity(framework)
	if err != nil {
		return nil, err
	}
	version, commit := buildIdentity()
	boot, err := serve.Boot(ctx, serve.Options{
		Config:          cfg,
		SubcommandLabel: "server",
		// Production honors the operator-configured server.bind_addr,
		// which may name a non-loopback interface.
		PreferConfigBindAddr: true,
		// The production auth path: the ONE shared JWKS-backed validator
		// factory (the same one the serve subcommand injects). No
		// dev-only seam is composed.
		AuthValidatorFactory: serve.NewJWKSAuthValidatorFactory(),
		RegisterCatalog:      registerCatalog,
		DisplayName:          cfg.Telemetry.ServiceName,
		InstanceID:           serve.InstanceID("harbor-server"),
		BuildVersion:         version,
		BuildCommit:          commit,
		FrameworkVersion:     resolvedFramework.Version,
		FrameworkCommit:      resolvedFramework.Commit,
		Stderr:               stderr,
		// No LLM-snapshot builder is injected → the default production
		// projection applies; a missing real provider fails loud at
		// driver construction.
	})
	if err != nil {
		return nil, err
	}
	return &Handle{h: boot}, nil
}

// resolveFrameworkIdentity validates the explicit Harbor framework identity a
// host elects to expose. The zero value omits framework_version and
// framework_commit while buildIdentity continues to report host metadata.
func resolveFrameworkIdentity(framework FrameworkIdentity) (FrameworkIdentity, error) {
	if framework.Version == "" && framework.Commit == "" {
		return FrameworkIdentity{}, nil
	}
	if framework.Version == "" || framework.Commit == "" {
		return FrameworkIdentity{}, ErrFrameworkIdentityIncomplete
	}
	return framework, nil
}

// buildIdentity resolves the hosting binary's build identity from the Go
// build info (the main module's version + the vcs revision when the
// binary was built from a checkout), so runtime.info from an externally
// served binary reports a real version instead of an empty string.
func buildIdentity() (version, commit string) {
	version, commit = "unknown", "unknown"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, commit
	}
	if v := info.Main.Version; v != "" {
		version = v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			commit = s.Value
			break
		}
	}
	return version, commit
}
