// from_config.go — the exported config→snapshot projections.
// Previously the `config.MemoryConfig` → `memory.ConfigSnapshot`
// projection was an inline literal in `cmd/harbor/cmd_dev.go` with a
// hand-maintained `harbortest/devstack` mirror — the silent-field-
// drop class. The projection now lives next to the snapshot type so a
// new snapshot field and its projection land in the same package.
//
// Import direction: the subsystem imports `internal/config`
// additively; config stays a leaf. Both `SnapshotFromConfig` and
// `RecallFromConfig` are optional sugar — `Open(ctx, snapshot, deps)` is
// unchanged and snapshot-first construction remains the headless golden path.

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
		MigrationMode:      cfg.MigrationMode,
		Strategy:           Strategy(cfg.Strategy),
		BudgetTokens:       cfg.BudgetTokens,
		RecoveryBacklogMax: cfg.RecoveryBacklogMax,
		RecentTurns:        cfg.RecentTurns,
		Retrieval:          RetrievalMode(cfg.Retrieval),
		RetrievalTopK:      cfg.RetrievalTopK,
	}
}

// RecallSettings carries the run-loop recall parameters the
// FetchMemoryBlocks helper reads. It is separate from ConfigSnapshot
// so a headless caller that builds its own RunSpec can supply recall
// settings without constructing a full config.MemoryConfig.
//
// The zero value is valid: Enabled=false means semantic recall is off
// and SearchTurns is never called.
type RecallSettings struct {
	// Enabled is true when memory.retrieval is "semantic". When false,
	// SearchTurns is never called and the prompt is unchanged.
	Enabled bool
	// TopK is the maximum number of recalled turns to inject. 0 falls
	// back to the memory subsystem default (DefaultSemanticTopK = 5).
	TopK int
	// MinScore is the cosine-similarity floor. Turns scoring below
	// this value are not injected. Valid range [-1, 1]; default 0.0.
	MinScore float64
}

// RecallFromConfig projects the operator-facing `config.MemoryConfig`
// block onto a RecallSettings value the run-loop memory step consumes.
// The field-parity test in from_config_test.go ensures every
// recall-relevant config field is either projected here or explicitly
// excluded with a reason, closing the silent-field-drop drift class.
func RecallFromConfig(cfg config.MemoryConfig) RecallSettings {
	return RecallSettings{
		Enabled:  RetrievalMode(cfg.Retrieval) == RetrievalSemantic,
		TopK:     cfg.RetrievalTopK,
		MinScore: cfg.RetrievalMinScore,
	}
}
