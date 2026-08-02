// from_config.go — the exported config→snapshot projection.
// Previously the `config.SkillsConfig` → `skills.ConfigSnapshot`
// projection was an inline literal in `cmd/harbor/cmd_dev.go` with no
// reachable equivalent for headless consumers — the silent-field-
// drop class. The projection now lives next to the snapshot type so a
// new snapshot field and its projection land in the same package.
//
// Import direction: the subsystem imports `internal/config`
// additively; config stays a leaf. `SnapshotFromConfig` is optional
// sugar — `Open(ctx, snapshot, deps)` is unchanged and snapshot-first
// construction remains the headless golden path.

package skills

import "github.com/hurtener/Harbor/internal/config"

// SnapshotFromConfig projects the operator-facing `config.SkillsConfig`
// block onto the skills package's decoupled ConfigSnapshot. Every
// config field maps 1:1; the field-parity test in from_config_test.go
// fails the build when a new config field lands without a projection
// (or an explicit exclusion naming why). `Directory` is excluded —
// the store snapshot and the directory config feed two different
// constructors; `DirectoryFromConfig` is the directory's projection.
// `SessionPersonalCutover` is also excluded: it is restart-required runtime
// boot-maintenance authority consumed by session-overlay assembly, not a
// SkillStore driver setting or Directory constructor input.
func SnapshotFromConfig(cfg config.SkillsConfig) ConfigSnapshot {
	return ConfigSnapshot{
		Driver:    cfg.Driver,
		DSN:       cfg.DSN,
		Retrieval: RetrievalMode(cfg.Retrieval),
	}
}

// DirectoryFromConfig projects the operator-facing
// `config.SkillsConfig.Directory` block onto a DirectoryConfig for
// `NewDirectory`.
//
// `fallbackMaxEntries` is consulted when `skills.directory.max_entries`
// is unset (zero): the run-loop callers pass the resolved
// `planner.skills_context_max` value so the pre-111d injection-budget
// knob keeps capping the injected block's length. A zero fallback
// leaves MaxEntries at zero and `NewDirectory` applies its own
// package default (30). The Pinned slice is copied so the caller's
// config struct cannot mutate the directory after construction.
func DirectoryFromConfig(cfg config.SkillsConfig, fallbackMaxEntries int) DirectoryConfig {
	maxEntries := cfg.Directory.MaxEntries
	if maxEntries == 0 {
		maxEntries = fallbackMaxEntries
	}
	return DirectoryConfig{
		Pinned:     append([]string(nil), cfg.Directory.Pinned...),
		MaxEntries: maxEntries,
		Selection:  Selection(cfg.Directory.Selection),
	}
}
