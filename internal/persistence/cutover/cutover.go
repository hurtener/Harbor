// Package cutover contains the non-destructive PostgreSQL split-to-unified
// cutover contract for Harbor's six durable PostgreSQL projections.
//
// The package deliberately does not own migration application or runtime pool
// construction. A destination is prepared by the migration runner through a
// direct, session-affine PostgreSQL connection (normally port 5432); this
// package then inspects, copies, and reconciles rows. Reads may use a
// transaction-pooled endpoint because the cutover reader takes no advisory
// lock. No operation in this package drops, truncates, or deletes a source.
package cutover

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// LedgerTable is the single migration ledger table used by all six
// PostgreSQL projections. The subsystem column is part of the primary key;
// an integer from another store can therefore never satisfy verification.
const LedgerTable = "harbor_schema_migrations"

// IdentityTable stores the exact schema/contract identity proved by the
// migration runner before a store is opened.
const IdentityTable = "harbor_store_identity"

// LegacyLedgerTable is the v1.29.0 unqualified ledger. It is inspection-only
// here: it may be adopted only after the expected tables are proven and the
// operator explicitly runs the namespaced migration adoption step.
const LegacyLedgerTable = "schema_migrations"

// Subsystem identifies one Harbor-owned PostgreSQL projection.
type Subsystem string

const (
	SubsystemState     Subsystem = "state"
	SubsystemMemory    Subsystem = "memory"
	SubsystemArtifacts Subsystem = "artifacts"
	SubsystemSkills    Subsystem = "skills"
	SubsystemTurns     Subsystem = "sessions.turns"
	SubsystemRollups   Subsystem = "observability.rollups"
)

// AllSubsystems is the exhaustive v1.29.1 cutover order. Keep this list
// append-only and update the registry test when a new compatible projection is
// introduced.
var AllSubsystems = []Subsystem{
	SubsystemState,
	SubsystemMemory,
	SubsystemArtifacts,
	SubsystemSkills,
	SubsystemTurns,
	SubsystemRollups,
}

// TableSpec describes a table that belongs to one projection. KeyColumns are
// used only for deterministic source ordering; they never alter row content.
type TableSpec struct {
	Name       string
	KeyColumns []string
}

type subsystemSpec struct {
	sub            Subsystem
	tables         []TableSpec
	requiredLedger bool
}

// Spec returns the exhaustive Harbor-owned table contract for sub.
func Spec(sub Subsystem) (subsystemSpec, error) {
	switch sub {
	case SubsystemState:
		return subsystemSpec{sub: sub, tables: []TableSpec{{Name: "state_records", KeyColumns: []string{"tenant_id", "user_id", "session_id", "run_id", "kind"}}}, requiredLedger: true}, nil
	case SubsystemMemory:
		return subsystemSpec{sub: sub, tables: []TableSpec{{Name: "memory_state", KeyColumns: []string{"tenant_id", "user_id", "session_id", "run_id", "kind"}}}, requiredLedger: true}, nil
	case SubsystemArtifacts:
		return subsystemSpec{sub: sub, tables: []TableSpec{{Name: "artifacts_blobs", KeyColumns: []string{"tenant", "user", "session", "namespace", "id"}}}, requiredLedger: true}, nil
	case SubsystemSkills:
		return subsystemSpec{sub: sub, tables: []TableSpec{
			{Name: "skills", KeyColumns: []string{"tenant_id", "user_id", "session_id", "scope", "agent_id", "name"}},
			{Name: "installed_packages", KeyColumns: []string{"tenant_id", "user_id", "agent_id", "name"}},
			{Name: "installed_package_supports", KeyColumns: []string{"tenant_id", "user_id", "agent_id", "name", "path"}},
		}, requiredLedger: true}, nil
	case SubsystemTurns:
		return subsystemSpec{sub: sub, tables: []TableSpec{
			{Name: "turn_rows", KeyColumns: []string{"tenant_id", "user_id", "session_id", "sequence", "turn_id"}},
			{Name: "turn_sessions", KeyColumns: []string{"tenant_id", "user_id", "session_id"}},
		}, requiredLedger: true}, nil
	case SubsystemRollups:
		return subsystemSpec{sub: sub, tables: []TableSpec{
			{Name: "rollup_rows", KeyColumns: []string{"bucket_start", "tenant_id", "user_id", "session_id", "model"}},
			{Name: "rollup_checkpoint", KeyColumns: []string{"id"}},
			{Name: "rollup_fence", KeyColumns: []string{"tenant_id", "user_id", "session_id"}},
		}, requiredLedger: true}, nil
	default:
		return subsystemSpec{}, fmt.Errorf("cutover: unsupported subsystem %q", sub)
	}
}

