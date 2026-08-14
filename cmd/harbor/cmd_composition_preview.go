// cmd/harbor/cmd_composition_preview.go — `harbor composition-preview`.
//
// The operator-facing CLI consumer of the read-only
// `agent_config.composition.preview` Protocol method — the
// effective-composition preview path: it reports what the strict
// run-start composer WOULD compose for the caller's verified
// identity + the effective boot-agent, WITHOUT materialising anything:
// no lifecycle creation, no admin pack verb, no AgentConfig revision
// write, no SkillStore/ArtifactStore write. It is a THIN caller over
// the same Protocol client surface a headless consumer uses — never a
// second composition path (one composition resolver).
//
// The CLI resolves the Bearer token exactly like the inspect-*
// subcommands (HARBOR_TOKEN, then ~/.harbor/token), attaches the
// operator-supplied (tenant, user, session) triple as the verified
// caller identity (identity is mandatory — §6), and targets the
// --agent effective boot-agent. It does NOT mint or revive agents and
// never invents a user/session identity: the body carries only
// `agent_id`, and the caller's own verified triple rides the
// X-Harbor-* headers + token principal.
//
// Output:
//
//   - Human (default): a deterministic multi-line rendering of the
//     typed outcome (available | unavailable | conflict | retired),
//     the deterministic set hashes, and the effective items with
//     their exact boot|revision|both provenance + canonical semantic
//     hash. Golden-pinned (testdata/golden/composition-preview-*.txt).
//   - --json: the canonical wire response re-encoded (the same shape
//     the Protocol client returns), so scripting consumers pipe the
//     exact Protocol surface.
//
// The typed unavailable/conflict/retired outcomes are SUCCESSFUL
// previews (exit 0) — the operator asked for a preview and received
// the typed verdict, never a fabricated error and never a blank
// state. Transport / auth / validation failures exit 1 with a
// structured CLIError.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// Stable CLI error codes for `harbor composition-preview`. New codes
// ADD entries to this block; existing codes are wire contracts pinned
// by the golden + unit tests. Identity/auth/bind failures reuse the
// shared inspect-* codes (same failure, same exit shape).
const (
	// CodeCompositionPreviewAgentMissing — no --agent was supplied.
	// Exit 1.
	CodeCompositionPreviewAgentMissing = "composition_preview_agent_missing"
	// CodeCompositionPreviewConnectFailed — the Protocol client could
	// not reach the Runtime (transport error, refused connection).
	// Exit 1.
	CodeCompositionPreviewConnectFailed = "composition_preview_connect_failed"
	// CodeCompositionPreviewHTTPStatus — the Runtime answered a
	// non-2xx status (bad token, unknown method, un-wired preview).
	// Exit 1.
	CodeCompositionPreviewHTTPStatus = "composition_preview_http_status"
)

// flagAgent is the `--agent` flag name for the composition-preview
// verb — the effective boot-agent whose composition is previewed.
const flagAgent = "agent"

// newCompositionPreviewCmd builds the `composition-preview` cobra
// subcommand.
func newCompositionPreviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "composition-preview",
		Short: "preview the effective composition a Runtime would compose for an agent",
		Long: `Preview what the strict run-start composer WOULD compose for your
verified identity + the effective boot-agent, without materialising
anything. Read-only: no lifecycle creation, no admin pack verb, no
config revision write, no skill/artifact store write.

The preview reports the typed outcome (available | unavailable |
conflict | retired), the deterministic set hashes (boot_pack_set_hash,
combined_hash, revision_hash), and the effective operator-tier items
with their exact boot|revision|both provenance and canonical semantic
hash — what a next run would actually compose.

Identity: the (tenant, user, session) triple is mandatory (CLAUDE.md
§6) and is YOUR verified caller identity — the CLI never mints or
revives an agent and never invents a user/session. The Bearer token
comes from HARBOR_TOKEN (preferred) or ~/.harbor/token (see
"harbor dev" stderr for HARBOR_DEV_TOKEN=).

Examples:
  HARBOR_TOKEN=$jwt harbor composition-preview \\
    --tenant dev --user dev --session dev --agent harbor-dev-agent

  harbor composition-preview --tenant dev --user dev --session dev \\
    --agent harbor-dev-agent --json`,
		RunE: runCompositionPreview,
	}
	cmd.Flags().String(flagBind, DefaultBind, "Runtime bind (host:port or full URL)")
	cmd.Flags().String(flagTenant, "", "tenant id (required)")
	cmd.Flags().String(flagUser, "", "user id (required)")
	cmd.Flags().String(flagSession, "", "session id (required)")
	cmd.Flags().String(flagAgent, "", "effective boot-agent id (required)")
	return cmd
}

