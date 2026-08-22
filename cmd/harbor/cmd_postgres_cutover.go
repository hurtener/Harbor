// cmd/harbor/cmd_postgres_cutover.go — non-destructive PostgreSQL cutover.
//
// `harbor postgres cutover` is intentionally a one-shot operator tool, not a
// runtime data service. It discovers source projections from actual
// information_schema tables, prepares no schema itself, copies only the six
// Harbor-owned projections, and refuses success until source/destination
// counts and canonical SHA-256 manifests agree. Source databases are never
// dropped or mutated.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	_ "github.com/jackc/pgx/v5/stdlib" // register the pgx database/sql driver

	"github.com/hurtener/Harbor/internal/persistence/cutover"
)

const (
	codePostgresCutoverInvalid  = "postgres_cutover_invalid"
	codePostgresCutoverRefused  = "postgres_cutover_refused"
	codePostgresCutoverInternal = "postgres_cutover_internal_error"
)

type postgresCutoverFlags struct {
	source      string
	destination string
	subsystem   string
	mode        string
	manifest    string
	frozen      bool
	dryRun      bool
	batchSize   int
}

func newPostgresCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "postgres",
		Short: "inspect and operate PostgreSQL persistence",
	}
	cmd.AddCommand(newPostgresCutoverCmd())
	return cmd
}