// TableNames returns the required physical tables for sub in migration order.
func TableNames(sub Subsystem) ([]string, error) {
	spec, err := Spec(sub)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(spec.tables))
	for _, table := range spec.tables {
		result = append(result, table.Name)
	}
	return result, nil
}

// ParseSubsystem accepts the operator spelling used in config and manifests.
func ParseSubsystem(value string) (Subsystem, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "state":
		return SubsystemState, nil
	case "memory":
		return SubsystemMemory, nil
	case "artifacts", "artifact":
		return SubsystemArtifacts, nil
	case "skills", "skill":
		return SubsystemSkills, nil
	case "sessions.turns", "sessions/turns", "turns":
		return SubsystemTurns, nil
	case "observability.rollups", "observability/rollups", "rollups", "rollup":
		return SubsystemRollups, nil
	default:
		return "", fmt.Errorf("cutover: unsupported subsystem %q (want state, memory, artifacts, skills, sessions.turns, or observability.rollups)", value)
	}
}

// MigrationIdentity is the content-bound identity recorded in the namespaced
// ledger and store identity table. Checksum is the immutable SHA-256 of the
// embedded migration file.
type MigrationIdentity struct {
	Subsystem string `json:"subsystem"`
	Filename  string `json:"filename"`
	Version   int64  `json:"version"`
	Checksum  string `json:"checksum"`
}

// LegacyMigration records only what the v1.29.0 ledger exposed. Its presence
// is never sufficient to prove a subsystem by itself.
type LegacyMigration struct {
	Version   int64  `json:"version"`
	AppliedAt string `json:"applied_at,omitempty"`
}

// SchemaSnapshot is the result of an actual database inspection. Tables are
// observed from information_schema, never inferred from a DSN or environment
// variable. Namespaced and legacy ledgers are included for diagnostics.
type SchemaSnapshot struct {
	Tables       []string            `json:"tables"`
	Namespaced   []MigrationIdentity `json:"namespaced_migrations,omitempty"`
	Legacy       []LegacyMigration   `json:"legacy_migrations,omitempty"`
	Identities   []StoreIdentity     `json:"store_identities,omitempty"`
	TableRows    map[string][]Row    `json:"-"`
	TableColumns map[string][]string `json:"-"`
	// TableFingerprints is populated by InspectSQL. It contains only row
	// counts/hashes, never bodies, so a large production source is not retained
	// in memory while it is being classified or copied.
	TableFingerprints map[string]TableManifest `json:"-"`
}

// StoreIdentity is the subsystem-level contract proof stored alongside the
// migration ledger. Migration filename/version/checksum rows live in
// MigrationIdentity; this row binds the schema contract itself.
type StoreIdentity struct {
	Subsystem     string `json:"subsystem"`
	SchemaVersion int64  `json:"schema_version"`
	Contract      string `json:"contract_checksum_sha256,omitempty"`
}

// Class is the source/destination posture discovered from observed schema.
type Class string

const (
	ClassCompatible      Class = "compatible"
	ClassLegacyAdoptable Class = "legacy_adoptable"
	ClassEmpty           Class = "empty"
	ClassMisprovisioned  Class = "misprovisioned"
	ClassUnknown         Class = "unknown"
)

// Classification is a diagnostic, machine-readable schema decision.
type Classification struct {
	Subsystem        Subsystem `json:"subsystem"`
	Class            Class     `json:"class"`
	ExpectedTables   []string  `json:"expected_tables"`
	ObservedTables   []string  `json:"observed_tables"`
	MissingTables    []string  `json:"missing_tables,omitempty"`
	UnexpectedTables []string  `json:"unexpected_tables,omitempty"`
	ObservedLedger   string    `json:"observed_ledger,omitempty"`
	Diagnostic       string    `json:"diagnostic,omitempty"`
}

