// Package entry is the shared TUI attach entry point consumed by both
// `harbor tui --attach` (cmd_tui.go) and the public `sdk/tui.Run` facade.
//
// It encapsulates the connection-only attach flow: resolve the token,
// build the Protocol client, probe RuntimeInfo, construct the Controller,
// build the RuntimeModel, and run the Bubble Tea program. The entry
// point holds NO Runtime, stack, event-bus, or server handle — it is a
// pure Protocol client consumer, preserving the client-only boundary.
//
// `sdk/tui` forwards to Run through this package; `cmd/harbor/cmd_tui.go`
// calls Run directly. Both paths share one implementation so the attach
// experience is identical regardless of how the TUI is launched.
package entry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/app"
	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

// TokenResolver resolves a bearer credential and its source path for one
// attach. The cmd_tui.go path injects its resolveTUIAuth/resolveTUIInitialToken
// pair; sdk/tui injects a simpler static-file resolver. A nil resolver
// means the caller has already supplied a TokenSource and no file
// resolution is needed.
type TokenResolver func(ctx context.Context, session string) (token string, sourcePath string, err error)

// Options carries the connection-only inputs Run consumes. No Runtime,
// stack, or server handle is present — the boundary is Protocol-only.
type Options struct {
	// BaseURL is the Runtime REST/SSE base URL (e.g. http://127.0.0.1:8686).
	BaseURL string
	// Token is the rotating JWT source. Resolved before every request and
	// SSE reconnect. When nil, TokenResolver MUST be non-nil.
	Token protocolclient.TokenSource
	// Session is the authorized session to restore or select. May be empty;
	// the entry point restores the last durable session when blank.
	Session string
	// TokenResolver, when non-nil, resolves the initial token + source path
	// from a file or environment. When Token is already set, this is nil.
	TokenResolver TokenResolver
	// StateFile is the local interaction-state path. Empty defaults to
	// ~/.harbor/tui-state.json.
	StateFile string
	// Compact selects compact transcript presentation.
	Compact bool
	// CompactSet reports whether the caller explicitly set Compact (via
	// --compact / --compact=false). When true, the explicit value — even
	// false — overrides any stored-true preference restored from local
	// interaction state. When false, the stored preference wins (the
	// default for SDK callers that never pass a flag).
	CompactSet bool
	// Input / Output are the terminal streams. When nil, os.Stdin /
	// os.Stdout are used (the co-launch and attach paths both want the
	// process terminal).
	Input  io.Reader
	Output io.Writer
	// Now is the clock for token expiry checks. Defaults to time.Now.
	Now func() time.Time
}

// Run executes the complete attach flow: resolve token, build client,
// probe RuntimeInfo, construct Controller, build RuntimeModel, run the
// Bubble Tea program. Returns nil on clean quit, an error on any
// failure (auth, network, state, terminal).
//
// The flow is identical to cmd_tui.go's runTUI — extracted here so
// `sdk/tui.Run` and `harbor tui --attach` share one implementation.
func Run(ctx context.Context, opts Options) error {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return errors.New("tui: base URL is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	input := opts.Input
	if input == nil {
		input = os.Stdin
	}
	output := opts.Output
	if output == nil {
		output = os.Stdout
	}
	statePath := opts.StateFile
	if statePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("tui: home directory: %w", err)
		}
		statePath = filepath.Join(home, ".harbor", "tui-state.json")
	}

	var tokens *conversation.LifetimeTokenSource
	var identity types.IdentityScope

	switch {
	case opts.Token != nil:
		// A pre-built TokenSource (the sdk/tui facade path). Derive
		// identity from the first token resolution.
		token, err := opts.Token.Token(ctx, types.IdentityScope{})
		if err != nil {
			return fmt.Errorf("tui: resolve token: %w", err)
		}
		claims, err := conversation.ParseToken(token, now())
		if err != nil {
			return err
		}
		identity = claims.Identity
		if strings.TrimSpace(opts.Session) != "" {
			identity.Session = strings.TrimSpace(opts.Session)
		}
		tokens = conversation.NewTokenSource("", token)
	case opts.TokenResolver != nil:
		token, sourcePath, err := opts.TokenResolver(ctx, opts.Session)
		if err != nil {
			return err
		}
		claims, err := conversation.ParseToken(token, now())
		if err != nil {
			return err
		}
		identity = claims.Identity
		if strings.TrimSpace(opts.Session) != "" {
			identity.Session = strings.TrimSpace(opts.Session)
		}
		tokens = conversation.NewTokenSource(sourcePath, token)
	default:
		return errors.New("tui: token source or token resolver is required")
	}

	probe, err := protocolclient.New(protocolclient.Connection{BaseURL: opts.BaseURL, Token: tokens, Identity: identity})
	if err != nil {
		return err
	}
	info, err := probe.RuntimeInfo(ctx)
	if err != nil {
		return err
	}
	store := conversation.NewStore(statePath)
	fingerprint := info.InstanceID + "@" + info.WireSurfaceDigest
	if restored, loadErr := store.LastSession(identity.Tenant, identity.User, fingerprint); loadErr != nil {
		return loadErr
	} else if opts.Session == "" && restored != "" {
		identity.Session = restored
	}
	interaction, _, err := store.Load(identity, fingerprint)
	if err != nil {
		return err
	}
	interaction.Compact = opts.Compact || interaction.Compact
	if opts.CompactSet {
		// An explicit flag (including --compact=false) overrides the
		// stored preference. Without this, a stored-true preference
		// survives `--compact=false` — the operator's explicit choice
		// is silently dropped.
		interaction.Compact = opts.Compact
	}
	theme := ui.CompileTheme(ui.EnvironmentFrom(os.LookupEnv))
	if interaction.Theme == string(ui.ModeLight) {
		theme = ui.NewTheme(ui.ModeLight, theme.Profile())
	}
	if interaction.Theme == string(ui.ModeDark) {
		theme = ui.NewTheme(ui.ModeDark, theme.Profile())
	}
	updates := conversation.NewNotifier(64)
	controller, err := conversation.NewController(opts.BaseURL, tokens, identity, func(update conversation.Update) {
		updates.Notify(update)
	})
	if err != nil {
		return err
	}
	defer func() { _ = controller.Close() }()
	defer updates.Close()
	exportPath := filepath.Join(filepath.Dir(statePath), "exports", identity.Session+".md")
	model := app.NewRuntimeModel(ctx, 80, 24, theme, controller, updates, app.RuntimeOptions{Compact: interaction.Compact, ReducedMotion: interaction.ReducedMotion, Fingerprint: fingerprint, ExportPath: exportPath, State: interaction, Store: &store})
	// When the caller did not override the streams, mount on the process
	// terminal so Bubble Tea performs its own TTY discovery. Handing it explicit
	// os.Stdin/os.Stdout suppresses that: the alternate screen never learns the
	// real window size and frames leak into the terminal's scrollback.
	if opts.Input == nil && opts.Output == nil {
		if err := app.RunTerminal(ctx, model); err != nil {
			return err
		}
		return nil
	}
	if err := app.Run(ctx, input, output, model); err != nil {
		return err
	}
	return nil
}
