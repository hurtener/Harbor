package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	"github.com/hurtener/Harbor/internal/tui/app"
	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

const (
	flagTUIAttach    = "attach"
	flagTUISession   = "session"
	flagTUITokenFile = "token-file"
	flagTUIStateFile = "state-file"
	flagTUICompact   = "compact"
)

// newTUICmd exposes the authenticated Protocol-only terminal client.
func newTUICmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tui", Short: "attach the native conversation client to a Runtime", Long: `Attach Harbor's native terminal conversation client to a running Runtime.

All state and streaming data travels through authenticated Harbor Protocol REST
and SSE. The command never starts, embeds, or directly accesses a Runtime.
Credentials are resolved before every request and reconnect from HARBOR_TOKEN or
the token file. Signed tokens are never extended or written to local state.`, Args: cobra.NoArgs, RunE: runTUI}
	cmd.Flags().String(flagTUIAttach, "", "Runtime base URL (required)")
	cmd.Flags().String(flagTUISession, "", "authorized session to restore or select")
	cmd.Flags().String(flagTUITokenFile, "", "rotating JWT file (default ~/.harbor/token)")
	cmd.Flags().String(flagTUIStateFile, "", "local interaction-state file (default ~/.harbor/tui-state.json)")
	cmd.Flags().Bool(flagTUICompact, false, "use compact transcript presentation")
	return cmd
}

func runTUI(cmd *cobra.Command, _ []string) error {
	base, err := cmd.Flags().GetString(flagTUIAttach)
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	if strings.TrimSpace(base) == "" {
		return emitCLIError(cmd, CLIError{Subcommand: "tui", Code: CodeBindInvalid, Message: "--attach is required", Hint: "pass --attach http://127.0.0.1:18080"})
	}
	tokenPath, err := cmd.Flags().GetString(flagTUITokenFile)
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	statePath, err := cmd.Flags().GetString(flagTUIStateFile)
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	session, err := cmd.Flags().GetString(flagTUISession)
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	compact, err := cmd.Flags().GetBool(flagTUICompact)
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	if tokenPath == "" {
		tokenPath = filepath.Join(home, tokenFileRel)
	}
	if statePath == "" {
		statePath = filepath.Join(home, ".harbor", "tui-state.json")
	}
	auth, sourcePath, err := resolveTUIAuth(tokenPath, cmd.Flags().Changed(flagTUITokenFile), session)
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	initialToken, err := resolveTUIInitialToken(auth.Token, session)
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	claims, err := conversation.ParseToken(initialToken, now())
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	identity := claims.Identity
	if strings.TrimSpace(session) != "" {
		identity.Session = strings.TrimSpace(session)
	}
	tokens := conversation.NewTokenSource(sourcePath, initialToken)
	probe, err := protocolclient.New(protocolclient.Connection{BaseURL: base, Token: tokens, Identity: identity})
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	info, err := probe.RuntimeInfo(cmd.Context())
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	store := conversation.NewStore(statePath)
	fingerprint := info.InstanceID + "@" + info.WireSurfaceDigest
	if restored, loadErr := store.LastSession(identity.Tenant, identity.User, fingerprint); loadErr != nil {
		return emitCLIError(cmd, tuiCLIError(loadErr))
	} else if session == "" && restored != "" {
		identity.Session = restored
	}
	interaction, _, err := store.Load(identity, fingerprint)
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	if cmd.Flags().Changed(flagTUICompact) {
		interaction.Compact = compact
	}
	theme := ui.CompileTheme(ui.EnvironmentFrom(os.LookupEnv))
	if interaction.Theme == string(ui.ModeLight) {
		theme = ui.NewTheme(ui.ModeLight, theme.Profile())
	}
	if interaction.Theme == string(ui.ModeDark) {
		theme = ui.NewTheme(ui.ModeDark, theme.Profile())
	}
	updates := conversation.NewNotifier(64)
	controller, err := conversation.NewController(base, tokens, identity, func(update conversation.Update) {
		updates.Notify(update)
	})
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	defer func() { _ = controller.Close() }()
	defer updates.Close()
	exportPath := filepath.Join(filepath.Dir(statePath), "exports", identity.Session+".md")
	model := app.NewRuntimeModel(cmd.Context(), 80, 24, theme, controller, updates, app.RuntimeOptions{Compact: interaction.Compact, ReducedMotion: interaction.ReducedMotion, Fingerprint: fingerprint, ExportPath: exportPath, State: interaction, Store: &store})
	input, output := cmd.InOrStdin(), cmd.OutOrStdout()
	if _, injectedInput := input.(interface{ Len() int }); injectedInput {
		err = app.Run(cmd.Context(), input, output, model)
	} else {
		err = app.Run(cmd.Context(), os.Stdin, os.Stdout, model)
	}
	if err != nil {
		return emitCLIError(cmd, tuiCLIError(err))
	}
	return nil
}

func resolveTUIAuth(tokenPath string, explicit bool, session string) (inspectAuth, string, error) {
	if explicit {
		body, err := os.ReadFile(tokenPath)
		if err != nil {
			return inspectAuth{}, "", fmt.Errorf("read TUI token file %s: %w", tokenPath, err)
		}
		value := strings.TrimSpace(string(body))
		if value == "" {
			return inspectAuth{}, "", fmt.Errorf("TUI token file %s is empty", tokenPath)
		}
		if _, err = resolveTUIInitialToken(value, session); err != nil {
			return inspectAuth{}, "", err
		}
		return inspectAuth{Token: value, Source: "file"}, tokenPath, nil
	}
	auth, err := resolveToken(os.Getenv, os.UserHomeDir, os.ReadFile)
	if err != nil {
		return inspectAuth{}, "", err
	}
	if auth.Source == "file" {
		return auth, tokenPath, nil
	}
	return auth, "", nil
}

var now = time.Now

func tuiCLIError(err error) CLIError {
	code := "tui_failed"
	hint := "check the Runtime URL, token expiry, token-file permissions, and identity scope"
	if errors.Is(err, conversation.ErrTokenExpired) || errors.Is(err, conversation.ErrTokenUnavailable) {
		code = CodeAuthRequired
		hint = "replace HARBOR_TOKEN or ~/.harbor/token with a non-expired JWT for the target session"
	}
	return CLIError{Subcommand: "tui", Code: code, Message: fmt.Sprintf("%v", err), Hint: hint}
}

func resolveTUIInitialToken(value, requestedSession string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "{") {
		if requestedSession != "" {
			claims, err := conversation.ParseToken(value, now())
			if err != nil {
				return "", err
			}
			if claims.Identity.Session != requestedSession {
				return "", fmt.Errorf("%w: credential session %s does not match requested %s", conversation.ErrTokenUnavailable, claims.Identity.Session, requestedSession)
			}
		}
		return value, nil
	}
	var tokens map[string]string
	if err := json.Unmarshal([]byte(value), &tokens); err != nil {
		return "", fmt.Errorf("tui: malformed token map: %w", err)
	}
	keys := make([]string, 0, len(tokens))
	for key := range tokens {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		token := strings.TrimSpace(tokens[key])
		claims, err := conversation.ParseToken(token, now())
		if err != nil {
			continue
		}
		if requestedSession == "" || claims.Identity.Session == requestedSession {
			return token, nil
		}
	}
	return "", conversation.ErrTokenUnavailable
}
