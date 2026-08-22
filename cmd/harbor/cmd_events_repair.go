package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

const (
	codeLegacyRepairInvalid  = "legacy_head_repair_invalid"
	codeLegacyRepairRefused  = "legacy_head_repair_refused"
	codeLegacyRepairInternal = "legacy_head_repair_internal_error"
)

type legacyRepairFlags struct {
	driver        string
	dsn           string
	mode          string
	freezeAck     bool
	timeout       time.Duration
	maxHeads      int
	maxDuplicates int
}

func newEventsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "events",
		Short: "inspect and repair durable event persistence",
	}
	cmd.AddCommand(newRepairLegacyHeadsCmd())
	return cmd
}

func newRepairLegacyHeadsCmd() *cobra.Command {
	flags := legacyRepairFlags{
		driver:        "postgres",
		mode:          string(durable.LegacyRepairInspect),
		timeout:       2 * time.Minute,
		maxHeads:      10000,
		maxDuplicates: 10000,
	}
	cmd := &cobra.Command{
		Use:   "repair-legacy-heads",
		Short: "inspect or repair duplicate references in legacy durable heads",
		Long: `Inspect legacy durable event heads without reading or printing payloads.

The default is a content-free dry-run. Apply requires the old event writers to
be stopped, an explicit --freeze-ack, and (for PostgreSQL) a direct, session-
affine port 5432 DSN. PgBouncer/transaction-pooled 6432 is never accepted for
mutation. Immutable entry bodies are never changed or deleted; only validated
redundant head references are removed through a conditional generation check.

Use --mode verify after apply to prove that no duplicate references remain.
The command opens the selected StateStore directly and never boots a Runtime
or EventBus writer.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRepairLegacyHeads(cmd, flags)
		},
	}
	cmd.Flags().StringVar(&flags.driver, "driver", flags.driver, "StateStore driver: postgres, sqlite, or inmem")
	cmd.Flags().StringVar(&flags.dsn, "dsn", "", "StateStore DSN (required for postgres and sqlite; never printed)")
	cmd.Flags().StringVar(&flags.mode, "mode", flags.mode, "operation: inspect, apply, or verify")
	cmd.Flags().BoolVar(&flags.freezeAck, "freeze-ack", false, "acknowledge that every event writer for the selected store is stopped/drained (required for apply)")
	cmd.Flags().BoolVar(&flags.freezeAck, "acknowledge-writer-drained", false, "alias for --freeze-ack")
	cmd.Flags().DurationVar(&flags.timeout, "timeout", flags.timeout, "maximum repair operation duration")
	cmd.Flags().IntVar(&flags.maxHeads, "max-heads", flags.maxHeads, "fail closed if the selected store has more heads than this bound")
	cmd.Flags().IntVar(&flags.maxDuplicates, "max-duplicates", flags.maxDuplicates, "fail closed if more duplicate sequence values are found than this bound")
	return cmd
}

func runRepairLegacyHeads(cmd *cobra.Command, flags legacyRepairFlags) error {
	mode := durable.LegacyRepairMode(strings.ToLower(strings.TrimSpace(flags.mode)))
	if mode != durable.LegacyRepairInspect && mode != durable.LegacyRepairApply && mode != durable.LegacyRepairVerify {
		return emitCLIError(cmd, CLIError{Subcommand: "events repair-legacy-heads", Message: fmt.Sprintf("unknown --mode %q (want inspect, apply, or verify)", flags.mode), Code: codeLegacyRepairInvalid})
	}
	if flags.timeout <= 0 {
		return emitCLIError(cmd, CLIError{Subcommand: "events repair-legacy-heads", Message: "--timeout must be positive", Code: codeLegacyRepairInvalid})
	}
	if flags.maxHeads <= 0 || flags.maxDuplicates <= 0 {
		return emitCLIError(cmd, CLIError{Subcommand: "events repair-legacy-heads", Message: "--max-heads and --max-duplicates must be positive", Code: codeLegacyRepairInvalid})
	}
	driver := strings.ToLower(strings.TrimSpace(flags.driver))
	if driver != "postgres" && driver != "sqlite" && driver != "inmem" {
		return emitCLIError(cmd, CLIError{Subcommand: "events repair-legacy-heads", Message: fmt.Sprintf("unsupported --driver %q", flags.driver), Code: codeLegacyRepairInvalid})
	}
	if driver != "inmem" && strings.TrimSpace(flags.dsn) == "" {
		return emitCLIError(cmd, CLIError{Subcommand: "events repair-legacy-heads", Message: "--dsn is required for the selected StateStore driver", Code: codeLegacyRepairInvalid})
	}
	if mode == durable.LegacyRepairApply {
		if !flags.freezeAck {
			return emitCLIError(cmd, CLIError{Subcommand: "events repair-legacy-heads", Message: "apply requires --freeze-ack after every event writer is stopped/drained", Code: codeLegacyRepairRefused, Hint: "run the default inspect first, stop/suspend all writers, then rerun with --freeze-ack"})
		}
		if driver == "postgres" {
			if err := durable.ValidateLegacyRepairApplyDSN(flags.dsn); err != nil {
				return emitCLIError(cmd, CLIError{Subcommand: "events repair-legacy-heads", Message: err.Error(), Code: codeLegacyRepairRefused, Hint: "use a direct/session-affine PostgreSQL port 5432 DSN; transaction-pooled 6432 is read-only"})
			}
		}
	}
	if mode == durable.LegacyRepairApply && flags.timeout < time.Second {
		return emitCLIError(cmd, CLIError{Subcommand: "events repair-legacy-heads", Message: "--timeout is too short for a safe apply", Code: codeLegacyRepairInvalid})
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), flags.timeout)
	defer cancel()
	store, cleanup, err := openLegacyRepairStore(driver, flags.dsn)
	if err != nil {
		return emitCLIError(cmd, CLIError{Subcommand: "events repair-legacy-heads", Message: "open StateStore failed", Code: codeLegacyRepairInternal})
	}
	defer cleanup()
	report, err := durable.RepairLegacyHeads(ctx, store, durable.LegacyRepairOptions{
		Mode:          mode,
		WriterDrained: flags.freezeAck,
		MaxAttempts:   8,
		MaxHeads:      flags.maxHeads,
		MaxDuplicates: flags.maxDuplicates,
	})
	if err != nil {
		code := codeLegacyRepairRefused
		if !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "pending") {
			code = codeLegacyRepairInternal
		}
		return emitCLIError(cmd, CLIError{Subcommand: "events repair-legacy-heads", Message: err.Error(), Code: code})
	}
	if err := writeLegacyRepairReport(cmd, report); err != nil {
		return emitCLIError(cmd, CLIError{Subcommand: "events repair-legacy-heads", Message: "write report failed", Code: codeLegacyRepairInternal})
	}
	return nil
}

func openLegacyRepairStore(driver, dsn string) (state.StateStore, func(), error) {
	cfg := config.StateConfig{Driver: driver, DSN: dsn, MigrationMode: sqlmigrate.ModeVerify}
	if driver != "postgres" {
		store, err := state.Open(context.Background(), cfg)
		if err != nil {
			return nil, func() {}, err
		}
		return store, func() { _ = store.Close(context.Background()) }, nil
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, func() {}, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(30 * time.Second)
	store, err := postgres.NewWithDB(cfg, db)
	if err != nil {
		_ = db.Close()
		return nil, func() {}, err
	}
	return store, func() {
		_ = store.Close(context.Background())
		_ = db.Close()
	}, nil
}

func writeLegacyRepairReport(cmd *cobra.Command, report durable.LegacyRepairReport) error {
	if resolveJSONMode(cmd) {
		buf, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", buf)
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "mode=%s heads_scanned=%d affected_heads=%d duplicate_sequences=%d redundant_references=%d\n", report.Mode, report.HeadsScanned, report.AffectedHeadCount, report.DuplicateSequenceCount, report.RedundantReferenceCount); err != nil {
		return err
	}
	for _, head := range report.Heads {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "head=%s outcome=%s before=%s after=%s expected_generation=%s applied_generation=%s receipt=%s\n", head.HeadIdentityHash, head.Outcome, head.BeforeHeadHash, head.AfterHeadHash, head.ExpectedGeneration, head.AppliedGeneration, head.ReceiptID); err != nil {
			return err
		}
		for _, duplicate := range head.Duplicates {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  sequence=%d positions=%v entry_hash=%s metadata_hash=%s type=%s payload_sequence=%d\n", duplicate.Sequence, duplicate.Positions, duplicate.EntryHash, duplicate.MetadataHash, duplicate.PayloadType, duplicate.PayloadSeq); err != nil {
				return err
			}
		}
	}
	return nil
}
