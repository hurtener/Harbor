package sqlmigrate

import (
	"strings"
	"testing"
)

func TestMigrationLockName_UsesAuthoritativeDatabaseIdentity(t *testing.T) {
	// DSN credentials, query parameters, and spelling are intentionally absent
	// from this key: the migration runner obtains this identity from the direct
	// PostgreSQL session after it connects.
	sharedA := migrationLockName(migrationDatabaseIdentityKey("harbor", "10.0.0.7", 5432), "memory", "memory")
	sharedB := migrationLockName(migrationDatabaseIdentityKey("harbor", "10.0.0.7", 5432), "memory", "memory")
	if sharedA != sharedB {
		t.Fatalf("equivalent DSN identities produced different lock names: %q != %q", sharedA, sharedB)
	}
	differentDatabase := migrationLockName(migrationDatabaseIdentityKey("other", "10.0.0.7", 5432), "memory", "memory")
	if sharedA == differentDatabase {
		t.Fatalf("different database identities collided: %q", sharedA)
	}
	differentServer := migrationLockName(migrationDatabaseIdentityKey("harbor", "10.0.0.8", 5432), "memory", "memory")
	if sharedA == differentServer {
		t.Fatalf("different PostgreSQL server identities collided: %q", sharedA)
	}
}

func TestValidateLegacyShape_RejectsWrongSubsystemLedger(t *testing.T) {
	err := validateLegacyShape(
		PostgresMigrationSpec{Subsystem: "memory", RequiredTables: []string{"memory_state"}},
		map[int]namedLedgerRow{},
		map[int]struct{}{1: {}},
		map[string]struct{}{"schema_migrations": {}, "state_records": {}},
		map[string]map[string]struct{}{"state_records": {"event_id": {}}},
		map[string]struct{}{},
		"memory/postgres",
	)
	if err == nil {
		t.Fatal("wrong state ledger/schema was accepted as memory")
	}
	for _, want := range []string{"memory", "state_records", "memory_state", "remediation"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}