// Classify determines whether sub is safe to inspect/copy. In particular,
// memory never accepts state_records as its schema: the exact v1.29.0 false
// readiness shape is returned as ClassMisprovisioned with remediation.
func Classify(sub Subsystem, snapshot SchemaSnapshot) (Classification, error) {
	spec, err := Spec(sub)
	if err != nil {
		return Classification{}, err
	}
	observed := make(map[string]bool, len(snapshot.Tables))
	for _, table := range snapshot.Tables {
		observed[table] = true
	}
	expected := make([]string, 0, len(spec.tables))
	missing := make([]string, 0)
	for _, table := range spec.tables {
		expected = append(expected, table.Name)
		if !observed[table.Name] {
			missing = append(missing, table.Name)
		}
	}
	result := Classification{Subsystem: sub, ExpectedTables: expected, ObservedTables: append([]string(nil), snapshot.Tables...), MissingTables: missing}
	sort.Strings(result.ObservedTables)
	sort.Strings(result.ExpectedTables)
	for _, table := range snapshot.Tables {
		if table == LedgerTable || table == IdentityTable || table == LegacyLedgerTable || contains(expected, table) {
			continue
		}
		if knownTable(table) {
			result.UnexpectedTables = append(result.UnexpectedTables, table)
		}
	}
	sort.Strings(result.UnexpectedTables)
	if len(snapshot.Namespaced) > 0 {
		result.ObservedLedger = LedgerTable
	} else if len(snapshot.Legacy) > 0 {
		result.ObservedLedger = LegacyLedgerTable
	}

	if len(missing) == 0 {
		if err := validateIdentity(sub, snapshot); err != nil {
			result.Class = ClassMisprovisioned
			result.Diagnostic = err.Error()
			return result, nil
		}
		if len(snapshot.Namespaced) > 0 {
			result.Class = ClassCompatible
			result.Diagnostic = fmt.Sprintf("%s schema and %s identity are present", sub, LedgerTable)
			return result, nil
		}
		if len(snapshot.Legacy) > 0 {
			result.Class = ClassLegacyAdoptable
			result.Diagnostic = fmt.Sprintf("%s tables are present with legacy %s; inspect historical migration bodies, then explicitly adopt into %s", sub, LegacyLedgerTable, LedgerTable)
			return result, nil
		}
		result.Class = ClassMisprovisioned
		result.Diagnostic = fmt.Sprintf("expected %s schema is present but no %s or %s identity exists; apply the direct PostgreSQL migration runner before cutover", sub, LedgerTable, IdentityTable)
		return result, nil
	}

	knownObserved := false
	for _, table := range snapshot.Tables {
		if knownTable(table) || table == LedgerTable || table == IdentityTable || table == LegacyLedgerTable {
			knownObserved = true
			break
		}
	}
	if !knownObserved {
		result.Class = ClassEmpty
		result.Diagnostic = fmt.Sprintf("no %s-owned tables or migration ledger observed", sub)
		return result, nil
	}
	result.Class = ClassMisprovisioned
	result.Diagnostic = fmt.Sprintf("expected subsystem %s tables %s are missing; observed tables/ledger=%s/%s. Do not copy by DSN name; provision the correct subsystem through direct 5432 migrations or identify the real source", sub, strings.Join(missing, ", "), strings.Join(result.ObservedTables, ", "), result.ObservedLedger)
	return result, nil
}

