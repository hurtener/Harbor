// from_config.go — the exported config→snapshot projection. The
// projection lives next to the snapshot type so a new snapshot field
// and its projection land in the same package; the field-parity test
// in from_config_test.go fails the build when a `config.EmbeddingsConfig`
// field lands without a projection (or an explicit exclusion naming
// why).
//
// Import direction: the subsystem imports `internal/config`
// additively; config stays a leaf. `SnapshotFromConfig` is optional
// sugar — `Open(ctx, snapshot, deps)` is unchanged and snapshot-first
// construction remains the headless golden path.

package embeddings

import "github.com/hurtener/Harbor/internal/config"

// SnapshotFromConfig projects the operator-facing
// `config.EmbeddingsConfig` block onto the embeddings package's
// decoupled ConfigSnapshot. Every config field maps 1:1.
func SnapshotFromConfig(cfg config.EmbeddingsConfig) ConfigSnapshot {
	return ConfigSnapshot{
		Driver:     cfg.Driver,
		Provider:   cfg.Provider,
		Model:      cfg.Model,
		APIKey:     cfg.APIKey,
		BaseURL:    cfg.BaseURL,
		Timeout:    cfg.Timeout,
		Dimensions: cfg.Dimensions,
	}
}
