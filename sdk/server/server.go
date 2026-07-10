package server

import (
	"context"

	"github.com/hurtener/Harbor/internal/runtime/serve/external"
	"github.com/hurtener/Harbor/sdk/config"
	"github.com/hurtener/Harbor/sdk/tools"
)

// Handle is the running Protocol server Open returns: Serve binds the
// listener and runs until ctx cancels; Close drains every subsystem;
// BindAddr reports the bound address.
type Handle = external.Handle

// ErrConfigRequired is returned by Open when neither a config value nor
// a config-path is supplied. Compare via errors.Is.
var ErrConfigRequired = external.ErrConfigRequired

// Options carries Open's injection points. It is deliberately minimal —
// the production posture (JWKS from cfg.Identity, full config Validate)
// is not configurable.
type Options struct {
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
}

// Open composes the production Protocol server from cfg (or, when cfg is
// nil, from Options.ConfigPath) and returns a Handle whose Serve binds
// the listener. The configuration is validated loud before any
// subsystem opens, and the JWT validator is always built from
// cfg.Identity — a missing JWKS source fails Open, naming the field.
//
// This is the facade's single Options adapter: it forwards to the
// internal serving band, which owns the JWKS factory, the config
// re-validation, and the pre-policy registrar wiring.
func Open(ctx context.Context, cfg *config.Config, opts Options) (*Handle, error) {
	return external.Open(ctx, cfg, opts.ConfigPath, opts.RegisterCatalog)
}