func validateIdentity(sub Subsystem, snapshot SchemaSnapshot) error {
	if len(snapshot.Namespaced) > 0 && len(snapshot.Identities) == 0 {
		return fmt.Errorf("expected subsystem %s, observed %s migration rows but no %s store identity; apply the direct 5432 namespaced migration runner", sub, LedgerTable, IdentityTable)
	}
	matchedLedger := false
	for _, identity := range snapshot.Namespaced {
		if identity.Subsystem != string(sub) {
			continue
		}
		matchedLedger = true
		if identity.Version <= 0 || identity.Filename == "" || !isLowerSHA256(identity.Checksum) {
			return fmt.Errorf("expected subsystem %s, observed incomplete migration identity in %s (filename=%q version=%d checksum=%q); apply the direct 5432 namespaced migration runner", sub, LedgerTable, identity.Filename, identity.Version, identity.Checksum)
		}
	}
	if len(snapshot.Namespaced) > 0 && !matchedLedger {
		return fmt.Errorf("expected subsystem %s, observed only another subsystem's migration identity in %s; integer versions cannot cross stores; inspect the database and run the %s migration adoption/apply procedure", sub, LedgerTable, sub)
	}
	if len(snapshot.Identities) > 0 {
		found := false
		for _, identity := range snapshot.Identities {
			if identity.Subsystem == string(sub) {
				found = true
				if identity.SchemaVersion <= 0 || !isLowerSHA256(identity.Contract) {
					return fmt.Errorf("expected subsystem %s, observed incomplete store identity in %s (schema_version=%d contract_checksum_sha256=%q); apply the direct 5432 namespaced migration runner", sub, IdentityTable, identity.SchemaVersion, identity.Contract)
				}
			}
		}
		if !found {
			return fmt.Errorf("expected subsystem %s, observed no matching store identity in %s; another subsystem cannot satisfy verify; inspect actual tables and re-run direct 5432 migrations", sub, IdentityTable)
		}
	}
	return nil
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isLowerSHA256(value string) bool {
	return isSHA256(value) && value == strings.ToLower(value)
}

func knownTable(table string) bool {
	for _, sub := range AllSubsystems {
		spec, _ := Spec(sub)
		for _, t := range spec.tables {
			if t.Name == table {
				return true
			}
		}
	}
	return false
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// Row is a canonical database row. []byte values are encoded by encoding/json
// as base64, preserving the exact BYTEA value in the manifest.
type Row map[string]any

// TableManifest is a deterministic count/hash record for one table.
type TableManifest struct {
	Table         string   `json:"table"`
	Columns       []string `json:"columns"`
	RowCount      int64    `json:"row_count"`
	ContentSHA256 string   `json:"content_sha256"`
}

// SubsystemManifest is the reconciliation unit for one projection.
type SubsystemManifest struct {
	Subsystem      Subsystem       `json:"subsystem"`
	Classification Classification  `json:"classification"`
	Tables         []TableManifest `json:"tables"`
	RowCount       int64           `json:"row_count"`
	ContentSHA256  string          `json:"content_sha256"`
}

// Manifest is the machine-readable output consumed by the operator cutover
// procedure. GeneratedAt is intentionally absent so two equivalent manifests
// have byte-identical JSON and can be checked into an incident record.
type Manifest struct {
	SchemaVersion         int                 `json:"schema_version"`
	Source                string              `json:"source"`
	Destination           string              `json:"destination,omitempty"`
	Frozen                bool                `json:"frozen"`
	Subsystems            []SubsystemManifest `json:"source_subsystems"`
	DestinationSubsystems []SubsystemManifest `json:"destination_subsystems,omitempty"`
}

// NewManifest creates a deterministic manifest shell.
func NewManifest(source, destination string, frozen bool) Manifest {
	return Manifest{SchemaVersion: 1, Source: source, Destination: destination, Frozen: frozen, Subsystems: make([]SubsystemManifest, 0, len(AllSubsystems))}
}

// AddSubsystem appends a subsystem in canonical order. Duplicate entries are
// refused because a manifest with two competing claims cannot prove cutover.
func (m *Manifest) AddSubsystem(value SubsystemManifest) error {
	if m == nil {
		return errors.New("cutover: nil manifest")
	}
	for _, existing := range m.Subsystems {
		if existing.Subsystem == value.Subsystem {
			return fmt.Errorf("cutover: duplicate subsystem %s in manifest", value.Subsystem)
		}
	}
	m.Subsystems = append(m.Subsystems, value)
	sort.Slice(m.Subsystems, func(i, j int) bool { return m.Subsystems[i].Subsystem < m.Subsystems[j].Subsystem })
	return nil
}

// CanonicalRowsHash returns a stable SHA-256 over table name, sorted column
// names, and sorted canonical row JSON. Ordering is independent of query
// planner choices while identity/body bytes remain exact.
func CanonicalRowsHash(table string, columns []string, rows []Row) (string, error) {
	if strings.TrimSpace(table) == "" {
		return "", errors.New("cutover: table name is required")
	}
	orderedColumns := append([]string(nil), columns...)
	sort.Strings(orderedColumns)
	rowJSON := make([][]byte, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			return "", errors.New("cutover: nil row cannot be hashed")
		}
		buf, err := json.Marshal(row)
		if err != nil {
			return "", fmt.Errorf("cutover: marshal %s row: %w", table, err)
		}
		rowJSON = append(rowJSON, buf)
	}
	sort.Slice(rowJSON, func(i, j int) bool { return string(rowJSON[i]) < string(rowJSON[j]) })
	envelope := struct {
		Table   string   `json:"table"`
		Columns []string `json:"columns"`
		Rows    [][]byte `json:"rows"`
	}{Table: table, Columns: orderedColumns, Rows: rowJSON}
	buf, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("cutover: marshal canonical %s manifest: %w", table, err)
	}
	digest := sha256.Sum256(buf)
	return hex.EncodeToString(digest[:]), nil
}

