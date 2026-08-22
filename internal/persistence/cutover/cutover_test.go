package cutover_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/persistence/cutover"
)

func TestSpec_AllSixProjectionSchemasRegistered(t *testing.T) {
	t.Helper()
	want := map[cutover.Subsystem][]string{
		cutover.SubsystemState:     {"state_records"},
		cutover.SubsystemMemory:    {"memory_state"},
		cutover.SubsystemArtifacts: {"artifacts_blobs"},
		cutover.SubsystemSkills:    {"skills", "installed_packages", "installed_package_supports"},
		cutover.SubsystemTurns:     {"turn_rows", "turn_sessions"},
		cutover.SubsystemRollups:   {"rollup_rows", "rollup_checkpoint", "rollup_fence"},
	}
	if len(cutover.AllSubsystems) != len(want) {
		t.Fatalf("AllSubsystems=%v, want six entries", cutover.AllSubsystems)
	}
	for _, sub := range cutover.AllSubsystems {
		got, err := cutover.TableNames(sub)
		if err != nil {
			t.Fatalf("TableNames(%s): %v", sub, err)
		}
		if len(got) != len(want[sub]) {
			t.Fatalf("TableNames(%s)=%v, want %v", sub, got, want[sub])
		}
		for i := range got {
			if got[i] != want[sub][i] {
				t.Errorf("TableNames(%s)[%d]=%q, want %q", sub, i, got[i], want[sub][i])
			}
		}
	}
}

func TestClassify_WrongStateLedgerCannotSatisfyMemory(t *testing.T) {
	snapshot := cutover.SchemaSnapshot{
		Tables: []string{cutover.LegacyLedgerTable, "state_records"},
		Legacy: []cutover.LegacyMigration{{Version: 1}},
	}
	got, err := cutover.Classify(cutover.SubsystemMemory, snapshot)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.Class != cutover.ClassMisprovisioned {
		t.Fatalf("class=%s, want %s (%s)", got.Class, cutover.ClassMisprovisioned, got.Diagnostic)
	}
	for _, term := range []string{"memory", "memory_state", "state_records", "schema_migrations"} {
		if !strings.Contains(got.Diagnostic, term) {
			t.Errorf("diagnostic %q does not name %q", got.Diagnostic, term)
		}
	}
}

func TestClassify_LegacyCorrectShapeRequiresExplicitAdoption(t *testing.T) {
	snapshot := cutover.SchemaSnapshot{
		Tables: []string{cutover.LegacyLedgerTable, "memory_state"},
		Legacy: []cutover.LegacyMigration{{Version: 1}},
	}
	got, err := cutover.Classify(cutover.SubsystemMemory, snapshot)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.Class != cutover.ClassLegacyAdoptable {
		t.Fatalf("class=%s, want %s", got.Class, cutover.ClassLegacyAdoptable)
	}
	if !strings.Contains(got.Diagnostic, cutover.LedgerTable) || !strings.Contains(got.Diagnostic, "explicitly adopt") {
		t.Fatalf("diagnostic=%q, want namespaced ledger and explicit adoption", got.Diagnostic)
	}
}

func TestCanonicalRowsHash_PreservesBytesAndIgnoresPlannerOrder(t *testing.T) {
	first, err := cutover.CanonicalRowsHash("state_records", []string{"kind", "bytes"}, []cutover.Row{
		{"kind": "a", "bytes": []byte{0, 1, 2}},
		{"kind": "b", "bytes": []byte{255}},
	})
	if err != nil {
		t.Fatalf("first hash: %v", err)
	}
	second, err := cutover.CanonicalRowsHash("state_records", []string{"bytes", "kind"}, []cutover.Row{
		{"kind": "b", "bytes": []byte{255}},
		{"kind": "a", "bytes": []byte{0, 1, 2}},
	})
	if err != nil {
		t.Fatalf("second hash: %v", err)
	}
	if first != second {
		t.Fatalf("hash differs with row/column order: %s != %s", first, second)
	}
	changed, err := cutover.CanonicalRowsHash("state_records", []string{"kind", "bytes"}, []cutover.Row{{"kind": "a", "bytes": []byte{0, 1, 3}}})
	if err != nil {
		t.Fatalf("changed hash: %v", err)
	}
	if first == changed {
		t.Fatal("hash did not change after BYTEA mutation")
	}
}

func TestBuildAndReconcile_PopulatedAllSix(t *testing.T) {
	validChecksum := strings.Repeat("a", 64)
	for _, sub := range cutover.AllSubsystems {
		t.Run(string(sub), func(t *testing.T) {
			tables, err := cutover.TableNames(sub)
			if err != nil {
				t.Fatal(err)
			}
			rows := make(map[string][]cutover.Row, len(tables))
			columns := make(map[string][]string, len(tables))
			for _, table := range tables {
				columns[table] = []string{"id", "body"}
				rows[table] = []cutover.Row{{"id": "identity", "body": []byte("exact-body")}}
			}
			snapshot := cutover.SchemaSnapshot{
				Tables:       append(append([]string{}, tables...), cutover.LedgerTable, cutover.IdentityTable),
				Namespaced:   []cutover.MigrationIdentity{{Subsystem: string(sub), Filename: "0001_init.sql", Version: 1, Checksum: validChecksum}},
				Identities:   []cutover.StoreIdentity{{Subsystem: string(sub), SchemaVersion: 1, Contract: validChecksum}},
				TableRows:    rows,
				TableColumns: columns,
			}
			a, err := cutover.BuildSubsystemManifest(sub, snapshot)
			if err != nil {
				t.Fatalf("source manifest: %v", err)
			}
			b, err := cutover.BuildSubsystemManifest(sub, snapshot)
			if err != nil {
				t.Fatalf("destination manifest: %v", err)
			}
			if err := cutover.Reconcile(a, b); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
		})
	}
}

