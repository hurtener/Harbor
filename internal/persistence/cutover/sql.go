package cutover

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// SQLQuerier is the read-only part of the database/sql surface. It makes the
// classifier testable without a live database while keeping production code on
// parameterized database/sql queries.
type SQLQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// SQLExecutor is the write portion used by the resumable copier.
type SQLExecutor interface {
	SQLQuerier
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// InspectSQL discovers tables and ledgers from information_schema, then loads
// the six projection tables into a deterministic snapshot. The operation is
// read-only and is safe through transaction-pooled PgBouncer 6432.
func InspectSQL(ctx context.Context, db SQLQuerier, sub Subsystem) (SchemaSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return SchemaSnapshot{}, err
	}
	snapshot := SchemaSnapshot{TableRows: map[string][]Row{}, TableColumns: map[string][]string{}}
	rows, err := db.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		return SchemaSnapshot{}, fmt.Errorf("cutover: inspect tables: %w", err)
	}
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			_ = rows.Close()
			return SchemaSnapshot{}, fmt.Errorf("cutover: scan table list: %w", err)
		}
		snapshot.Tables = append(snapshot.Tables, table)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return SchemaSnapshot{}, fmt.Errorf("cutover: iterate table list: %w", err)
	}
	if err := rows.Close(); err != nil {
		return SchemaSnapshot{}, fmt.Errorf("cutover: close table list: %w", err)
	}

	if contains(snapshot.Tables, LedgerTable) {
		ledger, err := inspectNamespacedLedger(ctx, db)
		if err != nil {
			return SchemaSnapshot{}, err
		}
		snapshot.Namespaced = ledger
	}
	if contains(snapshot.Tables, LegacyLedgerTable) {
		legacy, err := inspectLegacyLedger(ctx, db)
		if err != nil {
			return SchemaSnapshot{}, err
		}
		snapshot.Legacy = legacy
	}
	if contains(snapshot.Tables, IdentityTable) {
		identities, err := inspectStoreIdentity(ctx, db)
		if err != nil {
			return SchemaSnapshot{}, err
		}
		snapshot.Identities = identities
	}

	spec, err := Spec(sub)
	if err != nil {
		return SchemaSnapshot{}, err
	}
	for _, table := range spec.tables {
		if !contains(snapshot.Tables, table.Name) {
			continue
		}
		columns, err := inspectColumns(ctx, db, table.Name)
		if err != nil {
			return SchemaSnapshot{}, err
		}
		snapshot.TableColumns[table.Name] = columns
		data, err := inspectRows(ctx, db, table, columns)
		if err != nil {
			return SchemaSnapshot{}, err
		}
		snapshot.TableRows[table.Name] = data
	}
	return snapshot, nil
}

