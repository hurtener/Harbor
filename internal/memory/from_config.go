// from_config.go — the exported config→snapshot projection.
// Previously the `config.MemoryConfig` → `memory.ConfigSnapshot`
// projection was an inline literal in `cmd/harbor/cmd_dev.go` with a
// hand-maintained `harbortest/devstack` mirror — the silent-field-
// drop class. The projection now lives next to the snapshot type so a
// new snapshot field and its projection land in the same package.
//
// Import direction: the subsystem imports `internal/config`
// additively; config stays a leaf. `SnapshotFromConfig` is optional
// sugar — `Open(ctx, snapshot, deps)` is unchanged and snapshot-first
// construction remains the headless golden path.

package memory

import "github.com/hurtener/Harbor/internal/config"

// SnapshotFromConfig projects the operator-facing `config.MemoryConfig`
// block onto the memory package's decoupled ConfigSnapshot. Every
// config field maps 1:1; the field-parity test in from_config_test.go
// fails the build when a new config field lands without a projection
// (or an explicit exclusion naming why).
func SnapshotFromConfig(cfg config.MemoryConfig) ConfigSnapshot {
	return ConfigSnapshot{
		Driver:             cfg.Driver,
		DSN:                cfg.DSN,
		Strategy:           Strategy(cfg.Strategy),
		BudgetTokens:       cfg.BudgetTokens,
		RecoveryBacklogMax: cfg.RecoveryBacklogMax,
		Retrieval:          RetrievalMode(cfg.Retrieval),
		RetrievalTopK:      cfg.RetrievalTopK,
	}
}
