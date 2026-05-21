// cmd/harbor/cmd_console.go — the `harbor console` subcommand
// (Phase 73m / D-129; RFC §7, D-091).
//
// D-091 pins the Console deployment model: the Harbor Runtime ships
// headless, and the full SvelteKit Console is served EXCLUSIVELY by the
// `harbor console` subcommand, which bakes the static build into
// `cmd/harbor` via `embed.FS` (console_embed.go). The Console build is
// NEVER embedded into `harbor dev` — `harbor dev --help` advertises no
// console-serving flag, and the smoke script greps for that.
//
// # Why `harbor console` is self-contained
//
// `harbor console` boots the SAME embedded Runtime stack `harbor dev`
// boots (config load, every subsystem, the Phase 60 Protocol mux) AND
// additionally mounts the embedded Console static build at `/`. The
// result is a single, self-contained Console deployment: an operator
// runs `harbor console`, opens the printed URL in a browser, and the
// Console is already attached to a live Runtime on the same process.
// D-091's "the Console can also run on a different machine, attached to
// a remote Runtime" remains true — the Console is a Protocol client and
// the operator can re-point it at any remote Runtime via the Settings
// page Connected-Runtimes card. The co-resident Runtime is the
// zero-config default, not a constraint.
//
// This does NOT violate the D-091 binding rule: the rule forbids
// embedding the Console into `harbor dev`. `harbor console` IS the
// subcommand D-091 designates to serve the Console.
//
// # Identity + auth posture
//
// The Protocol surface `harbor console` serves is gated by the SAME
// Phase 61 JWT auth.Middleware + identity-scope checks as `harbor dev`
// — every `/v1/*` call carries a verified `(tenant, user, session)`
// triple. The static Console assets at `/` are public (an SPA's HTML /
// JS bundle is not a secret); the Console authenticates to the Protocol
// surface from the browser using the bearer token the operator
// configures on the Settings page.

package main

import (
	"context"
	_ "embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// defaultConsoleConfig is the embedded zero-config default `harbor
// console` boots from when no `harbor.yaml` exists and `--config` was
// not passed (the first-clone-convenience path — §13's dev-only
// escape-hatch clause). Every driver is in-memory + the LLM seam is the
// `mock` driver; the fallback path enables the mock escape hatch and
// prints the §13 stderr banner so the dev-only posture is unmistakable.
//
//go:embed console_default.yaml
var defaultConsoleConfig []byte

// Flag names for the `console` subcommand. Declared as constants so the
// cmd body, tests, and the help golden reference one spelling.
const (
	flagConsolePort   = "port"
	flagConsoleConfig = "config"
	flagConsoleBind   = "bind"
)

// DefaultConsolePort is the loopback port `harbor console` listens on
// when the operator does not override via `--port` / `--bind`. Distinct
// from `harbor dev`'s 18080 so an operator can run both side by side.
const DefaultConsolePort = 18790

// newConsoleCmd builds the `console` cobra subcommand. Flags:
//
//	--config <path>  default `harbor.yaml`
//	--port <int>     default 18790
//	--bind <host:port> overrides --port; `127.0.0.1:0` requests an
//	                   ephemeral port (the D-104 pattern the e2e
//	                   harness uses).
//
// The subcommand deliberately mirrors `harbor dev`'s flag surface so an
// operator's muscle memory carries over; the ONE behavioural difference
// is that `harbor console` ALSO serves the embedded Console build.
func newConsoleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "console",
		Short: "serve the Harbor Console (static build + Protocol surface)",
		Long: `Serve the Harbor Console.

` + "`harbor console`" + ` bakes the static SvelteKit Console build into the
binary (D-091) and serves it at ` + "`/`" + `, alongside an embedded Harbor
Runtime + Protocol surface on the same port. Open the printed URL in a
browser and the Console is already attached to a live Runtime.

The Console is a Protocol client: from the Settings page an operator can
re-point it at any remote Harbor Runtime. The co-resident Runtime is the
zero-config default, not a constraint.

The default port is ` + fmt.Sprintf("%d", DefaultConsolePort) + `; override via --port or --bind.
` + "`--bind 127.0.0.1:0`" + ` requests an ephemeral port and prints the bound
address on stderr (HARBOR_DEV_BOUND=host:port).

The Console build is served ONLY by this subcommand — never by
` + "`harbor dev`" + ` (D-091 binding rule).

Examples:
  harbor console
  harbor console --port 9000
  harbor console --bind 127.0.0.1:0`,
		Args: cobra.NoArgs,
		RunE: runConsole,
	}
	cmd.Flags().String(flagConsoleConfig, DefaultDevConfig, "path to harbor.yaml")
	cmd.Flags().Int(flagConsolePort, DefaultConsolePort, "loopback port for the Console + Protocol server")
	cmd.Flags().String(flagConsoleBind, "", "host:port to bind (overrides --port; 127.0.0.1:0 = ephemeral)")
	return cmd
}