func inspectNamespacedLedger(ctx context.Context, db SQLQuerier) ([]MigrationIdentity, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT subsystem, filename, version, checksum_sha256
		FROM harbor_schema_migrations
		ORDER BY subsystem, version`)
	if err != nil {
		return nil, fmt.Errorf("cutover: inspect %s: %w", LedgerTable, err)
	}
	defer func() { _ = rows.Close() }()
	var out []MigrationIdentity
	for rows.Next() {
		var row MigrationIdentity
		if err := rows.Scan(&row.Subsystem, &row.Filename, &row.Version, &row.Checksum); err != nil {
			return nil, fmt.Errorf("cutover: scan %s: %w", LedgerTable, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cutover: iterate %s: %w", LedgerTable, err)
	}
	return out, nil
}

func inspectLegacyLedger(ctx context.Context, db SQLQuerier) ([]LegacyMigration, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT version, applied_at
		FROM schema_migrations
		ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("cutover: inspect %s: %w", LegacyLedgerTable, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LegacyMigration
	for rows.Next() {
		var row LegacyMigration
		if err := rows.Scan(&row.Version, &row.AppliedAt); err != nil {
			return nil, fmt.Errorf("cutover: scan %s: %w", LegacyLedgerTable, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cutover: iterate %s: %w", LegacyLedgerTable, err)
	}
	return out, nil
}

func inspectStoreIdentity(ctx context.Context, db SQLQuerier) ([]StoreIdentity, error) {
	// The migration core's identity contract includes subsystem and the
	// release-owned schema contract checksum. Keep this query explicit: a
	// destination with a different identity must not be treated as ready.
	rows, err := db.QueryContext(ctx, `
		SELECT subsystem, schema_version, contract_checksum_sha256
		FROM harbor_store_identity
		ORDER BY subsystem`)
	if err != nil {
		return nil, fmt.Errorf("cutover: inspect %s: %w", IdentityTable, err)
	}
	defer func() { _ = rows.Close() }()
	var identities []StoreIdentity
	for rows.Next() {
		var identity StoreIdentity
		if err := rows.Scan(&identity.Subsystem, &identity.SchemaVersion, &identity.Contract); err != nil {
			return nil, fmt.Errorf("cutover: scan %s: %w", IdentityTable, err)
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cutover: iterate %s: %w", IdentityTable, err)
	}
	return identities, nil
}

func inspectColumns(ctx context.Context, db SQLQuerier, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = $1
		  AND is_generated = 'NEVER'
		ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("cutover: inspect columns for %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, fmt.Errorf("cutover: scan columns for %s: %w", table, err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cutover: iterate columns for %s: %w", table, err)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("cutover: table %s has no insertable columns", table)
	}
	return columns, nil
}

func inspectRows(ctx context.Context, db SQLQuerier, table TableSpec, columns []string) ([]Row, error) {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteIdentifier(column)
	}
	order := make([]string, 0, len(table.KeyColumns))
	for _, key := range table.KeyColumns {
		if contains(columns, key) {
			order = append(order, quoteIdentifier(key))
		}
	}
	query := "SELECT " + strings.Join(quoted, ", ") + " FROM " + quoteIdentifier(table.Name)
	if len(order) > 0 {
		query += " ORDER BY " + strings.Join(order, ", ")
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("cutover: read %s: %w", table.Name, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Row
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(values))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("cutover: scan %s: %w", table.Name, err)
		}
		row := make(Row, len(columns))
		for i, column := range columns {
			row[column] = cloneValue(values[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cutover: iterate %s: %w", table.Name, err)
	}
	return out, nil
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case []byte:
		return append([]byte(nil), typed...)
	default:
		return value
	}
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// BuildSubsystemManifest hashes the observed rows for one subsystem.
func BuildSubsystemManifest(sub Subsystem, snapshot SchemaSnapshot) (SubsystemManifest, error) {
	classification, err := Classify(sub, snapshot)
	if err != nil {
		return SubsystemManifest{}, err
	}
	spec, err := Spec(sub)
	if err != nil {
		return SubsystemManifest{}, err
	}
	manifest := SubsystemManifest{Subsystem: sub, Classification: classification}
	for _, table := range spec.tables {
		rows := snapshot.TableRows[table.Name]
		columns := snapshot.TableColumns[table.Name]
		if len(columns) == 0 {
			if classification.Class == ClassEmpty || contains(classification.MissingTables, table.Name) {
				continue
			}
			return SubsystemManifest{}, fmt.Errorf("cutover: %s table %s has no inspected columns", sub, table.Name)
		}
		hash, err := CanonicalRowsHash(table.Name, columns, rows)
		if err != nil {
			return SubsystemManifest{}, err
		}
		manifest.Tables = append(manifest.Tables, TableManifest{Table: table.Name, Columns: append([]string(nil), columns...), RowCount: int64(len(rows)), ContentSHA256: hash})
		manifest.RowCount += int64(len(rows))
	}
	manifest.ContentSHA256, err = CanonicalSubsystemHash(sub, manifest.Tables)
	if err != nil {
		return SubsystemManifest{}, err
	}
	sort.Slice(manifest.Tables, func(i, j int) bool { return manifest.Tables[i].Table < manifest.Tables[j].Table })
	return manifest, nil
}

// CopyOptions controls the bounded, resumable copy. BatchSize applies to row
// writes; a cancelled copy returns without a success manifest and can be
// safely retried because inserts are idempotent on destination conflicts.
type CopyOptions struct {
	SourceDSN      string
	DestinationDSN string
	Frozen         bool
	BatchSize      int
}

// CopySubsystem copies one source projection into an already-migrated
// destination and reconciles both sides before returning success. It never
// mutates source. The caller owns opening/closing *sql.DB pools so a runtime
// manager can account for them.
func CopySubsystem(ctx context.Context, source, destination SQLExecutor, sub Subsystem, opts CopyOptions) (Manifest, error) {
	if !opts.Frozen {
		return Manifest{}, fmt.Errorf("cutover: refusing %s copy without freeze/drain acknowledgement; pass --freeze-ack only after writes are quiescent", sub)
	}
	if err := ValidateApplyDSN(opts.DestinationDSN); err != nil {
		return Manifest{}, err
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 256
	}
	sourceSnapshot, err := InspectSQL(ctx, source, sub)
	if err != nil {
		return Manifest{}, err
	}
	destinationSnapshot, err := InspectSQL(ctx, destination, sub)
	if err != nil {
		return Manifest{}, err
	}
	sourceManifest, err := BuildSubsystemManifest(sub, sourceSnapshot)
	if err != nil {
		return Manifest{}, err
	}
	destinationManifest, err := BuildSubsystemManifest(sub, destinationSnapshot)
	if err != nil {
		return Manifest{}, err
	}
	if sourceManifest.Classification.Class == ClassMisprovisioned || sourceManifest.Classification.Class == ClassUnknown {
		return Manifest{}, fmt.Errorf("cutover: refusing source %s: %s", sub, sourceManifest.Classification.Diagnostic)
	}
	if destinationManifest.Classification.Class != ClassCompatible {
		return Manifest{}, fmt.Errorf("cutover: destination %s is not namespaced/verified: %s; apply migrations over direct 5432 before copy", sub, destinationManifest.Classification.Diagnostic)
	}
	if sourceManifest.Classification.Class != ClassEmpty {
		if err := copySnapshotRows(ctx, destination, sourceSnapshot, sub, opts.BatchSize); err != nil {
			return Manifest{}, err
		}
	}
	destinationSnapshot, err = InspectSQL(ctx, destination, sub)
	if err != nil {
		return Manifest{}, err
	}
	destinationManifest, err = BuildSubsystemManifest(sub, destinationSnapshot)
	if err != nil {
		return Manifest{}, err
	}
	if err := Reconcile(sourceManifest, destinationManifest); err != nil {
		return Manifest{}, err
	}
	manifest := NewManifest(opts.SourceDSN, opts.DestinationDSN, true)
	if err := manifest.AddSubsystem(sourceManifest); err != nil {
		return Manifest{}, err
	}
	manifest.DestinationSubsystems = append(manifest.DestinationSubsystems, destinationManifest)
	return manifest, nil
}

func copySnapshotRows(ctx context.Context, destination SQLExecutor, snapshot SchemaSnapshot, sub Subsystem, batchSize int) error {
	return CopyRows(ctx, sub, snapshot, batchSize, func(ctx context.Context, table string, columns []string, row Row) error {
		placeholders := make([]string, len(columns))
		quotedColumns := make([]string, len(columns))
		for i, column := range columns {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
			quotedColumns[i] = quoteIdentifier(column)
		}
		query := "INSERT INTO " + quoteIdentifier(table) + " (" + strings.Join(quotedColumns, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ") ON CONFLICT DO NOTHING"
		values := make([]any, len(columns))
		for i, column := range columns {
			values[i] = row[column]
		}
		if _, err := destination.ExecContext(ctx, query, values...); err != nil {
			return fmt.Errorf("cutover: copy %s.%s row: %w", sub, table, err)
		}
		return nil
	})
}

// RowWriter receives one exact source row. CopyRows is exported so a
// deployment wrapper can attach a bounded transaction/checkpoint writer while
// the core's deterministic interruption/resume behavior remains tested
// without a live database.
type RowWriter func(context.Context, string, []string, Row) error

// CopyRows walks the known projection tables in deterministic batches. It
// never mutates the snapshot and stops immediately on cancellation or writer
// failure; retrying from the same snapshot is safe when the writer is
// idempotent (the SQL writer uses ON CONFLICT DO NOTHING and final hash
// reconciliation catches divergent conflicts).
func CopyRows(ctx context.Context, sub Subsystem, snapshot SchemaSnapshot, batchSize int, writer RowWriter) error {
	if writer == nil {
		return fmt.Errorf("cutover: nil row writer for %s", sub)
	}
	if batchSize <= 0 {
		batchSize = 256
	}
	spec, err := Spec(sub)
	if err != nil {
		return err
	}
	for _, table := range spec.tables {
		rows := snapshot.TableRows[table.Name]
		columns := snapshot.TableColumns[table.Name]
		if len(rows) == 0 {
			continue
		}
		for start := 0; start < len(rows); start += batchSize {
			end := start + batchSize
			if end > len(rows) {
				end = len(rows)
			}
			for offset, row := range rows[start:end] {
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("cutover: copy %s.%s interrupted after %d rows: %w", sub, table.Name, start+offset, err)
				}
				if err := writer(ctx, table.Name, columns, row); err != nil {
					return fmt.Errorf("cutover: copy %s.%s row %d: %w", sub, table.Name, start+offset, err)
				}
			}
		}
	}
	return nil
}
