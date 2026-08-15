package sqlmigrate

import "fmt"

// Mode controls how a Postgres-backed store treats its embedded migrations
// when the store opens. The zero value resolves to ModeApply so existing
// configurations retain their migration-at-boot behavior.
type Mode string

const (
	// ModeApply takes the subsystem's session advisory lock and applies every
	// embedded migration that is not yet recorded in schema_migrations.
	ModeApply Mode = "apply"
	// ModeVerify performs a read-only schema_migrations ledger check. It takes
	// no session advisory lock and executes no DDL, transaction, or write.
	ModeVerify Mode = "verify"
)

// Resolve validates m and returns its effective value. Empty resolves to
// ModeApply for backward compatibility with configurations created before
// migration modes existed.
func (m Mode) Resolve() (Mode, error) {
	switch m {
	case "", ModeApply:
		return ModeApply, nil
	case ModeVerify:
		return ModeVerify, nil
	default:
		return "", fmt.Errorf("unknown postgres migration mode %q (want %q or %q)", m, ModeApply, ModeVerify)
	}
}
