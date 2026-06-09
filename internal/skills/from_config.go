// from_config.go — the exported config→snapshot projection (Phase 110c,
// D-196). Before 110c the `config.SkillsConfig` → `skills.ConfigSnapshot`
// projection was an inline literal in `cmd/harbor/cmd_dev.go` with no
// reachable equivalent for headless consumers — the D-155 silent-field-
// drop class. The projection now lives next to the snapshot type so a
// new snapshot field and its projection land in the same package.
//
// Import direction (D-193): the subsystem imports `internal/config`
// additively; config stays a leaf. `SnapshotFromConfig` is optional
// sugar — `Open(ctx, snapshot, deps)` is unchanged and snapshot-first
// construction remains the headless golden path.

package skills

import "github.com/hurtener/Harbor/internal/config"

// SnapshotFromConfig projects the operator-facing `config.SkillsConfig`
// block onto the skills package's decoupled ConfigSnapshot. Every
// config field maps 1:1; the field-parity test in from_config_test.go
// fails the build when a new config field lands without a projection
// (or an explicit exclusion naming why).
func SnapshotFromConfig(cfg config.SkillsConfig) ConfigSnapshot {
	return ConfigSnapshot{
		Driver: cfg.Driver,
		DSN:    cfg.DSN,
	}
}