// runConsole is the cobra RunE entry for `harbor console`. It boots the
// embedded Runtime stack with the Console-asset router enabled, serves
// until SIGINT / SIGTERM, then drains. Every failure path returns a
// CLIError so the structured-error surface routes through the root.
func runConsole(cmd *cobra.Command, _ []string) error {
	cfgPath, _ := cmd.Flags().GetString(flagConsoleConfig)
	port, _ := cmd.Flags().GetInt(flagConsolePort)
	bindFlag, _ := cmd.Flags().GetString(flagConsoleBind)

	// `--bind` (or HARBOR_BIND) overrides `--port`. `127.0.0.1:0`
	// requests an ephemeral port — the D-104 pattern the e2e harness
	// uses to run sibling Consoles concurrently without colliding.
	bindAddrOverride := bindFlag
	if bindAddrOverride == "" {
		bindAddrOverride = os.Getenv("HARBOR_BIND")
	}
	if bindAddrOverride != "" {
		if p, ok := parsePortFromBind(bindAddrOverride); ok {
			port = p
		}
	}
	allowMock := os.Getenv(EnvDevAllowMock) == "1"

	logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Zero-config first-clone path: when no harbor.yaml exists and the
	// operator did not pass an explicit --config, materialise the
	// embedded default config to a temp file and boot from it. The
	// embedded default uses the `mock` LLM driver, so this path
	// auto-enables the §13 dev-only escape hatch (and bootDevStack
	// prints the [DEV-ONLY MOCK LLM …] banner). An operator running a
	// real Console deployment passes --config to a harbor.yaml wired to
	// a real provider.
	if _, statErr := os.Stat(cfgPath); statErr != nil && os.IsNotExist(statErr) {
		tmpCfg := filepath.Join(os.TempDir(), "harbor-console-default.yaml")
		if writeErr := os.WriteFile(tmpCfg, defaultConsoleConfig, 0o600); writeErr != nil {
			return emitCLIError(cmd, CLIError{
				Subcommand: "console",
				Message:    fmt.Sprintf("could not materialise the embedded default config: %v", writeErr),
				Code:       CodeBootInternal,
				Hint:       "pass --config <path> to a harbor.yaml you control",
			})
		}
		cfgPath = tmpCfg
		allowMock = true
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"harbor console: no harbor.yaml found — booting the embedded zero-config default (in-memory drivers + mock LLM). Pass --config for a real deployment.")
	}

	stack, err := bootDevStack(ctx, devBootOptions{
		cfgPath:         cfgPath,
		port:            port,
		bindAddr:        bindAddrOverride,
		allowMock:       allowMock,
		logger:          logger,
		stderr:          cmd.ErrOrStderr(),
		serveConsole:    true,
		subcommandLabel: "console",
	})
	if err != nil {
		return emitCLIError(cmd, bootErrorToCLIError(err))
	}
	defer stack.close(context.Background())

	if err := stack.serve(ctx); err != nil {
		return emitCLIError(cmd, CLIError{
			Subcommand: "console",
			Message:    fmt.Sprintf("console server stopped: %v", err),
			Code:       CodeBootInternal,
			Hint:       "check the server log lines above for the originating subsystem",
		})
	}
	return nil
}