// CanonicalSubsystemHash combines table manifests and their hashes in table
// name order. It is the high-level source/destination equality assertion.
func CanonicalSubsystemHash(sub Subsystem, tables []TableManifest) (string, error) {
	ordered := append([]TableManifest(nil), tables...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Table < ordered[j].Table })
	for i := range ordered {
		if ordered[i].Table == "" || !isSHA256(ordered[i].ContentSHA256) {
			return "", fmt.Errorf("cutover: invalid %s table manifest for %q", sub, ordered[i].Table)
		}
	}
	buf, err := json.Marshal(struct {
		Subsystem Subsystem       `json:"subsystem"`
		Tables    []TableManifest `json:"tables"`
	}{Subsystem: sub, Tables: ordered})
	if err != nil {
		return "", fmt.Errorf("cutover: marshal %s manifest: %w", sub, err)
	}
	digest := sha256.Sum256(buf)
	return hex.EncodeToString(digest[:]), nil
}

// Reconcile refuses success when any table count or content hash differs.
func Reconcile(source, destination SubsystemManifest) error {
	if source.Subsystem != destination.Subsystem {
		return fmt.Errorf("cutover: manifest subsystem mismatch: source=%s destination=%s", source.Subsystem, destination.Subsystem)
	}
	if source.Classification.Class == ClassMisprovisioned || destination.Classification.Class == ClassMisprovisioned {
		return fmt.Errorf("cutover: %s is misprovisioned: source=%s destination=%s", source.Subsystem, source.Classification.Diagnostic, destination.Classification.Diagnostic)
	}
	if source.RowCount != destination.RowCount || source.ContentSHA256 != destination.ContentSHA256 {
		return fmt.Errorf("cutover: %s reconciliation refused: source rows/hash=%d/%s destination rows/hash=%d/%s; inspect the manifests and resume copy", source.Subsystem, source.RowCount, source.ContentSHA256, destination.RowCount, destination.ContentSHA256)
	}
	if len(source.Tables) != len(destination.Tables) {
		return fmt.Errorf("cutover: %s table omission: source has %d tables, destination has %d", source.Subsystem, len(source.Tables), len(destination.Tables))
	}
	byName := make(map[string]TableManifest, len(destination.Tables))
	for _, table := range destination.Tables {
		byName[table.Table] = table
	}
	for _, table := range source.Tables {
		other, ok := byName[table.Table]
		if !ok || table.RowCount != other.RowCount || table.ContentSHA256 != other.ContentSHA256 {
			return fmt.Errorf("cutover: %s table %s mismatch or omission: source rows/hash=%d/%s destination rows/hash=%d/%s", source.Subsystem, table.Table, table.RowCount, table.ContentSHA256, other.RowCount, other.ContentSHA256)
		}
	}
	return nil
}

// ValidateApplyDSN rejects the known transaction-pooled posture for any
// operation that can apply/bootstrap migrations. Unknown endpoints must carry
// an explicit direct=true query parameter so an operator cannot accidentally
// put an advisory lock on PgBouncer 6432.
func ValidateApplyDSN(dsn string) error {
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" {
		return errors.New("cutover: direct apply DSN is required")
	}
	if strings.Contains(trimmed, "pgbouncer_mode=transaction") || strings.Contains(trimmed, "pool_mode=transaction") {
		return fmt.Errorf("cutover: migration apply refuses transaction-pooled DSN; use direct PostgreSQL 5432, never PgBouncer 6432")
	}
	if strings.Contains(trimmed, ":6432") && !strings.Contains(trimmed, "direct=true") {
		return fmt.Errorf("cutover: migration apply DSN %q resolves to PgBouncer 6432; use a direct 5432 endpoint (or an explicitly proven session-affine endpoint with direct=true)", redactedDSN(trimmed))
	}
	return nil
}

func redactedDSN(dsn string) string {
	if at := strings.Index(dsn, "@"); at >= 0 {
		if scheme := strings.Index(dsn, "://"); scheme >= 0 {
			return dsn[:scheme+3] + "***@" + dsn[at+1:]
		}
	}
	return dsn
}

// RedactDSN removes URL userinfo before a DSN is written to a manifest or
// operator log. A bare keyword DSN is returned unchanged because it may be a
// local test value; callers should still avoid persisting credentials there.
func RedactDSN(dsn string) string { return redactedDSN(dsn) }
