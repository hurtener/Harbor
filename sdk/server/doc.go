// Package server is the public SDK facade for serving the Harbor
// Protocol from your own Go binary — the external-serving on-ramp that
// makes an agent with compiled in-process tools reachable over the wire
// at parity with the stock harbor serve.
//
// Open turns a validated configuration into a running Protocol server:
//
//	import (
//	    _ "github.com/hurtener/Harbor/sdk/drivers/prod"
//
//	    "github.com/hurtener/Harbor/sdk/config"
//	    "github.com/hurtener/Harbor/sdk/server"
//	)
//
//	cfg, _ := config.Load(ctx, "harbor.yaml")
//	h, err := server.Open(ctx, cfg, server.Options{
//	    RegisterCatalog: agent.RegisterTools,
//	})
//	// ... handle err ...
//	defer h.Close(ctx)
//	_ = h.Serve(ctx) // blocks until ctx cancels
//
// # Production-only by construction
//
// Open ALWAYS builds the JWT validator from cfg.Identity (the operator's
// JWK Set) and re-runs the full configuration Validate, so a missing
// JWKS source or an otherwise-invalid config fails loud at Open, naming
// the field — never at the first request. There is no dev-signer, no
// mock-LLM knob, and none of the dev-only injection seams the runtime's
// dev loop uses. A served binary is exactly the production surface.
//
// The local-development loop is the three-command harbor token flow —
// the same one a self-hosted harbor serve operator uses:
//
//	harbor token keygen --out ./keys
//	# set identity.jwks_file: ./keys/jwks.json in harbor.yaml
//	harbor token mint --key ./keys/private.pem \
//	    --tenant acme --user alice --session s1 \
//	    --issuer <your issuer> --audience <your audience>
//
// # Compiled tools keep their declared policy
//
// Options.RegisterCatalog rides the assembly's pre-policy catalog seam:
// a tool it registers is wrapped with its declared approval / OAuth /
// policy shell (from tools.entries in harbor.yaml) before the run loop
// can dispatch it — identical to an operator's YAML-declared tool.
// Registering tools any other way (after Open returns) skips that shell.
//
// This package is an alias-based facade over the internal serving band;
// no mechanism lives here beyond the single Options adapter.
package server