// runCompositionPreview is the cobra RunE entry. It resolves the
// identity triple + token + bind, then delegates to the testable core.
func runCompositionPreview(cmd *cobra.Command, _ []string) error {
	bind, _ := cmd.Flags().GetString(flagBind)       //nolint:errcheck // flag statically registered; lookup cannot fail
	tenant, _ := cmd.Flags().GetString(flagTenant)   //nolint:errcheck // flag statically registered; lookup cannot fail
	user, _ := cmd.Flags().GetString(flagUser)       //nolint:errcheck // flag statically registered; lookup cannot fail
	session, _ := cmd.Flags().GetString(flagSession) //nolint:errcheck // flag statically registered; lookup cannot fail
	agent, _ := cmd.Flags().GetString(flagAgent)     //nolint:errcheck // flag statically registered; lookup cannot fail
	jsonMode := resolveJSONMode(cmd)

	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(user) == "" || strings.TrimSpace(session) == "" {
		return emitCLIError(cmd, CLIError{
			Subcommand: "composition-preview",
			Code:       CodeIdentityIncomplete,
			Message:    "--tenant, --user, --session are all required",
			Hint:       "pass --tenant=T --user=U --session=S; the Runtime rejects requests with an incomplete identity scope (CLAUDE.md §6)",
		})
	}
	if strings.TrimSpace(agent) == "" {
		return emitCLIError(cmd, CLIError{
			Subcommand: "composition-preview",
			Code:       CodeCompositionPreviewAgentMissing,
			Message:    "--agent is required",
			Hint:       "pass --agent <effective-boot-agent-id> — the agent whose effective composition is previewed (e.g. harbor-dev-agent)",
		})
	}

	endpoint, err := previewBaseURL(bind)
	if err != nil {
		return emitCLIError(cmd, asCLIErrorOr(err, "composition-preview"))
	}

	auth, err := resolveTokenFromOS()
	if err != nil {
		return emitCLIError(cmd, asCLIErrorOr(err, "composition-preview"))
	}

	return runCompositionPreviewAgainst(cmd.Context(), cmd.OutOrStdout(), compositionPreviewOpts{
		Endpoint: endpoint,
		Identity: prototypes.IdentityScope{Tenant: tenant, User: user, Session: session},
		AgentID:  strings.TrimSpace(agent),
		Auth:     auth,
		JSON:     jsonMode,
		Client:   defaultInspectClient(),
	}, func(cli CLIError) error { return emitCLIError(cmd, cli) })
}

// compositionPreviewOpts bundles the inputs
// runCompositionPreviewAgainst needs. Kept as a struct so tests drive
// each path against an httptest server without re-creating cobra
// wiring.
type compositionPreviewOpts struct {
	Endpoint string
	Identity prototypes.IdentityScope
	AgentID  string
	Auth     inspectAuth
	JSON     bool
	Client   *http.Client
}

// compositionPreviewClient is the slice of the Protocol client the
// composition-preview verb needs. protocolclient.New returns the narrow
// `Client`; the concrete client implements this additive surface.
type compositionPreviewClient interface {
	AgentConfigCompositionPreview(context.Context, prototypes.AgentConfigCompositionPreviewRequest) (prototypes.AgentConfigCompositionPreviewResponse, error)
}

