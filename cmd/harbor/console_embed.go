// cmd/harbor/console_embed.go — the `harbor console` static-asset
// embed (Phase 73m / D-129).
//
// D-091 pins the Console deployment: the static SvelteKit build is
// baked into `cmd/harbor` via `embed.FS` and served EXCLUSIVELY by the
// `harbor console` subcommand — never by `harbor dev`. This file is the
// embed surface.
//
// # Why a committed `consoledist/` directory, not `web/console/build/`
//
// `//go:embed` paths are resolved relative to the package directory and
// MUST point inside the module tree at build time. `web/console/build/`
// is gitignored (CLAUDE.md §13 forbids committing build artifacts), so
// embedding it directly would fail `go build` on a bare checkout.
//
// Instead, `cmd/harbor/consoledist/` is a committed directory carrying
// a single committed `.gitkeep` (the directory's only tracked file —
// everything else is gitignored). `make console-build` produces the
// real SvelteKit static build and stages it into `consoledist/`; the
// production binary then serves the real Console. A bare checkout (no
// `make console-build`) still builds and boots — `harbor console`
// serves a synthesized "run make console-build" placeholder page when
// no `index.html` is embedded. This is the same posture the §4.2 smoke
// skeleton takes: the surface is always present and behaves sensibly
// at every build stage.

package main

import (
	"embed"
	"io/fs"
)

// consoleDistFS holds the embedded Console static build. The `all:`
// prefix includes files whose names begin with `_` or `.` —
// SvelteKit's adapter-static emits an `_app/` directory, so the
// default embed (which skips `_`-prefixed entries) would silently drop
// the entire JS/CSS bundle. `all:` is mandatory here.
//
//go:embed all:consoledist
var consoleDistFS embed.FS

// consoleAssets returns the embedded Console build rooted at the
// `consoledist` directory (so a request for `/index.html` maps to the
// embedded `consoledist/index.html`). Fails loudly if the sub-FS
// cannot be resolved — the embed directive guarantees the directory
// exists, so an error here is an impossible-by-construction wiring
// fault (CLAUDE.md §5).
func consoleAssets() (fs.FS, error) {
	return fs.Sub(consoleDistFS, "consoledist")
}
