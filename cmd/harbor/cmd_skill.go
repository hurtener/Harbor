// cmd/harbor/cmd_skill.go — `harbor skill import` / `harbor skill rm`
// (Phase 111d, D-201).
//
// The operator-facing half of the canonical skills surface: both
// verbs are THIN callers over the same Go-level surface a headless
// consumer uses (`importer.ImportAndStore` / `SkillStore.Delete`) —
// never a second implementation. They operate on the store resolved
// from `harbor.yaml` (the SAME `skills:` block + projection
// `harbor dev` boots with), so an import lands exactly where the dev
// binary will read it. The resolved driver + DSN (userinfo-redacted)
// is printed so the operator can SEE where the skill landed — a
// mismatch silently importing into nowhere is the failure mode the
// print closes.
//
// Identity: skills are identity-scoped (§6). The verbs default to the
// `harbor dev` single-operator triple (dev/dev/dev) and expose
// --tenant / --user / --session for everything else.
//
// Exit codes: 0 success; 1 rejection / failure with a structured
// CLIError (honest stderr, `--json` honoured).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
	"github.com/hurtener/Harbor/internal/state"
)

// Stable CLI error codes for `harbor skill`. Wire contracts — smoke
// + golden tests assert against them.
const (
	// CodeSkillConfigInvalid — the config failed to load/validate, or
	// it declares no skills store.
	CodeSkillConfigInvalid = "skill_config_invalid"
	// CodeSkillImportRejected — the importer or the store's conflict
	// policy rejected the import (invalid Skills.md, duplicate name,
	// pack-overwrite refusal).
	CodeSkillImportRejected = "skill_import_rejected"
	// CodeSkillRmFailed — the named skill does not exist under the
	// identity, or the delete failed.
	CodeSkillRmFailed = "skill_rm_failed"
	// CodeSkillInternal — unexpected subsystem failure (store open,
	// bus open, I/O).
	CodeSkillInternal = "skill_internal_error"
)

// Flag names shared by the skill verbs.
const (
	flagSkillConfig    = "config"
	flagSkillTenant    = "tenant"
	flagSkillUser      = "user"
	flagSkillSession   = "session"
	flagSkillOverwrite = "overwrite"
)

// newSkillCmd builds the `skill` verb family.
func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "manage the runtime skill catalog (import / rm)",
		Long: `Manage the runtime skill catalog declared in harbor.yaml.

The verbs operate on the SAME store harbor dev serves (driver + DSN
from the config's skills: block), under an explicit identity triple
(default: the harbor dev triple ` + DevTenant + `/` + DevUser + `/` + DevSession + `).

  harbor skill import <path>   parse a Skills.md file and store it
  harbor skill rm <name>       remove a skill by name`,
	}
	cmd.AddCommand(newSkillImportCmd())
	cmd.AddCommand(newSkillRmCmd())
	return cmd
}

// addSkillFlags registers the flags both verbs share.
func addSkillFlags(cmd *cobra.Command) {
	cmd.Flags().String(flagSkillConfig, DefaultDevConfig, "path to harbor.yaml")
	cmd.Flags().String(flagSkillTenant, DevTenant, "tenant_id the skill is scoped to")
	cmd.Flags().String(flagSkillUser, DevUser, "user_id the skill is scoped to")
	cmd.Flags().String(flagSkillSession, DevSession, "session_id the skill is scoped to")
}

func newSkillImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <path>",
		Short: "import a Skills.md file into the configured skill store",
		Long: `Parse a spec-compliant Skills.md file (YAML frontmatter + ## Steps
body; inline attachments resolve relative to the file's directory) and
store it as an Origin=pack skill in the store harbor.yaml declares.

A skill whose name already exists is REJECTED (exit 1) unless
--overwrite is passed. Output names what happened — imported /
overwrote / rejected with the validator's reason.`,
		Args: cobra.ExactArgs(1),
		RunE: runSkillImport,
	}
	addSkillFlags(cmd)
	cmd.Flags().Bool(flagSkillOverwrite, false, "replace an existing skill with the same name")
	return cmd
}

func newSkillRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "remove a skill by name from the configured skill store",
		Args:  cobra.ExactArgs(1),
		RunE:  runSkillRm,
	}
	addSkillFlags(cmd)
	return cmd
}

// skillStoreHandle bundles the opened subsystem chain a skill verb
// needs plus its reverse-order closer.
type skillStoreHandle struct {
	cfg       *config.Config
	store     skills.SkillStore
	artifacts artifacts.ArtifactStore
	close     func(context.Context)
}

// openSkillStore loads + validates the config and opens the minimal
// dependency chain the store needs (audit → state → events → artifacts
// → skills), mirroring the assembly fan-out's head so the verb hits
// the SAME store `harbor dev` serves. Returns a CLIError-shaped error
// on failure.
func openSkillStore(ctx context.Context, sub, cfgPath string) (*skillStoreHandle, error) {
	cfg, err := config.Load(ctx, cfgPath)
	if err != nil {
		return nil, CLIError{
			Subcommand: sub,
			Message:    fmt.Sprintf("load config %q: %v", cfgPath, err),
			Code:       CodeSkillConfigInvalid,
			Hint:       "pass --config or create harbor.yaml (see examples/dev.yaml)",
		}
	}
	if cfg.Skills.Driver == "" {
		return nil, CLIError{
			Subcommand: sub,
			Message:    "config declares no skills store (skills.driver is empty)",
			Code:       CodeSkillConfigInvalid,
			Hint:       "add a skills: block to harbor.yaml (driver: localdb, dsn: <path>) — see examples/dev.yaml",
		}
	}

	var closers []func(context.Context) error
	closeAll := func(cctx context.Context) {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i](cctx) //nolint:errcheck // best-effort teardown of a one-shot CLI process
		}
	}
	fail := func(stage string, err error) (*skillStoreHandle, error) {
		closeAll(ctx)
		return nil, CLIError{
			Subcommand: sub,
			Message:    fmt.Sprintf("%s: %v", stage, err),
			Code:       CodeSkillInternal,
		}
	}

	red, err := audit.Open(ctx, cfg.Audit)
	if err != nil {
		return fail("audit", err)
	}
	stateStore, err := state.Open(ctx, cfg.State)
	if err != nil {
		return fail("state", err)
	}
	closers = append(closers, stateStore.Close)
	bus, err := events.OpenWith(ctx, cfg.Events, red, events.Deps{State: stateStore})
	if err != nil {
		return fail("events", err)
	}
	closers = append(closers, bus.Close)
	artStore, err := artifacts.Open(ctx, cfg.Artifacts)
	if err != nil {
		return fail("artifacts", err)
	}
	closers = append(closers, artStore.Close)
	store, err := skills.Open(ctx, skills.SnapshotFromConfig(cfg.Skills), skills.Deps{Bus: bus})
	if err != nil {
		return fail("skills store", err)
	}
	closers = append(closers, store.Close)

	return &skillStoreHandle{cfg: cfg, store: store, artifacts: artStore, close: closeAll}, nil
}

// skillIdentity reads the identity flags off cmd.
func skillIdentity(cmd *cobra.Command) identity.Identity {
	tenant, _ := cmd.Flags().GetString(flagSkillTenant)   //nolint:errcheck // flag statically registered; lookup cannot fail
	user, _ := cmd.Flags().GetString(flagSkillUser)       //nolint:errcheck // flag statically registered; lookup cannot fail
	session, _ := cmd.Flags().GetString(flagSkillSession) //nolint:errcheck // flag statically registered; lookup cannot fail
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
}