// runCompositionPreviewAgainst is the testable core: builds the
// Protocol client against the Runtime, issues the read-only
// composition-preview call, and renders the typed result to out.
// Tests pass an httptest.Server's URL via opts.Endpoint.
func runCompositionPreviewAgainst(
	ctx context.Context,
	out io.Writer,
	opts compositionPreviewOpts,
	emit func(CLIError) error,
) error {
	protocol, err := protocolclient.New(protocolclient.Connection{
		BaseURL:  opts.Endpoint,
		Token:    protocolclient.StaticToken(opts.Auth.Token, opts.Identity),
		Identity: opts.Identity,
	}, protocolclient.WithHTTPClient(opts.Client))
	if err != nil {
		return emit(CLIError{
			Subcommand: "composition-preview",
			Code:       CodeCompositionPreviewConnectFailed,
			Message:    err.Error(),
			Hint:       "verify the Runtime is bound on the given host:port (try `curl " + DefaultBind + "/healthz`)",
		})
	}

	// The concrete Protocol client implements the additive v1.28
	// composition-preview method (the curated `Client` interface keeps
	// its narrow projection). The type assertion is the CLI's honest
	// contract with the shipped client surface.
	previewer, ok := protocol.(compositionPreviewClient)
	if !ok {
		return emit(CLIError{
			Subcommand: "composition-preview",
			Code:       CodeCompositionPreviewConnectFailed,
			Message:    "this CLI build's Protocol client does not implement the composition-preview method",
			Hint:       "rebuild the harbor binary from a build that ships the v1.28 Protocol surface",
		})
	}

	// The target triple is deliberately OMITTED: the ordinary path
	// resolves the caller's own verified triple from ctx, so the body
	// carries only the effective agent id — the CLI never invents a
	// user/session identity.
	resp, err := previewer.AgentConfigCompositionPreview(ctx, prototypes.AgentConfigCompositionPreviewRequest{
		AgentID: opts.AgentID,
	})
	if err != nil {
		var protocolErr *protocolclient.ProtocolError
		if errors.As(err, &protocolErr) {
			hint := "verify the Bearer token claims and that the Runtime is healthy at the given --bind"
			switch protocolErr.Status {
			case http.StatusUnauthorized:
				hint = "the Bearer token was rejected — check HARBOR_TOKEN claims and expiry"
			case http.StatusForbidden:
				hint = "the token's scope did not authorise the preview — the effective agent must be in the caller's signed agent_reach"
			case http.StatusNotImplemented:
				hint = "this Runtime does not ship the composition-preview surface — check the Runtime version and CLI compatibility"
			}
			return emit(CLIError{
				Subcommand: "composition-preview",
				Code:       CodeCompositionPreviewHTTPStatus,
				Message:    protocolErr.Error(),
				Hint:       hint,
			})
		}
		return emit(CLIError{
			Subcommand: "composition-preview",
			Code:       CodeCompositionPreviewConnectFailed,
			Message:    fmt.Sprintf("preview: %v", err),
			Hint:       "transport-layer failure; check the network and the Runtime status",
		})
	}

	if opts.JSON {
		return writeCompositionPreviewJSON(out, resp)
	}
	return writeCompositionPreviewHuman(out, resp)
}

// writeCompositionPreviewJSON re-encodes the typed wire response so
// the --json output is the exact Protocol surface (deterministic
// struct field order). A marshal failure is loud — never a silent
// partial shape.
func writeCompositionPreviewJSON(out io.Writer, resp prototypes.AgentConfigCompositionPreviewResponse) error {
	buf, err := json.Marshal(resp)
	if err != nil {
		return CLIError{
			Subcommand: "composition-preview",
			Code:       CodeCompositionPreviewConnectFailed,
			Message:    fmt.Sprintf("encode JSON: %v", err),
			Hint:       "internal encoder error; please report",
		}
	}
	buf = append(buf, '\n')
	if _, wErr := out.Write(buf); wErr != nil {
		return CLIError{
			Subcommand: "composition-preview",
			Code:       CodeCompositionPreviewConnectFailed,
			Message:    fmt.Sprintf("write JSON output: %v", wErr),
			Hint:       "stdout write failed; check the output pipe",
		}
	}
	return nil
}

