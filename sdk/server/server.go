package server

import (
	"context"
	"io"
	"os"

	"github.com/hurtener/Harbor/internal/runtime/serve/external"
	"github.com/hurtener/Harbor/sdk/config"
	"github.com/hurtener/Harbor/sdk/llm"
	"github.com/hurtener/Harbor/sdk/tools"
)

// Handle is the running Protocol server Open returns: Serve binds the
// listener and runs until ctx cancels; Close drains every subsystem;
// BindAddr reports the bound address.
type Handle = external.Handle

// ErrConfigRequired is returned by Open when neither a config value nor
// a config-path is supplied. Compare via errors.Is.
var ErrConfigRequired = external.ErrConfigRequired

// ErrFrameworkIdentityIncomplete is returned when Options.Framework names
// only a version or only a commit. runtime.info reports framework provenance
// as an immutable pair, separate from the hosting binary's build identity.
var ErrFrameworkIdentityIncomplete = external.ErrFrameworkIdentityIncomplete

// FrameworkIdentity is the Harbor framework release compiled into the host.
// It is deliberately distinct from the host program's own Go build metadata:
// a compiled agent can use it to make runtime.info identify the Harbor source
// that provides its runtime semantics.
//
// Leave both fields empty to retain the compatibility fallback, which derives
// runtime.info's existing build_* fields from the hosting binary's Go build
// info. When set, Version and Commit are required together and are reported
// verbatim as framework_version and framework_commit.
type FrameworkIdentity struct {
	// Version is the pinned Harbor product version (for example, "v1.28.0").
	Version string
	// Commit is the immutable Harbor source revision for Version.
	Commit string
}

// Options carries Open's injection points. It is deliberately minimal —
// the production posture (JWKS from cfg.Identity, full config Validate)
// is not configurable.
type Options struct {
	// ExternalGrant supplies host-owned grant seams such as an in-process
	// credential resolver. Boot configuration remains authoritative for mode,
	// route, verifier keys, and stock coordinator endpoints.
	ExternalGrant llm.ExternalGrantConfig

	// RegisterCatalog, when non-nil, registers the served agent's
	// compiled in-process tools on the runtime catalog at the pre-policy
	// seam, so each tool receives its declared approval / OAuth / policy
	// wrapping (from tools.entries in harbor.yaml). Pass your project's
	// RegisterTools here. A non-nil error fails Open loud.
	RegisterCatalog func(catalog tools.ToolCatalog) error

	// ConfigPath is the load-from-config convenience: when Open is
	// called with a nil config, the configuration is loaded and
	// validated from this path. Ignored when a non-nil config is passed
	// to Open.
	ConfigPath string

	// Stderr is where the serve band writes Runtime lifecycle banners
	// (the HARBOR_DEV_BOUND line, the CORS-wildcard warning, the pprof
	// banner) and where slog output lands when a caller injects its own
	// logger. Nil defaults to os.Stderr — the headless posture. A
	// co-launch binary (e.g. `harbor serve --tui` or a generated
	// `--tui` binary) sets this to a captured buffer so Bubble Tea
	// frames are never overwritten; on failure the terminal is restored
	// before the captured stderr is printed.
	Stderr io.Writer

	// Framework identifies the Harbor framework release compiled into this
	// host. When non-zero, runtime.info reports this Version and Commit as
	// framework_version and framework_commit alongside the hosting program's
	// existing build_* metadata. Leave it zero to omit the additive framework
	// fields. Set Version and Commit together; a partial value fails Open with
	// ErrFrameworkIdentityIncomplete.
	Framework FrameworkIdentity
}

// Open composes the production Protocol server from cfg (or, when cfg is
// nil, from Options.ConfigPath) and returns a Handle whose Serve binds
// the listener. The configuration is validated loud before any
// subsystem opens — a missing JWKS source fails Open, naming the field —
// and the JWT validator is always built from cfg.Identity, its JWKS
// source fetched synchronously while the boot composes (a bad source
// fails Open with everything opened so far drained; never a server that
// starts and rejects every request).
//
// This is the facade's single Options adapter: it forwards to the
// internal serving band, which owns the JWKS factory, the config
// re-validation, and the pre-policy registrar wiring.
func Open(ctx context.Context, cfg *config.Config, opts Options) (*Handle, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	return external.OpenWithStderr(ctx, cfg, opts.ConfigPath, stderr, opts.RegisterCatalog,
		external.FrameworkIdentity(opts.Framework), opts.ExternalGrant)
}