// redactDSN strips userinfo from URL-shaped DSNs (postgres://u:p@h/db)
// so the resolved-store print never leaks credentials. Bare file
// paths (the localdb shape) pass through unchanged.
func redactDSN(dsn string) string {
	if !strings.Contains(dsn, "://") {
		return dsn
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	u.User = url.User("****")
	return u.String()
}

// skillImportOutput is the --json wire shape for a successful import.
type skillImportOutput struct {
	Result string                `json:"result"` // "imported" | "overwrote"
	Driver string                `json:"driver"`
	DSN    string                `json:"dsn"`
	Report importer.ImportReport `json:"report"`
}

func runSkillImport(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	cfgPath, _ := cmd.Flags().GetString(flagSkillConfig)    //nolint:errcheck // flag statically registered; lookup cannot fail
	overwrite, _ := cmd.Flags().GetBool(flagSkillOverwrite) //nolint:errcheck // flag statically registered; lookup cannot fail
	jsonMode, quiet := resolveJSONMode(cmd), resolveQuietMode(cmd)

	handle, err := openSkillStore(ctx, "skill import", cfgPath)
	if err != nil {
		var cli CLIError
		if errors.As(err, &cli) {
			return emitCLIError(cmd, cli)
		}
		return emitCLIError(cmd, CLIError{Subcommand: "skill import", Message: err.Error(), Code: CodeSkillInternal})
	}
	defer handle.close(ctx)

	id := skillIdentity(cmd)
	var opts []importer.ImportStoreOption
	if overwrite {
		opts = append(opts, importer.WithOverwrite())
	}
	rep, err := importer.ImportAndStore(ctx, id, handle.store, importer.Deps{Store: handle.artifacts}, args[0], opts...)
	if err != nil {
		hint := ""
		if errors.Is(err, importer.ErrDuplicateSkillName) {
			hint = "pass --overwrite to replace, or `harbor skill rm <name>` first"
		}
		return emitCLIError(cmd, CLIError{
			Subcommand: "skill import",
			Message:    fmt.Sprintf("rejected: %v", err),
			Code:       CodeSkillImportRejected,
			Hint:       hint,
		})
	}

	result := "imported"
	if rep.Overwrote {
		result = "overwrote"
	}
	out := skillImportOutput{
		Result: result,
		Driver: handle.cfg.Skills.Driver,
		DSN:    redactDSN(handle.cfg.Skills.DSN),
		Report: rep,
	}
	if jsonMode {
		enc := json.NewEncoder(cmd.OutOrStdout())
		return enc.Encode(out)
	}
	if !quiet {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %q (scope=%s, steps=%d, attachments=%d)\n",
			result, rep.Name, rep.Scope, rep.Steps, rep.Attachments)
		fmt.Fprintf(cmd.OutOrStdout(), "store: driver=%s dsn=%s\n", out.Driver, out.DSN)
	}
	return nil
}

// skillRmOutput is the --json wire shape for a successful rm.
type skillRmOutput struct {
	Result string `json:"result"` // "removed"
	Name   string `json:"name"`
	Driver string `json:"driver"`
	DSN    string `json:"dsn"`
}

func runSkillRm(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	cfgPath, _ := cmd.Flags().GetString(flagSkillConfig) //nolint:errcheck // flag statically registered; lookup cannot fail
	jsonMode, quiet := resolveJSONMode(cmd), resolveQuietMode(cmd)

	handle, err := openSkillStore(ctx, "skill rm", cfgPath)
	if err != nil {
		var cli CLIError
		if errors.As(err, &cli) {
			return emitCLIError(cmd, cli)
		}
		return emitCLIError(cmd, CLIError{Subcommand: "skill rm", Message: err.Error(), Code: CodeSkillInternal})
	}
	defer handle.close(ctx)

	id := skillIdentity(cmd)
	name := args[0]
	if err := handle.store.Delete(ctx, identity.Quadruple{Identity: id}, name); err != nil {
		hint := ""
		if errors.Is(err, skills.ErrSkillNotFound) {
			hint = "names are identity-scoped — check --tenant/--user/--session match the import"
		}
		return emitCLIError(cmd, CLIError{
			Subcommand: "skill rm",
			Message:    fmt.Sprintf("remove %q: %v", name, err),
			Code:       CodeSkillRmFailed,
			Hint:       hint,
		})
	}

	out := skillRmOutput{
		Result: "removed",
		Name:   name,
		Driver: handle.cfg.Skills.Driver,
		DSN:    redactDSN(handle.cfg.Skills.DSN),
	}
	if jsonMode {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}
	if !quiet {
		fmt.Fprintf(cmd.OutOrStdout(), "removed %q\n", name)
		fmt.Fprintf(cmd.OutOrStdout(), "store: driver=%s dsn=%s\n", out.Driver, out.DSN)
	}
	return nil
}