// writeCompositionPreviewHuman renders the deterministic human-mode
// shape pinned by testdata/golden/composition-preview-human.txt. The
// typed outcome always renders (never a blank state); the set
// hashes + items render for the available outcome.
func writeCompositionPreviewHuman(out io.Writer, resp prototypes.AgentConfigCompositionPreviewResponse) error {
	write := func(format string, args ...any) {
		// The CLI writes to cmd.OutOrStdout; a write failure surfaces
		// on the next line — the golden assertions run against an
		// in-memory buffer that cannot fail.
		_, _ = fmt.Fprintf(out, format, args...)
	}

	write("outcome: %s\n", resp.Outcome)
	if resp.Widened {
		write("widened: true\n")
	}
	switch resp.Outcome {
	case "available":
		if resp.BootPackSetHash != "" {
			write("boot_pack_set_hash: %s\n", resp.BootPackSetHash)
		}
		if resp.CombinedHash != "" {
			write("combined_hash: %s\n", resp.CombinedHash)
		}
		if resp.RevisionHash != "" {
			write("revision_hash: %s\n", resp.RevisionHash)
		}
		if resp.RevisionID != "" {
			write("revision_id: %s\n", resp.RevisionID)
		}
		if resp.ContentHash != "" {
			write("content_hash: %s\n", resp.ContentHash)
		}
		if len(resp.Items) == 0 {
			write("items: none\n")
			return nil
		}
		write("items:\n")
		for _, item := range resp.Items {
			write("  %s  source=%s  semantic_hash=%s\n", item.Name, item.Source, item.SemanticHash)
			if item.Skill.Title != "" {
				write("      %s\n", item.Skill.Title)
			}
			if item.Skill.Trigger != "" {
				write("      trigger: %s\n", item.Skill.Trigger)
			}
		}
	case "unavailable":
		write("note: no boot-declared composition is readable for this (tenant, agent) — or the caller is not entitled to the target (non-oracular)\n")
	case "conflict":
		if resp.ConflictName != "" {
			write("conflict_name: %s\n", resp.ConflictName)
		}
		write("note: the strict composer refused a boot/revision conflict — a canonical name whose semantic content differs across the boot baseline and the active revision; never a silent overwrite\n")
	case "retired":
		write("note: the effective agent's terminal lifecycle tombstone is installed — the composition is no longer readable\n")
	default:
		// Unknown outcomes are integrity failures on the wire — loud,
		// never a blank or a guessed render.
		write("note: unexpected outcome %q — the Runtime answered outside the typed set\n", resp.Outcome)
	}
	return nil
}

// previewBaseURL normalises --bind (bare host:port or a full
// http(s) URL) into the absolute base URL the Protocol client needs.
// A malformed bind returns a fail-loud CLIError so the operator sees
// what they typed wrong.
func previewBaseURL(bind string) (string, error) {
	bind = strings.TrimSpace(bind)
	if bind == "" {
		return "", CLIError{
			Subcommand: "composition-preview",
			Code:       CodeBindInvalid,
			Message:    "--bind is empty",
			Hint:       "pass --bind 127.0.0.1:18080 (or the URL the Runtime listens on)",
		}
	}
	if strings.HasPrefix(bind, "http://") || strings.HasPrefix(bind, "https://") {
		u, err := url.Parse(bind)
		if err != nil {
			return "", CLIError{
				Subcommand: "composition-preview",
				Code:       CodeBindInvalid,
				Message:    fmt.Sprintf("--bind %q is not a valid URL: %v", bind, err),
			}
		}
		return strings.TrimRight(u.String(), "/"), nil
	}
	return "http://" + bind, nil
}