func TestReconcile_RefusesCountAndHashMismatch(t *testing.T) {
	a := cutover.SubsystemManifest{Subsystem: cutover.SubsystemTurns, RowCount: 2, ContentSHA256: strings.Repeat("a", 64)}
	b := cutover.SubsystemManifest{Subsystem: cutover.SubsystemTurns, RowCount: 1, ContentSHA256: strings.Repeat("b", 64)}
	err := cutover.Reconcile(a, b)
	if err == nil || !strings.Contains(err.Error(), "reconciliation refused") {
		t.Fatalf("Reconcile error=%v, want mismatch refusal", err)
	}
}

func TestValidateApplyDSN_RejectsTransactionPool(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@example/harbor?pgbouncer_mode=transaction",
		"postgres://u:p@example:6432/harbor",
	} {
		if err := cutover.ValidateApplyDSN(dsn); err == nil {
			t.Errorf("ValidateApplyDSN(%q) succeeded", dsn)
		}
	}
	if err := cutover.ValidateApplyDSN("postgres://u:p@example:5432/harbor"); err != nil {
		t.Fatalf("direct 5432 rejected: %v", err)
	}
}

func TestCopyRequiresFreezeAcknowledgement(t *testing.T) {
	_, err := cutover.CopySubsystem(context.Background(), nil, nil, cutover.SubsystemState, cutover.CopyOptions{DestinationDSN: "postgres://example:5432/db"})
	if err == nil || !strings.Contains(err.Error(), "freeze/drain") {
		t.Fatalf("CopySubsystem error=%v, want freeze refusal", err)
	}
}

func TestCopyRows_InterruptionIsResumableWithIdempotentWriter(t *testing.T) {
	snapshot := cutover.SchemaSnapshot{
		TableRows: map[string][]cutover.Row{
			"state_records": {
				{"id": "one", "body": []byte("1")},
				{"id": "two", "body": []byte("2")},
				{"id": "three", "body": []byte("3")},
			},
		},
		TableColumns: map[string][]string{"state_records": {"id", "body"}},
	}
	destination := map[string]cutover.Row{}
	writes := 0
	err := cutover.CopyRows(context.Background(), cutover.SubsystemState, snapshot, 1, func(_ context.Context, _ string, _ []string, row cutover.Row) error {
		writes++
		if writes == 2 {
			return fmt.Errorf("simulated interruption")
		}
		destination[row["id"].(string)] = row
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "simulated interruption") {
		t.Fatalf("first copy error=%v, want interruption", err)
	}
	if len(destination) != 1 {
		t.Fatalf("destination rows after interruption=%d, want 1", len(destination))
	}
	if err := cutover.CopyRows(context.Background(), cutover.SubsystemState, snapshot, 1, func(_ context.Context, _ string, _ []string, row cutover.Row) error {
		key := row["id"].(string)
		if _, exists := destination[key]; !exists {
			destination[key] = row
		}
		return nil
	}); err != nil {
		t.Fatalf("resume copy: %v", err)
	}
	if len(destination) != 3 {
		t.Fatalf("destination rows after resume=%d, want 3", len(destination))
	}
}

func TestCopyRows_BoundedWriterSeesOnePayloadAtATime(t *testing.T) {
	const rowCount = 2000
	const payloadSize = 4096
	rows := make([]cutover.Row, rowCount)
	for i := range rows {
		rows[i] = cutover.Row{"id": fmt.Sprintf("row-%04d", i), "body": strings.Repeat("x", payloadSize)}
	}
	snapshot := cutover.SchemaSnapshot{
		TableRows:    map[string][]cutover.Row{"state_records": rows},
		TableColumns: map[string][]string{"state_records": {"id", "body"}},
	}
	active := 0
	peak := 0
	seen := 0
	if err := cutover.CopyRows(context.Background(), cutover.SubsystemState, snapshot, 64, func(_ context.Context, _ string, _ []string, row cutover.Row) error {
		active++
		if active > peak {
			peak = active
		}
		if len(row["body"].(string)) != payloadSize {
			t.Fatalf("payload length=%d, want %d", len(row["body"].(string)), payloadSize)
		}
		seen++
		active--
		return nil
	}); err != nil {
		t.Fatalf("CopyRows: %v", err)
	}
	if seen != rowCount || peak != 1 {
		t.Fatalf("seen=%d peak=%d, want seen=%d peak=1", seen, peak, rowCount)
	}
}
