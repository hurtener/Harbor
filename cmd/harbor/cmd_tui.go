package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/entry"
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
	cmd.Flags().Bool(flagTUICompact, false, "deprecated: the conversation always renders inline in native terminal scrollback; accepted for compatibility, has no effect")
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
	compactSet := cmd.Flags().Changed(flagTUICompact)
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
	explicit := cmd.Flags().Changed(flagTUITokenFile)
	// Streams stay nil in the normal case so the entry point mounts on the
	// process terminal via Bubble Tea's own TTY discovery — the same path
	// `serve --tui` and `dev --tui` take. Only streams genuinely overridden
	// on the command (tests inject buffers via cmd.SetIn/cmd.SetOut) are
	// passed through, which bypasses TTY discovery.
	//
	// Deliberate consequence: a REDIRECTED stdin is ignored on this path —
	// discovery falls back to /dev/tty, so the keyboard is authoritative and
	// `printf ... | harbor tui` does not drive the UI (script a PTY instead,
	// as the integration harness does). With no controlling terminal at all
	// the attach fails loudly rather than ghost-rendering into a pipe.
	// Guard-rail for future tests: a test that reaches the program without
	// overriding BOTH streams will grab the developer's real terminal.
	var input io.Reader
	var output io.Writer
	if in := cmd.InOrStdin(); in != os.Stdin {
		input = in
	}
	if out := cmd.OutOrStdout(); out != os.Stdout {
		output = out
	}
	err = entry.Run(cmd.Context(), entry.Options{
		BaseURL:    base,
		Session:    session,
		Compact:    compact,
		CompactSet: compactSet,
		StateFile:  statePath,
		Input:      input,
		Output:     output,
		Now:        now,
		TokenResolver: func(ctx context.Context, requestedSession string) (string, string, error) {
			auth, sourcePath, aerr := resolveTUIAuth(tokenPath, explicit, requestedSession)
			if aerr != nil {
				return "", "", aerr
			}
			initialToken, ierr := resolveTUIInitialToken(auth.Token, requestedSession)
			if ierr != nil {
				return "", "", ierr
			}
			return initialToken, sourcePath, nil
		},
	})
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