// consoleAssetHandler is the http.Handler that serves the embedded
// SvelteKit Console build. It is a SPA handler: a request for a real
// embedded file (`/_app/...`, `/favicon.png`, …) serves that file; a
// request for any other path serves `index.html` so SvelteKit's
// client-side router resolves the route (the adapter-static `fallback`
// contract). The handler is immutable after construction (D-025).
type consoleAssetHandler struct {
	assets   fs.FS
	fileSrv  http.Handler
	indexApp []byte
	logger   *slog.Logger
}

// consolePlaceholderIndex is the SPA shell served when no real Console
// build was staged into cmd/harbor/consoledist/ (a bare checkout that
// never ran `make console-build`). It keeps `harbor console` booting
// and serving a meaningful page — never a blank 404 — at every build
// stage (the §4.2 smoke-skeleton posture).
const consolePlaceholderIndex = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>Harbor Console — build not bundled</title></head>
<body><main style="font-family:system-ui,sans-serif;padding:2rem;max-width:40rem;margin:0 auto">
<h1>Harbor Console</h1>
<p>This <code>harbor</code> binary was built without the Console static
assets bundled.</p>
<p>Run <code>make console-build</code> (which builds the SvelteKit Console
and stages it into <code>cmd/harbor/consoledist/</code>), then rebuild the
binary with <code>make build</code>.</p>
</main></body></html>
`

// newConsoleAssetHandler builds the Console-asset handler over the
// embedded build. Fails loudly if the embed sub-FS cannot be resolved
// (impossible-by-construction — the embed directive guarantees the
// directory exists). When no `index.html` was staged (a bare checkout
// that never ran `make console-build`), the handler serves a
// synthesized placeholder shell — `harbor console` still boots and
// serves a meaningful page (CLAUDE.md §5 — never a silent blank 404).
func newConsoleAssetHandler(logger *slog.Logger) (*consoleAssetHandler, error) {
	if logger == nil {
		logger = slog.Default()
	}
	assets, err := consoleAssets()
	if err != nil {
		return nil, fmt.Errorf("resolve embedded console assets: %w", err)
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		// No real Console build staged — serve the placeholder shell.
		index = []byte(consolePlaceholderIndex)
	}
	return &consoleAssetHandler{
		assets:   assets,
		fileSrv:  http.FileServerFS(assets),
		indexApp: index,
		logger:   logger,
	}, nil
}

// ServeHTTP serves the Console SPA. A path that resolves to a real
// embedded file is served by the embedded file server; any other path
// (a SvelteKit client-side route like `/overview` or `/settings`)
// serves `index.html` so the SPA router resolves it.
func (h *consoleAssetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "console assets accept GET / HEAD only", http.StatusMethodNotAllowed)
		return
	}
	clean := strings.TrimPrefix(r.URL.Path, "/")
	if clean == "" {
		h.serveIndex(w)
		return
	}
	if f, err := h.assets.Open(clean); err == nil {
		_ = f.Close()
		h.fileSrv.ServeHTTP(w, r)
		return
	}
	// Unknown path — a SvelteKit client-side route. Serve the SPA
	// shell so the in-browser router resolves it (adapter-static
	// `fallback: 'index.html'`).
	h.serveIndex(w)
}

// serveIndex writes the embedded `index.html` SPA shell.
func (h *consoleAssetHandler) serveIndex(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(h.indexApp); err != nil {
		h.logger.Warn("console: index.html write failed", slog.String("error", err.Error()))
	}
}