func newPostgresCutoverCmd() *cobra.Command {
	flags := postgresCutoverFlags{}
	cmd := &cobra.Command{
		Use:   "cutover",
		Short: "reconcile split PostgreSQL projections into a prepared destination",
		Long: `Inspect or copy Harbor's six PostgreSQL projections without deleting
or mutating a source database.

The destination must already be migrated by the direct, session-affine
PostgreSQL migration runner on port 5432. Transaction-pooled PgBouncer 6432
is permitted for read-only inspect/verify only. Copy requires --freeze-ack
after the runtime is drained and writes are quiescent. A machine-readable
manifest is emitted only after every selected projection reconciles by row
count and canonical SHA-256 content hash.

Stage-one rollout keeps distinct per-subsystem DSNs. Run this command later,
one runtime at a time, to consolidate into one logical database. Harbor never
drops the old databases; rollback is a configuration change until an operator
independently removes them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPostgresCutover(cmd, flags)
		},
	}
	cmd.Flags().StringVar(&flags.source, "source", "", "source PostgreSQL DSN (required; discovered by actual schema)")
	cmd.Flags().StringVar(&flags.destination, "destination", "", "prepared destination PostgreSQL DSN (required for copy/verify)")
	cmd.Flags().StringVar(&flags.subsystem, "subsystem", "all", "projection: all, state, memory, artifacts, skills, sessions.turns, or observability.rollups")
	cmd.Flags().StringVar(&flags.mode, "mode", "inspect", "operation: inspect, copy, or verify")
	cmd.Flags().StringVar(&flags.manifest, "manifest", "", "write the canonical JSON manifest to this file (default: stdout)")
	cmd.Flags().BoolVar(&flags.frozen, "freeze-ack", false, "acknowledge that source writes are frozen/drained (required for copy)")
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "inspect only; never write the destination")
	cmd.Flags().IntVar(&flags.batchSize, "batch-size", 256, "maximum rows between resumable copy checkpoints")
	return cmd
}

func runPostgresCutover(cmd *cobra.Command, flags postgresCutoverFlags) error {
	ctx := cmd.Context()
	if strings.TrimSpace(flags.source) == "" {
		return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: "--source is required", Code: codePostgresCutoverInvalid})
	}
	if flags.batchSize <= 0 {
		return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: "--batch-size must be positive", Code: codePostgresCutoverInvalid})
	}
	mode := strings.ToLower(strings.TrimSpace(flags.mode))
	if flags.dryRun {
		mode = "inspect"
	}
	if mode != "inspect" && mode != "copy" && mode != "verify" {
		return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: fmt.Sprintf("unknown --mode %q (want inspect, copy, or verify)", flags.mode), Code: codePostgresCutoverInvalid})
	}
	if (mode == "copy" || mode == "verify") && strings.TrimSpace(flags.destination) == "" {
		return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: "--destination is required for copy/verify", Code: codePostgresCutoverInvalid})
	}
	if strings.TrimSpace(flags.destination) != "" && strings.TrimSpace(flags.destination) == strings.TrimSpace(flags.source) {
		return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: "source and destination DSNs must differ; Harbor will not self-copy a live database", Code: codePostgresCutoverInvalid})
	}
	if mode == "copy" && !flags.frozen {
		return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: "copy requires --freeze-ack after the source runtime is frozen or drained", Code: codePostgresCutoverRefused, Hint: "run inspect first, stop/drain writes, then rerun with --freeze-ack"})
	}
	if mode == "copy" {
		if err := cutover.ValidateApplyDSN(flags.destination); err != nil {
			return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: err.Error(), Code: codePostgresCutoverRefused, Hint: "apply/bootstrap through direct PostgreSQL 5432; use 6432 only for read-only verify"})
		}
	}
	subsystems, err := cutoverSelection(flags.subsystem)
	if err != nil {
		return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: err.Error(), Code: codePostgresCutoverInvalid})
	}

	sourceDB, err := openCutoverDB(ctx, flags.source)
	if err != nil {
		return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: fmt.Sprintf("open source: %v", err), Code: codePostgresCutoverInternal})
	}
	defer func() { _ = sourceDB.Close() }()
	var destinationDB *sql.DB
	if flags.destination != "" {
		destinationDB, err = openCutoverDB(ctx, flags.destination)
		if err != nil {
			return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: fmt.Sprintf("open destination: %v", err), Code: codePostgresCutoverInternal})
		}
		defer func() { _ = destinationDB.Close() }()
	}

	manifest := cutover.NewManifest(cutover.RedactDSN(flags.source), cutover.RedactDSN(flags.destination), flags.frozen)
	for _, sub := range subsystems {
		var sourceManifest cutover.SubsystemManifest
		sourceSnapshot, inspectErr := cutover.InspectSQL(ctx, sourceDB, sub)
		if inspectErr != nil {
			return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: fmt.Sprintf("inspect source %s: %v", sub, inspectErr), Code: codePostgresCutoverInternal})
		}
		sourceManifest, err = cutover.BuildSubsystemManifest(sub, sourceSnapshot)
		if err != nil {
			return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: fmt.Sprintf("classify source %s: %v", sub, err), Code: codePostgresCutoverRefused})
		}
		if err := manifest.AddSubsystem(sourceManifest); err != nil {
			return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: err.Error(), Code: codePostgresCutoverInternal})
		}
		if sourceManifest.Classification.Class == cutover.ClassMisprovisioned || sourceManifest.Classification.Class == cutover.ClassUnknown {
			if manifestErr := writeCutoverManifest(cmd, flags.manifest, manifest); manifestErr != nil {
				return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: fmt.Sprintf("write refusal manifest: %v", manifestErr), Code: codePostgresCutoverInternal})
			}
			return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: fmt.Sprintf("source %s refused: %s", sub, sourceManifest.Classification.Diagnostic), Code: codePostgresCutoverRefused, Hint: "classify by observed tables; do not copy a database because an environment variable named it for this subsystem"})
		}
		if destinationDB == nil {
			continue
		}
		destinationSnapshot, inspectErr := cutover.InspectSQL(ctx, destinationDB, sub)
		if inspectErr != nil {
			return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: fmt.Sprintf("inspect destination %s: %v", sub, inspectErr), Code: codePostgresCutoverInternal})
		}
		destinationManifest, buildErr := cutover.BuildSubsystemManifest(sub, destinationSnapshot)
		if buildErr != nil {
			return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: fmt.Sprintf("classify destination %s: %v", sub, buildErr), Code: codePostgresCutoverRefused})
		}
		switch mode {
		case "copy":
			if sourceManifest.Classification.Class != cutover.ClassEmpty {
				copyManifest, copyErr := cutover.CopySubsystem(ctx, sourceDB, destinationDB, sub, cutover.CopyOptions{SourceDSN: flags.source, DestinationDSN: flags.destination, Frozen: flags.frozen, BatchSize: flags.batchSize})
				if copyErr != nil {
					if manifestErr := writeCutoverManifest(cmd, flags.manifest, manifest); manifestErr != nil {
						return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: fmt.Sprintf("write copy refusal manifest: %v", manifestErr), Code: codePostgresCutoverInternal})
					}
					return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: copyErr.Error(), Code: codePostgresCutoverRefused, Hint: "copy is resumable; resolve the reported mismatch or cancellation, then rerun with the same source/destination"})
				}
				if len(copyManifest.DestinationSubsystems) == 1 {
					destinationManifest = copyManifest.DestinationSubsystems[0]
				}
			}
		case "verify":
			if err := cutover.Reconcile(sourceManifest, destinationManifest); err != nil {
				if manifestErr := writeCutoverManifest(cmd, flags.manifest, manifest); manifestErr != nil {
					return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: fmt.Sprintf("write verify refusal manifest: %v", manifestErr), Code: codePostgresCutoverInternal})
				}
				return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: err.Error(), Code: codePostgresCutoverRefused})
			}
		}
		if err := cutover.Reconcile(sourceManifest, destinationManifest); err != nil {
			if mode == "inspect" {
				// Inspect reports both sides but does not claim reconciliation.
				continue
			}
			return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: err.Error(), Code: codePostgresCutoverRefused})
		}
		manifest.DestinationSubsystems = append(manifest.DestinationSubsystems, destinationManifest)
	}
	if err := writeCutoverManifest(cmd, flags.manifest, manifest); err != nil {
		return emitCLIError(cmd, CLIError{Subcommand: "postgres cutover", Message: err.Error(), Code: codePostgresCutoverInternal})
	}
	return nil
}

func cutoverSelection(value string) ([]cutover.Subsystem, error) {
	if strings.EqualFold(strings.TrimSpace(value), "all") {
		return append([]cutover.Subsystem(nil), cutover.AllSubsystems...), nil
	}
	sub, err := cutover.ParseSubsystem(value)
	if err != nil {
		return nil, err
	}
	return []cutover.Subsystem{sub}, nil
}

func openCutoverDB(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	// This CLI intentionally uses one bounded connection per endpoint. The
	// runtime pool manager owns service pools; the one-shot tool must not
	// recreate six independent 25-open allowances while it runs.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(30 * time.Second)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return db, nil
}

func writeCutoverManifest(cmd *cobra.Command, path string, manifest cutover.Manifest) error {
	buf, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	buf = append(buf, '\n')
	if path != "" {
		if err := os.WriteFile(path, buf, 0o600); err != nil {
			return fmt.Errorf("write manifest %q: %w", path, err)
		}
		return nil
	}
	if _, err := io.Copy(cmd.OutOrStdout(), strings.NewReader(string(buf))); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}
